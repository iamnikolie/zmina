package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moveFixture is a repository with a bare remote it is origin to, and a task's worktree on a branch made from main.
func moveFixture(t *testing.T) (remote, repo string, tree *Tree) {
	t.Helper()

	remote = filepath.Join(t.TempDir(), "origin.git")
	run(t, filepath.Dir(remote), "init", "-q", "--bare", "-b", "main", remote)

	repo, tree, _ = preserveFixture(t)
	run(t, repo, "remote", "add", "origin", remote)
	run(t, repo, "push", "-q", "origin", "main")

	return remote, repo, tree
}

func TestCheckBranchRefusesWhatIsNotAPlainBranch(t *testing.T) {
	require.NoError(t, CheckBranch("zmina/alpha-acme-t-14"))
	require.NoError(t, CheckBranch("zmina/a.b-t-1"))

	for _, bad := range []string{"", "-x", "--upload-pack=x", ".hidden", "a b", "a..b", "a/", "a;b", "a$(b)", "a\nb", "a:b", "a~1", "a^", "a?", "a*",
		"a[", "a\\b", "/a", "a.lock", "a/.b", strings.Repeat("a", 201), "@{x}", "x@{1}"} {
		assert.Error(t, CheckBranch(bad), "%q", bad)
	}
}

// The uncommitted work of a worktree becomes one commit with the move's trailer, the worktree reads clean, no ignored file is in it,
// and the undo on the other end gives the work back uncommitted.
func TestCommitWIPAndUndoRoundTrip(t *testing.T) {
	remote, repo, tree := moveFixture(t)
	_ = remote

	// Nothing to commit: the tip as it is.
	tip, made, err := tree.CommitWIP("mv-1")
	require.NoError(t, err)
	assert.False(t, made)
	assert.Equal(t, out(t, repo, "rev-parse", "refs/heads/zmina/t-1"), tip)

	write(t, tree.Path, "a.txt", "changed\n")
	write(t, tree.Path, "new.txt", "new\n")
	write(t, tree.Path, "debug.log", "ignored\n")

	commit, made, err := tree.CommitWIP("mv-1")
	require.NoError(t, err)
	require.True(t, made)

	assert.Equal(t, commit, out(t, repo, "rev-parse", "refs/heads/zmina/t-1"), "the branch is at the commit")
	assert.Empty(t, out(t, tree.Path, "status", "--short"), "the worktree is clean at it: nothing staged either")
	assert.Equal(t, "changed\n", read(t, tree.Path, "a.txt"))
	assert.Equal(t, "ignored\n", read(t, tree.Path, "debug.log"), "an ignored file is left where it is")

	files := out(t, repo, "ls-tree", "-r", "--name-only", commit)
	assert.Contains(t, files, "new.txt")
	assert.NotContains(t, files, "debug.log", "an ignored file does not travel")

	carries, err := CarriesMove(repo, commit, "mv-1")
	require.NoError(t, err)
	assert.True(t, carries)

	carries, err = CarriesMove(repo, commit, "mv-2")
	require.NoError(t, err)
	assert.False(t, carries, "another move's id is not this move's trailer")

	// Again: it is clean now, and the tip is the commit already made.
	again, made, err := tree.CommitWIP("mv-1")
	require.NoError(t, err)
	assert.False(t, made)
	assert.Equal(t, commit, again)

	// The other end: the branch comes by origin, a worktree takes it, and the work is given back uncommitted.
	require.NoError(t, Push(repo, "zmina/t-1"))

	clone := filepath.Join(t.TempDir(), "clone")
	run(t, filepath.Dir(clone), "clone", "-q", remote, clone)

	sha, err := Fetch(clone, "zmina/t-1", "refs/zmina-move/mv-1/0")
	require.NoError(t, err)
	assert.Equal(t, commit, sha)

	require.NoError(t, MakeBranch(clone, "zmina/t-1", sha))

	taken, err := AddExisting(clone, filepath.Join(t.TempDir(), "wt", "clone-T-9"), "zmina/t-1")
	require.NoError(t, err)

	// Not this move's: nothing is undone.
	undone, err := taken.UndoWIP("mv-2")
	require.NoError(t, err)
	assert.False(t, undone)

	undone, err = taken.UndoWIP("mv-1")
	require.NoError(t, err)
	assert.True(t, undone)
	assert.Equal(t, out(t, repo, "rev-parse", commit+"~1"), out(t, clone, "rev-parse", "refs/heads/zmina/t-1"), "the branch is at its parent")
	assert.Equal(t, "changed\n", read(t, taken.Path, "a.txt"), "the work is in the files")
	assert.Equal(t, "new\n", read(t, taken.Path, "new.txt"))
	assert.NotContains(t, out(t, taken.Path, "status", "--short"), "debug.log")
	assert.Contains(t, out(t, taken.Path, "status", "--short"), "new.txt", "uncommitted again")

	require.NoError(t, DropRef(clone, "refs/zmina-move/mv-1/0"))
	assert.Empty(t, out(t, clone, "for-each-ref", "refs/zmina-move"))
	require.NoError(t, DropRef(clone, "refs/zmina-move/mv-1/0"), "a ref that is gone is no error")
}

func read(t *testing.T, dir, name string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)

	return string(raw)
}

