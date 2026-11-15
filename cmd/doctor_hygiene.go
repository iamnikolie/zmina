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

	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/tmux"
)

const (
	// loginWarn is how close a Claude namespace's login expiry has to be before doctor warns.
	loginWarn = 7 * 24 * time.Hour
	// askAge is how old an open ask for the owner gets before doctor names it.
	askAge = 7 * 24 * time.Hour
	// parkedAge is how old a parked item gets before doctor counts it.
	parkedAge = 14 * 24 * time.Hour
)

// loginTimeout is how long doctor waits for `claude auth status`; loginWaitDelay is how long after that it waits
// for the output pipes a child of claude still holds.
var loginTimeout, loginWaitDelay = 10 * time.Second, 2 * time.Second

// claudeAuth is what `claude auth status` prints.
type claudeAuth struct {
	LoggedIn       bool      `json:"loggedIn"`
	LoginExpiresAt time.Time `json:"loginExpiresAt"`
}

// checkLogin asks the claude binary whether the crew's namespace is logged in: a role started in a namespace that is
// not sits on the login screen and reads nothing.
func checkLogin(bin, namespace string, now time.Time, add func(name, level, format string, args ...any)) {
	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	defer cancel()

	// The deadline holds for what claude starts too: its process group is killed, and a child that kept stdout open
	// after claude exited is waited for loginWaitDelay at most.
	cmd := exec.CommandContext(ctx, bin, "auth", "status")
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+namespace)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = loginWaitDelay

	raw, err := cmd.Output()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // what claude left running
	}

	var st claudeAuth
	if jerr := json.Unmarshal(raw, &st); jerr != nil {
		why := "its output is not the JSON it prints"
		if err != nil {
			why = err.Error()
		}

		add("login", "warn", "%s: could not tell (claude auth status: %s)", namespace, why)

		return
	}

	switch {
	case !st.LoggedIn:
		add("login", "fail", "%s is not logged in: a role starts on the login screen (CLAUDE_CONFIG_DIR=%s claude, "+
			"then /login)", namespace, namespace)
	case !st.LoginExpiresAt.IsZero() && st.LoginExpiresAt.Sub(now) < loginWarn:
		add("login", "warn", "%s: the login expires %s", namespace, st.LoginExpiresAt.Local().Format("2006-01-02 15:04"))
	case !st.LoginExpiresAt.IsZero():
		add("login", "ok", "%s · expires %s", namespace, st.LoginExpiresAt.Local().Format("2006-01-02"))
	default:
		add("login", "ok", "%s", namespace)
	}
}

// checkHygiene names what piles up while the plumbing is fine: old questions for the owner, old parked items, copies
// of the crew file, worktrees of closed tasks. Each is one line with the command that shows it. st is the saved state
// (nil when it could not be read).
func checkHygiene(c *crew.Crew, st *shift.State, now time.Time, add func(name, level, format string, args ...any)) {
	var old []string

	for _, it := range askStore(c).Pending() {
		// A decided item waits on nobody, and the list stops showing it after a week.
		if it.Kind != asks.Decided && now.Sub(it.Created) >= askAge {
			old = append(old, it.ID)
		}
	}

	if len(old) > 0 {
		add("asks", "warn", "%d open for the owner over %s (%s): zmina todo ls", len(old), days(askAge), clipList(old))
	}

	old = old[:0]

	for _, it := range laterStore(c).Parked() {
		if now.Sub(it.Created) >= parkedAge {
			old = append(old, it.ID)
		}
	}

	if len(old) > 0 {
		add("later", "warn", "%d parked over %s (%s): take, drop or keep them, zmina later ls", len(old), days(parkedAge),
			clipList(old))
	}

	if copies := crewCopies(c.Path); len(copies) > 0 {
		noun := "copies"
		if len(copies) == 1 {
			noun = "copy"
		}

		add("crew", "warn", "%d %s of the crew file next to it (%s): zmina keeps the file's versions itself now "+
			"(zmina crew history lists them, --diff and --restore use them); the copies are old or hand-made, remove "+
			"them when you no longer need them", len(copies), noun, clipList(copies))
	}

	checkWorktrees(c, st, add)
	checkReview(c, add)
	checkReconcile(c, add)
}

// checkReview is the crew file's auto_review: on, with who reviews, or on and doing nothing since the routes name no reviewer.
// Nothing when it is off.
func checkReview(c *crew.Crew, add func(name, level, format string, args ...any)) {
	if c.AutoReview.Rounds == 0 {
		return
	}

	rule, ok := c.ReviewRule()
	if !ok {
		add("review", "warn", "auto_review is on and routes has no review rule: it does nothing")

		return
	}

	var who []string

	if rule.First != "" {
		who = append(who, "first "+rule.First)
	}

	if len(rule.Again) > 0 {
		who = append(who, "again "+strings.Join(rule.Again, ", "))
	}

	add("review", "ok", "auto_review: %d %s · %s", c.AutoReview.Rounds, plural(c.AutoReview.Rounds, "round", "rounds"),
		strings.Join(who, ", "))
}

