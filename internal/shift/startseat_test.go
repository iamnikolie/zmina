package shift

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/task"
)

// startCrew is the limits crew (two Claude seats, a Codex seat, UTC) with a seat that expired an hour before the fixture's clock, and
// reconcile: assign on, the key the new rules of StartBlock belong to.
const startCrew = limitsCrew + "  old:\n    expires: 2026-10-24T11:00:00Z\nreconcile: {assign: true}\n"

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

	started, why := f.s.StartOnTask(f.tk.list[0])
	require.Empty(t, why)
	assert.True(t, started)
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

	started, why := f.s.StartOnTask(f.tk.list[0])
	assert.Equal(t, "it was put down by hand", why)
	assert.False(t, started)
	assert.False(t, f.tm.Has("dev"))
	assert.Empty(t, f.tm.starts)
	assert.Empty(t, f.s.State.Role("dev").Inbox, "nothing is queued for a seat that is not started")
}

func TestStartOnTaskLetsOtherWorkComeFirst(t *testing.T) {
	f := fixtureWith(t, startCrew)
	f.tk.list = []task.Task{openTask("T-1", "dev"), openTask("T-2", "dev")}

	started, why := f.s.StartOnTask(f.tk.list[1])
	assert.Equal(t, "it holds T-1, which comes first", why)
	assert.False(t, started)
	assert.False(t, f.tm.Has("dev"))

	// A task of another seat, or one that is closed, is no other work.
	f.tk.list = []task.Task{openTask("T-1", "harvester"), {ID: "T-2", Role: "dev", State: task.Done}, openTask("T-3", "dev")}
	_, why = f.s.StartOnTask(f.tk.list[2])
	assert.Empty(t, why)
}

func TestStartOnTaskNeedsTheWorktree(t *testing.T) {
	f := fixtureWith(t, startCrew)
	gone := openTask("T-1", "dev")
	gone.Worktree = f.s.Crew.Dir + "/no-such-worktree"
	f.tk.list = []task.Task{gone}

	started, why := f.s.StartOnTask(gone)
	assert.Equal(t, "its worktree "+gone.Worktree+" is gone", why)
	assert.False(t, started)
	assert.False(t, f.tm.Has("dev"))
}

func TestStartOnTaskSaysWhyAStartFailed(t *testing.T) {
	f := fixtureWith(t, startCrew)
	f.tk.list = []task.Task{openTask("T-1", "dev")}
	f.tm.startErr = errors.New("tmux: no server\nrunning")

	started, why := f.s.StartOnTask(f.tk.list[0])
	assert.False(t, started)
	assert.Contains(t, why, "tmux: no server running", "one line")
	assert.NotContains(t, why, "\n")
	assert.False(t, f.tm.Has("dev"))
}

// A copy the routes picked is started as zmina up --as 2 starts it.
func TestStartOnTaskStartsACopy(t *testing.T) {
	f := fixtureWith(t, startCrew)
	f.tk.list = []task.Task{openTask("T-1", "dev#2")}

	started, why := f.s.StartOnTask(f.tk.list[0])
	require.Empty(t, why)
	assert.True(t, started)
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
	f := releasing(t, "reconcile: {assign: true}\n")

	f.s.Tick()
	require.Equal(t, []string{"designer"}, f.s.Released, "an idle seat that holds nothing is put down at once")
	require.NoError(t, f.s.FinishDown("designer"))
	assert.False(t, f.tm.Has("designer"))
	assert.False(t, f.s.State.Role("designer").Held, "a release is no hand's down")
	assert.Empty(t, f.s.StartBlock("designer", *f.now))

	f.tk.list = []task.Task{openTask("T-1", "designer")}
	started, why := f.s.StartOnTask(f.tk.list[0])
	require.Empty(t, why)
	assert.True(t, started)
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
	_, why := f.s.StartOnTask(f.tk.list[0])
	assert.Equal(t, "it was put down by hand", why)
	assert.False(t, f.tm.Has("dev"))
}

