package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// out is git's output in dir, trimmed.
func out(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	raw, err := cmd.CombinedOutput()
	require.NoError(t, err, string(raw))

	return strings.TrimSpace(string(raw))
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
}

// preserveFixture is a repository with a commit on main and a task's worktree and branch made from it, a .gitignore that
// ignores *.log, and a second worktree of another task, which must be left alone.
func preserveFixture(t *testing.T) (repo string, tree *Tree, other string) {
	t.Helper()

	repo = t.TempDir()
	run(t, repo, "init", "-q", "-b", "main")
	write(t, repo, "a.txt", "a\n")
	write(t, repo, ".gitignore", "*.log\n")
	run(t, repo, "add", ".")
	run(t, repo, "commit", "-q", "-m", "a")

	var err error

	tree, err = Add(repo, filepath.Join(t.TempDir(), "wt", "alpha"), "zmina/t-1", "")
	require.NoError(t, err)

	other = filepath.Join(t.TempDir(), "wt", "beta")
	_, err = Add(repo, other, "zmina/t-2", "")
	require.NoError(t, err)

	return repo, tree, other
}

// stack is the shared stash stack: Preserve must leave it as it found it.
func stack(t *testing.T, repo string) string { return out(t, repo, "stash", "list") }

// What a worktree with nothing in it that the base lacks keeps: nothing, and nothing in git changes.
func TestPreserveOfCleanMergedWorkIsNothing(t *testing.T) {
	repo, tree, _ := preserveFixture(t)
	refs := out(t, repo, "for-each-ref")

	got, err := tree.Preserve("T-1")
	require.NoError(t, err)
	assert.True(t, got.Empty())
	assert.Equal(t, refs, out(t, repo, "for-each-ref"), "no ref made")

	// Ignored files are no work to keep.
	write(t, tree.Path, "build.log", "noise")

	got, err = tree.Preserve("T-1")
	require.NoError(t, err)
	assert.True(t, got.Empty())
}

// The commits the base does not have are kept on a branch named for the task and the tip; kept twice it is the same branch.
func TestPreserveKeepsUnmergedCommitsOnABranch(t *testing.T) {
	repo, tree, _ := preserveFixture(t)

	for _, name := range []string{"b.txt", "c.txt"} {
		write(t, tree.Path, name, name)
		run(t, tree.Path, "add", ".")
		run(t, tree.Path, "commit", "-q", "-m", name)
	}

	tip := out(t, tree.Path, "rev-parse", "HEAD")

	got, err := tree.Preserve("T-1")
	require.NoError(t, err)
	require.Len(t, got.Branches, 1)
	assert.Equal(t, KeptBranch{Name: "zmina-preserve/t-1-" + tip[:8], Tip: tip, Commits: 2}, got.Branches[0])
	assert.Empty(t, got.Dirty, "a clean worktree has no changes to keep")
	assert.Equal(t, tip, out(t, repo, "rev-parse", "refs/heads/"+got.Branches[0].Name))

	again, err := tree.Preserve("T-1")
	require.NoError(t, err)
	assert.Equal(t, got, again, "the same work kept twice is one branch")

	// The branch outlives the removal of the worktree and the task's branch.
	require.NoError(t, tree.Remove(true))
	assert.Equal(t, tip, out(t, repo, "rev-parse", got.Branches[0].Name))
	assert.Equal(t, "b.txt\nc.txt", out(t, repo, "diff", "--name-only", "main", got.Branches[0].Name))
}

