package ledger

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/record"
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
	// onRemove runs after each removal; mergedErr, when set, is what Merged answers.
	onRemove  func()
	mergedErr error
	// repaired lists the repos Repair was asked about; topErr, when set, is what TopLevel answers.
	repaired []string
	topErr   error
	// heads is the commit of each worktree (by path), ahead the commits each branch has beyond its base; a worktree
	// with no entry stands at fakeHead.
	heads map[string]string
	ahead map[string]int
	// since is the commits each branch has since the commit it was made at (by branch), created what the reflog says it
	// was made at; sinceErr, when set, is what Since answers.
	since    map[string]int
	created  map[string]string
	sinceErr error
	// dirtyFn, when set, answers Dirty from the worktree's directory (a check that leaves files there is seen).
	dirtyFn func(path string) string
	// kept is what Preserve answers for a branch, preserveErr the error it answers when set; calls is every Preserve and
	// Remove in the order they were asked, "preserve <branch> as <id>" and "remove <branch>".
	kept        map[string]worktree.Preserved
	preserveErr error
	calls       []string
}

// fakeHead is the commit a fake worktree stands at until a test moves it.
const fakeHead = "0123456789abcdef0123456789abcdef01234567"

func (f *fakeTrees) Head(t worktree.Tree) (string, error) {
	if h, ok := f.heads[t.Path]; ok {
		return h, nil
	}

	return fakeHead, nil
}

func (f *fakeTrees) Ahead(t worktree.Tree) (int, error) { return f.ahead[t.Branch], nil }

func (f *fakeTrees) Since(t worktree.Tree, _ string) (int, error) {
	return f.since[t.Branch], f.sinceErr
}

func (f *fakeTrees) Created(t worktree.Tree) (string, error) { return f.created[t.Branch], nil }

func (f *fakeTrees) TopLevel(dir string) (string, error) {
	if f.topErr != nil {
		return "", f.topErr
	}

	return dir, nil
}

func (f *fakeTrees) Repair(_ context.Context, repo string) (string, error) {
	f.repaired = append(f.repaired, repo)

	return "", nil
}

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

func (f *fakeTrees) Dirty(t worktree.Tree) (string, error) {
	if f.dirtyFn != nil {
		return f.dirtyFn(t.Path), nil
	}

	return f.dirty[t.Path], nil
}
func (f *fakeTrees) Merged(t worktree.Tree) (bool, error) {
	if f.mergedErr != nil {
		return false, f.mergedErr
	}

	return f.merged[t.Branch] || !f.branches[t.Branch], nil
}

func (f *fakeTrees) Preserve(t worktree.Tree, id string) (worktree.Preserved, error) {
	f.calls = append(f.calls, "preserve "+t.Branch+" as "+id)

	return f.kept[t.Branch], f.preserveErr
}

func (f *fakeTrees) Remove(t worktree.Tree, _ bool) error {
	f.calls = append(f.calls, "remove "+t.Branch)
	f.removed = append(f.removed, t.Branch)
	delete(f.branches, t.Branch)

	if f.onRemove != nil {
		defer f.onRemove()
	}

	return os.RemoveAll(t.Path)
}

func fixture(t *testing.T, crew string) (*Ledger, *fakeTrees) {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "api"), 0o700))

	trees := &fakeTrees{dirty: map[string]string{}, merged: map[string]bool{}, branches: map[string]bool{},
		heads: map[string]string{}, ahead: map[string]int{}, since: map[string]int{}, created: map[string]string{}}
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

// A crew that declares nodes: puts the node in the branch: two nodes that push to one remote never meet on a branch, and
// a crew that declares none keeps the names it always had.
func TestNodeIsInTheBranchOfACrewThatDeclaresNodes(t *testing.T) {
	for name, c := range map[string]struct{ crew, node, want string }{
		"a named crew":   {"ops", "alpha", "zmina/alpha-ops-t-1"},
		"the default":    {"", "beta", "zmina/beta-t-1"},
		"no nodes: ":     {"ops", "", "zmina/ops-t-1"},
		"no nodes, none": {"", "", "zmina/t-1"},
	} {
		t.Run(name, func(t *testing.T) {
			l, trees := fixture(t, c.crew)
			l.Node = c.node

			tk, err := l.New(NewTask{Role: "dev", Asked: "x", Repo: "api", Worktree: true})
			require.NoError(t, err)
			assert.Equal(t, c.want, tk.Branch)
			assert.True(t, trees.branches[c.want])
		})
	}
}

