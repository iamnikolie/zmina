package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

// sweepCrew is a crew with an api repo and four tasks that have worktrees: T-1 done and merged, T-2 done with a
// commit main does not have, T-3 done with changes left in its worktree, T-4 still open.
type sweepCrew struct {
	file, repo string
	wt         map[string]string // worktree of T-1 … T-4
	branch     map[string]string
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()

	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"},
		args...)...).CombinedOutput()
	require.NoError(t, err, string(out))

	return strings.TrimSpace(string(out))
}

// newSweepCrew makes the crew; key is a line of the crew file ("worktree_gc: daily\n", or nothing).
func newSweepCrew(t *testing.T, key string) *sweepCrew {
	t.Helper()

	crewFile := testCrew(t, "  dev:\n")

	raw := mustRead(t, crewFile)
	require.NoError(t, os.WriteFile(crewFile, []byte(strings.Replace(string(raw), "roles:\n", key+"roles:\n", 1)), 0o600))

	repo := filepath.Join(filepath.Dir(crewFile), "api")
	require.NoError(t, os.MkdirAll(repo, 0o700))
	gitIn(t, repo, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a"), []byte("a"), 0o600))
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "a")

	sc := &sweepCrew{file: crewFile, repo: repo, wt: map[string]string{}, branch: map[string]string{}}

	for _, id := range []string{"T-1", "T-2", "T-3", "T-4"} {
		out, _, err := zm(t, crewFile, "task", "new", "dev", "--repo", "api", "--worktree", "work "+id)
		require.NoError(t, err)

		sc.wt[id] = strings.Fields(strings.SplitN(out, "worktree ", 2)[1])[0]
		sc.branch[id] = gitIn(t, sc.wt[id], "rev-parse", "--abbrev-ref", "HEAD")
	}

	commit := func(id string) {
		require.NoError(t, os.WriteFile(filepath.Join(sc.wt[id], "b-"+id), []byte(id), 0o600))
		gitIn(t, sc.wt[id], "add", ".")
		gitIn(t, sc.wt[id], "commit", "-q", "-m", id)
	}

	commit("T-1")
	gitIn(t, repo, "merge", "-q", "--ff-only", sc.branch["T-1"])
	_, _, err := zm(t, crewFile, "task", "done", "T-1")
	require.NoError(t, err)

	commit("T-2")
	_, _, err = zm(t, crewFile, "task", "done", "T-2")
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(sc.wt["T-3"], "left"), []byte("x"), 0o600))
	_, _, err = zm(t, crewFile, "task", "done", "T-3", "--force")
	require.NoError(t, err)

	return sc
}

func (sc *sweepCrew) exists(id string) bool {
	_, err := os.Stat(sc.wt[id])

	return err == nil
}

func (sc *sweepCrew) hasBranch(t *testing.T, id string) bool {
	t.Helper()

	return gitIn(t, sc.repo, "branch", "--list", sc.branch[id]) != ""
}

// sweptAt is when the ticker last began its sweep, from the saved state.
func (sc *sweepCrew) sweptAt(t *testing.T) time.Time {
	t.Helper()

	c, err := crew.Load(sc.file)
	require.NoError(t, err)

	st, err := shift.Read(c.StateDir())
	require.NoError(t, err)

	return st.WorktreeGCAt
}

// setSweptAt writes the time of the last sweep, as an older beat would have left it.
func (sc *sweepCrew) setSweptAt(t *testing.T, at time.Time) {
	t.Helper()

	c, err := crew.Load(sc.file)
	require.NoError(t, err)

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)

	l.WorktreeGCAt = at
	require.NoError(t, l.Save())
}

// With worktree_gc: daily the ticker removes a merged closed task's worktree and branch, and leaves an unmerged one,
// one with changes and an open task's; it does so once, and again only when a day has passed.
func TestTickSweepsMergedClosedWorktreesOnceADay(t *testing.T) {
	sc := newSweepCrew(t, "worktree_gc: daily\n")

	require.True(t, sc.sweptAt(t).IsZero())

	before := time.Now()

	out, _, err := zm(t, sc.file, "tick")
	require.NoError(t, err)
	assert.Contains(t, out, "worktree sweep: removed 1")

	assert.False(t, sc.exists("T-1"), "merged and clean: gone")
	assert.False(t, sc.hasBranch(t, "T-1"))
	assert.True(t, sc.exists("T-2"), "its branch is not merged")
	assert.True(t, sc.hasBranch(t, "T-2"))
	assert.True(t, sc.exists("T-3"), "uncommitted changes")
	assert.True(t, sc.exists("T-4"), "an open task")
	assert.FileExists(t, filepath.Join(sc.wt["T-3"], "left"))

	started := sc.sweptAt(t)
	assert.False(t, started.Before(before.Add(-time.Second)), "the sweep's start is remembered")

	out, _, err = zm(t, sc.file, "log", "--task", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "by ticker  worktree removed: ", "an event for the removal, the beat's own")

	// T-2 is merged now, but the sweep has run today.
	gitIn(t, sc.repo, "merge", "-q", "--no-ff", "-m", "merge T-2", sc.branch["T-2"])

	out, _, err = zm(t, sc.file, "tick")
	require.NoError(t, err)
	assert.NotContains(t, out, "worktree sweep")
	assert.True(t, sc.exists("T-2"), "not swept again within the day")
	assert.True(t, started.Equal(sc.sweptAt(t)), "and the day is still counted from the first sweep")

	sc.setSweptAt(t, time.Now().Add(-23*time.Hour))

	_, _, err = zm(t, sc.file, "tick")
	require.NoError(t, err)
	assert.True(t, sc.exists("T-2"), "23 hours is not a day")

	sc.setSweptAt(t, time.Now().Add(-25*time.Hour))

	out, _, err = zm(t, sc.file, "tick")
	require.NoError(t, err)
	assert.Contains(t, out, "worktree sweep: removed 1")
	assert.False(t, sc.exists("T-2"), "merged by now: gone with the next day's sweep")
	assert.True(t, sc.exists("T-3"), "changes are never swept, however many days")
	assert.True(t, sc.exists("T-4"))
	assert.True(t, sc.sweptAt(t).After(started))
}

