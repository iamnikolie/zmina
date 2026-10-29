package task

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
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	a, err := s.Add(Task{Role: "dev", Asked: "fix the login"}, now)
	require.NoError(t, err)
	assert.Equal(t, "T-1", a.ID)
	assert.Equal(t, Open, a.State)

	for range 9 {
		_, err = s.Add(Task{Role: "designer", Asked: "x"}, now)
		require.NoError(t, err)
	}

	all, err := s.All()
	require.NoError(t, err)
	require.Len(t, all, 10)
	assert.Equal(t, "T-10", all[9].ID, "ordered by number, not by name")

	got, err := s.Get("t-1")
	require.NoError(t, err)
	assert.Equal(t, "fix the login", got.Asked)

	got.State = Done
	require.NoError(t, s.Put(got))
	assert.Len(t, s.Active(), 9)

	_, err = s.Get("99")
	assert.Error(t, err)

	_, err = ParseState("finished")
	assert.Error(t, err)
}

func TestLegacyWaitingIsWaiting(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	a, err := s.Add(Task{Role: "dev", Asked: "x"}, now)
	require.NoError(t, err)

	a.State = "waiting-mako"
	require.NoError(t, s.Put(a))

	got, err := s.Get(a.ID)
	require.NoError(t, err)
	assert.Equal(t, Waiting, got.State, "a file written under the old name reads as waiting")
	assert.Len(t, s.Active(), 1)

	st, err := ParseState("waiting-mako")
	require.NoError(t, err)
	assert.Equal(t, Waiting, st)
}

func TestDamagedFileIsNeverWrittenOver(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	for range 2 {
		_, err := s.Add(Task{Role: "dev", Asked: "x"}, now)
		require.NoError(t, err)
	}

	broken := filepath.Join(s.Dir, "T-3.json")
	require.NoError(t, os.WriteFile(broken, []byte(`{"id": "T-3", "state": `), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "T-4.json"), []byte(`{"id": "T-9", "state": "open"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "T-5.json"), []byte(`{"id": "T-5", "state": "paused"}`), 0o600))

	next, err := s.Add(Task{Role: "dev", Asked: "y"}, now)
	require.NoError(t, err)
	assert.Equal(t, "T-6", next.ID, "numbered above every file, readable or not")

	raw, _ := os.ReadFile(broken)
	assert.Equal(t, `{"id": "T-3", "state": `, string(raw), "untouched")

	tasks, problems, err := s.Scan()
	require.NoError(t, err)
	require.Len(t, problems, 3)
	assert.Contains(t, problems[0], "T-3")
	assert.Contains(t, problems[1], `the file says it is "T-9"`)
	assert.Contains(t, problems[2], `unknown state "paused"`)

	var ids []string
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}

	assert.Equal(t, []string{"T-1", "T-2", "T-5", "T-6"}, ids)
	assert.Len(t, s.Active(), 4, "an unknown state holds its role")

	require.ErrorIs(t, s.write(Task{ID: "T-6"}, true), ErrExists)
}
