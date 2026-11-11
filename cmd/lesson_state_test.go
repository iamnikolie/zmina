package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/lesson"
	"github.com/iamnikolie/zmina/internal/shift"
)

// lessonsCrew is a crew with lessons on, its agentic-os with an origin, and two lessons waiting; the seat answers
// LS-1 as an edit of CLAUDE.md and LS-2 as written already, and runs before (a hook of the test) each answer.
func lessonsCrew(t *testing.T, before func(c *crew.Crew)) (file, origin string, c *crew.Crew) {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}

	file = testCrew(t, "  dev:\n")

	var checkout string

	origin, checkout = lessonsRepo(t)
	withTop(t, file, "agents: "+filepath.Join(checkout, "agents")+"\nlessons: on\n")
	t.Setenv("ZMINA_ROLE", "dev")

	for _, rule := range []string{"Say which file a result is in", "Reply short"} {
		_, _, err := zm(t, file, "lesson", "add", rule, "--why", "the owner asked", "--applies", "every")
		require.NoError(t, err)
	}

	var err error

	c, err = crew.Load(file)
	require.NoError(t, err)

	was := lessonSeat

	t.Cleanup(func() { lessonSeat = was })

	lessonSeat = func(string) lesson.Seat {
		return func(_ context.Context, _, _, schema string, _ float64) ([]byte, error) {
			if before != nil {
				before(c)
			}

			var structured any = lesson.Plan{Items: []lesson.Item{
				{ID: "LS-1", Verdict: "every", File: "CLAUDE.md", New: "- Say which file a result is in.", Reason: "all"},
				{ID: "LS-2", Verdict: "written", Reason: "said in CLAUDE.md"},
			}}
			if schema == lesson.CheckSchema {
				structured = map[string]any{"checks": []lesson.Check{{ID: "LS-1", OK: true, Reason: "fine"}}}
			}

			raw, _ := json.Marshal(structured)

			return json.Marshal(map[string]any{"subtype": "success", "total_cost_usd": 0.1,
				"structured_output": json.RawMessage(raw)})
		}
	}

	return file, origin, c
}

// readOnly makes dir a directory nothing can be written into until the test ends.
func readOnly(t *testing.T, dir string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

func notesOf(t *testing.T, c *crew.Crew) []string {
	t.Helper()

	st, err := shift.Read(c.StateDir())
	require.NoError(t, err)

	return st.Notes
}

// A lesson that cannot be saved is not dropped silently: the run still records its MR, and says what failed, as an
// event, a note for the dispatcher and an error for the one who ran it.
func TestLessonTriageSaysWhenALessonCannotBeSaved(t *testing.T) {
	file, _, c := lessonsCrew(t, func(c *crew.Crew) { readOnly(t, lessonStore(c).Dir) })

	out, _, err := zm(t, file, "lesson", "triage")
	require.Error(t, err, "the run did not save what it meant to")
	assert.ErrorContains(t, err, "lesson LS-1")
	assert.Contains(t, out, "LS-1 -> CLAUDE.md", "the plan is still printed")

	st, err := lesson.LoadState(lessonDir(c))
	require.NoError(t, err)
	require.NotNil(t, st.Open, "the MR is pushed: it is recorded as open, or the next day's run opens another")
	assert.Contains(t, st.LastError, "lesson LS-1")
	assert.True(t, st.Running.IsZero())

	log, _, err := zm(t, file, "log")
	require.NoError(t, err)
	assert.Contains(t, log, "lesson-state-failed")

	notes := notesOf(t, c)
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "lessons: 2 records could not be saved")

	// Both stayed as they were: waiting, to be offered again.
	items, err := lessonStore(c).All()
	require.NoError(t, err)

	for _, l := range items {
		assert.Equal(t, lesson.New, l.State, l.ID)
	}

	todo, _, err := zm(t, file, "todo", "ls")
	require.NoError(t, err)
	assert.Contains(t, todo, "Lessons MR to review", "the owner's todo was written all the same")
}

// An MR that is pushed is recorded when the owner's todo for it cannot be written; the failure is told.
func TestLessonTriageRecordsThePushedMRWhenItsTodoFails(t *testing.T) {
	file, _, c := lessonsCrew(t, func(c *crew.Crew) { readOnly(t, c.Layout().Asks()) })

	_, _, err := zm(t, file, "lesson", "triage")
	require.Error(t, err)
	assert.ErrorContains(t, err, "the owner's todo for the MR")

	st, err := lesson.LoadState(lessonDir(c))
	require.NoError(t, err)
	require.NotNil(t, st.Open, "recorded without its todo")
	assert.Empty(t, st.Open.Todo)

	items, err := lessonStore(c).All()
	require.NoError(t, err)

	for _, l := range items {
		assert.NotEqual(t, lesson.New, l.State, l.ID+" was marked")
	}

	log, _, err := zm(t, file, "log")
	require.NoError(t, err)
	assert.Contains(t, log, "lesson-state-failed")
	assert.Len(t, notesOf(t, c), 1)
}

// The beat that settles a merged MR has nobody to return an error to: a lesson it could not save is an event, a note
// and a line of the beat's output.
func TestLessonsFollowSaysWhenAMergedLessonCannotBeSaved(t *testing.T) {
	file, origin, c := lessonsCrew(t, nil)

	_, _, err := zm(t, file, "lesson", "triage")
	require.NoError(t, err)

	st, err := lesson.LoadState(lessonDir(c))
	require.NoError(t, err)
	require.NotNil(t, st.Open)

	// The owner merges it on the forge and the source branch goes.
	branch := gitOut(t, origin, "for-each-ref", "--format=%(refname:short)", "refs/heads/lessons/")
	other := filepath.Join(t.TempDir(), "other")
	gitOut(t, filepath.Dir(other), "clone", "-q", origin, other)
	gitOut(t, other, "merge", "-q", "--no-ff", "-m", "Merge lessons", "origin/"+branch)
	gitOut(t, other, "push", "-q", "origin", "main", ":"+branch)

	_, _, err = zm(t, file, "todo", "done", st.Open.Todo)
	require.NoError(t, err)

	readOnly(t, lessonStore(c).Dir)

	var out bytes.Buffer

	a := &app{out: &out, err: os.Stderr, crew: file}
	a.lessonsBeat(time.Now().Add(lessonsRecheck))

	assert.Contains(t, out.String(), "lessons: ")
	assert.Contains(t, out.String(), "lesson LS-1")

	log, _, err := zm(t, file, "log")
	require.NoError(t, err)
	assert.Contains(t, log, "lesson-state-failed")
	assert.Contains(t, log, "lessons-merged", "the MR is settled all the same")

	notes := notesOf(t, c)
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "could not be saved")

	st, err = lesson.LoadState(lessonDir(c))
	require.NoError(t, err)
	assert.Nil(t, st.Open)
}
