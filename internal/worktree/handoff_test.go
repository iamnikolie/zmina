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

// The uncommitted work of a worktree becomes one commit with the move's trailer, kept under a private ref and nowhere else: the branch,
// the index and the files stay as they were. The other end gets the branch from origin, the work from the private ref, and restores
// it onto the branch's tip, uncommitted.
func TestCommitWIPAndRestoreRoundTrip(t *testing.T) {
	remote, repo, tree := moveFixture(t)

	// Nothing to commit: the tip as it is, and no ref.
	tip, made, err := tree.CommitWIP("mv-1", 7)
	require.NoError(t, err)
	assert.False(t, made)
	assert.Equal(t, out(t, repo, "rev-parse", "refs/heads/zmina/t-1"), tip)
	assert.Empty(t, out(t, repo, "for-each-ref", "refs/zmina-move"))

	write(t, tree.Path, "a.txt", "changed\n")
	write(t, tree.Path, "new.txt", "new\n")
	write(t, tree.Path, "debug.log", "ignored\n")

	before := out(t, tree.Path, "status", "--porcelain")

	commit, made, err := tree.CommitWIP("mv-1", 7)
	require.NoError(t, err)
	require.True(t, made)

	assert.Equal(t, tip, out(t, repo, "rev-parse", "refs/heads/zmina/t-1"), "the branch does not move")
	assert.Equal(t, commit, out(t, repo, "rev-parse", MoveRef("mv-1", 7)), "the commit is kept under the move's private ref")
	assert.Equal(t, "refs/zmina-move/mv-1/7", MoveRef("mv-1", 7))
	assert.Equal(t, before, out(t, tree.Path, "status", "--porcelain"), "the worktree is exactly as it was: nothing committed, staged or touched")
	assert.Equal(t, "ignored\n", read(t, tree.Path, "debug.log"), "an ignored file is left where it is")

	files := out(t, repo, "ls-tree", "-r", "--name-only", commit)
	assert.Contains(t, files, "new.txt")
	assert.NotContains(t, files, "debug.log", "an ignored file does not travel")
	assert.Equal(t, tip, out(t, repo, "rev-parse", commit+"^"), "one commit over the tip")

	carries, err := CarriesMove(repo, commit, "mv-1")
	require.NoError(t, err)
	assert.True(t, carries)

	carries, err = CarriesMove(repo, commit, "mv-2")
	require.NoError(t, err)
	assert.False(t, carries, "another move's id is not this move's trailer")

	// Again: the same files over the same tip are the commit already made.
	again, made, err := tree.CommitWIP("mv-1", 7)
	require.NoError(t, err)
	assert.True(t, made)
	assert.Equal(t, commit, again)

	// The branch and the private ref go to origin, the branch without the commit.
	require.NoError(t, Push(repo, "zmina/t-1"))
	require.NoError(t, PushWIP(context.Background(), repo, commit, "mv-1", 7))
	require.NoError(t, PushWIP(context.Background(), repo, commit, "mv-1", 7), "again: no change")

	assert.Equal(t, tip, out(t, remote, "rev-parse", "refs/heads/zmina/t-1"), "the branch on origin never carries the commit")
	assert.Equal(t, commit, out(t, remote, "rev-parse", "refs/zmina-move/mv-1/7"))

	clone := filepath.Join(t.TempDir(), "clone")
	run(t, filepath.Dir(clone), "clone", "-q", remote, clone)

	sha, err := Fetch(clone, "zmina/t-1", "refs/zmina-move/mv-1/b7")
	require.NoError(t, err)
	assert.Equal(t, tip, sha)

	wip, err := FetchWIP(context.Background(), clone, "mv-1", 7, "refs/zmina-move/mv-1/w7")
	require.NoError(t, err)
	assert.Equal(t, commit, wip)

	require.NoError(t, MakeBranch(clone, "zmina/t-1", sha))

	taken, err := AddExisting(clone, filepath.Join(t.TempDir(), "wt", "clone-T-9"), "zmina/t-1")
	require.NoError(t, err)

	// Not this move's: refused, nothing is changed.
	require.Error(t, taken.RestoreWIP(wip, "mv-2"))
	assert.Empty(t, out(t, taken.Path, "status", "--porcelain"))

	require.NoError(t, taken.RestoreWIP(wip, "mv-1"))
	assert.Equal(t, tip, out(t, clone, "rev-parse", "refs/heads/zmina/t-1"), "the branch stays at the tip origin has")
	assert.Equal(t, "changed\n", read(t, taken.Path, "a.txt"), "the work is in the files")
	assert.Equal(t, "new\n", read(t, taken.Path, "new.txt"))
	assert.NoFileExists(t, filepath.Join(taken.Path, "debug.log"))
	assert.Equal(t, before, out(t, taken.Path, "status", "--porcelain"), "git status says what it said at the other end")

	require.Error(t, taken.RestoreWIP(wip, "mv-1"), "not onto a worktree that already holds work")

	// The refs go: the local ones, and origin's.
	require.NoError(t, DeleteRemoteRef(context.Background(), repo, MoveRef("mv-1", 7)))
	require.NoError(t, DeleteRemoteRef(context.Background(), repo, MoveRef("mv-1", 7)), "a ref that is gone is no error")
	assert.Empty(t, out(t, remote, "for-each-ref", "refs/zmina-move"))

	refs, err := MoveRefs(clone, "mv-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"refs/zmina-move/mv-1/b7", "refs/zmina-move/mv-1/w7"}, refs)

	for _, r := range refs {
		require.NoError(t, DropRef(clone, r))
	}

	assert.Empty(t, out(t, clone, "for-each-ref", "refs/zmina-move"))
	require.NoError(t, DropRef(clone, "refs/zmina-move/mv-1/b7"), "a ref that is gone is no error")
}

// What is staged does not decide what travels: the files do. A file staged and edited again, one staged and deleted from the disk, a
// tracked file deleted, and a mode change all come back as the files were.
func TestCommitWIPCarriesTheFilesWhateverTheIndexHolds(t *testing.T) {
	remote, repo, tree := moveFixture(t)
	_ = remote

	write(t, tree.Path, "b.txt", "b v1\n")
	run(t, tree.Path, "add", "b.txt")
	write(t, tree.Path, "b.txt", "b v2\n") // staged, then edited again: the ordinary partial staging

	write(t, tree.Path, "c.txt", "c\n")
	run(t, tree.Path, "add", "c.txt")
	require.NoError(t, os.Remove(filepath.Join(tree.Path, "c.txt"))) // staged, then deleted from the disk

	require.NoError(t, os.Remove(filepath.Join(tree.Path, "a.txt"))) // a tracked file, deleted

	tip := out(t, repo, "rev-parse", "refs/heads/zmina/t-1")
	index := out(t, tree.Path, "ls-files", "--stage")

	commit, made, err := tree.CommitWIP("mv-1", 1)
	require.NoError(t, err, "no refusal for a state the files can give back")
	require.True(t, made)
	assert.Equal(t, index, out(t, tree.Path, "ls-files", "--stage"), "the index is not touched")

	files := out(t, repo, "ls-tree", "-r", "--name-only", commit)
	assert.Contains(t, files, "b.txt")
	assert.NotContains(t, files, "c.txt")
	assert.NotContains(t, files, "a.txt")

	clone := filepath.Join(t.TempDir(), "clone")
	run(t, filepath.Dir(clone), "clone", "-q", out(t, repo, "remote", "get-url", "origin"), clone)
	run(t, repo, "push", "-q", "origin", "refs/heads/zmina/t-1:refs/heads/zmina/t-1")
	run(t, clone, "fetch", "-q", "origin")
	run(t, clone, "branch", "zmina/t-1", tip)
	run(t, clone, "fetch", "-q", repo, "+"+MoveRef("mv-1", 1)+":"+MoveRef("mv-1", 1))

	taken, err := AddExisting(clone, filepath.Join(t.TempDir(), "wt", "clone-T-9"), "zmina/t-1")
	require.NoError(t, err)
	require.NoError(t, taken.RestoreWIP(commit, "mv-1"))

	assert.Equal(t, "b v2\n", read(t, taken.Path, "b.txt"))
	assert.NoFileExists(t, filepath.Join(taken.Path, "c.txt"))
	assert.NoFileExists(t, filepath.Join(taken.Path, "a.txt"))
	assert.Empty(t, out(t, taken.Path, "diff", "--cached", "--name-only"), "nothing is staged there")
	assert.Equal(t, "D a.txt\n?? b.txt", out(t, taken.Path, "status", "--porcelain"), "(out trims the blank before the unstaged deletion)")
}

// A worktree in the middle of a merge, a rebase, a cherry-pick or with conflicts, and one with a nested repository, are named by the
// preflight (CheckCarry) and refused by CommitWIP, and nothing is made.
func TestCheckCarryNamesWhatAWorktreeCannotGiveBack(t *testing.T) {
	_, repo, tree := moveFixture(t)

	require.NoError(t, tree.CheckCarry(), "a clean worktree, and one with plain changes, can travel")
	write(t, tree.Path, "plain.txt", "x\n")
	require.NoError(t, tree.CheckCarry())
	require.NoError(t, os.Remove(filepath.Join(tree.Path, "plain.txt")))

	// A merge that stopped before its commit (no conflict: the state is the trouble, not a path).
	other := filepath.Join(t.TempDir(), "other-wt")
	run(t, repo, "worktree", "add", "-q", "-b", "other", other, "main")
	write(t, other, "o.txt", "o\n")
	run(t, other, "add", ".")
	run(t, other, "commit", "-q", "-m", "o")

	run(t, tree.Path, "merge", "--no-commit", "--no-ff", "other")
	assert.FileExists(t, out(t, tree.Path, "rev-parse", "--git-path", "MERGE_HEAD"))

	err := tree.CheckCarry()
	require.ErrorContains(t, err, "a merge in progress")

	_, _, err = tree.CommitWIP("mv-1", 1)
	require.ErrorContains(t, err, "a merge in progress")
	assert.Empty(t, out(t, repo, "for-each-ref", "refs/zmina-move"), "nothing was made")

	run(t, tree.Path, "merge", "--abort")

	// A cherry-pick with a conflict: the state and the unmerged paths.
	write(t, tree.Path, "o.txt", "mine\n")
	run(t, tree.Path, "add", ".")
	run(t, tree.Path, "commit", "-q", "-m", "mine")

	cmd := exec.Command("git", "-C", tree.Path, "cherry-pick", "other")
	_, _ = cmd.CombinedOutput()

	err = tree.CheckCarry()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cherry-pick in progress")

	run(t, tree.Path, "cherry-pick", "--abort")
	require.NoError(t, tree.CheckCarry())

	// Conflicts with no operation recorded (an index left unmerged by hand).
	write(t, tree.Path, "u.txt", "base\n")
	run(t, tree.Path, "add", ".")
	run(t, tree.Path, "commit", "-q", "-m", "u")

	blob := out(t, tree.Path, "rev-parse", "HEAD:u.txt")
	run(t, tree.Path, "update-index", "--force-remove", "u.txt")

	cmd = exec.Command("git", "-C", tree.Path, "update-index", "--index-info")
	cmd.Stdin = strings.NewReader("100644 " + blob + " 1\tu.txt\n100644 " + blob + " 2\tu.txt\n")
	raw, err := cmd.CombinedOutput()
	require.NoError(t, err, string(raw))

	require.ErrorContains(t, tree.CheckCarry(), "conflicts")

	run(t, tree.Path, "reset", "-q", "--hard")

	// A nested repository.
	nested := filepath.Join(tree.Path, "vendor", "lib")
	require.NoError(t, os.MkdirAll(nested, 0o700))
	run(t, nested, "init", "-q")
	write(t, nested, "x.txt", "x\n")

	err = tree.CheckCarry()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vendor/lib")

	// A worktree that is gone has nothing to carry.
	gone := &Tree{Repo: repo, Path: filepath.Join(t.TempDir(), "nope"), Branch: "zmina/t-1"}
	require.NoError(t, gone.CheckCarry())
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

	_, _, err := tree.CommitWIP("mv-1", 1)
	require.Error(t, err)
	assert.Equal(t, before, out(t, repo, "rev-parse", "refs/heads/zmina/t-1"))
	require.NoError(t, os.RemoveAll(filepath.Join(tree.Path, "vendor")))

	// Detached HEAD: not the task's branch.
	run(t, tree.Path, "checkout", "-q", "--detach")
	write(t, tree.Path, "a.txt", "changed\n")

	_, _, err = tree.CommitWIP("mv-1", 1)
	require.ErrorContains(t, err, "is not on the task's branch")

	// A worktree that is gone.
	gone := &Tree{Repo: repo, Path: filepath.Join(t.TempDir(), "nope"), Branch: "zmina/t-1"}
	_, _, err = gone.CommitWIP("mv-1", 1)
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

	// What git resolves is what is compared: a rewrite (insteadOf) is applied, a push URL of its own is told apart.
	push, err := PushOrigin(repo)
	require.NoError(t, err)
	assert.Equal(t, remote, push, "no push URL of its own: the one fetches use")

	run(t, repo, "config", "url."+remote+".insteadOf", "alias:")
	run(t, repo, "remote", "set-url", "origin", "alias:")

	got, err = Origin(repo)
	require.NoError(t, err)
	assert.Equal(t, remote, got, "insteadOf applied")

	other := filepath.Join(t.TempDir(), "other.git")
	run(t, repo, "remote", "set-url", "--push", "origin", other)

	got, err = Origin(repo)
	require.NoError(t, err)
	assert.Equal(t, remote, got)

	push, err = PushOrigin(repo)
	require.NoError(t, err)
	assert.Equal(t, other, push, "pushurl")

	none, err := PushOrigin(bare)
	require.NoError(t, err)
	assert.Empty(t, none)

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
