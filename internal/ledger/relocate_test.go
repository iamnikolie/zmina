package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/worktree"
)

// movedTask records a closed task of a repository that is not at its recorded path.
func movedTask(t *testing.T, l *Ledger, repo string, state task.State, worktree bool) task.Task {
	t.Helper()

	tk, err := l.Store.Add(task.Task{Role: "dev", Asked: "x", Repo: repo, Base: "main", State: state}, l.Now())
	require.NoError(t, err)

	tk.Worktree = filepath.Join(l.Worktrees, filepath.Base(repo)+"-"+tk.ID)
	tk.Branch = l.branch(tk.ID)
	require.NoError(t, l.Store.Put(tk))

	if worktree {
		require.NoError(t, os.MkdirAll(tk.Worktree, 0o700))
	}

	return tk
}

// A repository gone from its recorded path is no git error for each of its tasks: each is kept with the same
// reason, and GroupMissing makes one line of them.
func TestGCKeepsTasksOfAMissingRepositoryAndGroupsThem(t *testing.T) {
	l, trees := fixture(t, "")

	gone := filepath.Join(t.TempDir(), "acme")

	// Git is never asked about a repository that is not there: it could only say it cannot change to it.
	l.Trees = failsInRepo{trees, gone}

	a := movedTask(t, l, gone, task.Done, true)
	b := movedTask(t, l, gone, task.Dropped, true)
	c := movedTask(t, l, gone, task.Done, false)
	alive, err := l.New(NewTask{Role: "dev", Asked: "alive", Repo: "api", Worktree: true})
	require.NoError(t, err)
	_, err = l.Set(alive.ID, Change{State: task.Done, Force: true})
	require.NoError(t, err)

	trees.merged[alive.Branch] = true

	got, err := l.GC(false, false)
	require.NoError(t, err)

	rest, missing := GroupMissing(got)

	require.Len(t, rest, 1, "the task of a repository that is there is judged as ever")
	assert.Equal(t, alive.ID, rest[0].ID)
	assert.True(t, rest[0].Removed)

	require.Len(t, missing, 1, "one entry for the repository, not one for each task")
	assert.Equal(t, gone, missing[0].Repo)
	assert.Equal(t, []string{a.ID, b.ID, c.ID}, missing[0].Tasks)
	assert.Equal(t, 1, missing[0].NoTree, "the one whose directory is gone too")
	assert.Equal(t, "repository moved or gone: "+gone+", 3 closed tasks", missing[0].Line())
	assert.Equal(t, "repository moved or gone: /x, 1 closed task", MissingRepo{Repo: "/x", Tasks: []string{"T-1"}}.Line())

	for _, tk := range []task.Task{a, b} {
		assert.DirExists(t, tk.Worktree, "a worktree directory that exists is never touched when its repository is gone")
	}

	for _, c := range got {
		if c.RepoGone != "" {
			assert.Contains(t, c.Kept, "repository moved or gone", "a caller that does not group still reads why")
		}
	}

	assert.Len(t, trees.removed, 1, "only the live repository's task was removed")
}

// failsInRepo is a git that cannot change to one repository's directory.
type failsInRepo struct {
	*fakeTrees
	repo string
}

func (f failsInRepo) Merged(t worktree.Tree) (bool, error) {
	if t.Repo == f.repo {
		return false, errors.New("git show-ref: fatal: cannot change to '" + t.Repo + "'")
	}

	return f.fakeTrees.Merged(t)
}

func (f failsInRepo) Dirty(t worktree.Tree) (string, error) {
	if t.Repo == f.repo {
		return "", errors.New("git status: fatal: cannot change to '" + t.Path + "'")
	}

	return f.fakeTrees.Dirty(t)
}

