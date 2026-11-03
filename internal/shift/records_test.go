package shift

import (
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestResultNotificationReconcilesAfterLostState(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	r, _ := f.s.Role("designer")
	shape := f.s.Crew.HandoffFor(r)
	values := map[string]string{}
	for _, s := range shape.Sections {
		values[s.Name] = "Recorded"
	}
	_, err := record.Save(f.s.Crew.StateDir(), record.Document{Version: 1, Kind: "result", Task: "T-1", Role: "designer", At: *f.now, Shape: shape, Values: values})
	require.NoError(t, err)
	f.s.NotifyRecords()
	require.Len(t, f.s.State.Signals, 1)
	f.s.State.Signals = nil
	f.s.NotifyRecords()
	assert.Empty(t, f.s.State.Signals)
	f.s.State = NewState()
	f.s.NotifyRecords()
	assert.Len(t, f.s.State.Signals, 1, "recover even if task closed and event log rotated")
}
