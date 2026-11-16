package crew

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// While a move is open on a node (its journal, or the owner's record of it) a push and a seat edit are refused there, and the message
// names the move and the way out; a take's marker is no such thing, and the move's own edit goes through.
func TestCrewFileIsNotChangedWhileAMoveIsOpen(t *testing.T) {
	old := "name: acme\nnamespace: /ns\nhome: alpha\nnodes:\n  alpha: {}\n  beta: {}\nroles:\n  dispatcher: {}\n  dev: {}\n"
	path := putDir(t, old)
	moves := filepath.Join(filepath.Dir(path), "moves")
	require.NoError(t, os.MkdirAll(moves, 0o700))

	assert.Empty(t, Layout{Dir: filepath.Dir(path)}.OpenMoves())

	// A take's marker blocks nothing: a file may have to be sent to the node that holds one, to mend the take.
	require.NoError(t, os.WriteFile(filepath.Join(moves, "dev.taking.json"), []byte(`{"id":"mv-0123456789ab"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(moves, "dev.lock"), nil, 0o600))
	assert.Empty(t, Layout{Dir: filepath.Dir(path)}.OpenMoves())

	_, err := Put(path, []byte(pushedFile), opts("beta", SumOf([]byte(old))))
	require.NoError(t, err)

	// A journal.
	require.NoError(t, os.WriteFile(filepath.Join(moves, "dev.json"), []byte(`{"id":"mv-0123456789ab","role":"dev","to":"beta"}`), 0o600))

	for name, try := range map[string]func() error{
		"a push": func() error {
			_, err := Put(path, []byte(old), opts("beta", SumOf([]byte(pushedFile))))

			return err
		},
		"a seat edit": func() error {
			e, err := Open(path)
			require.NoError(t, err)
			require.NoError(t, e.Set("dev", map[string]string{"about": "x"}))

			return e.Save()
		},
		"a restore": func() error {
			_, err := Restore(path, []byte(old), "me", "crew history")

			return err
		},
	} {
		err := try()
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "a move of dev to beta is open on this node (mv-0123456789ab)", name)
		assert.Contains(t, err.Error(), "zmina move dev beta --resume mv-0123456789ab", name)
		assert.Contains(t, err.Error(), "--abort mv-0123456789ab", name)
	}

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, pushedFile, string(got), "nothing was written")

	// The move's own edit is not refused.
	_, _, wrote, err := MovePlace(path, "dev", "alpha", SumOf([]byte(pushedFile)), "move", "zmina move")
	require.NoError(t, err)
	assert.True(t, wrote)

	// The owner's record: the same.
	require.NoError(t, os.Remove(filepath.Join(moves, "dev.json")))
	require.NoError(t, os.WriteFile(filepath.Join(moves, "dev.drive.json"), []byte(`{"id":"mv-0123456789ab","role":"dev","to":"beta"}`), 0o600))

	_, err = Put(path, []byte(pushedFile), opts("beta", ""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "this node drives the move mv-0123456789ab of dev to beta")

	// A record that does not read still counts: someone has to look.
	require.NoError(t, os.WriteFile(filepath.Join(moves, "dev.drive.json"), []byte(`{`), 0o600))

	_, err = Put(path, []byte(pushedFile), opts("beta", ""))
	require.ErrorContains(t, err, "does not read")

	// Closed: the file may change again.
	require.NoError(t, os.Remove(filepath.Join(moves, "dev.drive.json")))

	cur, err := os.ReadFile(path)
	require.NoError(t, err)

	_, err = Put(path, []byte(pushedFile), opts("beta", SumOf(cur)))
	require.NoError(t, err)
}