// A record whose repository and worktree directory are both gone loses its worktree record, when asked: nothing is
// left to lose. A directory that exists, a parked task and an open one never do.
func TestGCDropsTheWorktreeRecordOfWhatIsGoneForGood(t *testing.T) {
	l, trees := fixture(t, "")

	gone := filepath.Join(t.TempDir(), "acme")

	nothing := movedTask(t, l, gone, task.Done, false)
	dropped := movedTask(t, l, gone, task.Dropped, false)
	there := movedTask(t, l, gone, task.Done, true)
	parked := movedTask(t, l, gone, task.Parked, false)
	open := movedTask(t, l, gone, task.Open, false)

	var logged []string

	l.Log = func(_, event, id, detail string) { logged = append(logged, event+" "+id+" "+detail) }

	// Without the option nothing is dropped: the tasks are only grouped.
	got, err := l.GC(false, false)
	require.NoError(t, err)

	_, missing := GroupMissing(got)
	require.Len(t, missing, 1)
	assert.Equal(t, 3, missing[0].NoTree, "three have no directory left, one is parked")
	assert.Empty(t, logged)

	// A dry run says what would go and changes no record.
	got, err = l.GCWith(context.Background(), GCOptions{DryRun: true, DropGone: true})
	require.NoError(t, err)

	rest, missing := GroupMissing(got)
	require.Len(t, rest, 2)

	for _, c := range rest {
		assert.True(t, c.Dropped, c.ID)
	}

	require.Len(t, missing, 1)
	assert.Equal(t, []string{there.ID, parked.ID}, missing[0].Tasks)
	assert.Empty(t, logged)

	after, err := l.Store.Get(nothing.ID)
	require.NoError(t, err)
	assert.Equal(t, nothing.Worktree, after.Worktree, "a dry run drops nothing")

	// The real one.
	got, err = l.GCWith(context.Background(), GCOptions{DropGone: true})
	require.NoError(t, err)

	rest, missing = GroupMissing(got)
	require.Len(t, rest, 2)
	require.Len(t, missing, 1)
	assert.Len(t, logged, 2, "an event for each record dropped")
	assert.Contains(t, logged[0], "worktree record dropped")

	for _, id := range []string{nothing.ID, dropped.ID} {
		tk, err := l.Store.Get(id)
		require.NoError(t, err)
		assert.Empty(t, tk.Worktree, id)
		assert.Empty(t, tk.Branch, id)
		assert.Empty(t, tk.Base, id)
		assert.Equal(t, gone, tk.Repo, "the repository stays: it says what project the task was for")
	}

	for _, id := range []string{there.ID, parked.ID, open.ID} {
		tk, err := l.Store.Get(id)
		require.NoError(t, err)
		assert.NotEmpty(t, tk.Worktree, id)
	}

	assert.DirExists(t, there.Worktree)
	assert.Empty(t, trees.removed)

	// Dropped, they are not found again.
	got, err = l.GCWith(context.Background(), GCOptions{DropGone: true})
	require.NoError(t, err)

	_, missing = GroupMissing(got)
	require.Len(t, missing, 1)
	assert.Equal(t, []string{there.ID, parked.ID}, missing[0].Tasks)
}

func TestRelocatePointsRecordsAtTheNewRepository(t *testing.T) {
	l, trees := fixture(t, "")

	dir := t.TempDir()
	old, moved := filepath.Join(dir, "old", "acme"), filepath.Join(dir, "new", "acme")
	require.NoError(t, os.MkdirAll(moved, 0o700))

	closed := movedTask(t, l, old, task.Done, true)
	open := movedTask(t, l, old+"/", task.Open, false)
	other := movedTask(t, l, filepath.Join(dir, "old", "other"), task.Done, false)
	none, err := l.Store.Add(task.Task{Role: "dev", Asked: "no repo"}, l.Now())
	require.NoError(t, err)

	before, err := l.Store.Get(closed.ID)
	require.NoError(t, err)

	var logged []string

	l.Log = func(role, event, id, detail string) { logged = append(logged, role+"|"+event+"|"+id+"|"+detail) }

	// A dry run says what would change and writes nothing.
	r, err := l.Relocate(old, moved, true)
	require.NoError(t, err)
	require.Len(t, r.Tasks, 2)
	assert.Equal(t, moved, r.Tasks[0].Repo)
	assert.Zero(t, r.Written)
	assert.Empty(t, logged)

	same, err := l.Store.Get(closed.ID)
	require.NoError(t, err)
	assert.Equal(t, old, same.Repo)

	// The real one: closed and open tasks of that repository, one event with the count.
	r, err = l.Relocate(old, moved, false)
	require.NoError(t, err)
	assert.Equal(t, 2, r.Written)
	assert.True(t, r.HasWorktrees())
	assert.Equal(t, []string{"|relocate||2 tasks: " + old + " -> " + moved}, logged)

	for _, id := range []string{closed.ID, open.ID} {
		tk, err := l.Store.Get(id)
		require.NoError(t, err)
		assert.Equal(t, moved, tk.Repo, id)
	}

	tk, err := l.Store.Get(closed.ID)
	require.NoError(t, err)
	assert.Equal(t, before.Updated, tk.Updated, "the time of the last change is not a relocation's to move")
	assert.Equal(t, before.Worktree, tk.Worktree, "only the repository changes")
	assert.Equal(t, before.State, tk.State)

	untouched, err := l.Store.Get(other.ID)
	require.NoError(t, err)
	assert.Equal(t, other.Repo, untouched.Repo, "another repository's task is left alone")

	noRepo, err := l.Store.Get(none.ID)
	require.NoError(t, err)
	assert.Empty(t, noRepo.Repo)

	assert.Empty(t, trees.repaired, "relocate rewrites records; the links are repaired by the caller's separate step")
	assert.Empty(t, trees.removed, "relocate deletes nothing")
	assert.DirExists(t, closed.Worktree)

	// A gc after it judges them in the repository's new place.
	got, err := l.GC(true, false)
	require.NoError(t, err)

	_, missing := GroupMissing(got)
	require.Len(t, missing, 1, "only the other repository's task is still missing its repository")
	assert.Equal(t, filepath.Join(dir, "old", "other"), missing[0].Repo)
}