// A node's name may hold dots that git would refuse in a branch name: two in a row, one at the end, a trailing .lock. The branch
// is made safe and is a name git accepts (git check-ref-format says so when git is there).
func TestNodeNameIsMadeSafeForAGitBranch(t *testing.T) {
	for node, want := range map[string]string{
		"a..b":      "zmina/a.b-t-1",
		"a...b":     "zmina/a.b-t-1",
		"box.":      "zmina/box-t-1",
		"box..":     "zmina/box-t-1",
		"x.lock":    "zmina/x-t-1",
		"x.lock.":   "zmina/x-t-1",
		"a.b.lock.": "zmina/a.b-t-1",
		"box.local": "zmina/box.local-t-1",
		"alpha":     "zmina/alpha-t-1",
		"a-b_c":     "zmina/a-b_c-t-1",
	} {
		l, _ := fixture(t, "")
		l.Node = node

		got := l.branch("T-1")
		assert.Equal(t, want, got, node)

		if git, err := exec.LookPath("git"); err == nil {
			out, err := exec.Command(git, "check-ref-format", "--branch", got).CombinedOutput()
			assert.NoError(t, err, "%s: %s", got, out)
		}
	}
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

// A sweep under a deadline begins no task and starts no removal once the deadline is past, and hands back what it
// did with the context's error.
func TestGCContextStopsAtItsDeadline(t *testing.T) {
	l, trees := fixture(t, "")

	var made []task.Task

	for _, asked := range []string{"one", "two", "three"} {
		tk, err := l.New(NewTask{Role: "dev", Asked: asked, Repo: "api", Worktree: true})
		require.NoError(t, err)

		trees.merged[tk.Branch] = true
		_, err = l.Set(tk.ID, Change{State: task.Done, Force: true})
		require.NoError(t, err)

		made = append(made, tk)
	}

	// The deadline falls with the first removal.
	ctx, cancel := context.WithCancel(context.Background())
	trees.onRemove = cancel

	got, err := l.GCContext(ctx, false, false)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, got, 1, "the first task was collected, the others were not begun")
	assert.True(t, got[0].Removed)
	assert.Len(t, trees.removed, 1)

	for _, tk := range made[1:] {
		assert.DirExists(t, tk.Worktree, "a task past the deadline keeps its worktree for the next sweep")
	}

	// A deadline already past: nothing is begun.
	got, err = l.GCContext(ctx, false, false)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, got)
	assert.Len(t, trees.removed, 1)

	// Without one the rest goes.
	trees.onRemove = nil

	got, err = l.GCContext(context.Background(), false, false)
	require.NoError(t, err)
	assert.Len(t, got, 2)

	for _, tk := range made {
		assert.NoDirExists(t, tk.Worktree)
	}
}

// What a cut git call said is not what the task is: a read the deadline ended is no reason to report it kept.
func TestGCContextReportsNothingOfACutTask(t *testing.T) {
	l, trees := fixture(t, "")

	tk, err := l.New(NewTask{Role: "dev", Asked: "x", Repo: "api", Worktree: true})
	require.NoError(t, err)
	_, err = l.Set(tk.ID, Change{State: task.Done, Force: true})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	trees.mergedErr = errors.New("git merge-base: stopped: context canceled")

	// The deadline falls while Merged runs.
	l.Trees = cancelOnMerged{trees, cancel}

	got, err := l.GCContext(ctx, false, false)
	require.NoError(t, err, "the cut task was the last one: nothing was left undone")
	assert.Empty(t, got, "not reported as kept")
	assert.Empty(t, trees.removed)

	// The same error with the deadline not passed is a real answer.
	trees.mergedErr = errors.New("git merge-base: bad object")
	l.Trees = trees

	got, err = l.GCContext(context.Background(), false, false)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Kept, "bad object")
}

// cancelOnMerged ends its context while Merged runs.
type cancelOnMerged struct {
	*fakeTrees
	cancel context.CancelFunc
}

