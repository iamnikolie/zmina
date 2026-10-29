package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// check is one line of the doctor's report.
type check struct {
	Name   string `json:"name"`
	Level  string `json:"level"` // ok, warn, fail
	Detail string `json:"detail"`
}

func (a *app) doctorCommand() *cobra.Command {
	var asJSON bool

	c := &cobra.Command{
		Use:   "doctor",
		Short: "Check the setup: crew file, tmux, claude, its session registry, the ticker, state, lock, tasks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			checks := a.diagnose()

			if asJSON {
				if err := a.printJSON(checks); err != nil {
					return err
				}
			} else {
				for _, c := range checks {
					fmt.Fprintf(a.out, "%-4s  %-9s %s\n", c.Level, c.Name, c.Detail)
				}
			}

			for _, c := range checks {
				if c.Level == "fail" {
					return &exitError{code: 1, msg: "zmina doctor: something needs fixing (fail above)"}
				}
			}

			return nil
		},
	}

	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	return c
}

func (a *app) diagnose() []check {
	var out []check

	add := func(name, level, format string, args ...any) {
		out = append(out, check{Name: name, Level: level, Detail: fmt.Sprintf(format, args...)})
	}

	c, err := crew.Load(a.crewPath())
	if err != nil {
		add("crew", "fail", "%v", err)

		return out
	}

	add("crew", "ok", "%s · %d roles · tmux -L %s · namespace %s", c.Path, len(c.Roles), c.Socket(), c.Namespace)

	for _, w := range c.Warnings {
		add("crew", "warn", "%s", w)
	}

	for _, n := range c.Names() {
		if err := c.Roles[n].DirErr(); err != nil {
			add("crew", "warn", "%v — it does not start", err)
		}
	}

	if t, err := tmux.New(c.Socket()); err != nil {
		add("tmux", "fail", "%v", err)
	} else {
		add("tmux", "ok", "%s", t.Bin)
	}

	a.checkClaude(c, add)
	a.checkRegistry(c, add)
	a.checkTicker(c, add)

	st, err := shift.Read(c.StateDir())
	if err != nil {
		add("state", "fail", "%v", err)
	} else {
		format := fmt.Sprintf("version %d", st.Version)
		if st.Legacy {
			format = "first format, converted on the next write (the original kept as state.json.v1.bak)"
		}

		add("state", "ok", "%s · %s · %d roles remembered", c.Layout().State(), format, len(st.Roles))

		if !st.Stopped.IsZero() {
			add("state", "warn", "the shift is stopped since %s: the ticker starts nothing (zmina start)",
				st.Stopped.Format("01-02 15:04"))
		}

		failed := 0
		for _, r := range st.Roles {
			failed += len(r.Failed)
		}

		if failed > 0 {
			add("state", "warn", "%d queued lines could not be typed: zmina queue ls", failed)
		}
	}

	if holder := lockHolder(c.Layout().Lock()); holder != "" {
		add("lock", "warn", "held right now by %s (fine while a command runs; not for minutes)", holder)
	} else {
		add("lock", "ok", "free")
	}

	if _, problems, err := taskStore(c).Scan(); err != nil {
		add("tasks", "fail", "%v", err)
	} else if len(problems) > 0 {
		for _, p := range problems {
			add("tasks", "warn", "%s", p)
		}
	} else {
		add("tasks", "ok", "readable")
	}

	if _, problems, err := laterStore(c).Scan(); err != nil {
		add("later", "fail", "%v", err)
	} else {
		for _, p := range problems {
			add("later", "warn", "%s", p)
		}
	}

	a.checkSockets(c, add)

	return out
}

