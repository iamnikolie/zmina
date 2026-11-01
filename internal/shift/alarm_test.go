package shift

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/task"
)

// alarmed is the usual fixture with the crew's alarms and asks on disk, the dispatcher and harvester up.
func alarmed(t *testing.T) (fix, *alarm.Store, *asks.Store) {
	t.Helper()

	f := fixtureWith(t, crewYAML)
	al := &alarm.Store{Dir: f.s.Crew.Layout().Alarms()}
	as := &asks.Store{Dir: f.s.Crew.Layout().Asks()}
	f.s.Alarms, f.s.Asks = al, as
	f.s.Tick()

	return f, al, as
}

func set(t *testing.T, f fix, al *alarm.Store, a alarm.Alarm) alarm.Alarm {
	t.Helper()

	a, err := al.Add(a, *f.now)
	require.NoError(t, err)

	return a
}

func TestOwnerReminderLandsOnTheTodoOnce(t *testing.T) {
	f, al, as := alarmed(t)
	a := set(t, f, al, alarm.Alarm{Kind: alarm.Remind, At: f.now.Add(2 * time.Minute), Text: "the site is still on hold",
		Note: "decide on the one-pager", By: "dispatcher"})

	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	assert.Empty(t, f.s.Rang, "not yet")

	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	require.Len(t, f.s.Rang, 1)
	assert.Equal(t, Rung{ID: a.ID, Notify: "the site is still on hold"}, f.s.Rang[0])

	got, err := al.Get(a.ID)
	require.NoError(t, err)
	assert.Equal(t, alarm.Rang, got.State)
	assert.Equal(t, "A-1", got.Ask)

	ask, err := as.Get("A-1")
	require.NoError(t, err)
	assert.Equal(t, asks.Check, ask.Kind)
	assert.Equal(t, "the site is still on hold", ask.Title)
	assert.Contains(t, ask.Text, "decide on the one-pager")
	assert.Equal(t, "dispatcher", ask.Role)
	assert.True(t, f.ev.has("", "alarm"))

	f.s.Rang = nil
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	assert.Empty(t, f.s.Rang, "rings once")
	all, _, _ := as.Scan()
	assert.Len(t, all, 1)
}

func TestMissedAlarmRingsOnceAndSaysWhenItWasDue(t *testing.T) {
	f, al, as := alarmed(t)
	set(t, f, al, alarm.Alarm{Kind: alarm.Remind, At: f.now.Add(time.Minute), Text: "standup"})

	// The Mac slept through it.
	*f.now = f.now.Add(9 * time.Hour)
	f.s.Tick()
	require.Len(t, f.s.Rang, 1)
	assert.Contains(t, f.s.Rang[0].Notify, "standup (due ")

	ask, err := as.Get("A-1")
	require.NoError(t, err)
	assert.Contains(t, ask.Text, "(due ")
}

func TestRoleAlarmStartsTheRoleAndTypesItsLine(t *testing.T) {
	f, al, _ := alarmed(t)
	set(t, f, al, alarm.Alarm{Kind: alarm.Remind, Role: "designer", At: f.now.Add(time.Minute), Text: "check the logo",
		Note: "the client\nanswered"})

	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	require.True(t, f.tm.Has("designer"), "started")
	assert.Empty(t, f.s.Rang, "nothing left for the caller")
	assert.Equal(t, []string{"[zmina alarm] check the logo — the client answered"}, f.s.State.Role("designer").Inbox)

	// Booted and idle: the line is typed.
	*f.now = f.now.Add(3 * time.Minute)
	f.status("designer", "idle", f.now.Add(-time.Minute))
	f.s.Tick()
	assert.Equal(t, []string{"[zmina alarm] check the logo — the client answered"}, f.tm.typed["designer"])
}

