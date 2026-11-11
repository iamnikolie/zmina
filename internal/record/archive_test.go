package record

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

func archiveDoc(task, summary string) Document {
	return Document{Version: 1, Kind: "result", Task: task, Role: "dev", At: time.Now(),
		Shape:  crew.HandoffShape{Sections: []crew.Section{{Name: "summary"}}},
		Values: map[string]string{"summary": summary}}
}

// archive moves a saved record the way the retention pass does, into the month dir of the archive, and leaves the note.
func archive(t *testing.T, dir, month, kind, task string) string {
	t.Helper()

	live := filepath.Join(dir, kind+"s", task)
	dst := filepath.Join(dir, ArchiveRoot, month, kind+"s", task)
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o700))
	require.NoError(t, os.Rename(live, dst))
	require.NoError(t, os.MkdirAll(live, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(live, StubFile), []byte("Moved by zmina retention to "+filepath.Join(ArchiveRoot, month, kind+"s", task)+"\n"), 0o600))

	return dst
}

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

// A record the retention pass moved to the archive is still read, by its whole hash and by a start of it, listed
// among the task's revisions once, and loaded from the archive without writing anywhere.
func TestArchivedRecordIsReadFromTheArchive(t *testing.T) {
	dir := t.TempDir()
	d := archiveDoc("T-1", "shipped")
	ref, err := Save(dir, d)
	require.NoError(t, err)

	archive(t, dir, "2026-01", "result", "T-1")

	before := listing(t, dir)

	got, err := Load(dir, "result", "T-1", ref.Revision)
	require.NoError(t, err)
	assert.Equal(t, "shipped", got.Values["summary"])

	task, rev, err := Find(dir, "result", []string{"T-2", "T-1"}, ref.Revision[:8])
	require.NoError(t, err)
	assert.Equal(t, "T-1", task, "the records carried from T-1 are found there")
	assert.Equal(t, ref.Revision, rev)
	assert.Equal(t, []string{ref.Revision}, Revisions(dir, "result", []string{"T-1"}))

	// A record saved again after it was archived is the same revision: nothing is written, in the live directory or
	// in the archive.
	again, err := Save(dir, d)
	require.NoError(t, err)
	assert.Equal(t, ref.Revision, again.Revision)
	assert.Equal(t, before, listing(t, dir), "reading and saving the same record write nothing")

	// The sweep for notices lists what is kept in place only: an archived result is not announced again.
	all, err := All(dir, "result")
	require.NoError(t, err)
	assert.Empty(t, all)
}

// The newest month wins when a task was archived twice, a copy in place wins over both, and one revision in two
// places is one revision.
func TestArchivedRecordsOfTwoMonthsAndALiveCopy(t *testing.T) {
	dir := t.TempDir()

	first, err := Save(dir, archiveDoc("T-1", "first"))
	require.NoError(t, err)
	archive(t, dir, "2026-01", "result", "T-1")

	second, err := Save(dir, archiveDoc("T-1", "second"))
	require.NoError(t, err)
	archive(t, dir, "2026-03", "result", "T-1")

	assert.ElementsMatch(t, []string{first.Revision, second.Revision}, Revisions(dir, "result", []string{"T-1"}))

	for rev, want := range map[string]string{first.Revision: "first", second.Revision: "second"} {
		d, err := Load(dir, "result", "T-1", rev)
		require.NoError(t, err)
		assert.Equal(t, want, d.Values["summary"])
	}

	// The same revision archived in an older month and again in a newer one: the newer is read, listed once.
	raw, err := os.ReadFile(filepath.Join(dir, ArchiveRoot, "2026-01", "results", "T-1", first.Revision+".json"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ArchiveRoot, "2026-05", "results", "T-1"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ArchiveRoot, "2026-05", "results", "T-1", first.Revision+".json"), raw, 0o600))
	assert.Len(t, Revisions(dir, "result", []string{"T-1"}), 2)

	// Moved back by hand: in place again, and still one revision.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "results", "T-1", first.Revision+".json"), raw, 0o600))
	assert.Len(t, Revisions(dir, "result", []string{"T-1"}), 2)

	_, _, err = Find(dir, "result", []string{"T-1"}, first.Revision[:8])
	require.NoError(t, err, "one record in two places is not an ambiguous start")
}

// A note whose files are not in the archive is an error that names where the note says they went, and is still a
// not-exist error: a save of a new record in that directory goes on.
func TestArchivedNoteWithoutItsFilesIsAPlainError(t *testing.T) {
	dir := t.TempDir()
	ref, err := Save(dir, archiveDoc("T-1", "gone"))
	require.NoError(t, err)

	moved := archive(t, dir, "2026-01", "result", "T-1")
	require.NoError(t, os.RemoveAll(filepath.Join(dir, ArchiveRoot)))

	_, err = Load(dir, "result", "T-1", ref.Revision)
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.Contains(t, err.Error(), "not in the archive")
	assert.Contains(t, err.Error(), filepath.Join(ArchiveRoot, "2026-01", "results", "T-1"), moved)

	// No note, no archive: the plain not-exist as before.
	_, err = Load(dir, "result", "T-2", ref.Revision)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.NotContains(t, err.Error(), "archived")

	// A new record saved in the emptied directory is written there.
	next, err := Save(dir, archiveDoc("T-1", "new"))
	require.NoError(t, err)

	got, err := Load(dir, "result", "T-1", next.Revision)
	require.NoError(t, err)
	assert.Equal(t, "new", got.Values["summary"])
}

// Finding a revision by a start of it in a directory whose files are gone from the archive says where they went.
func TestFindNamesTheNoteOfAnEmptiedDirectory(t *testing.T) {
	dir := t.TempDir()
	ref, err := Save(dir, archiveDoc("T-1", "gone"))
	require.NoError(t, err)

	archive(t, dir, "2026-01", "result", "T-1")
	require.NoError(t, os.RemoveAll(filepath.Join(dir, ArchiveRoot)))

	_, _, err = Find(dir, "result", []string{"T-1"}, ref.Revision[:8])
	require.ErrorContains(t, err, "no result revision")
	require.ErrorContains(t, err, "not in the archive")
	require.ErrorContains(t, err, filepath.Join(ArchiveRoot, "2026-01", "results", "T-1"))
}
