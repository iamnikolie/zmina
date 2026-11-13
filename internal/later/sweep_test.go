package later

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/task"
)

var sweepNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func daysAgo(n int) time.Time { return sweepNow.Add(-time.Duration(n) * 24 * time.Hour) }

func ids(items []Item) []string {
	out := []string{}

	for _, it := range items {
		out = append(out, it.ID)
	}

	return out
}

// A sweep is about the open items that are old, not looked at lately and not waiting on a day: oldest first.
func TestStalePicksWhatAnItemIsNotTooYoungOrSetAsideFor(t *testing.T) {
	older := 14 * 24 * time.Hour
	items := []Item{
		{ID: "L-1", State: Parked, Created: daysAgo(20)},
		{ID: "L-2", State: Parked, Created: daysAgo(14)},                                     // exactly as old as older: stale
		{ID: "L-3", State: Parked, Created: daysAgo(13)},                                     // too young
		{ID: "L-4", State: Parked, Created: daysAgo(40), Reviewed: daysAgo(3)},               // kept lately
		{ID: "L-5", State: Parked, Created: daysAgo(40), Reviewed: daysAgo(14)},              // kept, but a sweep ago
		{ID: "L-6", State: Parked, Created: daysAgo(40)},                                     // its day is ahead
		{ID: "L-7", State: Parked, Created: daysAgo(40)},                                     // its day passed, still not rung
		{ID: "L-8", State: Parked, Created: daysAgo(40), Back: daysAgo(1)},                   // on the owner's todo
		{ID: "L-9", State: Done, Created: daysAgo(40)},                                       // closed
		{ID: "L-10", State: Taken, Created: daysAgo(40), Task: "T-1"},                        // closed
		{ID: "L-11", State: State("later"), Created: daysAgo(30)},                            // an unknown state is parked
		{ID: "L-12", State: Parked, Created: sweepNow.Add(time.Hour)},                        // a clock that was ahead
		{ID: "L-13", State: Parked, Created: daysAgo(20), Reviewed: sweepNow.Add(time.Hour)}, // reviewed "in the future"
	}
	back := map[string]time.Time{"L-6": sweepNow.Add(48 * time.Hour), "L-7": sweepNow.Add(-time.Hour)}

	got := Stale(items, nil, back, sweepNow, older)
	assert.ElementsMatch(t, []string{"L-1", "L-2", "L-5", "L-7", "L-11"}, ids(got))
	assert.Equal(t, []string{"L-5", "L-7", "L-11", "L-1", "L-2"}, ids(got), "oldest first")
}

// An item a released role's sweep was parked as (task park, an idle release) is the sweep itself, not something to sweep.
func TestStaleNeverListsAnItemParkedFromASweep(t *testing.T) {
	items := []Item{
		{ID: "L-1", State: Parked, Created: daysAgo(40), From: "T-4", Role: "dev", Text: "Sweep of 12 parked items"},
		{ID: "L-2", State: Parked, Created: daysAgo(40), From: "T-3", Role: "dev", Text: "fix the login"},
		{ID: "L-3", State: Parked, Created: daysAgo(40), From: "T-8", Text: "from a task that is gone"},
		{ID: "L-4", State: Parked, Created: daysAgo(40)},
	}
	tasks := []task.Task{
		{ID: "T-3", State: task.Parked, Later: "L-2"},
		{ID: "T-4", State: task.Parked, Later: "L-1", Sweep: []string{"L-5", "L-6"}},
	}

	assert.Equal(t, []string{"L-2", "L-3", "L-4"}, ids(Stale(items, tasks, nil, sweepNow, 14*24*time.Hour)))
	assert.Equal(t, []string{"L-1", "L-2", "L-3", "L-4"}, ids(Stale(items, nil, nil, sweepNow, 14*24*time.Hour)),
		"with no ledger to look at, nothing is told apart")
}

func TestStaleTieBreaksByNumberAndLeavesTheInputAlone(t *testing.T) {
	items := []Item{
		{ID: "L-10", State: Parked, Created: daysAgo(20)},
		{ID: "L-9", State: Parked, Created: daysAgo(20)},
		{ID: "L-2", State: Parked, Created: daysAgo(30)},
	}

	got := Stale(items, nil, nil, sweepNow, 14*24*time.Hour)
	assert.Equal(t, []string{"L-2", "L-9", "L-10"}, ids(got))
	assert.Equal(t, "L-10", items[0].ID, "the caller's slice keeps its order")
}

