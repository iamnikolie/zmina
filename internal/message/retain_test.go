package message

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// addAt stores a report whose revision reached stage at the given time, with its envelope queued.
func addAt(t *testing.T, s Store, key string, stage string, at time.Time) Message {
	t.Helper()

	r := report("body of "+key, 1)
	r.Key = key
	r.At = at
	r.ID = Identity(r.From, r.To, r.Key)
	r.Revision = Content(r)

	m, _, err := s.Add(r)
	require.NoError(t, err)

	v := m.Revisions[r.Revision]
	require.NoError(t, Advance(v, stage, at))
	v.Intents["report"] = Intent{Queued: true}
	require.NoError(t, s.Save(m))

	return m
}

// One unreadable, foreign or foreign-looking file does not hide the messages beside it: Scan returns what reads and
// names each file that does not, All leaves the bad ones out instead of failing.
func TestScanKeepsTheGoodMessagesAndNamesTheBadFiles(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	now := time.Now().UTC()
	a := addAt(t, s, "a", "queued_origin", now)
	b := addAt(t, s, "b", "queued_origin", now)
	c := addAt(t, s, "c", "queued_origin", now)

	broken := strings.Repeat("0", 64) + ".json"
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, broken), []byte("{not json"), 0o600))

	foreign := strings.Repeat("1", 64) + ".json"
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, foreign), []byte(`{"version":9,"id":"x"}`), 0o600))

	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "notes.json"), []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "unrelated.txt"), []byte("x"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(s.Dir, ArchiveDir), 0o700))

	all, bad, err := s.Scan()
	require.NoError(t, err)

	var ids []string
	for _, m := range all {
		ids = append(ids, m.ID)
	}

	assert.ElementsMatch(t, []string{a.ID, b.ID, c.ID}, ids)

	var files []string
	for _, p := range bad {
		files = append(files, p.File)
		assert.Equal(t, filepath.Join(s.Dir, p.File), p.Path)
		require.Error(t, p.Err)
		assert.Contains(t, p.Error(), p.File)
	}

	assert.ElementsMatch(t, []string{broken, foreign, "notes.json"}, files)

	got, err := s.All()
	require.NoError(t, err, "All no longer fails on the first bad file")
	assert.Len(t, got, 3)

	_, bad, err = (Store{Dir: filepath.Join(t.TempDir(), "none")}).Scan()
	require.NoError(t, err, "no directory yet is no messages")
	assert.Empty(t, bad)
}

// A message that did not change is not written again: the file keeps its identity (a write is a new file renamed
// over it); one that did change is written.
func TestSaveWritesOnlyWhatChanged(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	m := addAt(t, s, "a", "received", time.Now().UTC())
	path := filepath.Join(s.Dir, m.ID+".json")

	before, err := os.Stat(path)
	require.NoError(t, err)

	loaded, err := s.Load(m.ID)
	require.NoError(t, err)
	require.NoError(t, s.Save(loaded))

	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after), "unchanged: the file was not rewritten")

	loaded.Revisions[loaded.Latest].Count++
	require.NoError(t, s.Save(loaded))

	changed, err := os.Stat(path)
	require.NoError(t, err)
	assert.False(t, os.SameFile(before, changed), "changed: written")

	again, err := s.Load(m.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, again.Revisions[again.Latest].Count)
}

// A finished message is one read at its destination (or superseded), fully queued and untouched since the cutoff;
// anything else stays. Archived, it leaves Scan and All and is still the message of its id for Load.
func TestFinishedMessagesAreArchivedAndStillFound(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-60 * 24 * time.Hour)
	cutoff := now.Add(-30 * 24 * time.Hour)

	done := addAt(t, s, "done", "read", old)
	relayed := addAt(t, s, "relayed", "relayed", old)
	recent := addAt(t, s, "recent", "read", now.Add(-24*time.Hour))
	unread := addAt(t, s, "unread", "received", old)

	unsent := addAt(t, s, "unsent", "read", old)
	unsent.Revisions[unsent.Latest].Intents["receipt/read"] = Intent{}
	require.NoError(t, s.Save(unsent))

	ids, err := s.Finished(cutoff)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{done.ID, relayed.ID}, ids, "old, read, queued: nothing else")
	assert.NotContains(t, ids, recent.ID)
	assert.NotContains(t, ids, unread.ID)
	assert.NotContains(t, ids, unsent.ID)

	for _, id := range ids {
		require.NoError(t, s.Archive(id))
	}

	all, bad, err := s.Scan()
	require.NoError(t, err)
	assert.Empty(t, bad)
	assert.Len(t, all, 3, "the archive is not read by Scan")

	_, err = os.Stat(filepath.Join(s.Dir, ArchiveDir, done.ID+".json"))
	require.NoError(t, err)

	m, err := s.Load(done.ID)
	require.NoError(t, err, "an archived id is still found")
	assert.Equal(t, "read", m.Revisions[m.Latest].Stage)

	_, err = s.Load(strings.Repeat("a", 64))
	assert.ErrorIs(t, err, os.ErrNotExist, "a message that is nowhere is still not-exist")

	// A late receipt on an archived message lands on that message, not on a second one.
	rec := Receipt{ID: m.ID, Revision: m.Latest, Sequence: 1, Origin: "alpha/ops", Recipient: "beta/ops", Stage: "relayed", At: now}
	require.NoError(t, s.ApplyReceipt("beta/ops", "alpha/ops", rec, now))

	back, err := s.Load(m.ID)
	require.NoError(t, err)
	assert.Equal(t, "relayed", back.Revisions[back.Latest].Stage)
	assert.Contains(t, back.Revisions[back.Latest].RemoteObserved, "relayed")

	assert.Error(t, s.Archive("not-an-id"))
}

// What was said about a failure is kept in a file Scan does not take for a message, written only when it changes.
func TestReportedIsRememberedAndWrittenOnlyOnChange(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	assert.Empty(t, s.Reported())

	require.NoError(t, s.SetReported(map[string]string{"a.json": "invalid message record"}))
	assert.Equal(t, map[string]string{"a.json": "invalid message record"}, s.Reported())

	path := filepath.Join(s.Dir, reportedFile)
	before, err := os.Stat(path)
	require.NoError(t, err)

	require.NoError(t, s.SetReported(map[string]string{"a.json": "invalid message record"}))

	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after), "the same set is not written again")

	all, bad, err := s.Scan()
	require.NoError(t, err)
	assert.Empty(t, all)
	assert.Empty(t, bad, "the memory is not a message file")

	require.NoError(t, s.SetReported(nil))
	_, err = os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist, "nothing said any more: nothing kept")
	require.NoError(t, s.SetReported(nil))
}
