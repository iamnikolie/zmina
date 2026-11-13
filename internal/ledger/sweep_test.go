package ledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/task"
)

// sweepFixture is a ledger with a parked list of three old items and a sweep task over all of them.
func sweepFixture(t *testing.T) (*Ledger, task.Task) {
	t.Helper()

	l, _ := fixture(t, "")
	l.Dispatcher = "boss"
	l.Later = &later.Store{Dir: filepath.Join(l.Dir, "later")}

	old := l.Now().Add(-30 * 24 * time.Hour)

	for _, text := range []string{"one", "two", "three"} {
		_, err := l.Later.Add(later.Item{Text: text}, old)
		require.NoError(t, err)
	}

	tk, err := l.New(NewTask{Role: "dev", Asked: "sweep the parked list", By: "boss", Sweep: []string{"L-1", "L-2", "L-3"}})
	require.NoError(t, err)

	return l, tk
}

// A text that names its own task is built once the task has its number and is written with it: the record and the event
// have the final text, and what is not the header's is as it was given. A task with no such text keeps the one it was asked.
func TestAskedForBuildsTheTextOnceTheTaskIsNumbered(t *testing.T) {
	l, _ := fixture(t, "")

	var logged []string

	l.Log = func(_, event, id, detail string) { logged = append(logged, event+" "+id+" "+detail) }

	rows := "L-1 \"look at {task} and T-0\""
	built := func(id string) string {
		return "run zmina later swept " + id + ", then zmina task done " + id + "\n" + rows
	}

	sw, err := l.New(NewTask{Role: "dev", Asked: "a placeholder", Sweep: []string{"L-1"}, AskedFor: built})
	require.NoError(t, err)
	assert.Equal(t, built("T-1"), sw.Asked)
	assert.Contains(t, sw.Asked, "{task}", "a row that holds the literal is as it was written")

	got, err := l.Store.Get("T-1")
	require.NoError(t, err)
	assert.Equal(t, sw.Asked, got.Asked)
	assert.Equal(t, []string{"task T-1 new: " + sw.Asked}, logged)

	plain, err := l.New(NewTask{Role: "dev", Asked: "run {task}"})
	require.NoError(t, err)
	assert.Equal(t, "run {task}", plain.Asked)
}

func TestNewKeepsTheItemsASweepCovers(t *testing.T) {
	l, tk := sweepFixture(t)
	assert.Equal(t, []string{"L-1", "L-2", "L-3"}, tk.Sweep)

	got, err := l.Store.Get(tk.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"L-1", "L-2", "L-3"}, got.Sweep)

	plain, err := l.New(NewTask{Role: "dev", Asked: "a plain task"})
	require.NoError(t, err)
	assert.Empty(t, plain.Sweep)
}

// A sweep task is closed as done only when every item it covers has a verdict: a worker cannot say it is finished early.
func TestSweepTaskDoesNotCloseWhileItemsHaveNoVerdict(t *testing.T) {
	l, tk := sweepFixture(t)

	_, err := l.Set(tk.ID, Change{State: task.Done, By: "dev"})
	require.ErrorIs(t, err, ErrUnswept)
	assert.ErrorContains(t, err, "L-1, L-2, L-3")
	assert.ErrorContains(t, err, "zmina later swept "+tk.ID)

	got, _ := l.Store.Get(tk.ID)
	assert.Equal(t, task.Open, got.State, "the task stays open")

	_, err = l.Later.Close("L-1", later.Done, "fixed in the last release", l.Now())
	require.NoError(t, err)
	_, err = l.Later.Keep("L-2", "the API is not ready", tk.ID, l.Now())
	require.NoError(t, err)

	_, err = l.Set(tk.ID, Change{State: task.Done, By: "dev"})
	require.ErrorIs(t, err, ErrUnswept)
	assert.ErrorContains(t, err, "L-3")
	assert.NotContains(t, err.Error(), "L-1")
	assert.NotContains(t, err.Error(), "L-2")

	_, err = l.Later.Close("L-3", later.Taken, "", l.Now())
	require.NoError(t, err)

	closed, err := l.Set(tk.ID, Change{State: task.Done, By: "dev"})
	require.NoError(t, err)
	assert.Equal(t, task.Done, closed.State)
	assert.Nil(t, closed.Closed.Unchecked, "nothing was forced")
}

