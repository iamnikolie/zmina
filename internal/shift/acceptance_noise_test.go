package shift

import (
	"testing"
	"time"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func submitAcceptancePacket(t *testing.T, f fix) {
	t.Helper()
	f.s.State.Role("designer").Started = f.now.Add(-time.Hour)
	require.NotEmpty(t, f.s.FlushInbox("designer"))
	require.False(t, f.tk.list[0].Delivery.SentAt.IsZero())
}

func TestAcceptanceAlertSuppressesInactiveOrUnsubmittedWork(t *testing.T) {
	cases := []struct {
		name   string
		submit bool
		change func(fix)
	}{
		{"queued idle", false, func(f fix) {}},
		{"queued down", false, func(f fix) { delete(f.tm.up, "designer") }},
		{"queued busy", false, func(f fix) { f.status("designer", "busy", *f.now) }},
		{"down after submission", true, func(f fix) { delete(f.tm.up, "designer") }},
		{"held", true, func(f fix) { f.s.State.Role("designer").Held = true }},
		{"asleep", true, func(f fix) {
			f.s.Alarms = &alarm.Store{Dir: f.s.Crew.Layout().Alarms()}
			_, err := f.s.Alarms.Add(alarm.Alarm{Kind: alarm.Sleep, Role: "designer", At: f.now.Add(time.Hour)}, *f.now)
			require.NoError(t, err)
		}},
		{"waiting on person", true, func(f fix) { f.tk.list[0].State = task.Waiting; f.tk.list[0].On = "owner" }},
		{"done", true, func(f fix) { f.tk.list[0].State = task.Done }},
		{"dropped", true, func(f fix) { f.tk.list[0].State = task.Dropped }},
		{"parked", true, func(f fix) { f.tk.list[0].State = task.Parked }},
		{"busy on another task", true, func(f fix) {
			f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: "designer", State: task.Open, Asked: "Next change"})
			f.status("designer", "busy", *f.now)
		}},
		{"waiting dialog", true, func(f fix) { f.status("designer", "waiting", *f.now) }},
		{"new live session", true, func(f fix) { f.rg.byPID[f.tm.up["designer"]].SessionID = "new-session" }},
		{"changed task", true, func(f fix) { f.tk.list[0].Updated = f.now.UTC() }},
		{"failed submission", true, func(f fix) {
			f.s.State.Role("designer").Failed = []Failed{{Line: f.tk.list[0].Delivery.Line, Error: "unconfirmed"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := acceptanceFixture(t)
			if tc.submit {
				submitAcceptancePacket(t, f)
			}
			*f.now = f.now.Add(6 * time.Minute)
			tc.change(f)
			f.s.acceptanceAlerts()
			assert.Empty(t, f.s.State.Notes)
			assert.True(t, f.tk.list[0].Delivery.AlertedAt.IsZero(), "suppression must not consume a future valid alert")
		})
	}
}

func TestAcceptanceClockStartsAtSubmissionAndAlertsOnce(t *testing.T) {
	f := acceptanceFixture(t)
	*f.now = f.now.Add(10 * time.Minute)
	f.s.acceptanceAlerts()
	require.Empty(t, f.s.State.Notes, "queue time is not acceptance time")
	submitAcceptancePacket(t, f)
	assert.Equal(t, f.now.UTC(), f.tk.list[0].Delivery.SentAt)
	*f.now = f.now.Add(5*time.Minute - time.Second)
	f.s.acceptanceAlerts()
	require.Empty(t, f.s.State.Notes)
	*f.now = f.now.Add(time.Second)
	f.s.acceptanceAlerts()
	require.Len(t, f.s.State.Notes, 1)
	assert.Contains(t, f.s.State.Notes[0], "T-1 has no acceptance")
	f.s.acceptanceAlerts()
	assert.Len(t, f.s.State.Notes, 1)
	require.NoError(t, f.s.Up("dispatcher"))
	f.s.State.Role("dispatcher").Started = f.now.Add(-time.Hour)
	f.status("dispatcher", "idle", *f.now)
	assert.Contains(t, f.s.Deliver(), "T-1 has no acceptance")
	assert.Empty(t, f.s.State.Notes)
	f.s.acceptanceAlerts()
	assert.Empty(t, f.s.Deliver(), "a delivered alert is not repeated")
	assert.Len(t, f.tm.typed["dispatcher"], 1)
}

func TestPendingAcceptanceNoticeRechecksLiveEligibility(t *testing.T) {
	for _, change := range []string{"down", "held", "waiting", "busy", "session"} {
		t.Run(change, func(t *testing.T) {
			f := acceptanceFixture(t)
			submitAcceptancePacket(t, f)
			*f.now = f.now.Add(5 * time.Minute)
			f.s.acceptanceAlerts()
			require.Len(t, f.s.State.Notes, 1)
			switch change {
			case "down":
				delete(f.tm.up, "designer")
			case "held":
				f.s.State.Role("designer").Held = true
			case "waiting":
				f.tk.list[0].State = task.Waiting
			case "busy":
				f.status("designer", "busy", *f.now)
			case "session":
				f.rg.byPID[f.tm.up["designer"]].SessionID = "new-session"
			}
			assert.Empty(t, f.s.currentNotes(), "do not deliver an obsolete acceptance alert")
		})
	}
}

func TestClosedTaskPacketLeavesQueueWhileRoleWorksOnNextTask(t *testing.T) {
	for _, state := range []task.State{task.Done, task.Dropped, task.Parked} {
		t.Run(string(state), func(t *testing.T) {
			f := acceptanceFixture(t)
			old := f.tk.list[0].Delivery.Line
			f.tk.list[0].State = state
			f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: "designer", State: task.Open, Updated: f.now.UTC()})
			_, err := f.s.HandTask("designer", f.tk.list[1])
			require.NoError(t, err)
			current := f.tk.list[1].Delivery.Line
			require.NoError(t, f.s.Queue("designer", "ordinary note"))
			f.status("designer", "busy", *f.now)
			*f.now = f.now.Add(6 * time.Minute)
			f.s.Tick()
			assert.Equal(t, []string{current, "ordinary note"}, f.s.State.Role("designer").Inbox)
			assert.NotContains(t, f.tm.typed["designer"], old)
			assert.Empty(t, notesOf(f, "designer"))
			f.status("designer", "idle", *f.now)
			assert.NotEmpty(t, f.s.FlushInbox("designer"))
			assert.Equal(t, []string{current}, f.tm.typed["designer"])
		})
	}
}

