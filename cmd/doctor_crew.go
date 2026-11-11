package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

// checkModels warns about roles that name no model: they run on the default of the Claude Code or Codex that starts
// them, so a change of that default reprices them without the crew file saying so. A warning, never a failure.
func checkModels(c *crew.Crew, add func(name, level, format string, args ...any)) {
	var open []string

	for _, name := range c.Names() {
		if !pinsModel(c.Roles[name]) {
			open = append(open, name)
		}
	}

	if len(open) == 0 {
		add("models", "ok", "every role names its model")

		return
	}

	noun := "roles"
	if len(open) == 1 {
		noun = "role"
	}

	add("models", "warn", "%d %s with no model: (%s) run on whatever default their CLI has today, so a change of it "+
		"reprices them unseen; zmina seat set <role> model=<name> pins one", len(open), noun, clipList(open))
}

// pinsModel reports whether a role's model is named by the crew file: model:, a --model among its args (Claude), or a
// profile (Codex), which carries the model of its own.
func pinsModel(r *crew.Role) bool {
	if r.Model != "" {
		return true
	}

	if r.IsCodex() {
		return r.Profile != ""
	}

	return slices.ContainsFunc(r.Args, func(a string) bool { return a == "--model" || strings.HasPrefix(a, "--model=") })
}

// The releases of the agents' CLIs this zmina was tried with. zmina reads what neither publishes as an interface: the
// screen of a Claude Code session (its input line, dialogs, usage-limit notice), its session registry and transcript
// markers, and Codex's rollout files. A release outside the range is not known to break them, and is not known to work.
//
// To move the range: after a release has run a live crew for a day with zmina doctor clean and the tests of
// internal/tmux and internal/shift green against screens captured from it, raise max to its version; raise min only
// when zmina starts to need something a release added (the native delivery of Claude Code needed 2.1.224). An empty
// bound is no bound.
var (
	claudeCodeTested = versionRange{min: "2.1.224", max: "2.1.322"}
	codexTested      = versionRange{max: "0.167.0"}
)

// versionTimeout is how long doctor waits for `<cli> --version`; versionWaitDelay how long after that for the output
// pipes a child of the CLI still holds.
var versionTimeout, versionWaitDelay = 5 * time.Second, time.Second

// versionRange is the oldest and newest release a zmina was tested with, as dotted numbers (2.1.322).
type versionRange struct{ min, max string }

func (r versionRange) String() string {
	switch {
	case r.min != "" && r.max != "":
		return r.min + " to " + r.max
	case r.max != "":
		return "up to " + r.max
	}

	return "from " + r.min
}

// versionRe is the dotted number a CLI's --version line carries ("2.1.322 (Claude Code)", "codex-cli 0.167.0").
var versionRe = regexp.MustCompile(`\b(\d+)\.(\d+)\.(\d+)\b`)

// releaseOf reads a release out of a --version line; false when there is none.
func releaseOf(text string) ([3]int, bool) {
	m := versionRe.FindStringSubmatch(text)
	if m == nil {
		return [3]int{}, false
	}

	var v [3]int

	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}

	return v, true
}

// checkToolVersions prints the installed Claude Code (and Codex, when a role runs on it) and warns when it is outside
// the range this build was tested with. A version it cannot read is a warning too, never a failure: the binary's
// presence and flags are what the claude and codex checks fail on.
func checkToolVersions(c *crew.Crew, hasClaude, hasCodex bool, add func(name, level, format string, args ...any)) {
	path := shift.RolePath(os.Getenv("PATH"))

	if hasClaude {
		checkRelease("claude", "Claude Code", c.Claude, path, claudeCodeTested, add)
	}

	if hasCodex {
		checkRelease("codex", "Codex", c.Codex, path, codexTested, add)
	}
}

func checkRelease(name, title, command, path string, tested versionRange, add func(name, level, format string, args ...any)) {
	bin, err := lookIn(path, command)
	if err != nil {
		return
	}

	text, err := runVersion(bin)

	got, ok := releaseOf(text)
	if !ok {
		why := "its output has no version number"
		if err != nil {
			why = err.Error()
		}

		add(name, "warn", "%s: could not read the version (%s --version: %s); zmina was tried with %s", title, bin, why, tested)

		return
	}

	shown := fmt.Sprintf("%d.%d.%d", got[0], got[1], got[2])

	switch lo, hi := boundOf(tested.min), boundOf(tested.max); {
	case lo != nil && slices.Compare(got[:], lo[:]) < 0:
		add(name, "warn", "%s %s is older than the oldest this zmina was tried with (%s): what zmina reads from it may be missing", title, shown, tested)
	case hi != nil && slices.Compare(got[:], hi[:]) > 0:
		add(name, "warn", "%s %s is newer than the newest this zmina was tried with (%s): the screen, session registry and "+
			"transcript markers zmina reads are not a public interface, so idle, dialogs and limits may be misread until "+
			"zmina is updated", title, shown, tested)
	default:
		add(name, "ok", "%s %s (this zmina was tried with %s)", title, shown, tested)
	}
}

// boundOf is one end of the range as a release, nil when it is not set.
func boundOf(s string) *[3]int {
	if v, ok := releaseOf(s); ok {
		return &v
	}

	return nil
}

// runVersion is the first line `<bin> --version` prints, under a short deadline that holds for what the CLI starts too.
func runVersion(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = versionWaitDelay

	raw, err := cmd.Output()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // what the CLI left running
	}

	if ctx.Err() != nil {
		return "", fmt.Errorf("no answer in %s", versionTimeout)
	}

	first, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")

	return first, err
}

// checkNotify says whether anything can reach the owner's phone. A crew with no notify key at all gets a warning:
// reminders and asks that hold a role up then only land on the todo, and nobody may have meant that. notify: off is the
// owner saying he does, and gets a plain line.
func checkNotify(c *crew.Crew, add func(name, level, format string, args ...any)) {
	switch {
	case !c.Notify.Empty():
	case c.NotifyOff():
		add("alarms", "ok", "no notify channel set: nothing is pushed to the phone")
	default:
		add("alarms", "warn", "no notify command: a reminder for you and an ask that holds a role up only land on "+
			"the todo; nothing reaches your phone (notify: in the crew file; notify: off if you want none)")
	}
}
