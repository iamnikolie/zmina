package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// taskBranch and taskRepo are what T-1's file records: the branch zmina made for it and the repository's own path.
func taskBranch(t *testing.T, c *checkCrew) string {
	t.Helper()

	branch, _ := readTaskFile(t, c.file, "T-1")["branch"].(string)
	require.NotEmpty(t, branch)

	return branch
}

func taskRepo(t *testing.T, c *checkCrew) string {
	t.Helper()

	repo, _ := readTaskFile(t, c.file, "T-1")["repo"].(string)
	require.NotEmpty(t, repo)

	return repo
}

// droppedWork is a crew with one dropped task whose worktree holds a commit beyond the base and uncommitted work, tracked and
// untracked and ignored: what a forced collection would throw away.
func droppedWork(t *testing.T) (c *checkCrew, wt, tip string) {
	t.Helper()

	c = newCheckCrew(t, "")
	require.NoError(t, os.WriteFile(filepath.Join(c.repo, ".gitignore"), []byte("*.log\n"), 0o600))
	gitIn(t, c.repo, "add", ".")
	gitIn(t, c.repo, "commit", "-q", "-m", "ignore logs")

	wt = c.task(t)
	tip = commit(t, wt, "b")

	require.NoError(t, os.WriteFile(filepath.Join(wt, "a"), []byte("a changed"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "wip.txt"), []byte("wip"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "noise.log"), []byte("noise"), 0o600))

	_, _, err := zm(t, c.file, "task", "drop", "1", "--force")
	require.NoError(t, err)

	return c, wt, tip
}

// task drop closes the task and leaves its worktree and branch alone: nothing is thrown away there, so nothing is kept.
func TestTaskDropDiscardsNoGitWork(t *testing.T) {
	c, wt, tip := droppedWork(t)

	assert.DirExists(t, wt)
	assert.Equal(t, tip, gitIn(t, c.repo, "rev-parse", taskBranch(t, c)))
	assert.Empty(t, gitIn(t, c.repo, "branch", "--list", "zmina-preserve/*"))
	assert.Equal(t, "a changed", mustReadString(t, filepath.Join(wt, "a")))
}

