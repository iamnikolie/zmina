package shift

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// takenLine types an untracked line into the designer, which then takes it, works and is idle again an hour later:
// nothing else waits for it, so nothing but the beat, a command or the end of the session will look at the record of it.
func takenLine(t *testing.T) fix {
	t.Helper()

	f := typedFixture(t, crewYAML, "designer")
	require.NoError(t, f.s.Queue("designer", "do the thing"))
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	require.NotNil(t, f.s.State.Role("designer").Sent)

	*f.now = f.now.Add(time.Minute)
	f.status("designer", "busy", *f.now)
	*f.now = f.now.Add(time.Hour)
	f.status("designer", "idle", *f.now)

	return f
}

// zmina send to a role that took its last typed line is not refused for a line "not landed yet": the record of the one
// before is closed first, as the queue's delivery closes it.
func TestSendAfterTheRoleTookItsTypedLineIsNotRefused(t *testing.T) {
	f := takenLine(t)

	require.NoError(t, f.s.Send("designer", "/compact"))

	assert.Equal(t, []string{"do the thing", "/compact"}, f.tm.typed["designer"])
	assert.Nil(t, f.s.State.Role("designer").Sent, "the line before is closed")
}

// A line that is still in the input line, or that nothing shows taken, still blocks it, and the refusal is saved with
// what the look changed.
func TestSendAfterALineNothingShowsTakenIsRefusedAndSaysSo(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer")
	f.tm.onType = nil // its status never moves: nothing shows the line taken
	require.NoError(t, f.s.Queue("designer", "do the thing"))
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	err := f.s.Send("designer", "/compact")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "has not landed yet")
	assert.ErrorIs(t, err, ErrPartial, "the state is saved with the error")
	assert.Equal(t, []string{"do the thing"}, f.tm.typed["designer"])
}

// The beat closes the record of a line its role took, so queue ls does not list it as one in flight for the rest of the
// in-flight age.
func TestBeatClosesATypedLineTheRoleTook(t *testing.T) {
	f := takenLine(t)
	assert.True(t, listsTyped(f, "designer"), "listed while open")

	f.s.Tick()

	assert.Nil(t, f.s.State.Role("designer").Sent)
	assert.False(t, listsTyped(f, "designer"))
}

// listsTyped reports whether queue ls lists a typed line in flight for the role.
func listsTyped(f fix, role string) bool {
	for _, q := range f.s.Queues() {
		if q.Role == role && q.Sent != nil {
			return true
		}
	}

	return false
}

// A role put down long after its typed line was processed: the line is closed while its status can be read, and
// nothing is failed or told when the session is gone.
func TestDownOfAnIdleRoleLongAfterItsTypedLineWasProcessedFailsNothing(t *testing.T) {
	f := takenLine(t)

	require.NoError(t, f.s.Down("designer", DownOptions{}))

	rs := f.s.State.Role("designer")
	assert.Nil(t, rs.Sent, "closed by the down")

	f.s.Tick()

	assert.Empty(t, rs.Failed)
	assert.Empty(t, notesOf(f, "designer"), "the dispatcher is told nothing")
	assert.Zero(t, f.eventCount("dropped"))
}

// The session is gone and no status can be read: its conversation shows a turn that ended after the line, so the role
// took it. Closed, not failed.
func TestTypedLineTheEndedSessionShowsATurnAfterIsClosedNotFailed(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer")
	f.tm.onType = nil
	require.NoError(t, f.s.Queue("designer", "do the thing"))
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	rs := f.s.State.Role("designer")
	sentAt := rs.Sent.At
	rs.LastSession = "sid-designer"
	f.tm.die("designer")
	f.rg.endedAt = map[string]time.Time{"sid-designer": sentAt.Add(time.Hour)}

	f.s.settleEnded("designer", sentAt.Add(2*time.Hour))

	assert.Nil(t, rs.Sent)
	assert.Empty(t, rs.Failed)
	assert.Empty(t, notesOf(f, "designer"))
	assert.Zero(t, f.eventCount("dropped"))
}

// With no sign of it taken (no turn after it in the conversation, no status after it) the line is still failed as one
// that may have reached the role.
func TestTypedLineNothingShowsTakenIsStillFailedWhenItsSessionEnds(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer")
	f.tm.onType = nil
	require.NoError(t, f.s.Queue("designer", "do the thing"))
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	rs := f.s.State.Role("designer")
	rs.LastSession = "sid-designer"
	f.tm.die("designer")
	// Its last turn ended before the line was typed.
	f.rg.endedAt = map[string]time.Time{"sid-designer": rs.Sent.At.Add(-time.Hour)}

	f.s.settleEnded("designer", rs.Sent.At.Add(time.Hour))

	require.Len(t, rs.Failed, 1)
	assert.Equal(t, UnclearEnded, rs.Failed[0].Error)
}
