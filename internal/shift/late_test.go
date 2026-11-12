package shift

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/task"
)

// lateFixture is T-1 handed to the designer as hand-out 3, with a result of the designer's saved in the store.
func lateFixture(t *testing.T) fix {
	t.Helper()

	f := fixtureWith(t, crewYAML)
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open, Updated: f.now.UTC(),
		Delivery: &task.Delivery{ID: "pkt", Role: "designer", Gen: 3, Session: "sid-new", QueuedAt: f.now.UTC()}}}

	return f
}

// lateResult saves a result of T-1 with the text, as a report of a session does, and returns its revision.
func lateResult(t *testing.T, f fix, text string) string {
	t.Helper()

	r, err := f.s.Role("designer")
	require.NoError(t, err)

	shape := f.s.Crew.HandoffFor(r)
	values := map[string]string{}

	for _, sec := range shape.Sections {
		values[sec.Name] = text
	}

	ref, err := record.Save(f.s.Crew.StateDir(), record.Document{Version: 1, Kind: "result", Task: "T-1", Role: "designer",
		By: "designer", At: *f.now, Shape: shape, Values: values})
	require.NoError(t, err)

	return ref.Revision
}

func lateEvents(f fix) int {
	n := 0

	for _, e := range f.ev.list {
		if e.Event == "late-report" {
			n++
		}
	}

	return n
}

func lateNotes(f fix) int {
	n := 0

	for _, sig := range f.s.State.Signals {
		if strings.Contains(sig, "was handed out again") {
			n++
		}
	}

	return n
}

// A late report is listed in the task's file and changes nothing else of it, is one event, and is told to the dispatcher in
// one note that asks for nothing; the result notice does not announce its revision as the task's.
func TestLateReportIsKeptLoggedAndToldOnce(t *testing.T) {
	f := lateFixture(t)
	before, err := f.tk.Get("T-1")
	require.NoError(t, err)

	rev := lateResult(t, f, "from the old session")
	late := task.Late{What: "result", Revision: rev, By: "designer", Session: "sid-old", Gen: 3}

	fresh, err := f.s.RecordLate(before, late)
	require.NoError(t, err)
	assert.True(t, fresh)

	got, err := f.tk.Get("T-1")
	require.NoError(t, err)
	require.Len(t, got.Late, 1)
	assert.False(t, got.Late[0].At.IsZero())

	want := before
	want.Late = got.Late
	assert.Equal(t, want, got, "state, result, delivery and Updated are as they were")

	assert.Equal(t, 1, lateEvents(f))
	assert.Equal(t, 1, lateNotes(f), "%v", f.s.State.Signals)
	assert.Contains(t, f.s.State.Signals[0], "T-1 was handed out again (hand-out 3, to designer)")
	assert.Contains(t, f.s.State.Signals[0], "its result "+rev[:12]+" was kept as a revision, not the task's result")

	// The result notice would announce every revision in the store: this one is marked.
	f.s.NotifyRecords()
	assert.Zero(t, countSignals(f, "T-1"+resultReady), "%v", f.s.State.Signals)

	// The same report again: neither logged nor told.
	again, err := f.s.RecordLate(got, late)
	require.NoError(t, err)
	assert.False(t, again)
	assert.Equal(t, 1, lateEvents(f))
	assert.Equal(t, 1, lateNotes(f))

	// Another revision from the same session on the same hand-out: one more event, no more notes.
	got, err = f.tk.Get("T-1")
	require.NoError(t, err)

	late.Revision = lateResult(t, f, "a second late report")
	_, err = f.s.RecordLate(got, late)
	require.NoError(t, err)
	assert.Equal(t, 2, lateEvents(f))
	assert.Equal(t, 1, lateNotes(f), "told once for a caller on a hand-out")

	// Another session, or the next hand-out, is news again.
	got, err = f.tk.Get("T-1")
	require.NoError(t, err)

	other := task.Late{What: "done", By: "designer", Session: "sid-older", Gen: 3}
	_, err = f.s.RecordLate(got, other)
	require.NoError(t, err)
	assert.Equal(t, 2, lateNotes(f))
	assert.Contains(t, f.s.State.Signals[1], "its task done was refused")

	got, err = f.tk.Get("T-1")
	require.NoError(t, err)

	other.Gen = 4
	_, err = f.s.RecordLate(got, other)
	require.NoError(t, err)
	assert.Equal(t, 3, lateNotes(f))

	// A result of the current hand-out that was saved in the store is announced as ever.
	lateResult(t, f, "the task's own")
	f.s.NotifyRecords()
	assert.Equal(t, 1, countSignals(f, "T-1"+resultReady), "%v", f.s.State.Signals)
}

func countSignals(f fix, sub string) int {
	n := 0

	for _, sig := range f.s.State.Signals {
		if strings.Contains(sig, sub) {
			n++
		}
	}

	return n
}

// A task keeps its newest late reports only.
func TestLateReportsAreBounded(t *testing.T) {
	f := lateFixture(t)

	for i := range task.LateMax + 5 {
		tk, err := f.tk.Get("T-1")
		require.NoError(t, err)

		_, err = f.s.RecordLate(tk, task.Late{What: "set", By: "designer", Session: fmt.Sprintf("sid-%d", i), Gen: 3})
		require.NoError(t, err)
	}

	got, err := f.tk.Get("T-1")
	require.NoError(t, err)
	require.Len(t, got.Late, task.LateMax)
	assert.Equal(t, fmt.Sprintf("sid-%d", task.LateMax+4), got.Late[task.LateMax-1].Session)
	assert.Equal(t, "sid-5", got.Late[0].Session)
}