func (a *app) checkClaude(c *crew.Crew, add func(name, level, format string, args ...any)) {
	// Roles start with the caller's PATH less temp-dir shims: look claude up the same way.
	rolePath := shift.RolePath(os.Getenv("PATH"))

	bin, err := lookIn(rolePath, c.Claude)
	if err != nil {
		add("claude", "fail", "%s is not on the roles' PATH (set claude: in the crew file)", c.Claude)

		return
	}

	if !strings.Contains(c.Claude, "/") && shift.RolePath(filepath.Dir(bin)) == "" {
		add("claude", "fail", "%s lies in a temp dir that goes away with its terminal; set claude: to the "+
			"installed binary", bin)

		return
	}

	note := ""
	if here, err := exec.LookPath(c.Claude); err == nil && here != bin {
		note = " (roles skip " + here + ", a temp-dir shim of this terminal)"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	help, err := exec.CommandContext(ctx, bin, "--help").CombinedOutput()
	if err != nil {
		add("claude", "warn", "%s --help failed: %v", bin, err)

		return
	}

	var missing []string

	for _, flag := range []string{"--name", "--resume", "--remote-control", "--model"} {
		if !strings.Contains(string(help), flag) {
			missing = append(missing, flag)
		}
	}

	if len(missing) > 0 {
		add("claude", "fail", "%s lacks %s, which zmina passes", bin, strings.Join(missing, ", "))

		return
	}

	add("claude", "ok", "%s%s", bin, note)
}

// lookIn finds an executable the way a process with that PATH would.
func lookIn(path, name string) (string, error) {
	if strings.Contains(name, "/") {
		return exec.LookPath(name)
	}

	for _, d := range filepath.SplitList(path) {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}

	return "", exec.ErrNotFound
}

// checkRegistry reads Claude Code's session registry: zmina tells idle from busy by it, and it is not a public
// interface.
func (a *app) checkRegistry(c *crew.Crew, add func(name, level, format string, args ...any)) {
	dir := filepath.Join(c.Namespace, "sessions")

	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		add("registry", "warn", "no session entries in %s yet (none running, or Claude Code moved them)", dir)

		return
	}

	var entry map[string]any

	raw, err := os.ReadFile(files[0])
	if err == nil {
		err = json.Unmarshal(raw, &entry)
	}

	if err != nil {
		add("registry", "fail", "%s does not parse: %v", files[0], err)

		return
	}

	var missing []string

	for _, k := range []string{"pid", "sessionId", "name", "status", "statusUpdatedAt"} {
		if _, ok := entry[k]; !ok {
			missing = append(missing, k)
		}
	}

	if len(missing) > 0 {
		add("registry", "fail", "entries lack %s: Claude Code changed its registry; idle/busy and delivery will "+
			"not work", strings.Join(missing, ", "))

		return
	}

	detail := fmt.Sprintf("%d entries in %s", len(files), dir)
	if v, ok := entry["version"].(string); ok {
		detail += " · written by Claude Code " + v
	}

	add("registry", "ok", "%s", detail)
}

func (a *app) checkTicker(c *crew.Crew, add func(name, level, format string, args ...any)) {
	if st, err := shift.Read(c.StateDir()); err == nil {
		switch since := time.Since(st.LastTick); {
		case st.LastTick.IsZero():
			add("ticker", "warn", "no beat recorded: zmina ticker install, then zmina ticker status")
		case since > 3*time.Minute:
			add("ticker", "fail", "no beat for %s: zmina ticker status, %s", age(since), tickLog(c))
		default:
			add("ticker", "ok", "beat %s ago", since.Round(time.Second))
		}
	}
}

// checkSockets finds other crews under ~/.zmina that would share this crew's tmux server (and so its sessions and
// ticker).
func (a *app) checkSockets(c *crew.Crew, add func(name, level, format string, args ...any)) {
	files, _ := filepath.Glob(crew.ExpandHome("~/.zmina/*/crew.yaml"))
	files = append(files, crew.ExpandHome("~/.zmina/crew.yaml"))

	for _, f := range files {
		if f == c.Path {
			continue
		}

		other, err := crew.Load(f)
		if err != nil {
			continue
		}

		if other.Socket() == c.Socket() {
			add("crews", "fail", "%s uses the same tmux server (%s) and ticker; give one of them name:", f, c.Socket())
		}
	}
}

// lockHolder is the pid written in the lock file while another zmina holds it, empty when it is free.
func lockHolder(path string) string {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

		return ""
	}

	raw, _ := os.ReadFile(path)
	if pid := strings.TrimSpace(string(raw)); pid != "" {
		return "zmina pid " + pid
	}

	return "another zmina"
}