// Without the key, or with it off, the ticker sweeps nothing and remembers nothing.
func TestTickSweepsNothingUnlessAsked(t *testing.T) {
	for _, key := range []string{"", "worktree_gc: off\n"} {
		sc := newSweepCrew(t, key)

		out, _, err := zm(t, sc.file, "tick")
		require.NoError(t, err, key)
		assert.NotContains(t, out, "worktree sweep", key)

		assert.True(t, sc.exists("T-1"), "merged and closed, but nobody asked: "+key)
		assert.True(t, sc.sweptAt(t).IsZero(), key)
	}

	_, _, err := zm(t, newSweepCrew(t, "").file, "task", "gc")
	require.NoError(t, err, "plain task gc is as it was")
}

// slowGit puts a git first in PATH that, at its first show-ref, runs slow (a shell command) before the real one;
// the file started appears when it does.
func slowGit(t *testing.T, slow string) (started string) {
	t.Helper()

	real, err := exec.LookPath("git")
	require.NoError(t, err)

	dir := t.TempDir()
	started = filepath.Join(dir, "started")

	script := `#!/bin/sh
if [ "$3" = show-ref ] && [ ! -e '` + started + `' ]; then
  touch '` + started + `'
  ` + slow + `
fi
exec '` + real + `' "$@"
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return started
}

// The sweep holds no state lock: while git is slow in it, every other zmina can take the lock, and the beat has
// already written that it began.
func TestTickHoldsNoStateLockWhileItSweeps(t *testing.T) {
	sc := newSweepCrew(t, "worktree_gc: daily\n")

	c, err := crew.Load(sc.file)
	require.NoError(t, err)

	started := slowGit(t, "sleep 3")

	was := shift.LockWait
	shift.LockWait = time.Second

	t.Cleanup(func() { shift.LockWait = was })

	done := make(chan error, 1)

	go func() {
		_, _, err := zm(t, sc.file, "tick")
		done <- err
	}()

	require.Eventually(t, func() bool { _, err := os.Stat(started); return err == nil }, 20*time.Second,
		20*time.Millisecond, "the beat never began its sweep")

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err, "the beat holds the state lock while it sweeps")
	l.Release()

	assert.False(t, sc.sweptAt(t).IsZero(), "the sweep was claimed in a short locked step before it began")

	select {
	case err := <-done:
		t.Fatalf("the beat ended (%v) before git did: the test proved nothing", err)
	default:
	}

	require.NoError(t, <-done)
	assert.False(t, sc.exists("T-1"), "the sweep went on to the end")
}

// The sweep ends at its deadline even when git leaves a process holding its pipes; what it did not reach waits for
// the next sweep, and the beat does not fail.
func TestTickSweepStopsAtItsDeadline(t *testing.T) {
	sc := newSweepCrew(t, "worktree_gc: daily\n")

	was := worktreeSweepTimeout
	worktreeSweepTimeout = 500 * time.Millisecond

	t.Cleanup(func() { worktreeSweepTimeout = was })

	// A child of git that outlives it holding the pipes, for longer than the test allows the beat to take.
	slowGit(t, "sleep 10")

	begin := time.Now()

	out, _, err := zm(t, sc.file, "tick")
	require.NoError(t, err, "a sweep fails nothing")
	assert.Less(t, time.Since(begin), 8*time.Second, "the deadline held")
	assert.Contains(t, out, "worktree sweep: stopped at its 500ms deadline")
	assert.True(t, sc.exists("T-1"), "cut before it was known to be merged: kept")
	assert.False(t, sc.sweptAt(t).IsZero())

	// Not again today; tomorrow's sweep (git is fast now) does the rest.
	out, _, err = zm(t, sc.file, "tick")
	require.NoError(t, err)
	assert.NotContains(t, out, "worktree sweep")
	assert.True(t, sc.exists("T-1"))

	sc.setSweptAt(t, time.Now().Add(-25*time.Hour))
	worktreeSweepTimeout = time.Minute

	out, _, err = zm(t, sc.file, "tick")
	require.NoError(t, err)
	assert.Contains(t, out, "worktree sweep: removed 1")
	assert.False(t, sc.exists("T-1"))
}

// A sweep that cannot read the tasks says so in the beat's output and fails nothing.
func TestTickSweepFailureIsALineNotAnError(t *testing.T) {
	crewFile := testCrew(t, "")

	raw := mustRead(t, crewFile)
	require.NoError(t, os.WriteFile(crewFile, []byte(strings.Replace(string(raw), "roles:\n", "worktree_gc: daily\nroles:\n", 1)), 0o600))

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	// Where the tasks are is a file: they cannot be listed.
	require.NoError(t, os.MkdirAll(c.StateDir(), 0o700))
	require.NoError(t, os.WriteFile(c.Layout().Tasks(), []byte("not a directory"), 0o600))

	out, _, err := zm(t, crewFile, "tick")
	require.NoError(t, err)
	assert.Contains(t, out, "worktree sweep: task:")
}
