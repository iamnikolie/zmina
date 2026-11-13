package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/tmux"
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

	// The beat looked an hour ago and found too few: it does not look again before then.
	assert.Empty(t, sweepBeat(t, file, now.Add(30*time.Minute)))
	assert.Empty(t, sweepTasks(t, file))

	out := sweepBeat(t, file, now.Add(61*time.Minute))
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

// What a beat read before it took the lock is checked again under it: a sweep made meanwhile, or a task the role was handed,
// is seen by looking at what came after the highest task number the scan saw.
func TestAutomaticSweepChecksAgainUnderTheLock(t *testing.T) {
	file := testCrew(t, "  dev:\n"+autoSweep)
	c := mustCrew(t, file)

	for range 3 {
		parked(t, file, "old", 40*day, nil)
	}

	view := readLedger(c)
	require.NoError(t, view.err)
	assert.Empty(t, sweepRaced(c, view, "dev"), "nothing came after the scan")

	_, _, err := zm(t, file, "later", "sweep", "dev")
	require.NoError(t, err)
	assert.Equal(t, "T-1 is an open sweep", sweepRaced(c, view, "dev"))

	_, _, err = zm(t, file, "task", "done", "T-1", "--force")
	require.NoError(t, err)
	assert.Empty(t, sweepRaced(c, view, "dev"), "a sweep that is closed is not in the way")

	_, _, err = zm(t, file, "task", "new", "dev", "fix the login")
	require.NoError(t, err)
	assert.Equal(t, "dev was handed T-2", sweepRaced(c, view, "dev"))
	assert.Empty(t, sweepRaced(c, view, "dispatcher"))

	require.NoError(t, os.WriteFile(filepath.Join(c.Layout().Tasks(), "T-9.json"), []byte(`{"id": "T-9", `), 0o600))
	assert.Equal(t, "T-9.json does not read", sweepRaced(c, view, "dispatcher"))
}

func marker(t *testing.T, file string) sweepMarker {
	t.Helper()

	return readSweepMarker(mustCrew(t, file).Layout().LaterSweep())
}

// A failing attempt is not made again every beat: the marker keeps when the next may be, an hour on, and the log hears of each
// reason once a day.
func TestAutomaticSweepBacksOffAfterAFailureAndTellsEachReasonOnceADay(t *testing.T) {
	file := testCrew(t, "  dev:\n"+autoSweep)
	c := mustCrew(t, file)
	now := time.Now()

	for range 3 {
		parked(t, file, "old", 40*day, nil)
	}

	var tries []string

	reason := errors.New("tmux did not answer")
	keep := sweepStarter
	sweepStarter = func(a *app, s *shift.Shift, sp sweepSpec) (task.Task, tmux.Pane, error) {
		tries = append(tries, sp.Role)

		return task.Task{}, tmux.Pane{}, reason
	}
	t.Cleanup(func() { sweepStarter = keep })

	failures := func() []events.Event {
		all, err := eventLog(c).Read(events.Filter{})
		require.NoError(t, err)

		var out []events.Event

		for _, e := range all {
			if e.Event == "later-sweep-failed" {
				out = append(out, e)
			}
		}

		return out
	}

	out := sweepBeat(t, file, now)
	assert.Contains(t, out, "later sweep: tmux did not answer (tried again in 1h)")
	assert.Equal(t, []string{"dev"}, tries)
	require.Len(t, failures(), 1)
	assert.Equal(t, "tmux did not answer", failures()[0].Detail)
	assert.Equal(t, "ticker", failures()[0].By)

	mk := marker(t, file)
	assert.WithinDuration(t, now.Add(time.Hour), mk.NotBefore, time.Second)
	assert.WithinDuration(t, now, mk.Failed, time.Second)

	// The beats of the hour that follows try nothing.
	for _, after := range []time.Duration{time.Minute, 30 * time.Minute, 59 * time.Minute} {
		assert.Empty(t, sweepBeat(t, file, now.Add(after)))
	}

	assert.Equal(t, []string{"dev"}, tries)

	// An hour on it is tried again; the same reason is not another event within the day.
	out = sweepBeat(t, file, now.Add(time.Hour+time.Second))
	assert.Contains(t, out, "tried again in 1h")
	assert.Len(t, tries, 2)
	assert.Len(t, failures(), 1, "one event for a reason a day")

	sweepBeat(t, file, now.Add(23*time.Hour))
	assert.Len(t, failures(), 1)

	sweepBeat(t, file, now.Add(25*time.Hour))
	assert.Len(t, failures(), 2, "told again after a day")

	// Another reason is told at once.
	reason = errors.New("the routes name no seat")
	sweepBeat(t, file, now.Add(27*time.Hour))
	require.Len(t, failures(), 3)
	assert.Equal(t, "the routes name no seat", failures()[2].Detail)

	// And once it works the marker says when the next sweep is due: every after the one made.
	sweepStarter = keep
	require.Contains(t, sweepBeat(t, file, now.Add(29*time.Hour)), "later sweep: T-1 for dev")
	assert.WithinDuration(t, time.Now().Add(7*day), marker(t, file).NotBefore, 5*time.Second)
}