// task gc --force keeps what it removes: the commits on a zmina-preserve branch, the uncommitted changes (untracked files
// too, not ignored ones) under refs/zmina-preserve-dirty, says where and how to get them back, and only then removes the
// worktree and the branch. The stash stack stays as it was.
func TestTaskGCForceKeepsTheWorkBeforeRemovingIt(t *testing.T) {
	c, wt, tip := droppedWork(t)
	task := taskBranch(t, c)

	out, _, err := zm(t, c.file, "task", "gc", "--force", "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "T-1: would remove ")
	assert.Contains(t, out, "would be kept in git first (ignored files are not)")
	assert.DirExists(t, wt, "a dry run removes nothing")
	assert.Empty(t, gitIn(t, c.repo, "for-each-ref", "refs/zmina-preserve-dirty/"), "and keeps nothing")

	out, _, err = zm(t, c.file, "task", "gc", "--force")
	require.NoError(t, err)

	branch := "zmina-preserve/t-1-" + tip[:8]
	repo := "'" + taskRepo(t, c) + "'"
	assert.Contains(t, out, "T-1: removed ")
	assert.Contains(t, out, "T-1: kept 1 commit(s) beyond the base on branch "+branch+"; restore: git -C "+repo+" worktree add <a new directory> "+branch)
	assert.Contains(t, out, "T-1: kept its uncommitted changes, untracked files included (ignored files are not kept), as refs/zmina-preserve-dirty/t-1-")
	assert.Contains(t, out, "(a commit over "+tip[:8]+"); restore: git -C "+repo+" worktree add --detach <a new directory> refs/zmina-preserve-dirty/t-1-")

	assert.NoDirExists(t, wt)
	assert.Empty(t, gitIn(t, c.repo, "branch", "--list", task))
	assert.Equal(t, tip, gitIn(t, c.repo, "rev-parse", branch), "its commits did not")

	ref := strings.TrimSpace(gitIn(t, c.repo, "for-each-ref", "--format=%(refname)", "refs/zmina-preserve-dirty/"))
	require.True(t, strings.HasPrefix(ref, "refs/zmina-preserve-dirty/t-1-"), ref)
	assert.Equal(t, "a changed", gitIn(t, c.repo, "show", ref+":a"))
	assert.Equal(t, "wip", gitIn(t, c.repo, "show", ref+":wip.txt"))
	assert.NotContains(t, gitIn(t, c.repo, "ls-tree", "-r", "--name-only", ref), "noise.log")
	assert.Empty(t, gitIn(t, c.repo, "stash", "list"), "the shared stash stack")

	log, _, err := zm(t, c.file, "log", "--task", "1")
	require.NoError(t, err)
	assert.Contains(t, log, "work kept before removal: 1 commit(s) on branch "+branch)

	// What was kept comes back with the command it printed.
	restored := filepath.Join(t.TempDir(), "restored")
	gitIn(t, c.repo, "worktree", "add", "--detach", restored, ref)
	assert.Equal(t, "wip", mustReadString(t, filepath.Join(restored, "wip.txt")))
}

// A task whose work cannot be kept is not removed, and the command says how to remove it anyway: --no-preserve, which only
// goes with --force. A collection that is not forced never removes work, kept or not.
func TestTaskGCForceRefusesWhatItCannotKeepUnlessNoPreserve(t *testing.T) {
	c, wt, tip := droppedWork(t)

	// The name the work would be kept under is taken by something else.
	gitIn(t, c.repo, "branch", "zmina-preserve/t-1-"+tip[:8], "main")
	task := taskBranch(t, c)

	out, _, err := zm(t, c.file, "task", "gc", "--force")
	require.NoError(t, err)
	assert.Contains(t, out, "T-1: kept: its work could not be kept first (")
	assert.Contains(t, out, "so nothing was removed; --no-preserve removes it anyway")
	assert.DirExists(t, wt)
	assert.NotEmpty(t, gitIn(t, c.repo, "branch", "--list", task))

	_, _, err = zm(t, c.file, "task", "gc", "--no-preserve")
	require.ErrorContains(t, err, "--no-preserve goes with --force")

	out, _, err = zm(t, c.file, "task", "gc", "--force", "--no-preserve")
	require.NoError(t, err)
	assert.Contains(t, out, "T-1: removed ")
	assert.NotContains(t, out, "T-1: kept")
	assert.NoDirExists(t, wt)
	assert.Empty(t, gitIn(t, c.repo, "for-each-ref", "refs/zmina-preserve-dirty/"), "nothing was kept")
}

// Work that is merged and committed has nothing to keep: a forced collection is what it was, and says nothing of keeping.
func TestTaskGCForceOfCleanMergedWorkKeepsNothing(t *testing.T) {
	c := newCheckCrew(t, "")
	wt := c.task(t)

	_, _, err := zm(t, c.file, "task", "drop", "1")
	require.NoError(t, err)

	out, _, err := zm(t, c.file, "task", "gc", "--force")
	require.NoError(t, err)
	assert.Contains(t, out, "T-1: removed ")
	assert.NotContains(t, out, "T-1: kept")
	assert.NoDirExists(t, wt)
	assert.Empty(t, gitIn(t, c.repo, "for-each-ref", "refs/zmina-preserve-dirty/"))
	assert.Empty(t, gitIn(t, c.repo, "branch", "--list", "zmina-preserve/*"))
}

func mustReadString(t *testing.T, path string) string {
	t.Helper()

	return string(mustRead(t, path))
}

// A worktree with a nested git repository in it cannot be kept in a snapshot: it is not removed, and the line names the path and
// points at --no-preserve; and a user whose git config hides untracked files from git status still has them kept.
func TestTaskGCForceRefusesANestedRepositoryAndKeepsWhatStatusConfigHides(t *testing.T) {
	c := newCheckCrew(t, "")
	wt := c.task(t)

	nested := filepath.Join(wt, "vendor", "lib")
	require.NoError(t, os.MkdirAll(nested, 0o700))
	gitIn(t, nested, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(nested, "f.txt"), []byte("f"), 0o600))
	gitIn(t, nested, "add", ".")
	gitIn(t, nested, "commit", "-q", "-m", "f")

	_, _, err := zm(t, c.file, "task", "drop", "1", "--force")
	require.NoError(t, err)

	out, _, err := zm(t, c.file, "task", "gc", "--force")
	require.NoError(t, err)
	assert.Contains(t, out, "T-1: kept: its work could not be kept first (vendor/lib is a nested git repository or a submodule")
	assert.Contains(t, out, "so nothing was removed; --no-preserve removes it anyway")
	assert.DirExists(t, nested)

	// Without the nested repository, and with git status told to show no untracked file: they are kept all the same.
	require.NoError(t, os.RemoveAll(nested))
	gitIn(t, c.repo, "config", "status.showUntrackedFiles", "no")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "only-untracked.txt"), []byte("work"), 0o600))

	out, _, err = zm(t, c.file, "task", "gc", "--force")
	require.NoError(t, err)
	assert.Contains(t, out, "T-1: kept its uncommitted changes")

	ref := strings.TrimSpace(gitIn(t, c.repo, "for-each-ref", "--format=%(refname)", "refs/zmina-preserve-dirty/"))
	require.NotEmpty(t, ref)
	assert.Equal(t, "work", gitIn(t, c.repo, "show", ref+":only-untracked.txt"))
	assert.NoDirExists(t, wt)
}
