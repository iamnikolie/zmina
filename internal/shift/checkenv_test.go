package shift

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/donecheck"
	"github.com/iamnikolie/zmina/internal/task"
)

func envResult(exit int, err string) donecheck.Result {
	return donecheck.Result{Run: "make test", Source: donecheck.FromCrew, Commit: "abc123", Exit: exit, Err: err,
		Kind: donecheck.KindEnvironment, Output: "sh: make: not found"}
}

func checkEnvNotes(f fix) int {
	n := 0

	for _, sig := range f.s.State.Signals {
		if strings.Contains(sig, "its done check could not run") {
			n++
		}
	}

	return n
}

// told is what a caller does after the state is saved: it marks the cause told when the note was queued.
func told(t *testing.T, f fix, id string, tell CheckEnvTell, res donecheck.Result) {
	t.Helper()

	if tell.Queued {
		require.NoError(t, f.s.MarkCheckEnvTold(id, res))
	}
}

// A check that could not run is kept on its task, which is otherwise as it was, and the dispatcher's note is queued once for
// each task and cause: the same fault again is kept (the newest run) and not told; another cause, or another task, is news; a
// cause told before is not told again after another came between (A, B, A tells twice).
func TestRecordCheckEnvTellsOnceForEachTaskAndCause(t *testing.T) {
	f := lateFixture(t)
	f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: "designer", State: task.Open, Updated: f.now.UTC()})

	before, err := f.tk.Get("T-1")
	require.NoError(t, err)

	a, b := envResult(127, ""), envResult(-1, "did not start: no shell")

	tell, err := f.s.RecordCheckEnv(before, "designer", a)
	require.NoError(t, err)
	assert.True(t, tell.Queued)
	assert.False(t, tell.Already)
	assert.NoError(t, tell.Err)
	assert.Equal(t, 1, checkEnvNotes(f), "%v", f.s.State.Signals)

	note := f.s.State.Signals[0]
	assert.Contains(t, note, "T-1 (designer): its done check could not run, so the work was not judged: "+
		"exit 127: the shell did not find the command (make test)")
	assert.Contains(t, note, "the environment of the check is broken or a file it runs is missing in the worktree, not the task, which stays open")
	assert.Contains(t, note, "zmina task done T-1 again")

	got, err := f.tk.Get("T-1")
	require.NoError(t, err)
	require.NotNil(t, got.CheckEnv)
	assert.True(t, got.CheckEnv.Environment())
	assert.Equal(t, 127, got.CheckEnv.Exit)
	assert.Empty(t, got.CheckEnvTold, "not told until the caller says the state with the note is saved")

	want := before
	want.CheckEnv = got.CheckEnv
	assert.Equal(t, want, got, "state, result, delivery and Updated are as they were")

	told(t, f, "T-1", tell, a)

	// The same cause again: kept, not told.
	again := envResult(127, "")
	again.Output = "a newer output"

	got, err = f.tk.Get("T-1")
	require.NoError(t, err)

	tell, err = f.s.RecordCheckEnv(got, "designer", again)
	require.NoError(t, err)
	assert.True(t, tell.Already)
	assert.False(t, tell.Queued)
	assert.Equal(t, 1, checkEnvNotes(f))

	got, err = f.tk.Get("T-1")
	require.NoError(t, err)
	assert.Equal(t, "a newer output", got.CheckEnv.Output)

	// Another cause on the task is news.
	tell, err = f.s.RecordCheckEnv(got, "designer", b)
	require.NoError(t, err)
	assert.True(t, tell.Queued)
	assert.Equal(t, 2, checkEnvNotes(f))
	assert.Contains(t, f.s.State.Signals[1], "did not start: no shell")
	told(t, f, "T-1", tell, b)

	// The first cause comes back after the second: told already, the set remembers both. A, B, A tells twice.
	got, err = f.tk.Get("T-1")
	require.NoError(t, err)
	assert.Equal(t, []string{a.Fault(), b.Fault()}, got.CheckEnvTold)

	tell, err = f.s.RecordCheckEnv(got, "designer", a)
	require.NoError(t, err)
	assert.True(t, tell.Already)
	assert.Equal(t, 2, checkEnvNotes(f), "A, B, A is two tells, not three")

	// Another task with the first cause is news too.
	other, err := f.tk.Get("T-2")
	require.NoError(t, err)

	tell, err = f.s.RecordCheckEnv(other, "designer", a)
	require.NoError(t, err)
	assert.True(t, tell.Queued)
	assert.Equal(t, 3, checkEnvNotes(f))
}