func TestAcceptanceSuppressionDoesNotConsumeLaterValidAlert(t *testing.T) {
	f := acceptanceFixture(t)
	submitAcceptancePacket(t, f)
	*f.now = f.now.Add(5 * time.Minute)
	f.s.acceptanceAlerts()
	require.Len(t, f.s.State.Notes, 1)
	f.status("designer", "busy", *f.now)
	require.Empty(t, f.s.currentNotes())
	f.status("designer", "idle", *f.now)
	f.s.acceptanceAlerts()
	assert.Len(t, f.s.currentNotes(), 1, "withdrawn pending notice was never delivered")
}

func TestFreshSubmissionResetsLegacyQueueAgeAlert(t *testing.T) {
	f := acceptanceFixture(t)
	f.tk.list[0].Delivery.AlertedAt = f.now.UTC() // Old queued-age alert, before any submission.
	*f.now = f.now.Add(10 * time.Minute)
	submitAcceptancePacket(t, f)
	*f.now = f.now.Add(5 * time.Minute)
	f.s.acceptanceAlerts()
	assert.Len(t, f.s.State.Notes, 1)
}

func TestPendingAcceptanceNoticeIsBoundToSubmittedAttempt(t *testing.T) {
	f := acceptanceFixture(t)
	submitAcceptancePacket(t, f)
	*f.now = f.now.Add(5 * time.Minute)
	f.s.acceptanceAlerts()
	require.Len(t, f.s.State.Notes, 1)
	// Same packet submitted again after an explicit retry; the old pending notice is not its receipt.
	*f.now = f.now.Add(time.Minute)
	f.tk.list[0].Delivery.SentAt = f.now.UTC()
	f.tk.list[0].Delivery.AlertedAt = time.Time{}
	*f.now = f.now.Add(5 * time.Minute)
	assert.Empty(t, f.s.currentNotes())
	f.s.acceptanceAlerts()
	assert.Len(t, f.s.currentNotes(), 1)
}

func TestLegacyPendingAcceptanceNoticeCanBeReplacedWithoutLosingAlert(t *testing.T) {
	f := acceptanceFixture(t)
	submitAcceptancePacket(t, f)
	*f.now = f.now.Add(5 * time.Minute)
	d := f.tk.list[0].Delivery
	d.AlertedAt = f.now.UTC()
	old := "designer: T-1 has no acceptance after 5 min: submission unconfirmed"
	f.s.addNotes([]string{old})
	f.s.State.NoticeMeta[old] = NoticeMeta{Kind: "acceptance", Role: "designer", Task: "T-1", Handoff: d.ID, Count: 1}
	require.Empty(t, f.s.currentNotes(), "old unbound text is withdrawn")
	f.s.acceptanceAlerts()
	assert.Len(t, f.s.currentNotes(), 1, "a valid overdue submission still gets one current alert")
}
