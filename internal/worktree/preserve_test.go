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
	ownIndex := ownIndexPath(t, tree.Path)
	ownIndexBefore, err := os.ReadFile(ownIndex)
	require.NoError(t, err)
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

	// What the index holds is kept too, as the index is neither HEAD's tree nor the files': a second commit, over HEAD, of the
	// staged file alone.
	require.NotEmpty(t, got.Index)
	assert.True(t, strings.HasPrefix(got.Index, PreserveDirtyPrefix+"t-1-index-"), got.Index)
	assert.Equal(t, head[:8], got.IndexOver)
	assert.Equal(t, "staged", out(t, repo, "show", got.Index+":staged.txt"))
	assert.Equal(t, "a", out(t, repo, "show", got.Index+":a.txt"), "the index has the file as it was staged, not as it is on disk")

	ownIndexAfter, err := os.ReadFile(ownIndex)
	require.NoError(t, err)
	assert.Equal(t, ownIndexBefore, ownIndexAfter, "the worktree's own index file, byte for byte")

	after := out(t, repo, "for-each-ref", "--format=%(refname)")
	assert.Equal(t, refs+"\n"+got.Dirty+"\n"+got.Index, after)

	// Named by the tree of the snapshot: the same changes kept again are the same ref, and nothing else is made.
	treeID := out(t, repo, "rev-parse", got.Dirty+"^{tree}")
	assert.Equal(t, PreserveDirtyPrefix+"t-1-"+treeID[:8], got.Dirty)

	again, err := tree.Preserve("T-1")
	require.NoError(t, err)
	assert.Equal(t, got, again)
	assert.Equal(t, after, out(t, repo, "for-each-ref", "--format=%(refname)"), "no second ref")
	assert.Equal(t, "a changed", out(t, repo, "show", again.Dirty+":a.txt"))

	// Other changes are another snapshot, another ref.
	write(t, tree.Path, "more.txt", "more\n")

	third, err := tree.Preserve("T-1")
	require.NoError(t, err)
	assert.NotEqual(t, got.Dirty, third.Dirty)
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

// What is kept does not depend on the user's config: with status.showUntrackedFiles=no, git status shows no untracked file,
// and a worktree whose only work is one is still not clean, for Dirty or for Preserve.
func TestPreserveAndDirtyDoNotDependOnTheUsersStatusConfig(t *testing.T) {
	repo, tree, _ := preserveFixture(t)

	run(t, repo, "config", "status.showUntrackedFiles", "no")
	write(t, tree.Path, "only-untracked.txt", "work\n")
	write(t, tree.Path, "dir/inner.txt", "work\n")

	assert.Empty(t, out(t, tree.Path, "status", "--short"), "the config hides it from git status")

	dirty, err := tree.Dirty()
	require.NoError(t, err)
	assert.Contains(t, dirty, "?? only-untracked.txt")
	assert.Contains(t, dirty, "?? dir/inner.txt", "every untracked file, not the directory")

	got, err := tree.Preserve("T-1")
	require.NoError(t, err)
	require.NotEmpty(t, got.Dirty)
	assert.Equal(t, "work", out(t, repo, "show", got.Dirty+":only-untracked.txt"))
	assert.Equal(t, "work", out(t, repo, "show", got.Dirty+":dir/inner.txt"))
}

