package cmd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/shift"
)

// A command that fails leaves the state as it was; one that fails after part of its work (shift.Partial) saves it.
func TestFailedCommandSavesNothing(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n")
	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	state := filepath.Join(filepath.Dir(crewFile), "state.json")

	require.NoError(t, a.withState(func(s *shift.Shift) error { s.State.Role("dev").Held = true; return nil }))
	before, err := os.ReadFile(state)
	require.NoError(t, err)

	err = a.withState(func(s *shift.Shift) error {
		s.State.Role("dev").Held = false
		s.State.Role("dev").Inbox = []string{"half done"}

		return errors.New("refused")
	})
	require.EqualError(t, err, "refused")

	after, err := os.ReadFile(state)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "nothing of a failed command is saved")

	err = a.withState(func(s *shift.Shift) error {
		s.State.Role("dev").Inbox = []string{"done part"}

		return shift.Partial(errors.New("the other part failed"))
	})
	require.EqualError(t, err, "the other part failed", "the message is the command's own")
	assert.ErrorIs(t, err, shift.ErrPartial)

	require.NoError(t, a.readState(func(s *shift.Shift) error {
		assert.Equal(t, []string{"done part"}, s.State.Role("dev").Inbox, "the done part is saved")
		assert.True(t, s.State.Role("dev").Held)

		return nil
	}))

	require.NoError(t, a.withState(func(*shift.Shift) error { return nil }), "the lock was let go after the failure")
}

// A watch whose source fails fails the command, and its failure is recorded all the same (shift.Partial).
func TestFailedWatchRunIsRecorded(t *testing.T) {
	crewFile := testCrew(t, `watch:
  broken:
    kind: exec
    every: 5m
    run: "exit 3"
`)

	_, _, err := zm(t, crewFile, "watch", "run", "broken")
	require.Error(t, err)

	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	require.NoError(t, a.readState(func(s *shift.Shift) error {
		w := s.State.Watch["broken"]
		require.NotNil(t, w)
		assert.Equal(t, 1, w.Failures)
		assert.NotEmpty(t, w.LastError)

		return nil
	}))
}
