package shift

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A line typed into a role just before the role is started again (start) is settled by the one rule for the lines an
// ended session left (settleEnded), not dropped by start itself: it is never typed again (it may have been taken), the
// drop is an event that names the line, a tracked line keeps its ack (said again, listed in the next brief), and the
// new session starts with no line in flight.
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
			assert.Empty(t, rs.Failed)
			assert.Equal(t, 1, f.eventCount("dropped"), "settled with an event, not silently")
			assert.Contains(t, droppedDetail(f), line, "the event names the line")

			if tracked {
				require.NotNil(t, f.s.State.Acks[id], "the ack stays: said again, listed in the next brief")
				assert.False(t, f.s.State.Acks[id].First.IsZero())
			}
		})
	}
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
