package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// OutputMax is how much of a plugin's stdout and of its stderr is kept, each: what goes to the event log and to
// `zmina plugin test`. The rest is read and thrown away, so a plugin that prints without end blocks on nothing.
const OutputMax = 4 << 10

// waitDelay is how long after a plugin ends, or is killed at its deadline, the pipes it left open are waited for.
var waitDelay = 2 * time.Second

// ProgramPrefix is the name a plugin's program has on PATH: zmina-plugin-<name>.
const ProgramPrefix = "zmina-plugin-"

// Program is a plugin's program as it will run.
type Program struct {
	// Path is the executable, absolute; Args the rest of its argv (run's other words).
	Path string
	Args []string
	// Via says how it was found: run (a path the crew file gives), crew dir (plugins/<name>) or PATH. A program found on PATH
	// is found on the PATH of whoever looks: the ticker's is the one `zmina ticker install` froze, and may differ.
	Via string
}

// ErrNotFound is a plugin whose program is nowhere it is looked for.
var ErrNotFound = errors.New("program not found")

// Resolve finds a plugin's program, in this order: the path run's first word gives (absolute, or read from the crew dir),
// <crew dir>/plugins/<name> (or <word> for a bare word of run), and last PATH (the word of run, or zmina-plugin-<name>).
// Nowhere else. What it finds must be a regular executable file, owned by the user or root, that group and others cannot write;
// one that is not is refused with why, not passed over for another. The path it returns is absolute.
func Resolve(p crew.Plugin, crewDir string) (Program, error) {
	word := ProgramPrefix + p.Name
	local := p.Name

	var args []string

	if len(p.Run) > 0 {
		word, local = p.Run[0], p.Run[0]

		// The program runs with no shell, so a leading ~ in an argument is the home of whoever runs it, as in the first word and in every
		// other command of the crew file given as a list (crew.ExpandArgvHome).
		if len(p.Run) > 1 {
			args = crew.ExpandArgvHome(p.Run[1:])
		}

		if strings.Contains(word, "/") {
			path := crew.ExpandHome(word)
			if !filepath.IsAbs(path) {
				path = filepath.Join(crewDir, path)
			}

			if err := Check(path); err != nil {
				return Program{}, err
			}

			return Program{Path: path, Args: args, Via: "run"}, nil
		}
	}

	// The crew's own directory comes before PATH: what is kept with the crew is what the owner put there, and what runs does not
	// change with the PATH of whoever asks.
	here := filepath.Join(crewDir, "plugins", local)
	if _, err := os.Stat(here); err == nil {
		if err := Check(here); err != nil {
			return Program{}, err
		}

		return Program{Path: here, Args: args, Via: "crew dir"}, nil
	}

	found, err := exec.LookPath(word)
	if err != nil {
		return Program{}, fmt.Errorf("%w: %s is not in %s and %s is not on PATH", ErrNotFound, local, filepath.Join(crewDir, "plugins"), word)
	}

	if abs, err := filepath.Abs(found); err == nil {
		found = abs
	}

	if err := Check(found); err != nil {
		return Program{}, err
	}

	return Program{Path: found, Args: args, Via: "PATH"}, nil
}

// Check says a program may be run: a regular file, executable, owned by the user or root, and not writable by group or others (a
// program anyone could change is a program anyone could speak for the owner through).
func Check(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrNotFound, path)
	}

	mode := fi.Mode()

	switch {
	case !mode.IsRegular():
		return fmt.Errorf("%s is not a regular file", path)
	case mode.Perm()&0o111 == 0:
		return fmt.Errorf("%s is not executable (chmod +x)", path)
	case mode.Perm()&0o022 != 0:
		return fmt.Errorf("%s is writable by group or others, so it is not run (chmod go-w)", path)
	}

	return checkOwner(path, fi, os.Getuid())
}