// Uncommitted changes, tracked (staged or not) and untracked and not ignored, are kept as a commit over HEAD under a ref of
// their own; the worktree, its index, the repository's index and the stash stack are as they were.
func TestPreserveSnapshotsUncommittedChangesWithoutTouchingAnything(t *testing.T) {
	repo, tree, other := preserveFixture(t)

	write(t, tree.Path, "a.txt", "a changed\n")       // tracked, not staged
	write(t, tree.Path, "staged.txt", "staged\n")     // staged
	write(t, tree.Path, "dir/new.txt", "untracked\n") // untracked, in a directory
	write(t, tree.Path, "noise.log", "ignored\n")     // ignored
	run(t, tree.Path, "add", "staged.txt")

	head := out(t, tree.Path, "rev-parse", "HEAD")
	status := out(t, tree.Path, "status", "--porcelain=v2")
	cached := out(t, tree.Path, "diff", "--cached")
	mainIndex, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	require.NoError(t, err)

	otherStatus := out(t, other, "status", "--porcelain=v2")
	refs := out(t, repo, "for-each-ref", "--format=%(refname)")

	got, err := tree.Preserve("T-1")
	require.NoError(t, err)
	require.Empty(t, got.Branches)
	require.NotEmpty(t, got.Dirty)
	assert.True(t, strings.HasPrefix(got.Dirty, "refs/zmina-preserve-dirty/t-1-"), got.Dirty)
	assert.Equal(t, head[:8], got.DirtyOver)

	// What it holds: the working files as they were, over HEAD, and not what is ignored.
	assert.Equal(t, head, out(t, repo, "rev-parse", got.Dirty+"^"))
	assert.Equal(t, "a changed", out(t, repo, "show", got.Dirty+":a.txt"))
	assert.Equal(t, "staged", out(t, repo, "show", got.Dirty+":staged.txt"))
	assert.Equal(t, "untracked", out(t, repo, "show", got.Dirty+":dir/new.txt"))
	assert.Equal(t, "a.txt\ndir/new.txt\nstaged.txt", out(t, repo, "diff", "--name-only", head, got.Dirty))
	assert.NotContains(t, out(t, repo, "ls-tree", "-r", "--name-only", got.Dirty), "noise.log")
	assert.True(t, strings.HasPrefix(got.Dirty, PreserveDirtyPrefix))
	assert.Equal(t, "zmina", out(t, repo, "log", "-1", "--format=%an", got.Dirty))

	// What it left alone.
	assert.Equal(t, status, out(t, tree.Path, "status", "--porcelain=v2"), "the worktree's files and index")
	assert.Equal(t, cached, out(t, tree.Path, "diff", "--cached"))
	assert.Equal(t, "a changed\n", mustRead(t, filepath.Join(tree.Path, "a.txt")))
	assert.FileExists(t, filepath.Join(tree.Path, "noise.log"))

	index, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	require.NoError(t, err)
	assert.Equal(t, mainIndex, index, "the main checkout's index")
	assert.Equal(t, otherStatus, out(t, other, "status", "--porcelain=v2"), "another task's worktree")
	assert.Empty(t, stack(t, repo), "the shared stash stack")
	assert.Empty(t, out(t, repo, "branch", "--list", "zmina-preserve/*"), "no branch for changes: a ref outside refs/heads")

	after := out(t, repo, "for-each-ref", "--format=%(refname)")
	assert.Equal(t, refs+"\n"+got.Dirty, after)

	// Kept again, they are kept again: a snapshot is a commit of its own, and the worktree is still as it was.
	again, err := tree.Preserve("T-1")
	require.NoError(t, err)
	require.NotEmpty(t, again.Dirty)
	assert.Equal(t, "a changed", out(t, repo, "show", again.Dirty+":a.txt"))
}

