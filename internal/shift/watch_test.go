package shift

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/watch"
)

const watchYAML = crewYAML + `watch:
  ci-red:
    kind: exec
    every: 5m
    run: [detector]
    baseline: false
`

func ev(key, fp string) watch.Event {
	return watch.Event{Key: key, Fingerprint: fp, Text: "pipeline " + fp}
}

// cycle is one ticker run of the watch: begun, its source answering res, finished.
func cycle(t *testing.T, f fix, res watch.Result) []string {
	t.Helper()

	runs := f.s.DueWatches()
	require.Len(t, runs, 1)

	return f.s.FinishWatch(runs[0], res, false)
}

func TestWatchNotesThroughTheQueue(t *testing.T) {
	f := fixtureWith(t, watchYAML)

	cycle(t, f, watch.Result{Events: []watch.Event{ev("main", "red")}})
	assert.Equal(t, []string{"ci-red: pipeline red"}, f.s.State.Signals)
	assert.True(t, f.ev.has("watch:ci-red", "watch-noted"))
	assert.True(t, f.ev.has("watch:ci-red", "watch-run"))

	assert.Empty(t, f.s.DueWatches(), "not due again before every")

	*f.now = f.now.Add(5 * time.Minute)
	cycle(t, f, watch.Result{Events: []watch.Event{ev("main", "red")}})
	assert.Len(t, f.s.State.Signals, 1, "same fingerprint: no second note")
}

func TestWatchFailuresAndRecovery(t *testing.T) {
	f := fixtureWith(t, watchYAML)
	cycle(t, f, watch.Result{Events: []watch.Event{ev("main", "red")}})
	f.s.State.Signals = nil

	fail := watch.Result{Err: errors.New("dial tcp: lookup gitlab.example.com: no such host"), Stderr: []string{"boom"}}
	for range 4 {
		*f.now = f.now.Add(5 * time.Minute)
		did := cycle(t, f, fail)
		assert.Contains(t, did, "ci-red: boom", "stderr goes to the tick log")
	}

	assert.Equal(t, []string{"ci-red: failing for 3 runs: dial tcp: lookup gitlab.example.com: no such host"},
		f.s.State.Signals, "one health note per failing stretch")
	assert.Contains(t, f.s.State.Watch["ci-red"].Keys, "main", "a failed run never marks keys gone")
	assert.True(t, f.s.State.Watch["ci-red"].Keys["main"].GoneAt.IsZero())
	assert.True(t, f.ev.has("watch:ci-red", "watch-failed"))

	*f.now = f.now.Add(5 * time.Minute)
	cycle(t, f, watch.Result{Events: []watch.Event{ev("main", "red")}})
	*f.now = f.now.Add(5 * time.Minute)
	cycle(t, f, watch.Result{Events: []watch.Event{ev("main", "red")}})
	assert.Equal(t, "ci-red: recovered", f.s.State.Signals[1])
	assert.Len(t, f.s.State.Signals, 2, "recovered once")
}

func TestWatchQueueFullRetries(t *testing.T) {
	f := fixtureWith(t, watchYAML)
	for i := range signalsMax {
		f.s.State.Signals = append(f.s.State.Signals, "x"+strings.Repeat("y", i))
	}

	did := cycle(t, f, watch.Result{Events: []watch.Event{ev("main", "red")}})
	assert.Contains(t, strings.Join(did, "\n"), "not queued, retried next run")
	assert.NotContains(t, f.s.State.Watch["ci-red"].Keys, "main")

	f.s.State.Signals = nil
	*f.now = f.now.Add(5 * time.Minute)
	cycle(t, f, watch.Result{Events: []watch.Event{ev("main", "red")}})
	assert.Equal(t, []string{"ci-red: pipeline red"}, f.s.State.Signals)
}

func TestWatchRunsNeverOverlap(t *testing.T) {
	f := fixtureWith(t, watchYAML)

	runs := f.s.DueWatches()
	require.Len(t, runs, 1)

	*f.now = f.now.Add(time.Minute)
	assert.Empty(t, f.s.DueWatches(), "the previous run still goes")

	_, err := f.s.BeginWatch("ci-red")
	require.ErrorContains(t, err, "a run is going")
}