// The marker is read before anything else: a beat that is too early reads no task and no item. One that is missing,
// does not read or is past says to look.
func TestAutomaticSweepMarkerGatesTheBeat(t *testing.T) {
	file := testCrew(t, "  dev:\n"+autoSweep)
	c := mustCrew(t, file)
	now := time.Now()

	for range 3 {
		parked(t, file, "old", 40*day, nil)
	}

	path := c.Layout().LaterSweep()
	write := func(mk sweepMarker) {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		raw, err := json.Marshal(mk)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, raw, 0o600))
	}

	write(sweepMarker{NotBefore: now.Add(10 * time.Minute)})
	assert.Empty(t, sweepBeat(t, file, now), "not due before: nothing is looked at")
	assert.Empty(t, sweepTasks(t, file))

	require.NoError(t, os.WriteFile(path, []byte("not json {"), 0o600))
	assert.Contains(t, sweepBeat(t, file, now), "later sweep: T-1 for dev", "a marker that does not read says to look")

	require.NoError(t, os.Remove(path))
	_, _, err := zm(t, file, "task", "done", "T-1", "--force")
	require.NoError(t, err)

	// Past: due again after every, and the marker says so after the sweep it made.
	write(sweepMarker{NotBefore: now.Add(-time.Minute)})
	assert.Empty(t, sweepBeat(t, file, now.Add(time.Hour)), "within every of the last sweep: not due, and the marker now says when")
	assert.WithinDuration(t, mustTaskCreated(t, file, "T-1").Add(7*day), marker(t, file).NotBefore, time.Second)

	assert.Empty(t, sweepBeat(t, file, now.Add(6*day)), "the marker holds it")
}

func mustTaskCreated(t *testing.T, file, id string) time.Time {
	t.Helper()

	tk, err := taskStore(mustCrew(t, file)).Get(id)
	require.NoError(t, err)

	return tk.Created
}

// With fewer than three items to sweep the beat looks again in an hour, not at every beat; with a sweep open too.
func TestAutomaticSweepWaitsAnHourWhenTooFewQualifyOrASweepIsOpen(t *testing.T) {
	file := testCrew(t, "  dev:\n"+autoSweep)
	now := time.Now()

	parked(t, file, "old", 40*day, nil)
	parked(t, file, "old too", 40*day, nil)

	assert.Empty(t, sweepBeat(t, file, now))
	assert.WithinDuration(t, now.Add(time.Hour), marker(t, file).NotBefore, time.Second)

	// A third one is waited for too: the marker holds the beat for the hour.
	parked(t, file, "old three", 40*day, nil)
	assert.Empty(t, sweepBeat(t, file, now.Add(30*time.Minute)))
	assert.Empty(t, sweepTasks(t, file))

	require.Contains(t, sweepBeat(t, file, now.Add(61*time.Minute)), "later sweep: T-1 for dev")

	// While it is open, an hour at a time.
	later := now.Add(30 * day)
	assert.Empty(t, sweepBeat(t, file, later))
	assert.WithinDuration(t, later.Add(time.Hour), marker(t, file).NotBefore, time.Second)
	assert.Len(t, sweepTasks(t, file), 1)
}

// A task file that does not read may be a sweep: nothing is made while one is there, and the beat looks again in an hour.
func TestAutomaticSweepIsNotDueWhileATaskFileDoesNotRead(t *testing.T) {
	file := testCrew(t, "  dev:\n"+autoSweep)
	now := time.Now()

	for range 3 {
		parked(t, file, "old", 40*day, nil)
	}

	require.NoError(t, os.MkdirAll(filepath.Join(filepath.Dir(file), "tasks"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(file), "tasks", "T-3.json"), []byte(`{"id": "T-3", `), 0o600))

	assert.Empty(t, sweepBeat(t, file, now))
	assert.Empty(t, sweepTasks(t, file))
	assert.WithinDuration(t, now.Add(time.Hour), marker(t, file).NotBefore, time.Second)

	require.NoError(t, os.Remove(filepath.Join(filepath.Dir(file), "tasks", "T-3.json")))
	assert.Contains(t, sweepBeat(t, file, now.Add(61*time.Minute)), "later sweep: T-1 for dev")
}

func TestSweepFieldIsInTheTaskJSON(t *testing.T) {
	file := staleCrew(t, "")

	_, _, err := zm(t, file, "later", "sweep", "dev")
	require.NoError(t, err)

	out, _, err := zm(t, file, "task", "show", "T-1", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"sweep": [`)
}
