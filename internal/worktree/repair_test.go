package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// After the repository moves, a worktree that stayed where it was cannot find it; Repair makes it find the new place
// and deletes nothing.
func TestRepairAfterTheRepositoryMoved(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "old", "repo")
	require.NoError(t, os.MkdirAll(repo, 0o700))
	run(t, repo, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a"), 0o600))
	run(t, repo, "add", ".")
	run(t, repo, "commit", "-q", "-m", "a")

	path := filepath.Join(parent, "wt", "repo-T-1")
	_, err := Add(repo, path, "zmina/t-1", "")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(path, "left"), []byte("x"), 0o600))

	moved := filepath.Join(parent, "new", "repo")
	require.NoError(t, os.MkdirAll(filepath.Dir(moved), 0o700))
	require.NoError(t, os.Rename(repo, moved))

	_, err = gitCtx(context.Background(), path, "status", "--short")
	require.Error(t, err, "the worktree's link to its repository is broken")

	said, err := Repair(context.Background(), moved)
	require.NoError(t, err)
	assert.Contains(t, said, path, "git says what it repaired")

	out, err := gitCtx(context.Background(), path, "status", "--short")
	require.NoError(t, err)
	assert.Contains(t, out, "left", "the worktree reads again, its changes where they were")

	said, err = Repair(context.Background(), moved)
	require.NoError(t, err)
	assert.Empty(t, said, "nothing left to repair, nothing said")
}

// A context that is done ends the call.
func TestRepairEndsWithItsContext(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "init", "-q", "-b", "main")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Repair(ctx, repo)
	require.ErrorIs(t, err, context.Canceled)
}
