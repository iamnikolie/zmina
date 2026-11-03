package shift

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A queued line goes once the role's boot turn is over, not boot_grace after its start; a turn that ended before
// the start (a resumed conversation) is not this start's boot.
func TestQueuedLineGoesOnceTheBootTurnEnds(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	start := *f.now
	require.NoError(t, f.s.Queue("designer", "[dispatcher] T-1 packet"))
	f.status("designer", "idle", start)
	f.rg.endedAt["sid-designer"] = start.Add(-time.Hour)

	*f.now = start.Add(20 * time.Second)
	f.s.Tick()
	assert.Empty(t, f.tm.typed["designer"], "a turn of the resumed conversation is not the boot")

	f.rg.endedAt["sid-designer"] = start.Add(25 * time.Second)
	*f.now = start.Add(30 * time.Second)
	f.s.Tick()
	assert.Equal(t, []string{"[dispatcher] T-1 packet"}, f.tm.typed["designer"], "typed once the boot turn ended")
}

// Without a boot turn on record the line still goes after boot_grace, the longest a boot is waited for.
func TestQueuedLineWaitsBootGraceAtMost(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	start := *f.now
	require.NoError(t, f.s.Queue("designer", "[dispatcher] T-1 packet"))
	f.status("designer", "idle", start)

	*f.now = start.Add(time.Minute)
	f.s.Tick()
	assert.Empty(t, f.tm.typed["designer"])

	*f.now = start.Add(f.s.Crew.BootGraceD())
	f.s.Tick()
	assert.Equal(t, []string{"[dispatcher] T-1 packet"}, f.tm.typed["designer"])
}

// A line for a role on its own channel is posted as soon as its boot turn is over.
func TestNativePostGoesOnceTheBootTurnEnds(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	start := *f.now
	f.status("designer", "idle", start)
	require.NoError(t, f.s.Queue("designer", "[dispatcher] T-1 packet"))

	*f.now = start.Add(20 * time.Second)
	assert.Nil(t, f.s.native("designer", *f.now))
	assert.Empty(t, f.s.deliverInbox("designer", *f.now), "the boot is still running")

	f.rg.endedAt["sid-designer"] = start.Add(25 * time.Second)
	*f.now = start.Add(30 * time.Second)
	assert.Equal(t, "posted to designer: [dispatcher] T-1 packet", f.s.deliverInbox("designer", *f.now))
	assert.Equal(t, []string{"[dispatcher] T-1 packet"}, p.posted["sid-designer"])
}

// A dialog that holds a boot is told to the dispatcher within a minute, once, not boot_grace and blockedAfter later.
func TestBootDialogIsToldAtOnce(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	start := *f.now
	f.tm.drafts["designer"] = "-"
	f.tm.screen = "Do you want to use this API key?\n❯ 1. No\n  2. Yes"

	*f.now = start.Add(30 * time.Second)
	assert.Empty(t, f.s.watchScreen("designer", *f.now), "Claude Code is still drawing its screen")

	*f.now = start.Add(time.Minute)
	notes := f.s.watchScreen("designer", *f.now)
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "designer: up 60 s and still no input line, a dialog holds its boot")
	assert.Contains(t, notes[0], "use this API key")
	assert.True(t, f.ev.has("designer", "blocked"))

	for _, d := range []time.Duration{2 * time.Minute, 10 * time.Minute} {
		*f.now = start.Add(d)
		assert.Empty(t, f.s.watchScreen("designer", *f.now), "told once per stretch")
	}

	// Answered: the input line is back, and a later dialog is news again.
	f.tm.drafts["designer"] = ""
	*f.now = start.Add(11 * time.Minute)
	assert.Empty(t, f.s.watchScreen("designer", *f.now))
	f.tm.drafts["designer"] = "-"
	*f.now = start.Add(12 * time.Minute)
	assert.Empty(t, f.s.watchScreen("designer", *f.now))
	*f.now = start.Add(16 * time.Minute)
	assert.True(t, strings.Contains(strings.Join(f.s.watchScreen("designer", *f.now), "|"), "no input line on its screen"))
}
