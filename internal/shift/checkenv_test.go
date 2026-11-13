package shift

import (
	"strings"
	"testing"

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

// A check that could not run is kept on its task, which is otherwise as it was, and told to the dispatcher once for each
// task and cause: the same fault again is kept (the newest run) and not told; another cause, or another task, is news.
func TestRecordCheckEnvTellsOnceForEachTaskAndCause(t *testing.T) {
	f := lateFixture(t)
	f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: "designer", State: task.Open, Updated: f.now.UTC()})

	before, err := f.tk.Get("T-1")
	require.NoError(t, err)

	told, err := f.s.RecordCheckEnv(before, "designer", envResult(127, ""))
	require.NoError(t, err)
	assert.True(t, told)
	assert.Equal(t, 1, checkEnvNotes(f), "%v", f.s.State.Signals)

	note := f.s.State.Signals[0]
	assert.Contains(t, note, "T-1 (designer): its done check could not run, and the work was not judged: "+
		"exit 127: the shell did not find the command (make test)")
	assert.Contains(t, note, "the environment of the check is broken, not the task, which stays open")
	assert.Contains(t, note, "zmina task done T-1 again")

	got, err := f.tk.Get("T-1")
	require.NoError(t, err)
	require.NotNil(t, got.CheckEnv)
	assert.True(t, got.CheckEnv.Environment())
	assert.Equal(t, 127, got.CheckEnv.Exit)

	want := before
	want.CheckEnv = got.CheckEnv
	assert.Equal(t, want, got, "state, result, delivery and Updated are as they were")

	// The same cause again: kept, not told.
	again := envResult(127, "")
	again.Output = "a newer output"

	told, err = f.s.RecordCheckEnv(got, "designer", again)
	require.NoError(t, err)
	assert.False(t, told)
	assert.Equal(t, 1, checkEnvNotes(f))

	got, err = f.tk.Get("T-1")
	require.NoError(t, err)
	assert.Equal(t, "a newer output", got.CheckEnv.Output)

	// Another cause on the task is news.
	told, err = f.s.RecordCheckEnv(got, "designer", envResult(-1, "did not start: no shell"))
	require.NoError(t, err)
	assert.True(t, told)
	assert.Equal(t, 2, checkEnvNotes(f))
	assert.Contains(t, f.s.State.Signals[1], "did not start: no shell")

	// Another task with the first cause is news too.
	other, err := f.tk.Get("T-2")
	require.NoError(t, err)

	told, err = f.s.RecordCheckEnv(other, "designer", envResult(127, ""))
	require.NoError(t, err)
	assert.True(t, told)
	assert.Equal(t, 3, checkEnvNotes(f))
}

// A run that is no fault, and a task that is closed since, keep and tell nothing.
func TestRecordCheckEnvKeepsNothingThatIsNoFault(t *testing.T) {
	f := lateFixture(t)

	open, err := f.tk.Get("T-1")
	require.NoError(t, err)

	told, err := f.s.RecordCheckEnv(open, "designer", donecheck.Result{Run: "make test", Exit: 2})
	require.NoError(t, err)
	assert.False(t, told, "a failed check is no fault of the environment")

	closed := open
	closed.State = task.Done

	told, err = f.s.RecordCheckEnv(closed, "designer", envResult(127, ""))
	require.NoError(t, err)
	assert.False(t, told)

	got, err := f.tk.Get("T-1")
	require.NoError(t, err)
	assert.Nil(t, got.CheckEnv)
	assert.Zero(t, checkEnvNotes(f))
}