func TestWatchRestartNotesOnce(t *testing.T) {
	f := fixtureWith(t, watchYAML)

	runs := f.s.DueWatches()
	require.Len(t, runs, 1)
	// Killed between the source call and the commit: the mark stays, its process is gone.
	f.s.State.Watch["ci-red"].Running.PID = 999999

	*f.now = f.now.Add(time.Minute)
	cycle(t, f, watch.Result{Events: []watch.Event{ev("main", "red")}})
	assert.Equal(t, []string{"ci-red: pipeline red"}, f.s.State.Signals, "the next run notes exactly once")
	assert.Nil(t, f.s.State.Watch["ci-red"].Running)
}

func TestWatchStateLostIsABaseline(t *testing.T) {
	f := fixtureWith(t, strings.Replace(watchYAML, "    baseline: false\n", "", 1))

	cycle(t, f, watch.Result{Events: []watch.Event{ev("main", "red")}})
	assert.Empty(t, f.s.State.Signals, "the first run of a watch records only")

	f.s.State = NewState()
	*f.now = f.now.Add(time.Minute)
	cycle(t, f, watch.Result{Events: []watch.Event{ev("main", "red"), ev("dev", "red")}})
	assert.Empty(t, f.s.State.Signals, "a deleted state.json starts over as a baseline")

	*f.now = f.now.Add(5 * time.Minute)
	cycle(t, f, watch.Result{Events: []watch.Event{ev("main", "red"), ev("dev", "green")}})
	assert.Equal(t, []string{"ci-red: pipeline green"}, f.s.State.Signals)
}

func TestWatchBaselineByHand(t *testing.T) {
	f := fixtureWith(t, watchYAML)
	run, err := f.s.BeginWatch("ci-red")
	require.NoError(t, err)
	f.s.FinishWatch(run, watch.Result{Events: []watch.Event{ev("main", "red")}}, true)
	assert.Empty(t, f.s.State.Signals)
	assert.Equal(t, "red", f.s.State.Watch["ci-red"].Keys["main"].Fingerprint)

	n, err := f.s.ForgetWatch("ci-red", "")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestWatchOffAndPruned(t *testing.T) {
	f := fixtureWith(t, strings.Replace(watchYAML, "    baseline: false\n", "    enabled: false\n", 1))
	assert.Empty(t, f.s.DueWatches())

	f.s.State.Watch["gone"] = watch.NewState()
	f.s.PruneWatches()
	assert.NotContains(t, f.s.State.Watch, "gone")
}

func TestWatchDropIsLogged(t *testing.T) {
	f := fixtureWith(t, strings.Replace(watchYAML, "    baseline: false\n", "    quiet: 10m\n    baseline: false\n", 1))

	at := *f.now
	e := ev("g/r!12:review", "5")
	e.At = &at
	cycle(t, f, watch.Result{Events: []watch.Event{e}})
	assert.Empty(t, f.s.State.Signals)

	*f.now = f.now.Add(5 * time.Minute)
	did := cycle(t, f, watch.Result{})
	assert.Contains(t, did, "ci-red: dropped g/r!12:review (gone-before-quiet)")
	assert.True(t, f.ev.has("watch:ci-red", "watch-dropped"))
	assert.Contains(t, f.s.WatchViews()[0].Recent[0], "dropped g/r!12:review")
}

func TestRunTickRunsSourcesOutsideTheLock(t *testing.T) {
	f := fixtureWith(t, watchYAML)
	f.s.Tick()
	f.s.State.Notes = nil // nothing else waits: the watch's news is what goes this beat
	f.status("dispatcher", "idle", *f.now)
	*f.now = f.now.Add(bootPause)

	held := false
	locked := func(fn func(s *Shift) error) error {
		held = true
		defer func() { held = false }()

		return fn(f.s)
	}

	var lines []string

	err := RunTick(locked, func(runs []WatchRun) []watch.Result {
		assert.False(t, held, "the source runs without the lock")
		require.Len(t, runs, 1)

		return []watch.Result{{Events: []watch.Event{ev("main", "red")}}}
	}, func(l []string) { lines = append(lines, l...) })
	require.NoError(t, err)

	assert.Contains(t, strings.Join(lines, "\n"), "told the dispatcher: [zmina] ci-red: pipeline red")
	assert.Len(t, f.tm.typed["dispatcher"], 1, "typed once")
}
