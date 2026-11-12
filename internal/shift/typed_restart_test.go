package shift

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A line typed into a role just before the role is started again (start) is settled by the one rule for the lines an
// ended session left (settleEnded), not dropped by start itself, and it is never typed again (it may have been taken).
// A tracked line keeps its ack (said again, listed in the next brief) and is dropped with an event, not failed too: the
// ack is the one way it is said again. A line with no id is kept as a failed line that needs attention. The new
// session starts with no line in flight.
func TestTypedLineBeforeARestartIsSettledNotLostSilently(t *testing.T) {
	for name, tracked := range map[string]bool{"a tracked line": true, "a line with no id": false} {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("designer"))
			f.s.State.Role("designer").Inbound = true
			f.status("designer", "idle", *f.now)
			f.tm.onType = nil // the status never changes: the line never shows as landed
			*f.now = f.now.Add(3 * time.Minute)

			line, id := "remember the milk", ""
			if tracked {
				var err error

				id, err = f.s.QueueTracked("designer", "[dev] look at T-1")
				require.NoError(t, err)

				line = f.s.State.Role("designer").Inbox[0]
			} else {
				require.NoError(t, f.s.Queue("designer", line))
			}

			require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

			rs := f.s.State.Role("designer")
			require.NotNil(t, rs.Sent)
			require.Equal(t, line, rs.Sent.Line)

			// The pane ended and the role is started again before any beat saw it down.
			f.tm.die("designer")
			*f.now = f.now.Add(time.Minute)
			require.NoError(t, f.s.Up("designer"))

			assert.Nil(t, rs.Sent, "no line in flight for the new session")
			assert.Empty(t, rs.Inbox, "not typed again: it may have been taken")

			if tracked {
				assert.Empty(t, rs.Failed, "its ack says it again: no failed record beside it")
				assert.Equal(t, 1, f.eventCount("dropped"), "settled with an event, not silently")
				assert.Contains(t, droppedDetail(f), line, "the event names the line")
				require.NotNil(t, f.s.State.Acks[id], "the ack stays: said again, listed in the next brief")
				assert.False(t, f.s.State.Acks[id].First.IsZero())
				assert.Empty(t, notesOf(f, "designer"), "the dispatcher is not told a second time")

				return
			}

			require.Len(t, rs.Failed, 1, "a line nothing says again is kept for a look")
			assert.Equal(t, line, rs.Failed[0].Line)
			assert.Equal(t, UnclearEnded, rs.Failed[0].Error)
			assert.True(t, rs.Failed[0].Unclear(), "it may have reached the conversation")
			assert.Zero(t, f.eventCount("dropped"))
			require.NotEmpty(t, notesOf(f, "designer"))
			assert.Contains(t, notesOf(f, "designer")[0], "delivery failed")

			// A retry says it again only when asked for it.
			n, skipped, err := f.s.Retry("designer", false)
			require.NoError(t, err)
			assert.Zero(t, n)
			assert.Equal(t, 1, skipped)
			assert.Len(t, rs.Failed, 1)
			assert.Empty(t, rs.Inbox)
		})
	}
}

// A typed line that only made sense to the conversation that ended (a slash command, the boot a clear queued, a release
// or restart request) is dropped with an event, as before: a fresh session has its own boot and is not released.
func TestSessionBoundTypedLineIsStillDroppedWhenItsSessionEnds(t *testing.T) {
	for name, line := range map[string]string{
		"a release request":  "[zmina release] park what you hold and say so",
		"a restart request":  "[zmina] restart: write your note",
		"a recovery pointer": "[zmina recovery] read the pack",
	} {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("designer"))
			f.status("designer", "idle", *f.now)
			f.tm.onType = nil
			*f.now = f.now.Add(3 * time.Minute)
			require.NoError(t, f.s.Queue("designer", line))
			require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
			require.NotNil(t, f.s.State.Role("designer").Sent)

			f.s.settleEnded("designer", *f.now)

			rs := f.s.State.Role("designer")
			assert.Nil(t, rs.Sent)
			assert.Empty(t, rs.Failed, "nothing to look at: the conversation it was for is gone")
			assert.Equal(t, 1, f.eventCount("dropped"))
			assert.Empty(t, notesOf(f, "designer"))
		})
	}
}

// The boot a clear queued is session-bound too, known the way a posted one is: by being the role's queued boot.
func TestTypedBootLineIsDroppedWhenItsSessionEnds(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	f.tm.onType = nil
	*f.now = f.now.Add(3 * time.Minute)

	rs := f.s.State.Role("designer")
	rs.QueuedBoot = "boot again: you are the designer"
	require.NoError(t, f.s.Queue("designer", rs.QueuedBoot))
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	require.NotNil(t, rs.Sent)

	f.s.settleEnded("designer", *f.now)

	assert.Nil(t, rs.Sent)
	assert.Empty(t, rs.Failed)
	assert.Equal(t, 1, f.eventCount("dropped"))
}

// droppedDetail is the detail of the first dropped event the fake log holds.
func droppedDetail(f fix) string {
	for _, e := range f.ev.list {
		if e.Event == "dropped" {
			return e.Detail
		}
	}

	return ""
}
