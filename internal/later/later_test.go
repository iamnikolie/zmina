package later

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

	a, err := s.Add(Item{Text: "rotate the deploy key", Note: "all repos", State: Done}, now)
	require.NoError(t, err)
	assert.Equal(t, "L-1", a.ID)
	assert.Equal(t, Parked, a.State, "a new item is always parked")

	for range 10 {
		_, err = s.Add(Item{Text: "x"}, now)
		require.NoError(t, err)
	}

	got, err := s.Get("l-1")
	require.NoError(t, err)
	assert.Equal(t, "all repos", got.Note)

	parked := s.Parked()
	require.Len(t, parked, 11)
	assert.Equal(t, "L-11", parked[10].ID, "ordered by number, not by name")

	closed, err := s.Close("2", Dropped, "", now.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, Dropped, closed.State)
	assert.Equal(t, now.Add(time.Hour), closed.Updated)
	assert.Len(t, s.Parked(), 10)

	_, err = s.Close("L-2", Done, "", now)
	assert.ErrorContains(t, err, "already dropped")

	_, err = s.Get("99")
	assert.ErrorContains(t, err, "no item L-99")
}

func TestDamagedFileIsNeverWrittenOver(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

	_, err := s.Add(Item{Text: "x"}, now)
	require.NoError(t, err)

	broken := filepath.Join(s.Dir, "L-2.json")
	require.NoError(t, os.WriteFile(broken, []byte(`{"id": "L-2", `), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "L-3.json"), []byte(`{"id": "L-3", "state": "later"}`),
		0o600))

	next, err := s.Add(Item{Text: "y"}, now)
	require.NoError(t, err)
	assert.Equal(t, "L-4", next.ID, "numbered above every file, readable or not")

	raw, _ := os.ReadFile(broken)
	assert.Equal(t, `{"id": "L-2", `, string(raw), "untouched")

	items, problems, err := s.Scan()
	require.NoError(t, err)
	require.Len(t, problems, 1)
	assert.Contains(t, problems[0], "L-2")
	assert.Len(t, items, 3)
	assert.Len(t, s.Parked(), 3, "an unknown state stays on the list")
}
