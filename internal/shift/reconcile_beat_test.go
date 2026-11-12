package shift

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The beat reconciles messages twice, and the order is what each call is for: the first before the beat's decisions,
// so a note it queues for the dispatcher is typed in this same beat; the second after the beat's delivery, which it
// sees (a note refused at intake for a full queue is queued once the delivery has drained it, and the line just typed
// is the dispatcher's Sent). A simplification that drops either call fails here.
func TestMessagesAreReconciledBeforeAndAfterTheBeatsDelivery(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("dispatcher"))
	f.status("dispatcher", "idle", *f.now)
	*f.now = f.now.Add(3 * time.Minute)

	type seen struct {
		waiting int
		sent    bool
	}

	var calls []seen

	f.s.ReconcileMessages = func(s *Shift) error {
		calls = append(calls, seen{len(s.State.Signals), s.State.Role("dispatcher").Sent != nil})

		if len(calls) == 1 {
			_, err := s.Note("far-crew", "a report arrived", false)

			return err
		}

		return nil
	}

	lines := f.s.Tick()

	assert.Contains(t, strings.Join(lines, "\n"), "told the dispatcher: ",
		"the note the first call queued is typed in the same beat")
	assert.Equal(t, []seen{{0, false}, {0, true}}, calls,
		"once before the beat's work, once after its delivery, which it sees")
}
