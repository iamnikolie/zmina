package cmd

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/shift"
)

// A state written under a higher schema number than this zmina's is the one thing an older build must not write over:
// it would drop the fields it does not know (see TestStateFieldsGolden). Every way into the state that writes refuses
// it, says why, and leaves the file, its .prev and the directory as they were; the ones that only look still look.
func TestAStateFromANewerZminaIsNotWrittenByAnyCommand(t *testing.T) {
	crewFile := testCrew(t, "")
	dir := filepath.Dir(crewFile)
	state := filepath.Join(dir, "state.json")
	newer := `{"version": 99, "roles": {"dispatcher": {"inbox": ["a line"], "a_field_of_a_later_zmina": 1}}}`

	for name, run := range map[string]func() error{
		"a command": func() error {
			_, _, err := zm(t, crewFile, "send", "--when-idle", "dispatcher", "hi")

			return err
		},
		"the beat": func() error {
			_, _, err := zm(t, crewFile, "tick")

			return err
		},
		"the delivery step": func() error {
			_, _, err := zm(t, crewFile, "tick", "--deliver")

			return err
		},
		"the courier's pass": func() error {
			a := &app{crew: crewFile, out: io.Discard, err: io.Discard}

			s, err := a.newShift()
			require.NoError(t, err)

			_, err = a.courierDeliver(s, func(string) bool { return true })

			return err
		},
		// A look is refused too: an empty state shown in its place would pass for the crew's own.
		"a look": func() error {
			_, _, err := zm(t, crewFile, "ls")

			return err
		},
		"what waits for an ack": func() error {
			_, _, err := zm(t, crewFile, "pending")

			return err
		},
		"the state shown": func() error {
			_, _, err := zm(t, crewFile, "state", "show")

			return err
		},
		"state recover": func() error {
			_, _, err := zm(t, crewFile, "state", "recover")

			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(state, []byte(newer), 0o600))

			before := stateFiles(t, dir)

			err := run()
			require.ErrorIs(t, err, shift.ErrStateFuture)
			assert.Contains(t, err.Error(), "upgrade zmina")

			raw, rerr := os.ReadFile(state)
			require.NoError(t, rerr)
			assert.Equal(t, newer, string(raw), "the state is as the newer zmina left it")
			assert.NoFileExists(t, state+".prev", "no save began")
			assert.Equal(t, before, stateFiles(t, dir), "no state file was made or moved beside it")
		})
	}
}

// stateFiles is the names in dir that are state.json or one of its companions (.prev, .v1.bak, .broken-<time>), to see
// that a refused command saved nothing and moved nothing.
func stateFiles(t *testing.T, dir string) []string {
	t.Helper()

	names, err := filepath.Glob(filepath.Join(dir, "state.json*"))
	require.NoError(t, err)

	return names
}
