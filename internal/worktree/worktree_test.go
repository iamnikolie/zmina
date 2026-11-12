package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestWorktreeLifecycle(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a"), 0o600))
	run(t, repo, "add", ".")
	run(t, repo, "commit", "-q", "-m", "a")

	path := filepath.Join(t.TempDir(), "wt", "repo-T-1")

	tree, err := Add(filepath.Join(repo), path, "zmina/t-1", "")
	require.NoError(t, err)
	assert.Equal(t, "main", tree.Base)

	merged, err := tree.Merged()
	require.NoError(t, err)
	assert.True(t, merged, "a fresh branch has nothing of its own")

	require.NoError(t, os.WriteFile(filepath.Join(path, "b.txt"), []byte("b"), 0o600))

	dirty, err := tree.Dirty()
	require.NoError(t, err)
	assert.Contains(t, dirty, "b.txt")

	run(t, path, "add", ".")
	run(t, path, "commit", "-q", "-m", "b")

	merged, err = tree.Merged()
	require.NoError(t, err)
	assert.False(t, merged)
	assert.Error(t, tree.Remove(false), "an unmerged branch is not deleted without force (the clean worktree goes)")

	run(t, repo, "merge", "-q", "--ff-only", "zmina/t-1")

	merged, err = tree.Merged()
	require.NoError(t, err)
	assert.True(t, merged)

	require.NoError(t, tree.Remove(false))
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))

	_, err = Add(repo, path, "zmina/t-1", "")
	assert.NoError(t, err, "the branch name is free again")
}

func TestNotARepo(t *testing.T) {
	_, err := TopLevel(t.TempDir())
	assert.Error(t, err)
}

// A tree's reading calls end with its context: a cancelled one runs no git and says it was stopped, not that a
// branch is unmerged or a worktree clean; Remove does not take it.
func TestTreeCtxEndsTheReadingCalls(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a"), 0o600))
	run(t, repo, "add", ".")
	run(t, repo, "commit", "-q", "-m", "a")

	path := filepath.Join(t.TempDir(), "wt", "repo-T-1")

	tree, err := Add(repo, path, "zmina/t-1", "")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	tree.Ctx = ctx

	merged, err := tree.Merged()
	require.NoError(t, err)
	assert.True(t, merged)

	cancel()

	_, err = tree.Merged()
	require.ErrorIs(t, err, context.Canceled)

	_, err = tree.Dirty()
	require.ErrorIs(t, err, context.Canceled)
	assert.ErrorContains(t, err, "stopped")

	require.NoError(t, tree.Remove(false), "a removal is never cut by the sweep's deadline")
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}

func TestHeadAndAhead(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a"), 0o600))
	run(t, repo, "add", ".")
	run(t, repo, "commit", "-q", "-m", "a")

	tree, err := Add(repo, filepath.Join(t.TempDir(), "wt", "repo-T-1"), "zmina/t-1", "")
	require.NoError(t, err)

	start, err := tree.Head()
	require.NoError(t, err)
	assert.Len(t, start, 40)

	n, err := tree.Ahead()
	require.NoError(t, err)
	assert.Zero(t, n, "a fresh branch has nothing beyond its base")

	require.NoError(t, os.WriteFile(filepath.Join(tree.Path, "b.txt"), []byte("b"), 0o600))
	run(t, tree.Path, "add", ".")
	run(t, tree.Path, "commit", "-q", "-m", "b")

	head, err := tree.Head()
	require.NoError(t, err)
	assert.NotEqual(t, start, head)

	n, err = tree.Ahead()
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// The work reached its base: nothing is beyond it any more.
	run(t, repo, "merge", "-q", "--ff-only", "zmina/t-1")

	n, err = tree.Ahead()
	require.NoError(t, err)
	assert.Zero(t, n)

	tree.Base = "no-such-branch"
	_, err = tree.Ahead()
	assert.Error(t, err)

	tree.Path = filepath.Join(t.TempDir(), "gone")
	_, err = tree.Head()
	assert.Error(t, err)
}