// A snapshot holds a nested repository or a submodule only as a commit id, so the work in it would be lost: it is refused,
// naming the path, and nothing is made (no branch, no ref).
func TestPreserveRefusesNestedRepositoriesAndSubmodules(t *testing.T) {
	t.Run("an untracked directory with a repository of its own", func(t *testing.T) {
		repo, tree, _ := preserveFixture(t)
		write(t, tree.Path, "b.txt", "b")
		run(t, tree.Path, "add", ".")
		run(t, tree.Path, "commit", "-q", "-m", "b")

		nested := filepath.Join(tree.Path, "vendor", "lib")
		require.NoError(t, os.MkdirAll(nested, 0o700))
		run(t, nested, "init", "-q", "-b", "main")
		write(t, nested, "f.txt", "f")
		run(t, nested, "add", ".")
		run(t, nested, "commit", "-q", "-m", "f")

		_, err := tree.Preserve("T-1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "vendor/lib (a nested repository, or a submodule at a commit HEAD does not record)")
		assert.Empty(t, out(t, repo, "branch", "--list", "zmina-preserve/*"), "nothing made, not even the branch of the commits")
		assert.Empty(t, out(t, repo, "for-each-ref", "refs/zmina-preserve-dirty/"))
	})

	t.Run("two of them", func(t *testing.T) {
		_, tree, _ := preserveFixture(t)

		for _, name := range []string{"one", "two"} {
			nested := filepath.Join(tree.Path, name)
			require.NoError(t, os.MkdirAll(nested, 0o700))
			run(t, nested, "init", "-q", "-b", "main")
			write(t, nested, "f.txt", name)
			run(t, nested, "add", ".")
			run(t, nested, "commit", "-q", "-m", name)
		}

		_, err := tree.Preserve("T-1")
		require.ErrorContains(t, err, "(and 1 more)")
	})
}

// Only the task's own checkout is looked into, and a directory that exists and is not it is no "nothing to keep": it is an error
// that refuses the removal, naming the top level git found, and the other checkout's changes are never taken. A symlink to the
// worktree is the worktree; so is a spelling of its path that differs by case or by Unicode form, where the file system treats
// them as one name (a string comparison of paths would call them two directories and skip the work).
func TestPreserveNeverSnapshotsAnotherCheckoutAndRefusesWhatIsNotTheWorktree(t *testing.T) {
	repo, tree, _ := preserveFixture(t)

	// The path the task records is a plain directory inside the main checkout, which has work of its own.
	write(t, repo, "main-work.txt", "not the task's\n")
	plain := filepath.Join(repo, "left-over")
	require.NoError(t, os.MkdirAll(plain, 0o700))
	write(t, plain, "x.txt", "x")

	stray := *tree
	stray.Path = plain

	_, err := stray.Preserve("T-1")
	require.Error(t, err, "it exists and is not the checkout git sees there")
	assert.Contains(t, err.Error(), "left-over exists but is not a checkout of its own")
	assert.Contains(t, err.Error(), "git's top level for it is")
	assert.Empty(t, out(t, repo, "for-each-ref", "refs/zmina-preserve-dirty/"), "the main checkout's work was not taken")
	assert.Empty(t, out(t, repo, "branch", "--list", "zmina-preserve/*"), "and nothing was made")

	// A directory outside any repository is no checkout either.
	bare := filepath.Join(t.TempDir(), "leftover")
	require.NoError(t, os.MkdirAll(bare, 0o700))

	away := *tree
	away.Path = bare

	_, err = away.Preserve("T-1")
	require.ErrorContains(t, err, "git does not see a checkout in it")

	// A symlink to the task's worktree is the worktree.
	write(t, tree.Path, "wip.txt", "wip")

	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(tree.Path, link))

	viaLink := *tree
	viaLink.Path = link

	got, err := viaLink.Preserve("T-1")
	require.NoError(t, err)
	assert.Equal(t, "wip", out(t, repo, "show", got.Dirty+":wip.txt"))
}

// A path spelled with other letter cases, or with the other Unicode form, is the same directory on a file system that says
// so (macOS, Windows), and the work in it is kept: the worktree is found by file identity, not by a string. On a file system
// that tells such spellings apart there is no such directory, and the test says so and skips.
func TestPreserveFindsTheWorktreeBySameFileNotBySpelling(t *testing.T) {
	for name, spell := range map[string]func(string) string{
		"another case": func(base string) string { return strings.ToUpper(base) },
		"the other Unicode form": func(base string) string {
			return strings.ReplaceAll(base, "caf\u00e9", "cafe\u0301")
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			run(t, repo, "init", "-q", "-b", "main")
			write(t, repo, "a.txt", "a\n")
			run(t, repo, "add", ".")
			run(t, repo, "commit", "-q", "-m", "a")

			// The directory has a lower-case name with a precomposed letter; a spelling of it is asked for.
			home, err := os.MkdirTemp("", "zmina-spell-")
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(home) })

			tree, err := Add(repo, filepath.Join(home, "caf\u00e9-alpha"), "zmina/t-1", "")
			require.NoError(t, err)

			write(t, tree.Path, "wip.txt", "wip")

			spelled := *tree
			spelled.Path = filepath.Join(home, spell("caf\u00e9-alpha"))

			if spelled.Path == tree.Path {
				t.Skip("no other spelling to try")
			}

			if _, err := os.Stat(spelled.Path); err != nil {
				t.Skipf("this file system tells %q from %q: %v", filepath.Base(spelled.Path), filepath.Base(tree.Path), err)
			}

			got, err := spelled.Preserve("T-1")
			require.NoError(t, err, "the same directory under another spelling: it is looked into, not skipped")
			require.NotEmpty(t, got.Dirty, "its work is kept")
			assert.Equal(t, "wip", out(t, repo, "show", got.Dirty+":wip.txt"))
		})
	}
}