// checkReconcile is the crew file's reconcile: which parts are on and what each does. Nothing when it is off. A spare crew has no
// ticker by design, and restart and orphans are the ticker's: they never act there, which is a warning.
func checkReconcile(c *crew.Crew, add func(name, level, format string, args ...any)) {
	r := c.Reconcile
	if !r.On() {
		return
	}

	var parts []string

	if r.Assign {
		parts = append(parts, "assign (a task, or a line from the owner or the dispatcher, starts a seat that is down)")
	}

	if r.Restart {
		parts = append(parts, fmt.Sprintf("restart (a seat that holds an open task is started again; %d failed starts in a row mark it broken)", r.MaxTries()))
	}

	if mode := r.OrphanMode(); mode != crew.OrphansOff {
		text := "orphans: " + mode + " (a session no role names is reported once"
		if mode == crew.OrphansDown {
			text += ", and taken down when nothing says it is used"
		}

		parts = append(parts, text+")")
	}

	add("reconcile", "ok", "reconcile: %s", strings.Join(parts, " · "))

	if c.Spare && (r.Restart || r.OrphanMode() != crew.OrphansOff) {
		add("reconcile", "warn", "this is a spare crew (spare: true): it has no ticker by design, and restart and orphans are the ticker's, so they never act here")
	}
}

// checkOrphans warns of each live session of the crew's tmux server that no role names, when reconcile: orphans is on: the ticker
// reports it once and, with orphans: down, takes it down when nothing says someone uses it. Nothing when it is off, or tmux does not answer
// (the tmux check says that).
func checkOrphans(c *crew.Crew, st *shift.State, list []tmux.SessionInfo, add func(name, level, format string, args ...any)) {
	mode := c.Reconcile.OrphanMode()
	if mode == crew.OrphansOff {
		return
	}

	if st == nil {
		st = shift.NewState()
	}

	s := &shift.Shift{Crew: c, State: st}

	for _, o := range s.Orphans(list) {
		who := "started by this crew's zmina"
		if !o.Tagged {
			who = "not proven to be this crew's: left alone"
		}

		what := "reported by the ticker, once"
		if mode == crew.OrphansDown && o.Tagged {
			what = "taken down by the ticker once it is idle with nothing queued and no terminal attached"
		}

		add("orphans", "warn", "%s: a tmux session no role of the crew file names (%s; %s): zmina attach %s", o.Name, who, what, o.Name)
	}
}

// checkWorktrees is the closed tasks' worktrees: with worktree_gc on the ticker sweeps them daily, and what is left
// is what the sweep keeps (unmerged work, changes), so it is not a warning; with it off they pile up until someone
// runs `zmina task gc`.
func checkWorktrees(c *crew.Crew, st *shift.State, add func(name, level, format string, args ...any)) {
	n := closedWorktrees(c)

	if !c.WorktreeGCOn() {
		if n > 0 {
			add("worktrees", "warn", "%d closed tasks keep their worktree: zmina task gc --dry-run; "+
				"worktree_gc: daily in the crew file has the ticker remove the merged ones once a day", n)
		}

		return
	}

	last := "no sweep yet"
	if st != nil && !st.WorktreeGCAt.IsZero() {
		last = "last sweep " + st.WorktreeGCAt.In(c.Location()).Format("2006-01-02 15:04")
	}

	if n > 0 {
		add("worktrees", "ok", "worktree_gc: daily · %s · %d closed tasks still keep theirs (unmerged or with changes: "+
			"zmina task gc --dry-run)", last, n)

		return
	}

	add("worktrees", "ok", "worktree_gc: daily · %s", last)
}

// crewCopies are the files beside the crew file whose name starts with its own and says bak.
func crewCopies(path string) []string {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return nil
	}

	base := filepath.Base(path)

	var out []string

	for _, e := range entries {
		if n := e.Name(); n != base && strings.HasPrefix(n, base+".") && strings.Contains(n, "bak") {
			out = append(out, n)
		}
	}

	return out
}

// closedWorktrees counts done, dropped or parked tasks whose worktree is still on disk.
func closedWorktrees(c *crew.Crew) int {
	all, err := taskStore(c).All()
	if err != nil {
		return 0
	}

	n := 0

	for _, t := range all {
		if t.Worktree == "" || t.State.Active() {
			continue
		}

		if st, err := os.Stat(t.Worktree); err == nil && st.IsDir() {
			n++
		}
	}

	return n
}

// clipList is up to three items and how many more.
func clipList(items []string) string {
	if len(items) <= 3 {
		return strings.Join(items, ", ")
	}

	return fmt.Sprintf("%s and %d more", strings.Join(items[:3], ", "), len(items)-3)
}

// days is a whole number of days, as "7 days".
func days(d time.Duration) string { return fmt.Sprintf("%d days", int(d/(24*time.Hour))) }
