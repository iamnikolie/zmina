package ledger

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/worktree"
)

// fakeTrees is git without git: worktrees are directories, branches a set.
type fakeTrees struct {
	addErr   error
	dirty    map[string]string
	merged   map[string]bool
	branches map[string]bool
	removed  []string
}

func (f *fakeTrees) TopLevel(dir string) (string, error) { return dir, nil }

func (f *fakeTrees) ResolveBase(_, base string) (string, error) {
	if base == "" {
		return "main", nil
	}

	return base, nil
}

func (f *fakeTrees) Add(_, path, branch, _ string) error {
	if f.addErr != nil {
		return f.addErr
	}

	f.branches[branch] = true

	return os.MkdirAll(path, 0o700)
}

func (f *fakeTrees) Dirty(t worktree.Tree) (string, error) { return f.dirty[t.Path], nil }
func (f *fakeTrees) Merged(t worktree.Tree) (bool, error) {
	return f.merged[t.Branch] || !f.branches[t.Branch], nil
}

func (f *fakeTrees) Remove(t worktree.Tree, _ bool) error {
	f.removed = append(f.removed, t.Branch)
	delete(f.branches, t.Branch)

	return os.RemoveAll(t.Path)
}

func fixture(t *testing.T, crew string) (*Ledger, *fakeTrees) {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "api"), 0o700))

	trees := &fakeTrees{dirty: map[string]string{}, merged: map[string]bool{}, branches: map[string]bool{}}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	return &Ledger{Store: &task.Store{Dir: filepath.Join(dir, "tasks")}, Trees: trees, Dir: dir,
		Worktrees: filepath.Join(dir, "worktrees"), Crew: crew, Now: func() time.Time { return now }}, trees
}

func TestNewWithWorktree(t *testing.T) {
	l, trees := fixture(t, "")

	tk, err := l.New(NewTask{Role: "dev", Asked: "fix the login", Repo: "api", Worktree: true})
	require.NoError(t, err)
	assert.Equal(t, "T-1", tk.ID)
	assert.Equal(t, "zmina/t-1", tk.Branch)
	assert.Equal(t, "main", tk.Base)
	assert.Equal(t, filepath.Join(l.Worktrees, "api-T-1"), tk.Worktree)
	assert.True(t, trees.branches["zmina/t-1"])

	got, err := l.Store.Get("T-1")
	require.NoError(t, err)
	assert.Equal(t, tk.Worktree, got.Worktree, "the ledger knows the worktree")
}

func TestNamedCrewBranches(t *testing.T) {
	l, _ := fixture(t, "ops")

	tk, err := l.New(NewTask{Role: "dev", Asked: "x", Repo: "api", Worktree: true})
	require.NoError(t, err)
	assert.Equal(t, "zmina/ops-t-1", tk.Branch, "two crews never pick the same branch")
}