// A path that cannot be read at all, for any reason but being gone, is a failure to keep, not a worktree with nothing in it.
func TestPreserveRefusesAPathThatCannotBeRead(t *testing.T) {
	_, tree, _ := preserveFixture(t)

	bad := *tree
	bad.Path = filepath.Join(t.TempDir(), "x\x00y") // a name no file system takes: not "no such file"

	_, err := bad.Preserve("T-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be read")
}

// A ref that is there already for the same tree is the same work kept, whatever commit it points at: a retry is not refused.
func TestPreserveTakesARefOfTheSameTreeForTheSameWork(t *testing.T) {
	repo, tree, _ := preserveFixture(t)
	write(t, tree.Path, "wip.txt", "wip")

	first, err := tree.Preserve("T-1")
	require.NoError(t, err)

	treeID := out(t, repo, "rev-parse", first.Dirty+"^{tree}")

	// The ref is gone and another commit of the very same tree is put under the name: as a run that was cut off, or a clock
	// that moved, leaves it.
	run(t, repo, "update-ref", "-d", first.Dirty)
	other := out(t, repo, "commit-tree", treeID, "-m", "made by another run")
	run(t, repo, "update-ref", first.Dirty, other)

	again, err := tree.Preserve("T-1")
	require.NoError(t, err)
	assert.Equal(t, first.Dirty, again.Dirty)
	assert.Equal(t, other, out(t, repo, "rev-parse", first.Dirty), "what was there stays")
}

// A repository with no commit yet keeps its files; one with no file has nothing to keep.
func TestPreserveOfAWorktreeWithNoCommit(t *testing.T) {
	repo := t.TempDir()
	run(t, repo, "init", "-q", "-b", "main")

	tree := &Tree{Repo: repo, Path: repo, Branch: "main", Base: "main"}

	got, err := tree.Preserve("T-1")
	require.NoError(t, err)
	assert.True(t, got.Empty())

	write(t, repo, "first.txt", "first")

	got, err = tree.Preserve("T-1")
	require.NoError(t, err)
	require.NotEmpty(t, got.Dirty)
	assert.Empty(t, got.DirtyOver)
	assert.Equal(t, "first", out(t, repo, "show", got.Dirty+":first.txt"))
}

// ownIndexPath is the file of a worktree's own index.
func ownIndexPath(t *testing.T, dir string) string {
	t.Helper()

	p := out(t, dir, "rev-parse", "--git-path", "index")
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}

	return p
}

