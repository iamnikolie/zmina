package cmd

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

// sweepBeat runs the ticker's sweep step at now and returns what it printed.
func sweepBeat(t *testing.T, file string, now time.Time) string {
	t.Helper()

	var out bytes.Buffer

	a := &app{out: &out, err: &out, crew: file}
	a.laterSweepBeat(now)

	return out.String()
}

func sweepTasks(t *testing.T, file string) []task.Task {
	t.Helper()

	all, err := taskStore(mustCrew(t, file)).All()
	require.NoError(t, err)

	var out []task.Task

	for _, tk := range all {
		if len(tk.Sweep) > 0 {
			out = append(out, tk)
		}
	}

	return out
}

const autoSweep = "later_sweep: {role: dev}\n"

func TestAutomaticSweepIsOffUnlessTheCrewFileAsksForIt(t *testing.T) {
	file := testCrew(t, "  dev:\n")

	for range 5 {
		parked(t, file, "old", 40*day, nil)
	}

	assert.Empty(t, sweepBeat(t, file, time.Now()))
	assert.Empty(t, sweepTasks(t, file))
}

func TestAutomaticSweepMakesATaskForTheRoleWhenEnoughHasGoneStale(t *testing.T) {
	file := testCrew(t, "  dev:\n"+autoSweep)
	now := time.Now()

	parked(t, file, "old one", 40*day, nil)
	parked(t, file, "old two", 30*day, nil)
	assert.Empty(t, sweepBeat(t, file, now), "two stale items are too few")
	assert.Empty(t, sweepTasks(t, file))

	parked(t, file, "old three", 20*day, nil)
	parked(t, file, "young", day, nil)

	out := sweepBeat(t, file, now)
	assert.Contains(t, out, "later sweep: T-1 for dev, 3 of 3 stale parked items (dev is down: ")

	got := sweepTasks(t, file)
	require.Len(t, got, 1)
	assert.Equal(t, []string{"L-1", "L-2", "L-3"}, got[0].Sweep)
	assert.Equal(t, "dev", got[0].Role)
	assert.Equal(t, "ticker", got[0].By)
	assert.Contains(t, got[0].Asked, "zmina later swept T-1")

	evs, err := eventLog(mustCrew(t, file)).Read(events.Filter{Task: "T-1"})
	require.NoError(t, err)

	var found []events.Event

	for _, e := range evs {
		if e.Event == "later-sweep" {
			found = append(found, e)
		}
	}

	require.Len(t, found, 1, "one event for the sweep")
	assert.Equal(t, "ticker", found[0].By)
	assert.Equal(t, "dev", found[0].Role)

	// Nothing more while it is open, beat after beat.
	assert.Empty(t, sweepBeat(t, file, now.Add(time.Minute)))
	assert.Empty(t, sweepBeat(t, file, now.Add(30*day)))
	assert.Len(t, sweepTasks(t, file), 1)
}

func TestAutomaticSweepWaitsEveryAfterTheLastOne(t *testing.T) {
	file := testCrew(t, "  dev:\n"+"later_sweep: {role: dev, every: 3d, older: 14d, max: 4}\n")
	now := time.Now()

	for range 6 {
		parked(t, file, "old", 40*day, nil)
	}

	require.Contains(t, sweepBeat(t, file, now), "later sweep: T-1 for dev, 4 of 6 stale parked items")
	assert.Equal(t, []string{"L-1", "L-2", "L-3", "L-4"}, sweepTasks(t, file)[0].Sweep, "at most max, the oldest")

	// Closed by the owner at once, so the only thing in the way is the clock.
	_, _, err := zm(t, file, "task", "done", "T-1", "--force")
	require.NoError(t, err)

	assert.Empty(t, sweepBeat(t, file, now.Add(2*day)), "less than every since the last sweep was made")
	assert.Len(t, sweepTasks(t, file), 1)

	// L-1 to L-4 are stale and unkept again, and L-5, L-6 never had a verdict: six items, four of them taken.
	out := sweepBeat(t, file, now.Add(3*day+time.Minute))
	assert.Contains(t, out, "later sweep: T-2 for dev, 4 of 6 stale parked items")
	assert.Len(t, sweepTasks(t, file), 2)
}