func TestFailedWorktreeLeavesADroppedRecord(t *testing.T) {
	l, trees := fixture(t, "")
	trees.addErr = errors.New("fatal: a branch named 'zmina/t-1' already exists")

	tk, err := l.New(NewTask{Role: "dev", Asked: "x", Repo: "api", Worktree: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "T-1 recorded as dropped")

	got, err := l.Store.Get("T-1")
	require.NoError(t, err)
	assert.Equal(t, task.Dropped, got.State)
	assert.Contains(t, got.Note, "already exists")
	assert.Equal(t, tk.Worktree, got.Worktree, "where it was meant to be is on record")
}

func TestCloseRefusesUncommittedWork(t *testing.T) {
	l, trees := fixture(t, "")

	tk, err := l.New(NewTask{Role: "dev", Asked: "x", Repo: "api", Worktree: true})
	require.NoError(t, err)

	trees.dirty[tk.Worktree] = "?? b"

	_, err = l.Set("1", Change{State: task.Done})
	require.ErrorIs(t, err, ErrDirty)

	got, err := l.Set("1", Change{State: task.Done, Force: true, Note: "shipped"})
	require.NoError(t, err)
	assert.Equal(t, task.Done, got.State)

	_, err = l.Set("1", Change{})
	assert.Error(t, err, "nothing to change")
}

func TestGCRemovesOnlyMergedCleanWork(t *testing.T) {
	l, trees := fixture(t, "")

	merged, _ := l.New(NewTask{Role: "dev", Asked: "merged", Repo: "api", Worktree: true})
	open, _ := l.New(NewTask{Role: "dev", Asked: "unmerged", Repo: "api", Worktree: true})
	dirty, _ := l.New(NewTask{Role: "dev", Asked: "dirty", Repo: "api", Worktree: true})
	active, _ := l.New(NewTask{Role: "dev", Asked: "still open", Repo: "api", Worktree: true})

	trees.merged[merged.Branch] = true
	trees.merged[dirty.Branch] = true
	trees.dirty[dirty.Worktree] = " M a"

	for _, id := range []string{merged.ID, open.ID, dirty.ID} {
		_, err := l.Set(id, Change{State: task.Done, Force: true})
		require.NoError(t, err)
	}

	got, err := l.GC(false, false)
	require.NoError(t, err)

	byID := map[string]Collected{}
	for _, c := range got {
		byID[c.ID] = c
	}

	assert.True(t, byID[merged.ID].Removed)
	assert.Contains(t, byID[open.ID].Kept, "not merged")
	assert.Contains(t, byID[dirty.ID].Kept, "uncommitted")
	assert.NotContains(t, byID, active.ID, "an active task is never touched")
	assert.Equal(t, []string{merged.Branch}, trees.removed)

	_, err = l.Set(open.ID, Change{State: task.Dropped})
	require.NoError(t, err)

	got, _ = l.GC(true, true)
	require.Len(t, got, 2)
	assert.Equal(t, []string{merged.Branch}, trees.removed, "a dry run removes nothing")

	_, _ = l.GC(false, true)
	assert.Contains(t, trees.removed, open.Branch, "force takes a dropped task's unmerged branch")
	assert.NotContains(t, trees.removed, dirty.Branch, "force never takes a done task's work")
}

// The whole path once against real git.
func TestWithRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}

	dir := t.TempDir()
	repo := filepath.Join(dir, "api")
	require.NoError(t, os.MkdirAll(repo, 0o700))

	run := func(d string, args ...string) {
		t.Helper()

		out, err := exec.Command("git", append([]string{"-C", d, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...).CombinedOutput()
		require.NoError(t, err, string(out))
	}

	run(repo, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a"), []byte("a"), 0o600))
	run(repo, "add", ".")
	run(repo, "commit", "-q", "-m", "a")

	now := time.Now()
	l := &Ledger{Store: &task.Store{Dir: filepath.Join(dir, "tasks")}, Trees: Git{}, Dir: dir,
		Worktrees: filepath.Join(dir, "wt"), Now: func() time.Time { return now }}

	tk, err := l.New(NewTask{Role: "dev", Asked: "x", Repo: "api", Worktree: true})
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(tk.Worktree, "b"), []byte("b"), 0o600))
	_, err = l.Set(tk.ID, Change{State: task.Done})
	require.ErrorIs(t, err, ErrDirty)

	run(tk.Worktree, "add", ".")
	run(tk.Worktree, "commit", "-q", "-m", "b")
	_, err = l.Set(tk.ID, Change{State: task.Done})
	require.NoError(t, err)

	got, err := l.GC(false, false)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Kept, "not merged")

	run(repo, "merge", "-q", "--ff-only", tk.Branch)

	got, err = l.GC(false, false)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, got[0].Removed)

	_, err = os.Stat(tk.Worktree)
	assert.True(t, os.IsNotExist(err))

	// The same branch name again fails cleanly and leaves a dropped record.
	run(repo, "branch", "zmina/t-2")

	_, err = l.New(NewTask{Role: "dev", Asked: "y", Repo: "api", Worktree: true})
	require.Error(t, err)

	t2, _ := l.Store.Get("T-2")
	assert.Equal(t, task.Dropped, t2.State)
}