// What lives in the worktree's own index alone is kept too: a file staged and then deleted from the disk, an ignored file added
// with force and not committed, the version of a file that was staged before it was edited again. The index is read from a copy:
// its file is as it was, byte for byte.
func TestPreserveKeepsWhatLivesInTheIndexAlone(t *testing.T) {
	t.Run("a file staged and then deleted from the disk", func(t *testing.T) {
		repo, tree, _ := preserveFixture(t)
		write(t, tree.Path, "gone.txt", "staged then deleted\n")
		run(t, tree.Path, "add", "gone.txt")
		require.NoError(t, os.Remove(filepath.Join(tree.Path, "gone.txt")))

		before, err := os.ReadFile(ownIndexPath(t, tree.Path))
		require.NoError(t, err)

		got, err := tree.Preserve("T-1")
		require.NoError(t, err)
		assert.Empty(t, got.Dirty, "the files are HEAD's: there is nothing in them to keep")
		require.NotEmpty(t, got.Index)
		assert.Equal(t, "staged then deleted", out(t, repo, "show", got.Index+":gone.txt"))
		assert.Equal(t, out(t, repo, "rev-parse", "HEAD")[:8], got.IndexOver)
		assert.False(t, got.Empty())

		after, err := os.ReadFile(ownIndexPath(t, tree.Path))
		require.NoError(t, err)
		assert.Equal(t, before, after, "the worktree's own index is not touched")
		assert.Contains(t, out(t, tree.Path, "status", "--short"), "AD gone.txt", "and still says what it did")
	})

	t.Run("an ignored file added with force", func(t *testing.T) {
		repo, tree, _ := preserveFixture(t)
		write(t, tree.Path, "forced.log", "ignored but added\n")
		run(t, tree.Path, "add", "-f", "forced.log")

		got, err := tree.Preserve("T-1")
		require.NoError(t, err)
		assert.Empty(t, got.Dirty, "the files tree leaves an ignored file out; this one is a tracked one in the index only")
		require.NotEmpty(t, got.Index)
		assert.Equal(t, "ignored but added", out(t, repo, "show", got.Index+":forced.log"))
	})

	t.Run("the version staged before the file was edited again", func(t *testing.T) {
		repo, tree, _ := preserveFixture(t)
		write(t, tree.Path, "a.txt", "staged version\n")
		run(t, tree.Path, "add", "a.txt")
		write(t, tree.Path, "a.txt", "edited again\n")

		got, err := tree.Preserve("T-1")
		require.NoError(t, err)
		require.NotEmpty(t, got.Dirty)
		require.NotEmpty(t, got.Index)
		assert.Equal(t, "edited again", out(t, repo, "show", got.Dirty+":a.txt"))
		assert.Equal(t, "staged version", out(t, repo, "show", got.Index+":a.txt"))
	})

	t.Run("an index that is HEAD's or the files' is no second ref", func(t *testing.T) {
		repo, tree, _ := preserveFixture(t)

		write(t, tree.Path, "b.txt", "b\n")
		run(t, tree.Path, "add", "-A")

		got, err := tree.Preserve("T-1")
		require.NoError(t, err)
		require.NotEmpty(t, got.Dirty)
		assert.Empty(t, got.Index, "everything staged: the index is the files' tree")

		clean, tree2, _ := preserveFixture(t)
		write(t, tree2.Path, "c.txt", "c\n")

		got, err = tree2.Preserve("T-1")
		require.NoError(t, err)
		require.NotEmpty(t, got.Dirty)
		assert.Empty(t, got.Index, "nothing staged: the index is HEAD's tree")
		assert.Empty(t, out(t, clean, "for-each-ref", PreserveDirtyPrefix+"t-1-index-*"))
		assert.NotContains(t, out(t, repo, "for-each-ref", "--format=%(refname)"), "-index-")
	})

	t.Run("conflicts: the index cannot be written as a tree, so nothing is kept and the removal is refused", func(t *testing.T) {
		repo, tree, _ := preserveFixture(t)

		write(t, repo, "a.txt", "main side\n")
		run(t, repo, "commit", "-q", "-am", "main side")
		write(t, tree.Path, "a.txt", "task side\n")
		run(t, tree.Path, "commit", "-q", "-am", "task side")

		// The merge stops on a conflict, and git says so with a status: not a failure of this test.
		cmd := exec.Command("git", "-C", tree.Path, "-c", "user.email=t@t", "-c", "user.name=t", "merge", "-q", "main")
		_ = cmd.Run()
		require.Contains(t, out(t, tree.Path, "status", "--short"), "UU a.txt")

		_, err := tree.Preserve("T-1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot be written as a tree")
		assert.Empty(t, out(t, repo, "branch", "--list", "zmina-preserve/*"), "nothing made")
		assert.Empty(t, out(t, repo, "for-each-ref", "refs/zmina-preserve-dirty/"))
	})
}

// A ref kept before for the same tree is the one reused, and what it is over is what its commit says, not what the worktree's
// HEAD is now.
func TestPreserveReportsTheParentOfTheKeptCommitWhenItReusesARef(t *testing.T) {
	repo, tree, _ := preserveFixture(t)
	write(t, tree.Path, "wip.txt", "wip\n")

	first, err := tree.Preserve("T-1")
	require.NoError(t, err)
	require.NotEmpty(t, first.Dirty)

	over := out(t, repo, "rev-parse", first.Dirty+"^")
	assert.Equal(t, over[:8], first.DirtyOver)

	// The worktree's HEAD moves on, and the files tree is the same as it was (a commit that adds nothing to the tree): the
	// ref is reused, and the report says what the kept commit is over, which is not the new HEAD.
	run(t, tree.Path, "commit", "-q", "--allow-empty", "-m", "an empty commit")
	newHead := out(t, tree.Path, "rev-parse", "HEAD")
	require.NotEqual(t, over, newHead)

	again, err := tree.Preserve("T-1")
	require.NoError(t, err)
	assert.Equal(t, first.Dirty, again.Dirty, "the same ref")
	assert.Equal(t, over[:8], again.DirtyOver, "over its own parent")
	assert.NotEqual(t, newHead[:8], again.DirtyOver)
}

