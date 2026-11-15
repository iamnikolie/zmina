package shift

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/task"
)

// startCrew is the limits crew (two Claude seats, a Codex seat, UTC) with a seat that expired an hour before the fixture's clock.
const startCrew = limitsCrew + "  old:\n    expires: 2026-10-24T11:00:00Z\n"

func openTask(id, role string) task.Task {
	return task.Task{ID: id, Role: role, State: task.Open, Title: "fix the form"}
}

// StartBlock names the first reason a seat must not be started, and nothing when none holds.
func TestStartBlockSaysWhyASeatIsNotStarted(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(f fix)
		role  string
		want  string
	}{
		"nothing in the way": {func(fix) {}, "dev", ""},
		"the shift is stopped": {func(f fix) { f.s.State.Stopped = *f.now }, "dev",
			"the shift is stopped"},
		"the crew is retired": {func(f fix) { f.s.Crew.Retired = "wound down" }, "dev", "the crew is retired"},
		"put down by hand":    {func(f fix) { f.s.State.Role("dev").Held = true }, "dev", "it was put down by hand"},
		"broken":              {func(f fix) { f.s.State.Role("dev").Broken = true }, "dev", "it is broken (zmina ls)"},
		"going down":          {func(f fix) { f.s.State.Role("dev").Downing = *f.now }, "dev", "it is going down"},
		"being released":      {func(f fix) { f.s.State.Role("dev").Releasing = *f.now }, "dev", "it is going down"},
		"seat expired":        {func(fix) {}, "old", "its seat expired 2026-10-24T11:00:00Z"},
		"asleep, and held as a sleep leaves it": {func(f fix) {
			f.s.Alarms = &alarm.Store{Dir: f.s.Crew.Layout().Alarms()}
			_, err := f.s.Alarms.Add(alarm.Alarm{Kind: alarm.Sleep, Role: "dev", At: f.now.Add(time.Hour)}, *f.now)
			require.NoError(t, err)

			f.s.State.Role("dev").Held = true
		}, "dev", "it is asleep"},
		"plan held": {func(f fix) {
			f.claudeLimits(97, 10)
			f.s.limitsStep(*f.now)
		}, "dev", "its plan is held (zmina limits)"},
		"the dispatcher is held by nothing": {func(f fix) { f.s.State.Role("dispatcher").Held = false }, "dispatcher", ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, startCrew)
			c.setup(f)

			assert.Equal(t, c.want, f.s.StartBlock(c.role, *f.now))
		})
	}
}

// A seat the state has never heard of is no reason to make an entry for it: asking is no change.
func TestStartBlockRecordsNothing(t *testing.T) {
	f := fixtureWith(t, startCrew)

	assert.Empty(t, f.s.StartBlock("dev", *f.now))
	assert.NotContains(t, f.s.State.Roles, "dev")
}

// The task goes with the start: the seat is up in the task's place and has its packet queued; other work comes first.
func TestStartOnTaskStartsTheSeatAndHandsTheTask(t *testing.T) {
	f := fixtureWith(t, startCrew)
	f.tk.list = []task.Task{openTask("T-1", "dev")}

	require.Empty(t, f.s.StartOnTask(f.tk.list[0]))
	assert.True(t, f.tm.Has("dev"))
	assert.True(t, f.s.State.Role("dev").Up)
	assert.Contains(t, strings.Join(f.s.State.Role("dev").Inbox, "|"), "T-1")

	got, err := f.tk.Get("T-1")
	require.NoError(t, err)
	require.NotNil(t, got.Delivery, "the hand-out is recorded on the task")
}

func TestStartOnTaskRefusesWhatStartBlockRefuses(t *testing.T) {
	f := fixtureWith(t, startCrew)
	f.tk.list = []task.Task{openTask("T-1", "dev")}
	f.s.State.Role("dev").Held = true

	assert.Equal(t, "it was put down by hand", f.s.StartOnTask(f.tk.list[0]))
	assert.False(t, f.tm.Has("dev"))
	assert.Empty(t, f.tm.starts)
	assert.Empty(t, f.s.State.Role("dev").Inbox, "nothing is queued for a seat that is not started")
}