// A keep that answered another sweep, or none, does not count for this one; one that answered it counts whatever the clock
// said when it was made.
func TestOnlyAKeepForThisSweepIsItsVerdict(t *testing.T) {
	l, tk := sweepFixture(t)

	_, err := l.Later.Keep("L-1", "kept at the last sweep", "T-0", tk.Created.Add(time.Hour))
	require.NoError(t, err)
	_, err = l.Later.Keep("L-2", "kept outside any sweep", "", tk.Created.Add(time.Hour))
	require.NoError(t, err)
	_, err = l.Later.Keep("L-3", "kept for this one, the clock was set back", tk.ID, tk.Created.Add(-48*time.Hour))
	require.NoError(t, err)

	_, err = l.Set(tk.ID, Change{State: task.Done, By: "dev"})
	require.ErrorIs(t, err, ErrUnswept)
	assert.ErrorContains(t, err, "L-1, L-2")
	assert.NotContains(t, err.Error(), "L-3")
}

// The owner and the dispatcher close a sweep past its check, like a task past its own; a worker cannot, and the closing
// says what was bypassed.
func TestOnlyTheOwnerAndTheDispatcherCloseASweepEarly(t *testing.T) {
	l, tk := sweepFixture(t)

	_, err := l.Set(tk.ID, Change{State: task.Done, By: "dev", Force: true})
	require.ErrorIs(t, err, ErrCheckForce)

	got, _ := l.Store.Get(tk.ID)
	assert.Equal(t, task.Open, got.State)

	closed, err := l.Set(tk.ID, Change{State: task.Done, By: "boss", Force: true})
	require.NoError(t, err)
	assert.Equal(t, task.Done, closed.State)
	require.NotNil(t, closed.Closed)
	require.NotNil(t, closed.Closed.Unchecked)
	assert.Equal(t, "boss", closed.Closed.Unchecked.By)
	assert.Equal(t, "zmina later swept "+tk.ID, closed.Closed.Unchecked.Run)

	// Forcing a sweep that has its verdicts says nothing was bypassed.
	other, err := l.New(NewTask{Role: "dev", Asked: "again", By: "boss", Sweep: []string{"L-1"}})
	require.NoError(t, err)
	_, err = l.Later.Close("L-1", later.Dropped, "no longer wanted: the product is gone", l.Now())
	require.NoError(t, err)

	closed, err = l.Set(other.ID, Change{State: task.Done, By: "hand", Force: true})
	require.NoError(t, err)
	assert.Nil(t, closed.Closed.Unchecked)
}

// Dropping a sweep, changing it, and closing a task that is no sweep are as they were.
func TestSweepHookLeavesEverythingElseAlone(t *testing.T) {
	l, tk := sweepFixture(t)

	got, err := l.Set(tk.ID, Change{Note: "halfway"})
	require.NoError(t, err)
	assert.Equal(t, "halfway", got.Note)

	got, err = l.Set(tk.ID, Change{State: task.Waiting})
	require.NoError(t, err)
	assert.Equal(t, task.Waiting, got.State)

	got, err = l.Set(tk.ID, Change{State: task.Dropped, By: "dev"})
	require.NoError(t, err)
	assert.Equal(t, task.Dropped, got.State, "a sweep that is given up can be dropped")

	plain, err := l.New(NewTask{Role: "dev", Asked: "plain"})
	require.NoError(t, err)

	got, err = l.Set(plain.ID, Change{State: task.Done, By: "dev"})
	require.NoError(t, err)
	assert.Equal(t, task.Done, got.State)

	// A ledger with no parked list to look at (a test's, a tool's) asks nothing of a sweep.
	l.Later = nil

	swept, err := l.New(NewTask{Role: "dev", Asked: "sweep", Sweep: []string{"L-1"}})
	require.NoError(t, err)

	got, err = l.Set(swept.ID, Change{State: task.Done, By: "dev"})
	require.NoError(t, err)
	assert.Equal(t, task.Done, got.State)
}

func TestSweepThatCannotBeReadIsRefusedNotClosed(t *testing.T) {
	l, tk := sweepFixture(t)

	// An item file that does not read is no verdict.
	require.NoError(t, writeFile(filepath.Join(l.Later.Dir, "L-2.json"), `{"id": "L-2", `))
	_, err := l.Later.Close("L-1", later.Done, "x", l.Now())
	require.NoError(t, err)
	_, err = l.Later.Close("L-3", later.Done, "x", l.Now())
	require.NoError(t, err)

	_, err = l.Set(tk.ID, Change{State: task.Done, By: "dev"})
	require.ErrorIs(t, err, ErrUnswept)
	assert.ErrorContains(t, err, "L-2")
}

func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o600) }
