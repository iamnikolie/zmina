package shift

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reloadAcceptanceState(t *testing.T, f fix) {
	t.Helper()
	data, err := json.Marshal(f.s.State)
	require.NoError(t, err)
	f.s.State = NewState()
	require.NoError(t, json.Unmarshal(data, f.s.State))
}

func TestAcceptanceLegacyMarkerAndFreshSubmissionSurviveStateReload(t *testing.T) {
	f := acceptanceFixture(t)
	submitAcceptancePacket(t, f)
	d := f.tk.list[0].Delivery
	legacy := "T-1/" + d.ID
	f.s.State.AcceptanceNotified = map[string]time.Time{legacy: d.QueuedAt}
	reloadAcceptanceState(t, f)
	*f.now = f.now.Add(6 * time.Minute)
	f.s.acceptanceAlerts()
	require.Empty(t, f.s.State.Notes, "a committed legacy alert must remain deduplicated")

	// Same packet/queue generation, but a real explicit retry starts a new submitted attempt.
	require.NoError(t, f.s.Queue("designer", d.Line))
	require.NotEmpty(t, f.s.FlushInbox("designer"))
	assert.NotContains(t, f.s.State.AcceptanceNotified, legacy)
	reloadAcceptanceState(t, f)
	*f.now = f.now.Add(5 * time.Minute)
	f.s.acceptanceAlerts()
	require.Len(t, f.s.State.Notes, 1)
	tk := f.tk.list[0]
	assert.Equal(t, tk.Delivery.QueuedAt, f.s.State.AcceptanceNotified[acceptanceAlertKey(tk)])

	// Committing queue consumption keeps its marker, even if the informational task timestamp is lost.
	f.s.State.Notes, f.s.State.NoticeMeta = nil, nil
	reloadAcceptanceState(t, f)
	tk.Delivery.AlertedAt = time.Time{}
	require.NoError(t, f.s.Tasks.Put(tk))
	f.s.acceptanceAlerts()
	assert.Empty(t, f.s.State.Notes)
}

func TestAcceptanceWithdrawalCommitsWithQueueAndRearmsAfterReload(t *testing.T) {
	f := acceptanceFixture(t)
	submitAcceptancePacket(t, f)
	*f.now = f.now.Add(5 * time.Minute)
	f.s.acceptanceAlerts()
	require.Len(t, f.s.State.Notes, 1)
	reloadAcceptanceState(t, f)
	key := acceptanceAlertKey(f.tk.list[0])
	require.Contains(t, f.s.State.AcceptanceNotified, key)

	f.status("designer", "busy", *f.now)
	require.Empty(t, f.s.currentNotes())
	assert.NotContains(t, f.s.State.AcceptanceNotified, key)
	reloadAcceptanceState(t, f)
	f.s.acceptanceAlerts()
	require.Empty(t, f.s.State.Notes)

	f.status("designer", "idle", *f.now)
	f.s.acceptanceAlerts()
	require.Len(t, f.s.State.Notes, 1, "a withdrawn, undelivered notice must not spend the valid alert")
	reloadAcceptanceState(t, f)
	f.s.acceptanceAlerts()
	assert.Len(t, f.s.State.Notes, 1)
	assert.Equal(t, 1, f.s.State.NoticeMeta[f.s.State.Notes[0]].Count)
}