// An item has its verdict when it is closed or taken, or kept for this very sweep: what a keep says it answers is the
// task's id, not a time, so a clock that was set back between the task and the keep changes nothing.
func TestUnsweptIsWhatHasNoVerdictForThisSweep(t *testing.T) {
	items := []Item{
		{ID: "L-1", State: Parked, Created: daysAgo(20)},                                         // nothing yet
		{ID: "L-2", State: Parked, Created: daysAgo(20), Reviewed: daysAgo(30), KeptFor: "T-2"},  // kept at the last sweep
		{ID: "L-3", State: Parked, Created: daysAgo(20), Reviewed: sweepNow, KeptFor: "T-9"},     // kept for this one
		{ID: "L-4", State: Done, Created: daysAgo(20)},                                           // closed
		{ID: "L-5", State: Dropped, Created: daysAgo(20)},                                        // closed
		{ID: "L-6", State: Taken, Created: daysAgo(20), Task: "T-4"},                             // taken
		{ID: "L-7", State: Parked, Created: daysAgo(20), Reviewed: daysAgo(400), KeptFor: "T-9"}, // the clock was set back: still its verdict
		{ID: "L-8", State: Parked, Created: daysAgo(20), Reviewed: sweepNow},                     // kept outside any sweep: no verdict for one
	}

	got := Unswept(items, []string{"L-1", "L-2", "L-3", "L-4", "L-5", "L-6", "L-7", "L-8", "L-99"}, "T-9")
	assert.Equal(t, []string{"L-1", "L-2", "L-8", "L-99"}, got, "an item that does not read has no verdict to show")

	assert.Empty(t, Unswept(items, []string{"L-3", "L-4", "L-7"}, "T-9"))
	assert.Empty(t, Unswept(items, nil, "T-9"), "a task that covers nothing has nothing to wait for")
}

func TestSweepTasksAreFoundByWhatTheyCover(t *testing.T) {
	tasks := []task.Task{
		{ID: "T-1", State: task.Open, Created: daysAgo(30)},
		{ID: "T-2", State: task.Done, Sweep: []string{"L-1", "L-2"}, Created: daysAgo(20)},
		{ID: "T-3", State: task.Dropped, Sweep: []string{"L-3"}, Created: daysAgo(10)},
		{ID: "T-4", State: task.Open, Created: daysAgo(2)},
	}

	_, open := OpenSweep(tasks)
	assert.False(t, open, "closed sweeps are not open")
	assert.Equal(t, daysAgo(10), LastSweep(tasks), "the newest one made, whatever became of it")

	_, ok := InSweep(tasks, "L-1")
	assert.False(t, ok, "a closed sweep asks for no evidence")

	tasks = append(tasks, task.Task{ID: "T-5", State: task.Waiting, Sweep: []string{"L-7", "L-8"}, Created: daysAgo(1)})

	sw, open := OpenSweep(tasks)
	require.True(t, open)
	assert.Equal(t, "T-5", sw.ID, "a waiting sweep is still open")
	assert.Equal(t, daysAgo(1), LastSweep(tasks))

	got, ok := InSweep(tasks, "L-8")
	require.True(t, ok)
	assert.Equal(t, "T-5", got.ID)

	_, ok = InSweep(tasks, "L-9")
	assert.False(t, ok)

	assert.True(t, LastSweep([]task.Task{{ID: "T-1"}}).IsZero(), "none made")
}

// A file written before reviewed was kept reads as never reviewed, and an item never reviewed writes no key.
func TestReviewedIsOptionalInTheRecord(t *testing.T) {
	s := &Store{Dir: t.TempDir()}

	old := `{"id":"L-1","text":"old idea","state":"parked","created":"2026-10-01T10:00:00Z","updated":"2026-10-01T10:00:00Z"}`
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "L-1.json"), []byte(old), 0o600))

	got, err := s.Get("L-1")
	require.NoError(t, err)
	assert.True(t, got.Reviewed.IsZero())

	require.NoError(t, s.Put(got))

	raw, err := os.ReadFile(filepath.Join(s.Dir, "L-1.json"))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "reviewed")

	kept, err := s.Keep("1", "still the plan: the API is not ready", "", sweepNow)
	require.NoError(t, err)
	assert.Equal(t, sweepNow, kept.Reviewed)
	assert.Equal(t, sweepNow, kept.Updated)
	assert.Equal(t, "still the plan: the API is not ready", kept.Note)
	assert.Equal(t, "old idea", kept.Text)
	assert.Equal(t, Parked, kept.State)

	raw, err = os.ReadFile(filepath.Join(s.Dir, "L-1.json"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"reviewed": "2026-11-13T12:00:00Z"`)

	again, err := s.Keep("L-1", "", "T-7", sweepNow.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, "still the plan: the API is not ready", again.Note, "an empty note leaves the note")
	assert.Equal(t, sweepNow.Add(time.Hour), again.Reviewed)
	assert.Empty(t, kept.KeptFor, "kept outside a sweep answers none")
	assert.Equal(t, "T-7", again.KeptFor, "kept in one, it says which")

	raw, err = os.ReadFile(filepath.Join(s.Dir, "L-1.json"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"kept_for": "T-7"`)

	outside, err := s.Keep("L-1", "again, outside it", "", sweepNow.Add(2*time.Hour))
	require.NoError(t, err)
	assert.Empty(t, outside.KeptFor, "a keep outside a sweep is no answer to the one before")

	_, err = s.Close("L-1", Done, "", sweepNow)
	require.NoError(t, err)

	_, err = s.Keep("L-1", "x", "", sweepNow)
	assert.ErrorContains(t, err, "L-1 is already done")

	_, err = s.Keep("L-9", "x", "", sweepNow)
	assert.ErrorContains(t, err, "no item L-9")
}