func (c cancelOnMerged) Merged(t worktree.Tree) (bool, error) {
	defer c.cancel()

	return c.fakeTrees.Merged(t)
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

func TestNewKeepsWhatTheRoutesSaid(t *testing.T) {
	l, _ := fixture(t, "")

	plain, err := l.New(NewTask{Role: "dev", Asked: "x"})
	require.NoError(t, err)
	assert.Nil(t, plain.Route)

	routed, err := l.New(NewTask{Role: "dev2", Asked: "y", Route: &task.Route{Rule: 6, Name: "pool by limits", Why: "w", Said: "dev", Override: "o"}})
	require.NoError(t, err)

	got, err := l.Store.Get(routed.ID)
	require.NoError(t, err)
	assert.Equal(t, &task.Route{Rule: 6, Name: "pool by limits", Why: "w", Said: "dev", Override: "o"}, got.Route)
}

func TestNewKeepsCriteriaAndRefusesABadOne(t *testing.T) {
	l, _ := fixture(t, "")

	tk, err := l.New(NewTask{Role: "dev", Asked: "x", Criteria: []string{" tests pass ", "docs name the flag"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"tests pass", "docs name the flag"}, tk.Criteria)

	got, err := l.Store.Get(tk.ID)
	require.NoError(t, err)
	assert.Equal(t, tk.Criteria, got.Criteria)

	_, err = l.New(NewTask{Role: "dev", Asked: "x", Criteria: []string{"one\ntwo"}})
	require.ErrorContains(t, err, "one line")
	_, err = l.New(NewTask{Role: "dev", Asked: "x", Criteria: []string{""}})
	require.ErrorContains(t, err, "not empty")

	all, err := l.Store.All()
	require.NoError(t, err)
	assert.Len(t, all, 1, "a refused task leaves no record")
}

func TestSetReplacesAndClearsCriteria(t *testing.T) {
	l, _ := fixture(t, "")

	tk, err := l.New(NewTask{Role: "dev", Asked: "x", Criteria: []string{"a"}})
	require.NoError(t, err)

	next := []string{"b", "c"}
	got, err := l.Set(tk.ID, Change{Criteria: &next})
	require.NoError(t, err)
	assert.Equal(t, []string{"b", "c"}, got.Criteria, "the list is replaced, not added to")

	none := []string{}
	got, err = l.Set(tk.ID, Change{Criteria: &none})
	require.NoError(t, err)
	assert.Empty(t, got.Criteria)

	stored, err := l.Store.Get(tk.ID)
	require.NoError(t, err)
	assert.Empty(t, stored.Criteria)

	_, err = l.Set(tk.ID, Change{})
	assert.ErrorContains(t, err, "nothing to change", "no criteria given is no change")

	bad := []string{"a", ""}
	_, err = l.Set(tk.ID, Change{Criteria: &bad})
	assert.ErrorContains(t, err, "not empty")
}

func TestDoneNeedsAResultWhenTheTaskHasCriteria(t *testing.T) {
	l, _ := fixture(t, "")

	tk, err := l.New(NewTask{Role: "dev", Asked: "x", Criteria: []string{"tests pass"}})
	require.NoError(t, err)

	_, err = l.Set(tk.ID, Change{State: task.Done})
	require.ErrorIs(t, err, ErrNoResult)
	assert.Equal(t, "T-1: has criteria and no saved result: save one with zmina task result T-1 --file <json> "+
		"(--force closes it anyway)", err.Error())

	_, err = l.Set(tk.ID, Change{State: task.Dropped})
	require.NoError(t, err, "dropping gives the work up: no result to show")

	_, err = l.Set(tk.ID, Change{State: task.Open})
	require.NoError(t, err)

	got, err := l.Set(tk.ID, Change{State: task.Done, Force: true})
	require.NoError(t, err)
	assert.Equal(t, task.Done, got.State, "--force closes it anyway")

	_, err = l.Set(tk.ID, Change{State: task.Open})
	require.NoError(t, err)

	stored, err := l.Store.Get(tk.ID)
	require.NoError(t, err)

	stored.Result = &record.Ref{Revision: "abc123", Task: stored.ID}
	require.NoError(t, l.Store.Put(stored))

	got, err = l.Set(tk.ID, Change{State: task.Done})
	require.NoError(t, err)
	assert.Equal(t, task.Done, got.State, "a saved result lets it close")
}

func TestDoneWithoutCriteriaNeedsNoResult(t *testing.T) {
	l, _ := fixture(t, "")

	tk, err := l.New(NewTask{Role: "dev", Asked: "x"})
	require.NoError(t, err)

	got, err := l.Set(tk.ID, Change{State: task.Done})
	require.NoError(t, err)
	assert.Equal(t, task.Done, got.State)
}
