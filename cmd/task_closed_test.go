package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A worker that closes its task with a note and no result tells the dispatcher, with the note: at once, and again
// after the queue was lost (reconciled from the ledger).
func TestTaskDoneNoteNotifiesDispatcher(t *testing.T) {
	file := testCrew(t, "  support: {}\n")
	_, _, err := zm(t, file, "task", "new", "support", "Review fixture")
	require.NoError(t, err)

	t.Setenv("ZMINA_ROLE", "support")
	_, _, err = zm(t, file, "task", "done", "1", "--note", "review completed")
	require.NoError(t, err)

	out, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "T-1 done: review completed")

	a := &app{crew: file}
	require.NoError(t, a.withState(func(s *shift.Shift) error {
		s.NotifyRecords()
		s.NotifyClosed()

		return nil
	}))

	after, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(after, "review completed"), after)
}

// A worker that saved a result and then closes the task is told about once: by the result.
func TestTaskDoneAfterResultIsNotToldTwice(t *testing.T) {
	file := testCrew(t, "  support: {}\n")
	_, _, err := zm(t, file, "task", "new", "support", "Review fixture")
	require.NoError(t, err)

	t.Setenv("ZMINA_ROLE", "support")

	body := filepath.Join(t.TempDir(), "result.json")
	require.NoError(t, os.WriteFile(body, []byte(`{"summary":"Reviewed.","done":"all","remaining":"none",`+
		`"open_questions":"none","artifacts":"none","next_step":"none"}`), 0o600))
	_, _, err = zm(t, file, "task", "result", "1", "--file", body)
	require.NoError(t, err)
	_, _, err = zm(t, file, "task", "done", "1", "--note", "see the result")
	require.NoError(t, err)

	out, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "result ready")
	assert.NotContains(t, out, "see the result")
}

// The owner closing a task by hand tells no one; reopening it forgets the closure.
func TestTaskDoneByHandIsQuiet(t *testing.T) {
	file := testCrew(t, "  support: {}\n")
	_, _, err := zm(t, file, "task", "new", "support", "Review fixture")
	require.NoError(t, err)

	_, _, err = zm(t, file, "task", "done", "1", "--note", "closed by hand")
	require.NoError(t, err)

	out, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.NotContains(t, out, "closed by hand")

	_, _, err = zm(t, file, "task", "set", "1", "open")
	require.NoError(t, err)

	c, err := (&app{crew: file}).loadCrew()
	require.NoError(t, err)
	t1, err := taskStore(c).Get("T-1")
	require.NoError(t, err)
	assert.Nil(t, t1.Closed)
}