// checkOwner refuses a program that is owned by neither the user nor root: whoever owns a file may change it.
func checkOwner(path string, fi os.FileInfo, uid int) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}

	if owner := int(st.Uid); owner != uid && owner != 0 {
		return fmt.Errorf("%s is owned by uid %d, not by you or root, so it is not run", path, owner)
	}

	return nil
}

// Env is the environment a plugin gets: the caller's, less the variables zmina keeps as its own secrets (the cockpit's token),
// and ZMINA_PLUGIN (its name, which its answers' --by is checked against), ZMINA_PLUGIN_EVENT and ZMINA_CREW (the crew file).
func Env(base []string, name, event, crewPath string) []string {
	return append(tmux.WithoutEnv(base, tmux.SecretEnv), "ZMINA_PLUGIN="+name, "ZMINA_PLUGIN_EVENT="+event, "ZMINA_CREW="+crewPath)
}

// Spec is one run of a plugin: the program, what it gets on its stdin and in its environment, and its deadline.
type Spec struct {
	Program Program
	Dir     string
	Env     []string
	Stdin   []byte
	Timeout time.Duration
}

// Result is how a run ended. Stdout and Stderr are what it printed, at most OutputMax bytes each.
type Result struct {
	// Code is the exit code; -1 when it did not exit with one (not started, killed at its deadline).
	Code     int
	Stdout   string
	Stderr   string
	TimedOut bool
	// Err is why it did not run at all, or ended on a signal.
	Err  error
	Took time.Duration
}

// OK reports whether the plugin took the event: it ran and exited 0.
func (r Result) OK() bool { return r.Err == nil && !r.TimedOut && r.Code == 0 }

// Reason is how a run that failed is said in a line: the exit code with the first line of what it printed, the deadline,
// or why it did not start.
func (r Result) Reason(timeout time.Duration) string {
	switch {
	case r.TimedOut:
		return fmt.Sprintf("timed out after %s", timeout)
	case r.Err != nil:
		return r.Err.Error()
	case r.Code != 0:
		out := firstLine(r.Stderr)
		if out == "" {
			out = firstLine(r.Stdout)
		}

		if out != "" {
			return fmt.Sprintf("exit %d: %s", r.Code, out)
		}

		return fmt.Sprintf("exit %d", r.Code)
	}

	return ""
}

// firstLine is the first line of what a plugin printed, as one clean line: control characters gone (what it printed is data,
// and goes to the owner's terminal and the log), cut.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	line = strings.Join(strings.Fields(tmux.Printable(line)), " ")

	if r := []rune(line); len(r) > 200 {
		line = string(r[:199]) + "…"
	}

	return line
}

// Exec runs the program in a process group of its own with the event on its stdin, and ends the whole group at the
// deadline. It never reads the output as anything but data to keep.
func Exec(spec Spec) Result {
	ctx, cancel := context.WithTimeout(context.Background(), spec.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, spec.Program.Path, spec.Program.Args...)
	cmd.Dir, cmd.Env = spec.Dir, spec.Env
	cmd.Stdin = bytes.NewReader(spec.Stdin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay

	stdout, stderr := &capped{max: OutputMax}, &capped{max: OutputMax}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	start := time.Now()
	err := cmd.Run()
	res := Result{Stdout: stdout.buf.String(), Stderr: stderr.buf.String(), Took: time.Since(start)}

	var ee *exec.ExitError

	switch {
	case errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0:
		// It exited 0 and left a child holding its pipes (a plugin that backgrounds a send): it took the event; what the
		// child prints later is nobody's.
	case err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.TimedOut, res.Code = true, -1
	case err == nil:
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		res.Code = ee.ExitCode()
	case errors.As(err, &ee):
		res.Code, res.Err = -1, fmt.Errorf("ended on a signal: %s", ee.String())
	default:
		res.Code, res.Err = -1, err
	}

	return res
}

// capped keeps the first max bytes written and drops the rest, so a program that prints without end never fills memory;
// it keeps accepting writes so the program is not blocked on its pipe.
type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}

	return len(p), nil
}