// Without reconcile: assign StartBlock is the four reasons the review loop always had: a stopped shift, a seat asleep or broken, a plan
// held. A seat that was put down by hand, is going down, is past its expiry or belongs to a retired crew is started all the same, as it
// was: a crew that never set the key sees no change.
func TestStartBlockWithoutAssignIsTheReviewLoopsOldFour(t *testing.T) {
	body := limitsCrew + "  old:\n    expires: 2026-10-24T11:00:00Z\n"

	for name, c := range map[string]struct {
		setup func(f fix)
		role  string
		want  string
	}{
		"put down by hand":     {func(f fix) { f.s.State.Role("dev").Held = true }, "dev", ""},
		"going down":           {func(f fix) { f.s.State.Role("dev").Downing = *f.now }, "dev", ""},
		"being released":       {func(f fix) { f.s.State.Role("dev").Releasing = *f.now }, "dev", ""},
		"a retired crew":       {func(f fix) { f.s.Crew.Retired = "wound down" }, "dev", ""},
		"a seat past expiry":   {func(fix) {}, "old", ""},
		"the shift is stopped": {func(f fix) { f.s.State.Stopped = *f.now }, "dev", "the shift is stopped"},
		"broken":               {func(f fix) { f.s.State.Role("dev").Broken = true }, "dev", "it is broken (zmina ls)"},
		"asleep": {func(f fix) {
			f.s.Alarms = &alarm.Store{Dir: f.s.Crew.Layout().Alarms()}
			_, err := f.s.Alarms.Add(alarm.Alarm{Kind: alarm.Sleep, Role: "dev", At: f.now.Add(time.Hour)}, *f.now)
			require.NoError(t, err)
			f.s.State.Role("dev").Held = true
		}, "dev", "it is asleep"},
		"plan held": {func(f fix) {
			f.claudeLimits(97, 10)
			f.s.limitsStep(*f.now)
		}, "dev", "its plan is held (zmina limits)"},
	} {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, body)
			c.setup(f)

			assert.Equal(t, c.want, f.s.StartBlock(c.role, *f.now))
		})
	}

	// And such a seat is started, as zmina up starts it: the hold cleared.
	f := fixtureWith(t, body)
	f.tk.list = []task.Task{openTask("T-1", "dev")}
	f.s.State.Role("dev").Held = true

	started, why := f.s.StartOnTask(f.tk.list[0])
	assert.True(t, started, why)
	assert.False(t, f.s.State.Role("dev").Held, "UpWith's start")
}

// A start for a task or a line with assign on keeps the seat's count of failed starts, where zmina up and the key off clear it.
func TestStartOnTaskKeepsTheLadderOnlyWithAssign(t *testing.T) {
	f := fixtureWith(t, startCrew)
	f.tk.list = []task.Task{openTask("T-1", "dev")}
	f.s.State.Role("dev").QuickEnds = 2

	started, why := f.s.StartOnTask(f.tk.list[0])
	require.True(t, started, why)
	assert.Equal(t, 2, f.s.State.Role("dev").QuickEnds, "a task given again does not wipe the tries")
	assert.False(t, f.s.State.Role("dev").Held)

	g := fixtureWith(t, startCrew)
	g.s.State.Role("dev").QuickEnds = 2
	require.Empty(t, g.s.StartForLine("dev"))
	assert.Equal(t, 2, g.s.State.Role("dev").QuickEnds, "nor does a line")

	// zmina up does clear it, as it always did, and so does the start without the key.
	h := fixtureWith(t, limitsCrew)
	h.tk.list = []task.Task{openTask("T-1", "dev")}
	h.s.State.Role("dev").QuickEnds = 2
	started, why = h.s.StartOnTask(h.tk.list[0])
	require.True(t, started, why)
	assert.Zero(t, h.s.State.Role("dev").QuickEnds)

	i := fixtureWith(t, startCrew)
	i.s.State.Role("dev").QuickEnds = 2
	require.NoError(t, i.s.Up("dev"))
	assert.Zero(t, i.s.State.Role("dev").QuickEnds, "a hand's up re-arms")
}