// An item a sweep kept is not swept again until older has passed since: the list shrinks, it is not asked about again.
func TestAutomaticSweepLeavesKeptItemsAloneForOlder(t *testing.T) {
	file := testCrew(t, "  dev:\n"+"later_sweep: {role: dev, every: 1d, older: 14d}\n")
	now := time.Now()

	for range 4 {
		parked(t, file, "old", 40*day, nil)
	}

	require.Contains(t, sweepBeat(t, file, now), "T-1 for dev, 4 of 4")

	for _, id := range []string{"L-1", "L-2", "L-3"} {
		_, _, err := zm(t, file, "later", "keep", id, "--note", "still wanted")
		require.NoError(t, err)
	}

	_, _, err := zm(t, file, "later", "done", "L-4", "--note", "fixed in commit 4f2a9c1")
	require.NoError(t, err)

	_, _, err = zm(t, file, "task", "done", "T-1")
	require.NoError(t, err)

	assert.Empty(t, sweepBeat(t, file, now.Add(2*day)), "nothing is stale: three kept, one closed")
	assert.Empty(t, sweepBeat(t, file, now.Add(13*day)), "still within older of the keep")
	assert.Contains(t, sweepBeat(t, file, now.Add(15*day)), "T-2 for dev, 3 of 3 stale parked items", "kept a sweep ago: asked again")
}

func TestAutomaticSweepKeepsToTheTickersOwnConditions(t *testing.T) {
	file := testCrew(t, "  dev:\n"+autoSweep)
	now := time.Now()

	for range 4 {
		parked(t, file, "old", 40*day, nil)
	}

	// The shift is stopped: nothing is handed out, as a scheduled run is skipped.
	a := &app{out: &bytes.Buffer{}, err: &bytes.Buffer{}, crew: file}
	require.NoError(t, a.withState(func(s *shift.Shift) error { s.State.Stopped = now; return nil }))
	assert.Empty(t, sweepBeat(t, file, now))
	assert.Empty(t, sweepTasks(t, file))

	require.NoError(t, a.withState(func(s *shift.Shift) error { s.State.Stopped = time.Time{}; return nil }))

	// The role is busy with a task of its own: the sweep waits, as a scheduled run does.
	_, _, err := zm(t, file, "task", "new", "dev", "fix the login")
	require.NoError(t, err)
	assert.Empty(t, sweepBeat(t, file, now))
	assert.Empty(t, sweepTasks(t, file))

	_, _, err = zm(t, file, "task", "done", "T-1")
	require.NoError(t, err)

	assert.Contains(t, sweepBeat(t, file, now), "later sweep: T-2 for dev")
	assert.Len(t, sweepTasks(t, file), 1)
}

// Items waiting on a day or back on the owner's todo are not what the ticker sweeps, however many there are.
func TestAutomaticSweepCountsOnlyWhatQualifies(t *testing.T) {
	file := testCrew(t, "  dev:\n"+autoSweep)
	c := mustCrew(t, file)
	now := time.Now()

	for range 4 {
		id := parked(t, file, "waits", 40*day, nil)
		_, err := alarmStore(c).Add(alarm.Alarm{Kind: alarm.Later, At: now.Add(5 * day), When: "+5d", Later: id, Text: "w"}, now)
		require.NoError(t, err)
	}

	for range 3 {
		parked(t, file, "back", 40*day, func(it *later.Item) { it.Back = now.Add(-day) })
	}

	parked(t, file, "old one", 40*day, nil)
	parked(t, file, "old two", 40*day, nil)

	assert.Empty(t, sweepBeat(t, file, now))
	assert.Empty(t, sweepTasks(t, file))
}

// What a beat read before it took the lock is checked again under it: a sweep made meanwhile is seen.
func TestAutomaticSweepChecksAgainUnderTheLock(t *testing.T) {
	file := testCrew(t, "  dev:\n"+autoSweep)
	now := time.Now()

	for range 3 {
		parked(t, file, "old", 40*day, nil)
	}

	a := &app{out: &bytes.Buffer{}, err: &bytes.Buffer{}, crew: file}

	require.NoError(t, a.readState(func(s *shift.Shift) error {
		assert.True(t, laterSweepDue(s, *s.Crew.LaterSweep, now), "due: this is what the first read sees")

		return nil
	}))

	a.laterSweepBeat(now)
	require.Len(t, sweepTasks(t, file), 1)

	require.NoError(t, a.withState(func(s *shift.Shift) error {
		assert.False(t, laterSweepDue(s, *s.Crew.LaterSweep, now), "the open sweep is seen under the lock")

		return nil
	}))
}

func TestSweepFieldIsInTheTaskJSON(t *testing.T) {
	file := staleCrew(t, "")

	_, _, err := zm(t, file, "later", "sweep", "dev")
	require.NoError(t, err)

	out, _, err := zm(t, file, "task", "show", "T-1", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"sweep": [`)
}
