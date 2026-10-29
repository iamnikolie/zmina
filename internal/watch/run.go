package watch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
)

// Env is what a run needs besides the watch: where the crew keeps its state, the stored fingerprints and the
// kind's cache, both read under the lock before the run.
type Env struct {
	CrewPath string
	// Dir is the watch's private directory: its seen file, and the exec source's cache dir.
	Dir   string
	Seen  Seen
	Cache json.RawMessage
	// GL is the gl binary (default: gl from PATH, else ~/.local/bin/gl).
	GL string
}

// Run runs one watch's source, bounded by its timeout. It holds no lock: the caller diffs the result under one.
func Run(ctx context.Context, w *crew.Watch, env Env) Result {
	ctx, cancel := context.WithTimeout(ctx, w.TimeoutD())
	defer cancel()

	var res Result

	switch w.Kind {
	case crew.WatchExec:
		res = runExec(ctx, w, env)
	case crew.WatchGitLabMR:
		res = (&GitLab{W: w, Bin: env.GL, Seen: env.Seen}).Run(ctx, env.Cache)
	default:
		res.Err = fmt.Errorf("kind %q", w.Kind)
	}

	if res.Err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.Err = fmt.Errorf("timed out after %s", w.TimeoutD())
	}

	return res
}

// runExec starts the command with the seen file and its cache dir in the environment, and reads one JSON event per
// line from its output. Exit 0 means the output is the whole current set.
func runExec(ctx context.Context, w *crew.Watch, env Env) Result {
	if err := os.MkdirAll(env.Dir, 0o700); err != nil {
		return Result{Err: err}
	}

	seen := env.Dir + ".seen.json"
	if err := writeJSON(seen, env.Seen); err != nil {
		return Result{Err: err}
	}

	line := w.Run.Line()
	cmd := exec.CommandContext(ctx, line[0], line[1:]...)
	cmd.Dir = w.Dir
	cmd.Env = append(os.Environ(), "ZMINA_WATCH="+w.Name, "ZMINA_CREW="+env.CrewPath, "ZMINA_WATCH_SEEN="+seen,
		"ZMINA_WATCH_DIR="+env.Dir)
	// Its own process group, so a timeout ends whatever the command started too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second

	stdout, stderr := &capped{max: MaxOutput}, &capped{max: MaxStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	err := cmd.Run()
	res := Result{Stderr: lines(stderr.String())}

	if err != nil {
		res.Err = fmt.Errorf("%s: %s", w.Run.Line()[0], firstOr(res.Stderr, err.Error()))

		return res
	}

	if stdout.over {
		// Part of an event set would read as everything else being gone.
		res.Err = fmt.Errorf("%s: printed more than %d bytes; the run is not used", w.Run.Line()[0], MaxOutput)

		return res
	}

	res.Events, res.Err = ParseEvents(stdout.Bytes())

	return res
}

// MaxOutput bounds what a watch command may print as events; MaxStderr what of its stderr is kept.
var (
	MaxOutput = 1 << 20
	MaxStderr = 64 << 10
)

// capped keeps the first max bytes written and drops the rest, so a command that prints without end never fills
// memory; it keeps accepting writes so the command is not blocked. It holds its buffer rather than embedding it:
// an embedded bytes.Buffer would bring ReadFrom, and io.Copy would use that and skip the limit.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room < len(p) {
		c.over = true
		if room > 0 {
			c.buf.Write(p[:room])
		}

		return len(p), nil
	}

	return c.buf.Write(p)
}

func (c *capped) String() string { return c.buf.String() }

func (c *capped) Bytes() []byte { return c.buf.Bytes() }

// ParseEvents reads one JSON event per line; blank lines are skipped, anything else fails the run.
func ParseEvents(out []byte) ([]Event, error) {
	var events []Event

	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	for n := 1; sc.Scan(); n++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}

		var e Event
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			return nil, fmt.Errorf("output line %d is not a JSON event: %s", n, cut(text, 80))
		}

		e.Text = OneLine(e.Text)
		if e.Key == "" || e.Fingerprint == "" || e.Text == "" {
			return nil, fmt.Errorf("output line %d: key, fingerprint and text are required", n)
		}

		events = append(events, e)
	}

	return events, sc.Err()
}

func writeJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, path)
}

// Dir is a watch's private directory under the crew's state dir.
func Dir(stateDir, name string) string { return crew.Layout{Dir: stateDir}.Watch(name) }

func lines(s string) []string {
	var out []string

	for l := range strings.SplitSeq(strings.TrimRight(s, "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}

	return out
}

func firstOr(ls []string, fallback string) string {
	if len(ls) > 0 {
		return strings.TrimSpace(ls[0])
	}

	return fallback
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}

	return string(r[:n-1]) + "…"
}
