package inbox

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "inbox")}

	id, err := NewID()
	require.NoError(t, err)
	assert.True(t, ValidID(id))

	for _, bad := range []string{"", "in-", "in-xyz", "../state", "in-0123456789abc", "IN-0123456789ab"} {
		assert.False(t, ValidID(bad), bad)
		assert.Error(t, s.Put(Entry{ID: bad, Body: "x"}))
	}

	require.NoError(t, s.Put(Entry{ID: id, From: "remote box/web", Remote: true, Body: "line one\nline two"}))

	e, err := s.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "line one\nline two", e.Body)
	assert.True(t, e.Remote)
	assert.False(t, e.At.IsZero())

	_, err = s.Get("in-ffffffffffff")
	require.ErrorIs(t, err, ErrNotFound)

	// An entry past Keep goes at the next write.
	old := filepath.Join(s.Dir, id+".json")
	require.NoError(t, os.Chtimes(old, time.Now().Add(-Keep-time.Hour), time.Now().Add(-Keep-time.Hour)))

	id2, _ := NewID()
	require.NoError(t, s.Put(Entry{ID: id2, Body: "new"}))

	all, err := s.List()
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, id2, all[0].ID)
}

// An id made of a key is the same each time and is a valid one: a text stored again under it replaces the first copy.
func TestIDOf(t *testing.T) {
	a := IDOf("queue dev\x00one")
	assert.True(t, ValidID(a), a)
	assert.Equal(t, a, IDOf("queue dev\x00one"))
	assert.NotEqual(t, a, IDOf("queue dev\x00two"))
	assert.NotEqual(t, a, IDOf("queue ops\x00one"))

	s := &Store{Dir: filepath.Join(t.TempDir(), "inbox")}
	require.NoError(t, s.Put(Entry{ID: a, Body: "one"}))
	require.NoError(t, s.Put(Entry{ID: a, Body: "one"}))

	all, err := s.List()
	require.NoError(t, err)
	assert.Len(t, all, 1)
}