func TestStartOnTaskLetsOtherWorkComeFirst(t *testing.T) {
	f := fixtureWith(t, startCrew)
	f.tk.list = []task.Task{openTask("T-1", "dev"), openTask("T-2", "dev")}

	assert.Equal(t, "it holds T-1, which comes first", f.s.StartOnTask(f.tk.list[1]))
	assert.False(t, f.tm.Has("dev"))

	// A task of another seat, or one that is closed, is no other work.
	f.tk.list = []task.Task{openTask("T-1", "harvester"), {ID: "T-2", Role: "dev", State: task.Done}, openTask("T-3", "dev")}
	assert.Empty(t, f.s.StartOnTask(f.tk.list[2]))
}

func TestStartOnTaskNeedsTheWorktree(t *testing.T) {
	f := fixtureWith(t, startCrew)
	gone := openTask("T-1", "dev")
	gone.Worktree = f.s.Crew.Dir + "/no-such-worktree"
	f.tk.list = []task.Task{gone}

	assert.Equal(t, "its worktree "+gone.Worktree+" is gone", f.s.StartOnTask(gone))
	assert.False(t, f.tm.Has("dev"))
}

func TestStartOnTaskSaysWhyAStartFailed(t *testing.T) {
	f := fixtureWith(t, startCrew)
	f.tk.list = []task.Task{openTask("T-1", "dev")}
	f.tm.startErr = errors.New("tmux: no server\nrunning")

	why := f.s.StartOnTask(f.tk.list[0])
	assert.Contains(t, why, "tmux: no server running", "one line")
	assert.NotContains(t, why, "\n")
	assert.False(t, f.tm.Has("dev"))
}

// A copy the routes picked is started as zmina up --as 2 starts it.
func TestStartOnTaskStartsACopy(t *testing.T) {
	f := fixtureWith(t, startCrew)
	f.tk.list = []task.Task{openTask("T-1", "dev#2")}

	require.Empty(t, f.s.StartOnTask(f.tk.list[0]))
	assert.True(t, f.tm.Has("dev#2"))
	assert.True(t, f.s.State.Role("dev#2").Instance)
	assert.False(t, f.tm.Has("dev"), "the base seat is left as it was")
}

// A line starts the seat as an up does, with the tasks it holds handed: nothing when StartBlock says no.
func TestStartForLineStartsTheSeat(t *testing.T) {
	f := fixtureWith(t, startCrew)

	require.Empty(t, f.s.StartForLine("dev"))
	assert.True(t, f.tm.Has("dev"))

	f.s.State.Role("designer").Held = true
	assert.Equal(t, "it was put down by hand", f.s.StartForLine("designer"))
	assert.False(t, f.tm.Has("designer"))
}

// A seat the ticker released is not held: a task given to it afterwards starts it, which is the case the whole of reconcile: assign
// was made for. Put down by hand, it stays down.
func TestAReleasedSeatIsStartedForANewTask(t *testing.T) {
	f := releasing(t, "")

	f.s.Tick()
	require.Equal(t, []string{"designer"}, f.s.Released, "an idle seat that holds nothing is put down at once")
	require.NoError(t, f.s.FinishDown("designer"))
	assert.False(t, f.tm.Has("designer"))
	assert.False(t, f.s.State.Role("designer").Held, "a release is no hand's down")
	assert.Empty(t, f.s.StartBlock("designer", *f.now))

	f.tk.list = []task.Task{openTask("T-1", "designer")}
	require.Empty(t, f.s.StartOnTask(f.tk.list[0]))
	assert.True(t, f.tm.Has("designer"))
	assert.Contains(t, strings.Join(f.s.State.Role("designer").Inbox, "|"), "T-1")
}

// A seat the owner put down is the other thing: Held, and no task starts it.
func TestASeatPutDownByHandIsNotStartedForANewTask(t *testing.T) {
	f := fixtureWith(t, startCrew)
	require.NoError(t, f.s.Up("dev"))

	_, err := f.s.BeginDown("dev", DownOptions{})
	require.NoError(t, err)
	require.NoError(t, f.s.FinishDown("dev"))
	assert.True(t, f.s.State.Role("dev").Held)

	f.tk.list = []task.Task{openTask("T-1", "dev")}
	assert.Equal(t, "it was put down by hand", f.s.StartOnTask(f.tk.list[0]))
	assert.False(t, f.tm.Has("dev"))
}