// A seat that was started and whose packet could not be handed is up: started is true and why is the hand-out's error, never "not started".
func TestStartOnTaskThatCannotHandTheTaskIsStartedNotRefused(t *testing.T) {
	f := fixtureWith(t, startCrew)
	f.tk.list = []task.Task{openTask("T-1", "dev")}
	require.NoError(t, os.MkdirAll(f.s.Crew.StateDir(), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(f.s.Crew.StateDir(), "handoffs"), []byte("a file where the packets go"), 0o600))

	started, why := f.s.StartOnTask(f.tk.list[0])
	assert.True(t, started, "the seat is up")
	assert.NotEmpty(t, why, "and says what failed")
	assert.True(t, f.tm.Has("dev"))
	assert.True(t, f.s.State.Role("dev").Up)
	assert.Empty(t, f.s.State.Role("dev").Inbox)
}

// With assign on, a down a seat of the crew runs on a keep: manual seat that holds no open task is routine: the seat is not Held and a
// task given to it later starts it. The owner's down, the down of a seat that holds an open task, of a keep: always seat, an unknown
// Codex session's, and every down with the key off, leave it Held.
func TestAnAssignRoutineDownLeavesTheSeatNotHeld(t *testing.T) {
	waiting := task.Task{ID: "T-9", Role: "dev", State: task.Waiting}

	for name, c := range map[string]struct {
		body  string
		by    string
		role  string
		tasks []task.Task
		held  bool
	}{
		"the dispatcher puts down a seat with no task": {startCrew, "dispatcher", "dev", nil, false},
		"a seat of the crew, a copy of one":            {startCrew, "dev#2", "designer", nil, false},
		"it waits on the owner only":                   {startCrew, "dispatcher", "dev", []task.Task{waiting}, false},
		"another seat's task is none of its own":       {startCrew, "dispatcher", "dev", []task.Task{openTask("T-1", "designer")}, false},
		"the owner's hand":                             {startCrew, "hand", "dev", nil, true},
		"the cockpit":                                  {startCrew, "ui", "dev", nil, true},
		"it holds an open task":                        {startCrew, "dispatcher", "dev", []task.Task{openTask("T-1", "dev")}, true},
		"a keep: always seat":                          {startCrew, "dispatcher", "harvester", nil, true},
		"a Codex session no seat owns":                 {startCrew, ActorCodexThread, "dev", nil, true},
		"a name that is no seat's":                     {startCrew, "stranger", "dev", nil, true},
		"the key off":                                  {limitsCrew, "dispatcher", "dev", nil, true},
		"assign off, restart on":                       {"reconcile: {restart: true}\n" + limitsCrew, "dispatcher", "dev", nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, c.body)
			f.tk.list = c.tasks
			require.NoError(t, f.s.Up(c.role))

			f.s.By = c.by
			_, err := f.s.BeginDown(c.role, DownOptions{})
			require.NoError(t, err)
			f.s.ReleaseRoutineDown(c.role)

			assert.Equal(t, c.held, f.s.State.Role(c.role).Held)
		})
	}

	// And the seat the dispatcher put down is then started by a task; the owner's down is not.
	f := fixtureWith(t, startCrew)
	require.NoError(t, f.s.Up("dev"))
	f.s.By = "dispatcher"
	_, err := f.s.BeginDown("dev", DownOptions{})
	require.NoError(t, err)
	f.s.ReleaseRoutineDown("dev")
	require.NoError(t, f.s.FinishDown("dev"))

	f.tk.list = []task.Task{openTask("T-1", "dev")}
	started, why := f.s.StartOnTask(f.tk.list[0])
	assert.True(t, started, why)
	assert.True(t, f.ev.has("dev", "down-routine"))
}
