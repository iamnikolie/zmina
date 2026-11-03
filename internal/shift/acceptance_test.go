package shift

import (
	"encoding/json"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func acceptanceFixture(t *testing.T) fix {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open, Updated: f.now.UTC()}}
	_, err := f.s.HandTask("designer", f.tk.list[0])
	require.NoError(t, err)
	f.s.By = "designer"
	return f
}
func TestAcceptanceRequiresSubmissionCurrentRevisionAndAssignedLiveRole(t *testing.T) {
	f := acceptanceFixture(t)
	d := f.tk.list[0].Delivery
	require.ErrorContains(t, f.s.AcceptTask("T-1", d.ID), "live session")
	f.s.submittedTask("designer", d.Line)
	require.NoError(t, f.s.AcceptTask("T-1", d.ID))
	require.NoError(t, f.s.AcceptTask("T-1", d.ID))
	assert.False(t, f.tk.list[0].Delivery.AcceptedAt.IsZero())
	_, err := f.s.HandTask("designer", f.tk.list[0])
	require.NoError(t, err)
	assert.Len(t, f.s.State.Role("designer").Inbox, 1, "already accepted packet is not added again")
	require.ErrorContains(t, f.s.AcceptTask("T-1", "wrong"), "superseded")
	f.s.By = "harvester"
	require.ErrorContains(t, f.s.AcceptTask("T-1", d.ID), "assigned role")
}
func TestUnacceptedTaskAlertsOnceAndStalePacketsCannotAccept(t *testing.T) {
	f := acceptanceFixture(t)
	d := f.tk.list[0].Delivery
	submitAcceptancePacket(t, f)
	*f.now = f.now.Add(5 * time.Minute)
	f.s.acceptanceAlerts()
	require.Len(t, f.s.State.Notes, 1)
	assert.Contains(t, f.s.State.Notes[0], "T-1 has no acceptance")
	f.s.acceptanceAlerts()
	assert.Len(t, f.s.State.Notes, 1)
	f.tk.list[0].Updated = f.now.UTC()
	require.ErrorContains(t, f.s.AcceptTask("T-1", d.ID), "task changed")
	_, err := f.s.HandTask("designer", f.tk.list[0])
	require.NoError(t, err)
	assert.NotEqual(t, d.ID, f.tk.list[0].Delivery.ID)
}

func TestAcceptanceAlertsResolveBeforeDeliveryAndFreshSessionNeedsNewAcceptance(t *testing.T) {
	f := acceptanceFixture(t)
	d := f.tk.list[0].Delivery
	submitAcceptancePacket(t, f)
	*f.now = f.now.Add(5 * time.Minute)
	f.s.acceptanceAlerts()
	require.Len(t, f.s.State.Notes, 1)
	require.NoError(t, f.s.AcceptTask("T-1", d.ID))
	assert.Empty(t, f.s.currentNotes(), "do not tell dispatcher a resolved missing acceptance")
	f.rg.byPID[f.tm.up["designer"]].SessionID = "new-session"
	_, err := f.s.HandTask("designer", f.tk.list[0])
	require.NoError(t, err)
	assert.True(t, f.tk.list[0].Delivery.AcceptedAt.IsZero(), "a fresh runtime must read and accept even identical task content")
}

func TestScopedCommandQuotesOpaqueCrewPaths(t *testing.T) {
	assert.Equal(t, `'a'"'"'b $(word)'`, shellWord("a'b $(word)"))
}

func TestAcceptanceAlertReconcilesTaskWriteBeforeStateCommit(t *testing.T) {
	f := acceptanceFixture(t)
	submitAcceptancePacket(t, f)
	store := &task.Store{Dir: t.TempDir()}
	require.NoError(t, store.Put(f.tk.list[0]))
	f.s.Tasks = store
	before, err := json.Marshal(f.s.State)
	require.NoError(t, err)
	*f.now = f.now.Add(6 * time.Minute)
	f.s.acceptanceAlerts()
	require.Len(t, f.s.State.Notes, 1)
	// The task file survives, while the queue state did not commit before the crash.
	f.s.State = NewState()
	require.NoError(t, json.Unmarshal(before, f.s.State))
	f.s.acceptanceAlerts()
	require.Len(t, f.s.State.Notes, 1, "an informational task timestamp cannot suppress a lost alert")
	// A committed alert stays deduplicated after its queue item is consumed and state reloads.
	f.s.State.Notes = nil
	committed, err := json.Marshal(f.s.State)
	require.NoError(t, err)
	f.s.State = NewState()
	require.NoError(t, json.Unmarshal(committed, f.s.State))
	f.s.acceptanceAlerts()
	require.Empty(t, f.s.State.Notes)
	// Renewing the same packet does not start its clock until an actual submission.
	tk, err := store.Get("T-1")
	require.NoError(t, err)
	*f.now = f.now.Add(time.Hour)
	f.s.State.Role("designer").Started = *f.now
	_, err = f.s.HandTask("designer", tk)
	require.NoError(t, err)
	fresh, err := store.Get("T-1")
	require.NoError(t, err)
	assert.Equal(t, tk.Delivery.ID, fresh.Delivery.ID)
	require.True(t, fresh.Delivery.SentAt.IsZero())
	*f.now = f.now.Add(6 * time.Minute)
	f.s.acceptanceAlerts()
	require.Empty(t, f.s.State.Notes)
	require.NotEmpty(t, f.s.FlushInbox("designer"))
	*f.now = f.now.Add(5 * time.Minute)
	f.s.acceptanceAlerts()
	require.Len(t, f.s.State.Notes, 1)
}