// A note that could not be queued is not a dispatcher that was told: the fault is kept, nothing is queued, the error is the
// caller's to say, and the cause is not marked told, so that the next run tries again. The same for a state that could not be
// saved, which the caller finds out and then does not mark.
func TestRecordCheckEnvIsNotToldWhenTheNoteCouldNotBeQueued(t *testing.T) {
	f := lateFixture(t)

	for i := range signalsMax {
		_, err := f.s.Note("mrs", fmt.Sprintf("acme/site!%d approved", i), false)
		require.NoError(t, err)
	}

	open, err := f.tk.Get("T-1")
	require.NoError(t, err)

	res := envResult(127, "")

	tell, err := f.s.RecordCheckEnv(open, "designer", res)
	require.NoError(t, err, "the fault is kept")
	assert.False(t, tell.Queued)
	assert.False(t, tell.Already)
	require.Error(t, tell.Err)
	assert.Contains(t, tell.Err.Error(), "it is not reading them")

	got, err := f.tk.Get("T-1")
	require.NoError(t, err)
	require.NotNil(t, got.CheckEnv, "the fault is on the task")
	assert.Empty(t, got.CheckEnvTold, "and nobody was told")

	// Room again: the next run of the check tells it.
	f.s.State.Signals = nil

	tell, err = f.s.RecordCheckEnv(got, "designer", res)
	require.NoError(t, err)
	assert.True(t, tell.Queued)
	assert.Equal(t, 1, checkEnvNotes(f))
}

// The told causes are the last CheckEnvToldMax: a ninth distinct cause makes the oldest forgotten, and a cause marked twice is
// one entry. A cause is marked only while it is the fault on the task.
func TestMarkCheckEnvToldIsBounded(t *testing.T) {
	f := lateFixture(t)

	for i := range task.CheckEnvToldMax + 3 {
		res := envResult(100+i, "")
		res.Err = ""

		current, err := f.tk.Get("T-1")
		require.NoError(t, err)

		current.CheckEnv = &res
		require.NoError(t, f.tk.Put(current))

		require.NoError(t, f.s.MarkCheckEnvTold("T-1", res))
		require.NoError(t, f.s.MarkCheckEnvTold("T-1", res))
	}

	got, err := f.tk.Get("T-1")
	require.NoError(t, err)
	require.Len(t, got.CheckEnvTold, task.CheckEnvToldMax)
	assert.Equal(t, envResult(103, "").Fault(), got.CheckEnvTold[0])
	assert.Equal(t, envResult(100+task.CheckEnvToldMax+2, "").Fault(), got.CheckEnvTold[task.CheckEnvToldMax-1])

	// A task closed since is left alone.
	closed := got
	closed.State = task.Done
	require.NoError(t, f.tk.Put(closed))
	require.NoError(t, f.s.MarkCheckEnvTold("T-1", *got.CheckEnv))

	after, err := f.tk.Get("T-1")
	require.NoError(t, err)
	assert.Equal(t, got.CheckEnvTold, after.CheckEnvTold)

	require.Error(t, f.s.MarkCheckEnvTold("T-9", envResult(127, "")), "a task that is not there")
}

