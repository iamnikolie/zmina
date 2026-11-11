package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/task"
)

// What the retention pass moved to the archive is still read: task result, task checkpoint and task show of a task
// closed long ago, a start of the revision, the checkpoint a parked item carries into a new task, and a task that
// goes on after it (--after). Reading writes nothing in the archive; a note whose files are gone says so by name.
func TestArchivedResultAndCheckpointAreStillRead(t *testing.T) {
	file := routeCrew(t, "")
	routeLimits(t, file, 41, 50)

	c, err := crew.Load(file)
	require.NoError(t, err)

	_, _, err = zm(t, file, "task", "new", "dev", "the first piece")
	require.NoError(t, err)

	old := time.Now().UTC().Add(-90 * 24 * time.Hour)
	store := taskStore(c)

	tk, err := store.Get("T-1")
	require.NoError(t, err)

	save := func(kind, summary string) *record.Ref {
		ref, err := record.Save(c.StateDir(), record.Document{Version: 1, Kind: kind, Task: "T-1", Role: "dev", At: old,
			Shape:  crew.HandoffShape{Sections: []crew.Section{{Name: "summary"}}},
			Values: map[string]string{"summary": summary}})
		require.NoError(t, err)

		return &ref
	}

	tk.Result, tk.Checkpoint = save("result", "login form fixed"), save("checkpoint", "left off at the tests")
	tk.State, tk.Updated, tk.Closed = task.Done, old, &task.Closure{By: "dev", At: old, State: task.Done}
	require.NoError(t, store.Put(tk))

	read := func(args ...string) string {
		t.Helper()

		out, _, err := zm(t, file, args...)
		require.NoError(t, err, strings.Join(args, " "))

		return out
	}

	wantResult, wantCheckpoint, wantShow := read("task", "result", "T-1"), read("task", "checkpoint", "T-1"), read("task", "show", "T-1")
	assert.Contains(t, wantResult, "login form fixed")
	assert.Contains(t, wantCheckpoint, "left off at the tests")

	// The pass moves both.
	out := read("retention")
	assert.Contains(t, out, "results 1")
	assert.Contains(t, out, "checkpoints 1")
	assert.NoFileExists(t, filepath.Join(c.StateDir(), "results", "T-1", tk.Result.Revision+".json"))
	assert.NoFileExists(t, filepath.Join(c.StateDir(), "checkpoints", "T-1", tk.Checkpoint.Revision+".json"))

	archive := filepath.Join(c.StateDir(), record.ArchiveRoot)
	before := listing(t, archive)

	// The same answers as before the move.
	assert.Equal(t, wantResult, read("task", "result", "T-1"))
	assert.Equal(t, wantCheckpoint, read("task", "checkpoint", "T-1"))
	assert.Equal(t, wantShow, read("task", "show", "T-1"))
	assert.Equal(t, wantResult, read("task", "result", "T-1", "--revision", tk.Result.Revision[:8]), "a start of the revision")
	assert.Contains(t, read("task", "result", "T-1", "--json"), "login form fixed")

	// A task that goes on after it: the routes name it, and a parked item that carried its checkpoint into a new task
	// reads that checkpoint from the archive, as the task it was carried from.
	_, errOut, err := zm(t, file, "task", "new", "--route", "--after", "T-1", "the second piece")
	require.NoError(t, err, errOut)

	_, err = laterStore(c).Add(later.Item{Text: "go on from the checkpoint", Checkpoint: tk.Checkpoint}, time.Now())
	require.NoError(t, err)
	read("later", "take", "L-1", "dev")

	carried := read("task", "checkpoint", "T-3")
	assert.Contains(t, carried, "left off at the tests")
	assert.Contains(t, carried, "source task: T-1")

	assert.Equal(t, before, listing(t, archive), "reading writes nothing in the archive and moves nothing back")

	// The note stays, and a note whose files are gone is an error that names where they went.
	require.NoError(t, os.RemoveAll(archive))

	_, _, err = zm(t, file, "task", "result", "T-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in the archive")
	assert.Contains(t, err.Error(), filepath.Join(record.ArchiveRoot, time.Now().UTC().Format("2006-01"), "results", "T-1"))
}

// listing is every file under dir with its time and mode: what a read must leave as it was.
func listing(t *testing.T, dir string) []string {
	t.Helper()

	var out []string

	require.NoError(t, filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			out = append(out, p+" "+info.ModTime().String()+" "+info.Mode().String())
		}

		return err
	}))

	return out
}