func TestRoleAlarmResumesParkedWork(t *testing.T) {
	f, al, _ := alarmed(t)
	f.lt.list = []later.Item{{ID: "L-1", State: later.Parked, Role: "designer", Session: "sid-parked", Text: "logo"}}
	set(t, f, al, alarm.Alarm{Kind: alarm.Remind, Role: "designer", At: *f.now, Text: "go on with the logo"})

	f.s.Tick()
	assert.Contains(t, strings.Join(f.tm.argv["designer"], " "), "--resume sid-parked")
}

func TestSleepKeepsTheTasksAndWakesOnTheLastConversation(t *testing.T) {
	f, al, _ := alarmed(t)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", f.now.Add(-time.Minute))
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open}}

	a, wait, err := f.s.Sleep("designer", f.now.Add(time.Hour), "+1h", "", DownOptions{})
	require.NoError(t, err)
	assert.True(t, wait, "a down to finish")
	require.NoError(t, f.s.FinishDown("designer"))
	assert.False(t, f.tm.Has("designer"))
	assert.Contains(t, a.Text, "go on with T-1")
	assert.Equal(t, "sid-designer", f.s.State.Role("designer").LastSession)
	assert.Equal(t, f.now.Add(time.Hour), f.s.Asleep("designer"))
	assert.True(t, f.tk.list[0].State.Active(), "the task stays the role's")

	// Asleep again: the alarm moves, no second one.
	_, _, err = f.s.Sleep("designer", f.now.Add(2*time.Hour), "+2h", "look at the review", DownOptions{})
	require.NoError(t, err)
	assert.Len(t, al.Pending(), 1)

	_, _, err = f.s.Sleep("dispatcher", f.now.Add(time.Hour), "+1h", "", DownOptions{})
	require.Error(t, err)

	// The shift is stopped: the alarm waits, nothing starts.
	*f.now = f.now.Add(2 * time.Hour)
	f.s.Stop()
	f.s.Tick()
	assert.False(t, f.tm.Has("designer"))
	assert.Len(t, al.Pending(), 1)

	require.NoError(t, f.s.Start())
	f.s.Tick()
	require.True(t, f.tm.Has("designer"))
	assert.Contains(t, strings.Join(f.tm.argv["designer"], " "), "--resume sid-designer")
	assert.Equal(t, []string{"[zmina alarm] look at the review"}, f.s.State.Role("designer").Inbox)
	assert.False(t, f.s.State.Role("designer").Held)
	assert.Empty(t, al.Pending())
}

func TestLaterAlarmBringsTheItemBack(t *testing.T) {
	f, al, _ := alarmed(t)
	f.lt.list = []later.Item{{ID: "L-1", State: later.Parked, Text: "the one-pager"},
		{ID: "L-2", State: later.Done, Text: "gone"}}
	set(t, f, al, alarm.Alarm{Kind: alarm.Later, Later: "L-1", At: *f.now, Text: "the one-pager"})
	gone := set(t, f, al, alarm.Alarm{Kind: alarm.Later, Later: "L-2", At: *f.now, Text: "gone"})

	f.s.Tick()
	assert.Equal(t, *f.now, f.lt.list[0].Back)
	require.Len(t, f.s.Rang, 1)
	assert.Equal(t, "back from later: the one-pager", f.s.Rang[0].Notify)

	got, err := al.Get(gone.ID)
	require.NoError(t, err)
	assert.Equal(t, alarm.Off, got.State, "a closed item has nothing to bring back")
}

func TestRemoteAlarmIsATell(t *testing.T) {
	f, al, as := alarmed(t)
	set(t, f, al, alarm.Alarm{Kind: alarm.Remind, To: "box/ops", At: *f.now, Text: "rotate the dump"})

	f.s.Tick()
	require.Len(t, f.s.Rang, 1)
	assert.Equal(t, Rung{ID: "R-1", Tell: "box/ops", Line: "[zmina alarm] rotate the dump"}, f.s.Rang[0])

	all, _, _ := as.Scan()
	assert.Empty(t, all, "not the owner's todo")
}
