package shift

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/watch"
)

const mrURL = "https://example.com/acme/site/-/merge_requests/12"

func approved() watch.Event {
	return watch.Event{Key: "acme/site!12:approved", Fingerprint: "sha1", Text: "acme/site!12 approved, not merged",
		URL: mrURL}
}

// runFor runs the watch on every beat for d, its source reporting events each time.
func runFor(t *testing.T, f fix, d time.Duration, events ...watch.Event) {
	t.Helper()

	end := f.now.Add(d)
	for f.now.Before(end) {
		*f.now = f.now.Add(time.Minute)
		for _, run := range f.s.DueWatches() {
			f.s.FinishWatch(run, watch.Result{Events: events}, false)
		}
	}
}

func watchKeys(t *testing.T, f fix) []string {
	t.Helper()

	_, p, err := f.s.RecoveryPack()
	require.NoError(t, err)

	var ids []string
	for _, e := range p.Entries["watch_keys"] {
		ids = append(ids, e.ID)
	}

	return ids
}

// The receiver loses a note (typed while it compacted, then forgotten): the watch says it again within renotify,
// and the recovery pack lists it until it is acked.
func TestWatchNoteLostIsSaidAgainUntilAcked(t *testing.T) {
	f := fixtureWith(t, watchYAML)
	start := *f.now

	runFor(t, f, 5*time.Minute, approved())
	require.Equal(t, []string{"ci-red: acme/site!12 approved, not merged " + mrURL}, f.s.State.Signals)
	f.s.State.Signals = nil // typed into the dispatcher, then lost to a compaction

	assert.Equal(t, []string{"ci-red/acme/site!12:approved"}, watchKeys(t, f))
	_, p, err := f.s.RecoveryPack()
	require.NoError(t, err)
	assert.Equal(t, "zmina watch ack ci-red '"+mrURL+"'", p.Entries["watch_keys"][0].Ref)

	since := f.s.State.Watch["ci-red"].Keys["acme/site!12:approved"].Since
	runFor(t, f, 2*time.Hour, approved())
	require.Len(t, f.s.State.Signals, 1, "said again within renotify")
	assert.Equal(t, "ci-red: acme/site!12 approved, not merged "+mrURL+" (still pending since "+
		since.In(f.s.Crew.Location()).Format("Mon 2 Jan 15:04")+"; ack: zmina watch ack ci-red '"+mrURL+"')",
		f.s.State.Signals[0])
	assert.LessOrEqual(t, f.s.State.Watch["ci-red"].Keys["acme/site!12:approved"].NotedAt.Sub(start),
		2*time.Hour+5*time.Minute)
	assert.True(t, f.ev.has("watch:ci-red", "watch-renoted"))
	assert.Equal(t, []string{"ci-red/acme/site!12:approved"}, watchKeys(t, f), "still in the pack")

	views := f.s.WatchViews()
	assert.Equal(t, 1, views[0].Unacked)

	acked, err := f.s.AckWatch("ci-red", []string{mrURL}, false, "dispatcher")
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/site!12:approved"}, acked)
	assert.Empty(t, watchKeys(t, f), "acked: out of the pack")
	assert.Zero(t, f.s.WatchViews()[0].Unacked)

	f.s.State.Signals = nil
	runFor(t, f, 6*time.Hour, approved())
	assert.Empty(t, f.s.State.Signals, "acked: never said again")

	_, err = f.s.AckWatch("ci-red", []string{"nope"}, false, "hand")
	require.Error(t, err)
}

func TestWatchRenoteWaitsForTheEarlierNote(t *testing.T) {
	f := fixtureWith(t, watchYAML)

	runFor(t, f, 5*time.Minute, approved())
	runFor(t, f, 3*time.Hour, approved())
	assert.Len(t, f.s.State.Signals, 1, "the first note still waits to be typed: not said twice")
}

func TestWatchTaskCarryingTheNoteAcksIt(t *testing.T) {
	f := fixtureWith(t, watchYAML)
	f.tk.list = []task.Task{{ID: "T-1", Role: "dev", State: task.Done, URL: mrURL, Created: f.now.Add(-time.Hour)}}

	runFor(t, f, 5*time.Minute, approved())
	f.s.State.Signals = nil
	assert.Len(t, watchKeys(t, f), 1, "a task made before the note does not carry it")

	f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: "dev", State: task.Open, URL: mrURL, Created: *f.now})
	assert.Empty(t, watchKeys(t, f), "a task with its url carries it")
	assert.Zero(t, f.s.WatchViews()[0].Unacked)

	runFor(t, f, 3*time.Hour, approved())
	assert.Empty(t, f.s.State.Signals, "turned into a task: not said again")
	assert.Equal(t, "task T-2", f.s.State.Watch["ci-red"].Keys["acme/site!12:approved"].AckedBy)
}

func TestWatchTaskNamingTheKeyAcksIt(t *testing.T) {
	f := fixtureWith(t, watchYAML)

	runFor(t, f, 5*time.Minute, ev("disk:/data", "90"))
	f.tk.list = []task.Task{{ID: "T-3", Role: "dev", State: task.Open, Created: *f.now,
		Asked: "free space on the box (watch key disk:/data)."}}
	assert.Empty(t, watchKeys(t, f))

	f.tk.list[0].Asked = "free space on disk:/data2"
	assert.Len(t, watchKeys(t, f), 1, "a key matches as a whole word only")
}

func TestWatchKeyNoLongerReportedLeavesThePack(t *testing.T) {
	f := fixtureWith(t, watchYAML)

	runFor(t, f, 5*time.Minute, approved())
	f.s.State.Signals = nil
	require.Len(t, watchKeys(t, f), 1)

	runFor(t, f, 5*time.Minute)
	assert.Empty(t, watchKeys(t, f), "merged: the source stops reporting it")

	runFor(t, f, 3*time.Hour)
	assert.Empty(t, f.s.State.Signals)
}

func TestWatchAckAll(t *testing.T) {
	f := fixtureWith(t, watchYAML)

	runFor(t, f, 5*time.Minute, ev("a", "1"), ev("b", "1"))

	acked, err := f.s.AckWatch("ci-red", nil, true, "hand")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, acked)

	acked, err = f.s.AckWatch("ci-red", nil, true, "hand")
	require.NoError(t, err)
	assert.Empty(t, acked)

	_, err = f.s.AckWatch("no-such", nil, true, "hand")
	require.Error(t, err)
}

func TestWatchDryRunShowsRenote(t *testing.T) {
	f := fixtureWith(t, watchYAML)

	runFor(t, f, 5*time.Minute, approved())
	f.s.State.Signals = nil
	*f.now = f.now.Add(3 * time.Hour)

	outs := f.s.DryApply("ci-red", []watch.Event{approved()})
	assert.Equal(t, watch.Renoted, outs[0].Fate)
	assert.Zero(t, f.s.State.Watch["ci-red"].Keys["acme/site!12:approved"].Renotes, "a dry run writes nothing")
}