// What a tree cannot hold is refused, and the branch is not touched; a worktree that is on another branch is refused too.
func TestCommitWIPRefusals(t *testing.T) {
	_, repo, tree := moveFixture(t)
	before := out(t, repo, "rev-parse", "refs/heads/zmina/t-1")

	// A nested repository.
	nested := filepath.Join(tree.Path, "vendor", "lib")
	require.NoError(t, os.MkdirAll(nested, 0o700))
	run(t, nested, "init", "-q")
	write(t, nested, "x.txt", "x\n")

	_, _, err := tree.CommitWIP("mv-1")
	require.Error(t, err)
	assert.Equal(t, before, out(t, repo, "rev-parse", "refs/heads/zmina/t-1"))
	require.NoError(t, os.RemoveAll(filepath.Join(tree.Path, "vendor")))

	// Detached HEAD: not the task's branch.
	run(t, tree.Path, "checkout", "-q", "--detach")
	write(t, tree.Path, "a.txt", "changed\n")

	_, _, err = tree.CommitWIP("mv-1")
	require.ErrorContains(t, err, "is not on the task's branch")

	// A worktree that is gone.
	gone := &Tree{Repo: repo, Path: filepath.Join(t.TempDir(), "nope"), Branch: "zmina/t-1"}
	_, _, err = gone.CommitWIP("mv-1")
	require.ErrorContains(t, err, "is gone")
}

func TestPushRefusesABranchTheRemoteHasMovedOn(t *testing.T) {
	remote, repo, tree := moveFixture(t)

	write(t, tree.Path, "b.txt", "b\n")
	run(t, tree.Path, "add", ".")
	run(t, tree.Path, "commit", "-q", "-m", "b")
	require.NoError(t, Push(repo, "zmina/t-1"))
	require.NoError(t, Push(repo, "zmina/t-1"), "the same tip again is no change")

	// Another clone pushes a different commit under the same name.
	other := filepath.Join(t.TempDir(), "other")
	run(t, filepath.Dir(other), "clone", "-q", remote, other)
	run(t, other, "checkout", "-q", "-b", "x", "origin/zmina/t-1")
	write(t, other, "c.txt", "c\n")
	run(t, other, "add", ".")
	run(t, other, "commit", "-q", "-m", "c")
	run(t, other, "push", "-q", "origin", "x:zmina/t-1")

	write(t, tree.Path, "d.txt", "d\n")
	run(t, tree.Path, "add", ".")
	run(t, tree.Path, "commit", "-q", "-m", "d")

	require.Error(t, Push(repo, "zmina/t-1"), "a branch that is not a descendant of the remote one is refused")
}

func TestOriginAndFetchRefusals(t *testing.T) {
	remote, repo, _ := moveFixture(t)

	got, err := Origin(repo)
	require.NoError(t, err)
	assert.Equal(t, remote, got)

	bare := t.TempDir()
	run(t, bare, "init", "-q")

	got, err = Origin(bare)
	require.NoError(t, err)
	assert.Empty(t, got, "a repository with no origin says none")

	_, err = Fetch(repo, "zmina/t-1", "refs/heads/x")
	require.ErrorContains(t, err, "not one a move keeps")
	_, err = Fetch(repo, "--upload-pack=x", "refs/zmina-move/a/0")
	require.Error(t, err)
	_, err = Fetch(repo, "zmina/missing", "refs/zmina-move/a/0")
	require.Error(t, err, "a branch the remote has not")
	require.Error(t, DropRef(repo, "refs/heads/main"), "only a ref a move keeps")
	require.Error(t, MakeBranch(repo, "x", "not-a-sha"))
}

func TestAncestorCheckedOutAndFastForward(t *testing.T) {
	_, repo, tree := moveFixture(t)

	base := out(t, repo, "rev-parse", "main")
	write(t, tree.Path, "b.txt", "b\n")
	run(t, tree.Path, "add", ".")
	run(t, tree.Path, "commit", "-q", "-m", "b")

	tip := out(t, repo, "rev-parse", "refs/heads/zmina/t-1")

	yes, err := IsAncestor(repo, base, tip)
	require.NoError(t, err)
	assert.True(t, yes)

	yes, err = IsAncestor(repo, tip, base)
	require.NoError(t, err)
	assert.False(t, yes)

	_, err = IsAncestor(repo, "main", tip)
	require.Error(t, err, "whole commit ids only")

	where, err := CheckedOutAt(repo, "zmina/t-1")
	require.NoError(t, err)
	assert.True(t, samePath(t, where, tree.Path), where)

	none, err := CheckedOutAt(repo, "zmina/elsewhere")
	require.NoError(t, err)
	assert.Empty(t, none)

	// A branch no worktree has: it goes forward, only from where it is.
	require.NoError(t, MakeBranch(repo, "x", base))
	require.Error(t, FastForward(repo, "x", tip, tip), "not where the caller said it was")
	require.NoError(t, FastForward(repo, "x", base, tip))
	assert.Equal(t, tip, out(t, repo, "rev-parse", "refs/heads/x"))
	require.Error(t, FastForward(repo, "-x", base, tip))
}

func samePath(t *testing.T, a, b string) bool {
	t.Helper()

	ra, err := filepath.EvalSymlinks(a)
	require.NoError(t, err)

	rb, err := filepath.EvalSymlinks(b)
	require.NoError(t, err)

	return ra == rb
}
