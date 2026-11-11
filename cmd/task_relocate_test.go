package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

// moveRepo moves the sweep crew's repository the way a reorganised development directory would, and returns where
// it was (as the task records hold it: git resolves symbolic links) and where it is.
func (sc *sweepCrew) moveRepo(t *testing.T) (old, moved string) {
	t.Helper()

	old, err := filepath.EvalSymlinks(sc.repo)
	require.NoError(t, err)

	parent, err := filepath.EvalSymlinks(filepath.Dir(sc.repo))
	require.NoError(t, err)

	moved = filepath.Join(parent, "moved", "api")
	require.NoError(t, os.MkdirAll(filepath.Dir(moved), 0o700))
	require.NoError(t, os.Rename(sc.repo, moved))

	return old, moved
}

func repoOf(t *testing.T, sc *sweepCrew, id string) string {
	t.Helper()

	c, err := crew.Load(sc.file)
	require.NoError(t, err)

	tk, err := taskStore(c).Get(id)
	require.NoError(t, err)

	return tk.Repo
}

// The tasks of a repository that moved are one line in `task gc`, never a git error for each; the daily sweep says
// the same line, once, in its output and as its event.
func TestGCGroupsTheTasksOfARepositoryThatMoved(t *testing.T) {
	sc := newSweepCrew(t, "worktree_gc: daily\n")
	old, _ := sc.moveRepo(t)

	out, _, err := zm(t, sc.file, "task", "gc", "--dry-run")
	require.NoError(t, err)

	line := "repository moved or gone: " + old + ", 3 closed tasks"

	assert.Equal(t, 1, strings.Count(out, "repository moved or gone"), out)
	assert.Contains(t, out, line)
	assert.NotContains(t, out, "cannot change to", "no git error for each task")
	assert.NotContains(t, out, "T-1: kept", "the tasks are in their repository's line, not listed again")
	assert.Contains(t, out, "zmina task relocate <old-repo-path> <new-repo-path>")
	assert.Contains(t, out, "would remove 0, kept 3")

	for _, id := range []string{"T-1", "T-2", "T-3"} {
		assert.True(t, sc.exists(id), "a worktree that exists is never removed for want of its repository: "+id)
	}

	out, _, err = zm(t, sc.file, "tick")
	require.NoError(t, err)
	assert.Contains(t, out, "worktree sweep: "+line)
	assert.NotContains(t, out, "cannot change to")

	out, _, err = zm(t, sc.file, "log")
	require.NoError(t, err)
	assert.Contains(t, out, "worktree-sweep  by ticker  repository moved or gone: ", "the day's one event (log clips long lines)")
}

