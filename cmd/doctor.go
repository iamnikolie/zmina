package cmd

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/peer"
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
		Short: "Check the setup (crew file, tmux, claude and its login, the ticker, state, lock, tasks) and what piles up",
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

	for _, ck := range lessonsDoctor(c) {
		add("lessons", ck[0], "%s", ck[1])
	}

	if t, err := tmux.New(c.Socket()); err != nil {
		add("tmux", "fail", "%v", err)
	} else {
		add("tmux", "ok", "%s", t.Bin)
	}

	checkDisk(c, add)

	hasClaude, hasCodex := false, false
	for _, r := range c.Roles {
		if r.IsCodex() {
			hasCodex = true
		} else {
			hasClaude = true
		}
	}
	if hasClaude {
		a.checkClaude(c, add)
		a.checkRegistry(c, add)
	}
	checkBoot(c, add)
	checkContext(c, add)
	checkModels(c, add)
	if hasCodex {
		a.checkCodex(c, add)
	}
	checkToolVersions(c, hasClaude, hasCodex, add)
	a.checkTicker(c, add)
	checkCourier(c, add)

	st, err := shift.Read(c.StateDir())
	if err != nil {
		add("state", "fail", "%v", err)
		st = nil
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

		failed, attention := 0, 0
		for _, r := range st.Roles {
			for _, f := range r.Failed {
				if f.Unclear() {
					attention++
				} else {
					failed++
				}
			}
		}

		if attention > 0 {
			add("state", "warn", "%d queued lines need attention, they may have reached their role: zmina queue ls", attention)
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

	checkLockHolds(c, time.Now(), add)

	if _, problems, err := taskStore(c).Scan(); err != nil {
		add("tasks", "fail", "%v", err)
	} else if len(problems) > 0 {
		for _, p := range problems {
			add("tasks", "warn", "%s", p)
		}
	} else {
		add("tasks", "ok", "readable")
	}

	if bad, err := askStore(c).Unreadable(); err != nil {
		add("asks", "fail", "%v", err)
	} else if len(bad) > 0 {
		for _, file := range slices.Sorted(maps.Keys(bad)) {
			add("asks", "warn", "%s (on no list until mended)", bad[file])
		}
	} else {
		add("asks", "ok", "readable")
	}

	if _, problems, err := laterStore(c).Scan(); err != nil {
		add("later", "fail", "%v", err)
	} else {
		for _, p := range problems {
			add("later", "warn", "%s", p)
		}
	}

	if _, problems, err := alarmStore(c).Scan(); err != nil {
		add("alarms", "fail", "%v", err)
	} else {
		for _, p := range problems {
			add("alarms", "warn", "%s", p)
		}

		checkNotify(c, add)
	}

	checkHygiene(c, st, time.Now(), add)
	checkFailureHolds(c, st, time.Now(), add)
	checkStores(c, time.Now(), add)
	checkRotations(c, time.Now(), add)
	a.checkSockets(c, add)
	checkVersions(c, st, add)
	a.checkNode(add)

	return out
}

// checkBoot shows what zmina adds to every fresh role session's prefix: its standing rules, by what the crew file
// turns on, and whether the skill list (Claude Code's own, usually larger) is trimmed.
func checkBoot(c *crew.Crew, add func(name, level, format string, args ...any)) {
	s := &shift.Shift{Crew: c, Peer: peer.Inbox{}, CodexInbox: codex.Inbox{}, Self: self(), State: shift.NewState()}

	most, mostRole, total, n := 0, "", 0, 0
	for _, name := range c.Names() {
		size := s.RulesSize(c.Roles[name])
		total += size
		n++

		if size > most {
			most, mostRole = size, name
		}
	}

	if n == 0 {
		return
	}

	add("boot", "ok", "zmina's rules at a fresh start: %s characters a role on average (about %s tokens), %s most "+
		"(%s); they stay in every request of the session", kChars(total/n), kChars(total/n/4), mostRole, kChars(most))

	trimmed := c.Skills != nil
	for _, r := range c.Roles {
		trimmed = trimmed || r.Skills != nil
	}

	for _, r := range c.Roles {
		if !r.IsCodex() && !trimmed {
			add("boot", "ok", "skill list not trimmed: every Claude role is sent Claude Code's whole list with every "+
				"request, often several thousand tokens (skills: — zmina skill, The crew file)")

			break
		}
	}
}

// kChars is a count as 4.4k, or the number below a thousand.
func kChars(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}

	return strconv.FormatFloat(float64(n)/1000, 'f', 1, 64) + "k"
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

	if strings.TrimSpace(string(help)) == "" {
		add("claude", "warn", "%s --help printed nothing, so the flags zmina passes cannot be checked (a full disk can "+
			"leave a claude silent: see disk above)", bin)

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
	checkLogin(bin, c.Namespace, time.Now(), add)
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
	for _, l := range registryReport(c.Namespace, a.roleOwners) {
		add("registry", l.level, "%s", l.text)
	}
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

func (a *app) checkCodex(c *crew.Crew, add func(name, level, format string, args ...any)) {
	bin, err := lookIn(shift.RolePath(os.Getenv("PATH")), c.Codex)
	if err != nil {
		add("codex", "fail", "%s is not on the roles' PATH (set codex: in the crew file)", c.Codex)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	help, err := exec.CommandContext(ctx, bin, "app-server", "--help").CombinedOutput()
	if err != nil || !strings.Contains(string(help), "stdio") {
		add("codex", "fail", "%s needs App Server with stdio transport", bin)
		return
	}
	add("codex", "ok", "%s · App Server stdio · adapter registry %s", bin, filepath.Join(c.StateDir(), "codex"))
	for _, r := range c.Roles {
		if r.IsCodex() && r.CodexTransport == "shared" {
			home := r.CodexHome
			if home == "" {
				home = crew.ExpandHome("~/.codex")
			}
			socket := filepath.Join(home, "app-server-control", "app-server-control.sock")
			if st, err := os.Stat(socket); err != nil || st.Mode()&os.ModeSocket == 0 {
				add("codex", "fail", "role %s: shared daemon socket unavailable at %s; codex app-server daemon start", r.Name, socket)
			} else {
				add("codex", "ok", "role %s: shared daemon socket %s", r.Name, socket)
			}
		}
		if r.IsCodex() && r.Profile != "" {
			home := r.CodexHome
			if home == "" {
				home = crew.ExpandHome("~/.codex")
			}
			if _, err := codex.LoadProfile(home, r.Profile); err != nil {
				add("codex", "fail", "role %s: %v", r.Name, err)
			} else {
				add("codex", "ok", "role %s: profile %s", r.Name, r.Profile)
			}
		}
	}
}