func TestRelocateRefuses(t *testing.T) {
	l, trees := fixture(t, "")

	dir := t.TempDir()
	old, moved := filepath.Join(dir, "old"), filepath.Join(dir, "new")
	require.NoError(t, os.MkdirAll(moved, 0o700))

	tk := movedTask(t, l, old, task.Done, false)

	// A new path that is not a git repository: nothing is written.
	trees.topErr = errors.New("not in a git repository")
	_, err := l.Relocate(old, moved, false)
	require.ErrorContains(t, err, "is not a git repository")

	got, err := l.Store.Get(tk.ID)
	require.NoError(t, err)
	assert.Equal(t, old, got.Repo)

	// A path inside a repository is not its top.
	trees.topErr = nil
	l.Trees = insideRepo{trees, dir}

	_, err = l.Relocate(old, moved, false)
	require.ErrorContains(t, err, "not its top level")

	l.Trees = trees

	// The same path twice; an old path no task has (the others that are gone are named).
	_, err = l.Relocate(old, old, false)
	require.ErrorContains(t, err, "both the old and the new")

	_, err = l.Relocate(filepath.Join(dir, "typo"), moved, false)
	require.ErrorContains(t, err, "no task record points at "+filepath.Join(dir, "typo"))
	assert.ErrorContains(t, err, old, "the repository paths the records hold that are not there help to find the typo")

	_, err = l.Relocate("", moved, false)
	require.Error(t, err)

	got, err = l.Store.Get(tk.ID)
	require.NoError(t, err)
	assert.Equal(t, old, got.Repo, "nothing was written")
}

// insideRepo answers a TopLevel that is above the path asked.
type insideRepo struct {
	*fakeTrees
	top string
}

func (i insideRepo) TopLevel(string) (string, error) { return i.top, nil }

// A task file that does not read is not looked at, and the count says so: it may be one of the repository's.
func TestRelocateSaysHowManyFilesItCouldNotRead(t *testing.T) {
	l, _ := fixture(t, "")

	dir := t.TempDir()
	old, moved := filepath.Join(dir, "old"), filepath.Join(dir, "new")
	require.NoError(t, os.MkdirAll(moved, 0o700))

	first := movedTask(t, l, old, task.Done, false)
	require.NoError(t, os.WriteFile(filepath.Join(l.Store.Dir, "T-9.json"), []byte("{not json"), 0o600))

	r, err := l.Relocate(old, moved, false)
	require.NoError(t, err, "the file that reads is relocated")
	assert.Equal(t, 1, r.Written)
	assert.Equal(t, 1, r.Unreadable)

	got, err := l.Store.Get(first.ID)
	require.NoError(t, err)
	assert.Equal(t, moved, got.Repo)
}

// A write that fails is an error that says how far it got; the records stay as they were.
func TestRelocateFailsWithAWriteThatFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}

	l, _ := fixture(t, "")

	dir := t.TempDir()
	old, moved := filepath.Join(dir, "old"), filepath.Join(dir, "new")
	require.NoError(t, os.MkdirAll(moved, 0o700))

	tk := movedTask(t, l, old, task.Done, false)

	require.NoError(t, os.Chmod(l.Store.Dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(l.Store.Dir, 0o700) })

	var logged []string

	l.Log = func(_, event, _, _ string) { logged = append(logged, event) }

	_, err := l.Relocate(old, moved, false)
	require.ErrorContains(t, err, "0 of 1 records rewritten")
	assert.Empty(t, logged, "nothing was done, so nothing is logged")

	require.NoError(t, os.Chmod(l.Store.Dir, 0o700))

	got, err := l.Store.Get(tk.ID)
	require.NoError(t, err)
	assert.Equal(t, old, got.Repo)
}