// Commits and changes together, and a detached HEAD with commits of its own while the task's branch has none: each is kept.
func TestPreserveKeepsEverythingThatWouldBeLost(t *testing.T) {
	repo, tree, _ := preserveFixture(t)

	write(t, tree.Path, "b.txt", "b")
	run(t, tree.Path, "add", ".")
	run(t, tree.Path, "commit", "-q", "-m", "b")
	write(t, tree.Path, "wip.txt", "wip")

	got, err := tree.Preserve("T-1")
	require.NoError(t, err)
	require.Len(t, got.Branches, 1)
	assert.NotEmpty(t, got.Dirty)
	assert.Equal(t, got.Branches[0].Tip[:8], got.DirtyOver, "the changes are over the commit kept on the branch")
	assert.Equal(t, "wip", out(t, repo, "show", got.Dirty+":wip.txt"))

	t.Run("a detached head", func(t *testing.T) {
		repo, tree, _ := preserveFixture(t)

		run(t, tree.Path, "checkout", "-q", "--detach")
		write(t, tree.Path, "d.txt", "d")
		run(t, tree.Path, "add", ".")
		run(t, tree.Path, "commit", "-q", "-m", "d")

		tip := out(t, tree.Path, "rev-parse", "HEAD")

		got, err := tree.Preserve("T-1")
		require.NoError(t, err)
		require.Len(t, got.Branches, 1, "the branch has nothing; the commit of the detached head is lost with the worktree")
		assert.Equal(t, tip, got.Branches[0].Tip)
		assert.Equal(t, tip, out(t, repo, "rev-parse", got.Branches[0].Name))
	})
}

// A worktree whose directory is gone still has its branch's commits to keep, and no changes.
func TestPreserveWithTheWorktreeDirectoryGone(t *testing.T) {
	repo, tree, _ := preserveFixture(t)

	write(t, tree.Path, "b.txt", "b")
	run(t, tree.Path, "add", ".")
	run(t, tree.Path, "commit", "-q", "-m", "b")
	require.NoError(t, os.RemoveAll(tree.Path))

	got, err := tree.Preserve("T-1")
	require.NoError(t, err)
	require.Len(t, got.Branches, 1)
	assert.Empty(t, got.Dirty)
	assert.Equal(t, out(t, repo, "rev-parse", "refs/heads/zmina/t-1"), out(t, repo, "rev-parse", got.Branches[0].Name))
}

// Whatever keeps the work from being kept is an error, and nothing is made: a base that is not there to compare against, a
// name with nothing to make it from, a ref the name is taken by.
func TestPreserveRefusesWhatItCannotKeep(t *testing.T) {
	t.Run("the base is gone", func(t *testing.T) {
		repo, tree, _ := preserveFixture(t)
		write(t, tree.Path, "b.txt", "b")
		run(t, tree.Path, "add", ".")
		run(t, tree.Path, "commit", "-q", "-m", "b")

		tree.Base = "no-such-base"

		_, err := tree.Preserve("T-1")
		require.Error(t, err)
		assert.Empty(t, out(t, repo, "branch", "--list", "zmina-preserve/*"))
	})

	t.Run("no id", func(t *testing.T) {
		_, tree, _ := preserveFixture(t)
		_, err := tree.Preserve(" ")
		require.Error(t, err)
	})

	t.Run("the name is taken by another commit", func(t *testing.T) {
		repo, tree, _ := preserveFixture(t)
		write(t, tree.Path, "b.txt", "b")
		run(t, tree.Path, "add", ".")
		run(t, tree.Path, "commit", "-q", "-m", "b")

		tip := out(t, tree.Path, "rev-parse", "HEAD")
		run(t, repo, "branch", "zmina-preserve/t-1-"+tip[:8], "main")

		_, err := tree.Preserve("T-1")
		require.ErrorContains(t, err, "is there already")
	})

	t.Run("a stopped context", func(t *testing.T) {
		_, tree, _ := preserveFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		tree.Ctx = ctx
		_, err := tree.Preserve("T-1")
		require.Error(t, err)
	})
}

func TestRefPart(t *testing.T) {
	for in, want := range map[string]string{
		"T-12":       "t-12",
		" T-12 ":     "t-12",
		"alpha beta": "alpha-beta",
		"a/b..c":     "a-b..c",
		"..hidden":   "hidden",
		"-x-":        "x",
		"":           "",
		"é":          "",
	} {
		assert.Equal(t, want, refPart(in), in)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(raw)
}