// A repository that merely has submodules is preserved: one that is not initialised, and one that is initialised and clean,
// hold nothing a tree of the parent would lose. What blocks is work in one: uncommitted changes, a commit HEAD does not record.
func TestPreserveIsNotBlockedByASubmoduleThatHoldsNoWork(t *testing.T) {
	// A submodule, and a task worktree made after it was added, so that the worktree has it.
	submoduleFixture := func(t *testing.T) (repo string, tree *Tree, sub string) {
		t.Helper()

		sub = t.TempDir()
		run(t, sub, "init", "-q", "-b", "main")
		write(t, sub, "lib.txt", "lib\n")
		run(t, sub, "add", ".")
		run(t, sub, "commit", "-q", "-m", "lib")

		repo = t.TempDir()
		run(t, repo, "init", "-q", "-b", "main")
		write(t, repo, "a.txt", "a\n")
		run(t, repo, "add", ".")
		run(t, repo, "commit", "-q", "-m", "a")
		run(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "deps/sub")
		run(t, repo, "commit", "-q", "-m", "a submodule")

		var err error

		tree, err = Add(repo, filepath.Join(t.TempDir(), "wt", "alpha"), "zmina/t-1", "")
		require.NoError(t, err)

		return repo, tree, sub
	}

	t.Run("not initialised: its directory is empty", func(t *testing.T) {
		repo, tree, _ := submoduleFixture(t)
		write(t, tree.Path, "wip.txt", "wip\n")

		got, err := tree.Preserve("T-1")
		require.NoError(t, err, "a submodule that is only recorded is not in the way")
		assert.Equal(t, "wip", out(t, repo, "show", got.Dirty+":wip.txt"))
	})

	t.Run("initialised and clean", func(t *testing.T) {
		repo, tree, _ := submoduleFixture(t)
		run(t, tree.Path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "-q")
		require.FileExists(t, filepath.Join(tree.Path, "deps", "sub", "lib.txt"))
		write(t, tree.Path, "wip.txt", "wip\n")

		got, err := tree.Preserve("T-1")
		require.NoError(t, err)
		assert.Equal(t, "wip", out(t, repo, "show", got.Dirty+":wip.txt"))
	})

	t.Run("initialised and clean, and nothing else to keep", func(t *testing.T) {
		_, tree, _ := submoduleFixture(t)
		run(t, tree.Path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "-q")

		got, err := tree.Preserve("T-1")
		require.NoError(t, err)
		assert.True(t, got.Empty())
	})

	t.Run("uncommitted changes in it", func(t *testing.T) {
		repo, tree, _ := submoduleFixture(t)
		run(t, tree.Path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "-q")
		write(t, filepath.Join(tree.Path, "deps", "sub"), "mine.txt", "work in the submodule\n")

		_, err := tree.Preserve("T-1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "deps/sub (it has uncommitted changes)")
		assert.Empty(t, out(t, repo, "for-each-ref", "refs/zmina-preserve-dirty/"))
	})

	t.Run("a commit in it that HEAD does not record", func(t *testing.T) {
		_, tree, _ := submoduleFixture(t)
		run(t, tree.Path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "-q")

		subTree := filepath.Join(tree.Path, "deps", "sub")
		write(t, subTree, "new.txt", "new\n")
		run(t, subTree, "add", ".")
		run(t, subTree, "commit", "-q", "-m", "a commit of the submodule's own")

		_, err := tree.Preserve("T-1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "deps/sub (a nested repository, or a submodule at a commit HEAD does not record)")
	})

	t.Run("a gitlink that HEAD records with a directory that is gone", func(t *testing.T) {
		repo, tree, _ := submoduleFixture(t)
		require.NoError(t, os.RemoveAll(filepath.Join(tree.Path, "deps", "sub")))

		got, err := tree.Preserve("T-1")
		require.NoError(t, err, "the deletion is a change a snapshot holds")
		assert.NotEmpty(t, got.Dirty)
		assert.NotContains(t, out(t, repo, "ls-tree", "-r", got.Dirty), "deps/sub")
	})
}