// The record and the mark are two writes with the state saved between them, and a `task set --check` can come between: the mark
// is of the fault it was made for. A check that was cleared or changed since, or that has met another fault since, has nothing
// of this one to mark, and a stale cause is never put in the told set.
func TestMarkCheckEnvToldOnlyMarksTheFaultThatIsOnTheTask(t *testing.T) {
	a, other := envResult(127, ""), envResult(126, "")
	other.Exit = 126

	for name, between := range map[string]func(t *testing.T, f fix){
		"the check was cleared (task set --check -)": func(t *testing.T, f fix) {
			got, err := f.tk.Get("T-1")
			require.NoError(t, err)
			got.CheckEnv, got.CheckEnvTold = nil, nil
			require.NoError(t, f.tk.Put(got))
		},
		"the check was changed: its command is another": func(t *testing.T, f fix) {
			got, err := f.tk.Get("T-1")
			require.NoError(t, err)
			changed := *got.CheckEnv
			changed.Run = "make lint"
			got.CheckEnv = &changed
			require.NoError(t, f.tk.Put(got))
		},
		"the worktree moved to another commit and the check met the same fault": func(t *testing.T, f fix) {
			got, err := f.tk.Get("T-1")
			require.NoError(t, err)
			moved := *got.CheckEnv
			moved.Commit = "def456"
			got.CheckEnv = &moved
			require.NoError(t, f.tk.Put(got))
		},
		"the check met another fault since": func(t *testing.T, f fix) {
			got, err := f.tk.Get("T-1")
			require.NoError(t, err)
			got.CheckEnv = &other
			require.NoError(t, f.tk.Put(got))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := lateFixture(t)

			open, err := f.tk.Get("T-1")
			require.NoError(t, err)

			tell, err := f.s.RecordCheckEnv(open, "designer", a)
			require.NoError(t, err)
			require.True(t, tell.Queued)

			between(t, f)

			require.NoError(t, f.s.MarkCheckEnvTold("T-1", a))

			got, err := f.tk.Get("T-1")
			require.NoError(t, err)
			assert.Empty(t, got.CheckEnvTold, "a stale cause is not marked")
		})
	}

	t.Run("the fault still on the task is marked", func(t *testing.T) {
		f := lateFixture(t)

		open, err := f.tk.Get("T-1")
		require.NoError(t, err)

		_, err = f.s.RecordCheckEnv(open, "designer", a)
		require.NoError(t, err)
		require.NoError(t, f.s.MarkCheckEnvTold("T-1", a))

		got, err := f.tk.Get("T-1")
		require.NoError(t, err)
		assert.Equal(t, []string{a.Fault()}, got.CheckEnvTold)
	})
}

// Two runs of the check at once: the second reads the state after the first queued its note and before the first marked it, and
// finds the note waiting for the dispatcher: it is folded into it, no second note and no repeat count, and both marks leave one
// cause in the set.
func TestTwoRunsOfTheCheckAtOnceTellTheDispatcherOnce(t *testing.T) {
	f := lateFixture(t)

	first, second := envResult(127, ""), envResult(127, "")
	second.At = first.At.Add(time.Second)
	second.Output = "the second run's output"

	open, err := f.tk.Get("T-1")
	require.NoError(t, err)

	tellFirst, err := f.s.RecordCheckEnv(open, "designer", first)
	require.NoError(t, err)
	require.True(t, tellFirst.Queued)

	// The first has not marked yet: the second run's lock comes in between.
	between, err := f.tk.Get("T-1")
	require.NoError(t, err)

	tellSecond, err := f.s.RecordCheckEnv(between, "designer", second)
	require.NoError(t, err)
	assert.False(t, tellSecond.Queued, "folded into the note that waits")
	assert.True(t, tellSecond.Already)
	assert.Equal(t, 1, checkEnvNotes(f), "one note")
	require.Len(t, f.s.State.Signals, 1)
	assert.Equal(t, 1, f.s.State.SignalCounts[f.s.State.Signals[0]], "and not one that says it was repeated")

	// The newer run's result is the one on the task; the first's mark is of the same cause, check and commit, and is made.
	require.NoError(t, f.s.MarkCheckEnvTold("T-1", first))
	require.NoError(t, f.s.MarkCheckEnvTold("T-1", second))

	got, err := f.tk.Get("T-1")
	require.NoError(t, err)
	assert.Equal(t, []string{first.Fault()}, got.CheckEnvTold)
	assert.Equal(t, "the second run's output", got.CheckEnv.Output)
}

// A run that is no fault, and a task that is closed since, keep and tell nothing.
func TestRecordCheckEnvKeepsNothingThatIsNoFault(t *testing.T) {
	f := lateFixture(t)

	open, err := f.tk.Get("T-1")
	require.NoError(t, err)

	tell, err := f.s.RecordCheckEnv(open, "designer", donecheck.Result{Run: "make test", Exit: 2})
	require.NoError(t, err)
	assert.Equal(t, CheckEnvTell{}, tell, "a failed check is no fault of the environment")

	closed := open
	closed.State = task.Done

	tell, err = f.s.RecordCheckEnv(closed, "designer", envResult(127, ""))
	require.NoError(t, err)
	assert.Equal(t, CheckEnvTell{}, tell)

	got, err := f.tk.Get("T-1")
	require.NoError(t, err)
	assert.Nil(t, got.CheckEnv)
	assert.Zero(t, checkEnvNotes(f))
}
