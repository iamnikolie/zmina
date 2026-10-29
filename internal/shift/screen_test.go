package shift

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBlockedScreenIsToldOnce(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)

	*f.now = f.now.Add(3 * time.Minute)
	f.tm.drafts["designer"] = "-"
	f.tm.screen = "╭──────╮\n Do you trust the files in this folder?\n ❯ 1. Yes, proceed\n   2. No, exit\n╰──────╯"

	f.s.Tick()
	assert.Empty(t, notesOf(f, "designer"), "not yet: it may be drawing")

	*f.now = f.now.Add(4 * time.Minute)
	f.s.Tick()
	require.Len(t, notesOf(f, "designer"), 1)
	assert.Contains(t, notesOf(f, "designer")[0], "designer: no input line on its screen for 4 min, waiting on: "+
		"Do you trust the files in this folder? / ❯ 1. Yes, proceed / 2. No, exit")

	f.s.State.Notes = nil
	*f.now = f.now.Add(10 * time.Minute)
	f.s.Tick()
	assert.Empty(t, notesOf(f, "designer"), "once per stretch")

	f.tm.drafts["designer"] = ""
	f.s.Tick()
	f.tm.drafts["designer"] = "-"
	*f.now = f.now.Add(5 * time.Minute)
	f.s.Tick()
	*f.now = f.now.Add(5 * time.Minute)
	f.s.Tick()
	assert.Len(t, notesOf(f, "designer"), 1, "a new stretch is told again")
}

func TestAPIErrorAndLimitAreToldOnce(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	*f.now = f.now.Add(3 * time.Minute)

	f.rg.errors["sid-designer"] = "API Error: Connection lost mid-response."
	f.s.Tick()
	f.s.Tick()
	assert.Equal(t, []string{"designer: its last turn ended in an API error: API Error: Connection lost mid-response."},
		notesOf(f, "designer"))

	f.s.State.Notes = nil
	f.tm.screen = "❯ what does a usage limit reached screen look like?\n\n  ⎿  You've hit your limit · resets 9pm\n\n❯ "
	f.s.Tick()
	f.s.Tick()
	assert.Equal(t, []string{"designer: stopped by a usage limit: ⎿ You've hit your limit · resets 9pm"},
		notesOf(f, "designer"), "the notice, not the question about it")
}

func TestTypedLineThatDoesNotLand(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	f.tm.onType = nil // this claude takes nothing

	*f.now = f.now.Add(3 * time.Minute)
	require.NoError(t, f.s.Queue("designer", "review the landing"))
	require.NoError(t, f.s.Queue("designer", "then the footer"))

	f.s.Tick()
	assert.Equal(t, []string{"review the landing"}, f.tm.typed["designer"])

	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	assert.Len(t, f.tm.typed["designer"], 1, "nothing new before the first line lands")

	// It sits in the input line: Enter was lost.
	f.tm.drafts["designer"] = "review the landing"
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	assert.Equal(t, []string{"Enter"}, f.tm.keys["designer"])

	// Gone from the input line and still not taken: typed again.
	f.tm.drafts["designer"] = ""
	*f.now = f.now.Add(2 * time.Minute)
	f.s.Tick()
	assert.Equal(t, []string{"review the landing", "review the landing"}, f.tm.typed["designer"])

	*f.now = f.now.Add(2 * time.Minute)
	f.s.Tick()
	*f.now = f.now.Add(2 * time.Minute)
	f.s.Tick()
	assert.NotEmpty(t, f.s.State.Role("designer").Failed, "never taken: kept in zmina queue")
	assert.Contains(t, strings.Join(f.tm.typed["designer"], "|"), "then the footer", "the queue moves on")
}

func TestTypedLineLands(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)

	*f.now = f.now.Add(3 * time.Minute)
	require.NoError(t, f.s.Queue("designer", "one"))
	require.NoError(t, f.s.Queue("designer", "two"))

	f.s.Tick()
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	assert.Equal(t, []string{"one", "two"}, f.tm.typed["designer"], "its status changed: the next line goes")
	assert.Empty(t, f.tm.keys["designer"])
}

// notesOf are the waiting health notes about one role.
func notesOf(f fix, role string) []string {
	var out []string

	for _, n := range f.s.State.Notes {
		if strings.HasPrefix(n, role+":") {
			out = append(out, n)
		}
	}

	return out
}

func TestRolePathLeavesTempShimsOut(t *testing.T) {
	assert.Equal(t, "/Users/me/.local/bin:/opt/homebrew/bin:/usr/bin",
		RolePath("/var/folders/5s/x/T/cmux-cli-shims/F3CB:/Users/me/.local/bin:/tmp/x/bin:/opt/homebrew/bin::/usr/bin"))
}

func TestUpInAnotherDir(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	dir := t.TempDir()

	require.NoError(t, f.s.UpWith("designer#2", UpOptions{Dir: dir}))
	assert.Equal(t, dir, f.s.State.Role("designer#2").Dir)
	assert.Equal(t, dir, f.tm.dirs["designer#2"])

	for _, v := range f.s.Views() {
		if v.Role == "designer#2" {
			assert.Equal(t, dir, v.Dir)
		}
	}
}

func TestTrustAnsweredOnlyInItsOwnWorktree(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	wt := filepath.Join(f.s.Crew.Layout().Worktrees(), "api-T-3")
	f.tm.screen = " Accessing workspace:\n " + wt + "\n Quick safety check: Is this a project you created or one you trust?\n\n" +
		" ❯ No, exit\n   Yes, I trust this folder\n\n Enter to confirm · Esc to cancel"

	require.NoError(t, f.s.UpWith("designer#2", UpOptions{Dir: wt}))
	require.NoError(t, f.s.UpWith("designer#3", UpOptions{Dir: t.TempDir()}))
	f.tm.drafts["designer#2"], f.tm.drafts["designer#3"] = "-", "-"

	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	assert.Equal(t, []string{"Down", "Enter"}, f.tm.keys["designer#2"], "its own task worktree, without the boot grace")
	assert.Empty(t, f.tm.keys["designer#3"], "any other folder is a person's call")
	assert.True(t, f.ev.has("designer#2", "trusted"))

	f.s.Tick()
	assert.Len(t, f.tm.keys["designer#2"], 2, "once per start")
}