// task relocate points the records at the new path, repairs the worktrees' links with git, and lets gc judge the
// tasks again; a dry run changes nothing and a path that is no repository is refused.
func TestRelocateAfterTheRepositoryMoved(t *testing.T) {
	sc := newSweepCrew(t, "")
	old, moved := sc.moveRepo(t)

	// Refused: the new path is not a git repository (or not a path at all); nothing changes.
	_, _, err := zm(t, sc.file, "task", "relocate", old, filepath.Dir(moved))
	require.ErrorContains(t, err, "is not a git repository")

	_, _, err = zm(t, sc.file, "task", "relocate", old)
	require.Error(t, err)

	_, _, err = zm(t, sc.file, "task", "relocate", filepath.Join(filepath.Dir(old), "typo"), moved)
	require.ErrorContains(t, err, "no task record points at")

	// A dry run names every task, the open one too, and changes nothing.
	out, _, err := zm(t, sc.file, "task", "relocate", old, moved, "--dry-run")
	require.NoError(t, err)

	for _, id := range []string{"T-1", "T-2", "T-3", "T-4"} {
		assert.Contains(t, out, id+" · ")
		assert.Equal(t, old, repoOf(t, sc, id), "a dry run changes nothing")
	}

	assert.Contains(t, out, "would relocate 4 task records")
	assert.Contains(t, out, "would run git worktree repair in "+moved)

	_, err = os.Stat(filepath.Join(sc.wt["T-1"], ".git"))
	require.NoError(t, err)

	// The links are broken until it is done: git cannot find the repository from the worktree.
	require.Error(t, exec.Command("git", "-C", sc.wt["T-1"], "status", "--short").Run(),
		"the worktree cannot find its repository")

	out, _, err = zm(t, sc.file, "task", "relocate", old, moved)
	require.NoError(t, err)
	assert.Contains(t, out, "relocated 4 task records from "+old+" to "+moved)
	assert.Contains(t, out, "git worktree repair in "+moved)

	for _, id := range []string{"T-1", "T-2", "T-3", "T-4"} {
		assert.Equal(t, moved, repoOf(t, sc, id), id)
	}

	gitIn(t, sc.wt["T-1"], "status", "--short")
	assert.FileExists(t, filepath.Join(sc.wt["T-3"], "left"), "relocate deletes nothing")

	out, _, err = zm(t, sc.file, "log")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(out, "relocate"), "one event")
	assert.Contains(t, out, "relocate  by hand  4 tasks: ")

	// Nothing points at the old path now.
	_, _, err = zm(t, sc.file, "task", "relocate", old, moved)
	require.ErrorContains(t, err, "no task record points at")

	// And gc judges the tasks as it did before the move: the merged clean one goes, the rest stay.
	out, _, err = zm(t, sc.file, "task", "gc")
	require.NoError(t, err)
	assert.NotContains(t, out, "repository moved or gone")
	assert.Contains(t, out, "T-1: removed")
	assert.Contains(t, out, "T-2: kept: branch")
	assert.Contains(t, out, "T-3: kept: uncommitted")
	assert.Contains(t, out, "removed 1, kept 2")
	assert.False(t, sc.exists("T-1"))
	assert.True(t, sc.exists("T-2"))
	assert.True(t, sc.exists("T-3"))
}

// A task whose repository and worktree directory are both gone is nothing to lose: task gc --drop-gone drops its
// worktree record and counts it; without the flag it only says how many could go.
func TestGCDropGoneDropsTheRecordsOfWhatIsGone(t *testing.T) {
	sc := newSweepCrew(t, "")
	old, _ := sc.moveRepo(t)

	require.NoError(t, os.RemoveAll(sc.wt["T-2"]))

	out, _, err := zm(t, sc.file, "task", "gc", "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "repository moved or gone: "+old+", 3 closed tasks")
	assert.Contains(t, out, "1 of them have no worktree directory left: zmina task gc --drop-gone")

	out, _, err = zm(t, sc.file, "task", "gc", "--drop-gone", "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "T-2: would drop the worktree record of ")
	assert.Contains(t, out, "repository moved or gone: "+old+", 2 closed tasks")
	assert.Contains(t, out, "would remove 0, would drop 1 worktree records, kept 2")

	c, err := crew.Load(sc.file)
	require.NoError(t, err)

	tk, err := taskStore(c).Get("T-2")
	require.NoError(t, err)
	assert.NotEmpty(t, tk.Worktree, "a dry run drops nothing")

	out, _, err = zm(t, sc.file, "task", "gc", "--drop-gone")
	require.NoError(t, err)
	assert.Contains(t, out, "T-2: dropped the worktree record of ")
	assert.Contains(t, out, "removed 0, dropped 1 worktree records, kept 2")

	tk, err = taskStore(c).Get("T-2")
	require.NoError(t, err)
	assert.Empty(t, tk.Worktree)
	assert.Empty(t, tk.Branch)
	assert.Equal(t, old, tk.Repo)

	for _, id := range []string{"T-1", "T-3"} {
		assert.True(t, sc.exists(id), "a directory that exists is never removed: "+id)
	}

	out, _, err = zm(t, sc.file, "log", "--task", "2")
	require.NoError(t, err)
	assert.Contains(t, out, "worktree record dropped")

	out, _, err = zm(t, sc.file, "task", "gc", "--drop-gone")
	require.NoError(t, err)
	assert.Contains(t, out, "repository moved or gone: "+old+", 2 closed tasks", "gone from the count for good")
}
