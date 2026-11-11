// Package ledger is what can be done with tasks: record one (with its own git worktree), change it, close it, and
// collect the worktrees of closed tasks. The rules live here, not in the command line: a task with a worktree is
// written before the worktree is made, so a failure leaves a record, never an orphan; a worktree with uncommitted
// work does not close; only merged work is collected.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/worktree"
)

// Trees is git: worktrees and branches.
type Trees interface {
	TopLevel(dir string) (string, error)
	ResolveBase(repo, base string) (string, error)
	Add(repo, path, branch, base string) error
	Dirty(t worktree.Tree) (string, error)
	Merged(t worktree.Tree) (bool, error)
	Remove(t worktree.Tree, force bool) error
	// Repair re-points the linked worktrees of repo, which has moved, at it (git worktree repair); it says what git
	// said.
	Repair(ctx context.Context, repo string) (string, error)
}

// Git is Trees over the git command.
type Git struct{}

// TopLevel is worktree.TopLevel.
func (Git) TopLevel(dir string) (string, error) { return worktree.TopLevel(dir) }

// ResolveBase is worktree.ResolveBase.
func (Git) ResolveBase(repo, base string) (string, error) { return worktree.ResolveBase(repo, base) }

// Add is worktree.Add.
func (Git) Add(repo, path, branch, base string) error {
	_, err := worktree.Add(repo, path, branch, base)

	return err
}

// Dirty is Tree.Dirty.
func (Git) Dirty(t worktree.Tree) (string, error) { return t.Dirty() }

// Merged is Tree.Merged.
func (Git) Merged(t worktree.Tree) (bool, error) { return t.Merged() }

// Remove is Tree.Remove.
func (Git) Remove(t worktree.Tree, force bool) error { return t.Remove(force) }

// Repair is worktree.Repair.
func (Git) Repair(ctx context.Context, repo string) (string, error) {
	return worktree.Repair(ctx, repo)
}

// Ledger is one crew's tasks.
type Ledger struct {
	Store *task.Store
	Trees Trees
	// Dir is the crew's directory: repos given by name are under it.
	Dir string
	// Worktrees is where task worktrees go.
	Worktrees string
	// Crew is the crew's name, put in branch names so two crews never pick the same branch; empty for the default
	// crew.
	Crew string
	Now  func() time.Time
	// Log records an event (role, event, task id, detail); nil is quiet.
	Log func(role, event, id, detail string)
}

// NewTask is what a new task is made of.
type NewTask struct {
	Role, Asked, Note, By string
	// Title, Project and URL say what the work is, what it is for and where to see it.
	Title, Project, URL string
	// Repo is a directory under the crew's dir or a path; Worktree gives the task its own worktree of it, from Base
	// (default: the repo's current branch).
	Repo     string
	Worktree bool
	Base     string
	// Budget is what the task may cost (see task.Task.Budget).
	Budget float64
	// Route is what the crew file's routes said for the task, kept in its record; nil when the crew has none.
	Route *task.Route
	// Criteria is what must be true for the task to count as finished (task.Task.Criteria), one line each.
	Criteria []string
}

// New records a task. With a worktree, the task is written first — with the worktree's path, branch and base — and
// the worktree made after; if git fails, the task stays, dropped, with the error in its note.
func (l *Ledger) New(n NewTask) (task.Task, error) {
	if n.Worktree && n.Repo == "" {
		return task.Task{}, errors.New("a worktree needs a repo")
	}

	if n.Budget < 0 {
		return task.Task{}, errors.New("a budget is a positive number of dollars")
	}

	criteria, err := task.CleanCriteria(n.Criteria)
	if err != nil {
		return task.Task{}, err
	}

	t := task.Task{Role: n.Role, Asked: n.Asked, Note: n.Note, By: n.By, Title: n.Title, Project: n.Project, URL: n.URL,
		Budget: n.Budget, Route: n.Route, Criteria: criteria}

	if n.Repo != "" {
		repo, err := l.repo(n.Repo)
		if err != nil {
			return task.Task{}, err
		}

		t.Repo = repo

		if n.Worktree {
			if t.Base, err = l.Trees.ResolveBase(repo, n.Base); err != nil {
				return task.Task{}, err
			}
		}
	}

	t, err = l.Store.Add(t, l.Now())
	if err != nil {
		return t, err
	}

	if n.Worktree {
		t.Worktree = filepath.Join(l.Worktrees, filepath.Base(t.Repo)+"-"+t.ID)
		t.Branch = l.branch(t.ID)

		if err := l.Store.Put(t); err != nil {
			return t, err
		}

		if err := l.Trees.Add(t.Repo, t.Worktree, t.Branch, t.Base); err != nil {
			t.State, t.Note, t.Updated = task.Dropped, "worktree not made: "+err.Error(), l.Now()
			if perr := l.Store.Put(t); perr != nil {
				return t, errors.Join(err, perr)
			}

			l.log(t.Role, "task", t.ID, "dropped: worktree not made: "+err.Error())

			return t, fmt.Errorf("%s recorded as dropped, its worktree could not be made: %w", t.ID, err)
		}
	}

	l.log(t.Role, "task", t.ID, "new: "+t.Asked)

	return t, nil
}

// branch is zmina/t-14 for the default crew and zmina/<crew>-t-14 for a named one.
func (l *Ledger) branch(id string) string {
	if l.Crew == "" {
		return "zmina/" + strings.ToLower(id)
	}

	return "zmina/" + l.Crew + "-" + strings.ToLower(id)
}

// repo resolves a repo given by name (a directory under the crew's dir) or by path to its top level.
func (l *Ledger) repo(repo string) (string, error) {
	p := crew.ExpandHome(repo)
	if !filepath.IsAbs(p) {
		p = filepath.Join(l.Dir, p)
	}

	return l.Trees.TopLevel(p)
}

// Change is what Set may change; empty fields stay.
type Change struct {
	State task.State
	// On is who a waiting task waits on, when not the owner ("-": the owner again). It goes with waiting.
	On   string
	Note string
	Role string
	// Title, Project and URL replace the task's; "-" clears one.
	Title, Project, URL string
	// Budget replaces the task's budget, in dollars; "-" or 0 clears it.
	Budget string
	// Criteria replaces the task's criteria when not nil; an empty list clears them.
	Criteria *[]string
	// Force closes a task although its worktree has uncommitted changes, or although it has criteria and no result.
	Force bool
	// By is who makes the change (a role, "hand"); a task it closes keeps it (Task.Closed).
	By string
}

// ErrDirty is returned when a task would close over uncommitted work.
var ErrDirty = errors.New("uncommitted changes")

// ErrNoResult is returned when a task with criteria would be closed as done with no result saved.
var ErrNoResult = errors.New("has criteria and no saved result")

// Set changes a task. Closing one (done, dropped) whose worktree has uncommitted changes is refused without Force;
// the worktree stays either way until GC. So is closing as done a task that has criteria and no saved result.
func (l *Ledger) Set(id string, c Change) (task.Task, error) {
	t, err := l.Store.Get(id)
	if err != nil {
		return t, err
	}

	var changes []string

	if c.State != "" {
		if !c.State.Active() && t.Worktree != "" && !c.Force {
			dirty, err := l.Trees.Dirty(tree(t))
			if err != nil {
				return t, err
			}

			if dirty != "" {
				return t, fmt.Errorf("%s: %w in %s:\n%s\ncommit them, or --force closes it anyway (the worktree "+
					"stays until 'zmina task gc')", t.ID, ErrDirty, t.Worktree, dirty)
			}
		}

		if c.State == task.Done && len(t.Criteria) > 0 && t.Result == nil && !c.Force {
			return t, fmt.Errorf("%s: %w: save one with zmina task result %s --file <json> (--force closes it anyway)",
				t.ID, ErrNoResult, t.ID)
		}

		t.State = c.State
		changes = append(changes, string(c.State))

		if t.State != task.Waiting {
			t.On = ""
		}
	}

	switch {
	case c.On == "":
		if c.State == task.Waiting {
			t.On = ""
		}
	case t.State != task.Waiting:
		return t, fmt.Errorf("%s: --on goes with waiting (it is %s)", t.ID, t.State)
	case c.On == "-":
		t.On = ""
		changes = append(changes, "on the owner")
	default:
		t.On = c.On
		changes = append(changes, "on "+c.On)
	}

	if c.Role != "" {
		t.Role = c.Role
		changes = append(changes, "role "+c.Role)
	}

	if c.Note != "" {
		t.Note = c.Note
		changes = append(changes, "note: "+c.Note)
	}

	for _, f := range []struct {
		name, value string
		field       *string
	}{{"title", c.Title, &t.Title}, {"project", c.Project, &t.Project}, {"url", c.URL, &t.URL}} {
		switch f.value {
		case "":
		case "-":
			*f.field = ""
			changes = append(changes, f.name+" cleared")
		default:
			*f.field = f.value
			changes = append(changes, f.name+": "+f.value)
		}
	}

	switch c.Budget {
	case "":
	case "-", "0":
		t.Budget = 0
		changes = append(changes, "budget cleared")
	default:
		v, err := strconv.ParseFloat(strings.TrimPrefix(c.Budget, "$"), 64)
		if err != nil || v < 0 {
			return t, fmt.Errorf("budget %q: a number of dollars, or - to clear it", c.Budget)
		}

		t.Budget = v
		changes = append(changes, fmt.Sprintf("budget $%g", v))
	}

	if c.Criteria != nil {
		criteria, err := task.CleanCriteria(*c.Criteria)
		if err != nil {
			return t, err
		}

		t.Criteria = criteria

		if len(criteria) == 0 {
			changes = append(changes, "criteria cleared")
		} else {
			changes = append(changes, fmt.Sprintf("criteria: %d", len(criteria)))
		}
	}

	if len(changes) == 0 {
		return t, errors.New("nothing to change: give a state, --note, --role, --title, --project, --url, --budget or " +
			"--criteria")
	}

	t.Updated = l.Now()

	switch {
	case c.State == "":
	case c.State.Active():
		t.Closed = nil
	case c.By != "":
		t.Closed = &task.Closure{By: c.By, At: t.Updated, State: c.State, Note: c.Note}
	}

	if err := l.Store.Put(t); err != nil {
		return t, err
	}

	l.log(t.Role, "task", t.ID, strings.Join(changes, ", "))

	return t, nil
}

// Collected is what GC did with one closed task's worktree.
type Collected struct {
	ID string
	// Removed is set when the worktree and branch went (or would, on a dry run); Kept says why not.
	Removed bool
	Kept    string
	Detail  string
	// RepoGone is the repository path the task records when nothing is there any more (moved, or deleted); Kept
	// says so. Git cannot judge such a task's work, so it stays; GroupMissing makes one line of every repository.
	RepoGone string
	// NoTree is set with RepoGone when the worktree directory is gone too: nothing is left on disk.
	NoTree bool
	// Dropped is set when the task's worktree record was dropped (or would be, on a dry run): see
	// GCOptions.DropGone.
	Dropped bool
}

// GCOptions is what a collection may do.
type GCOptions struct {
	// DryRun says what would be done and does none of it.
	DryRun bool
	// Force also removes a dropped task's unmerged branch and changes.
	Force bool
	// DropGone drops the worktree record (the task's worktree, branch and base) of a done or dropped task whose
	// repository and worktree directory are both gone: nothing is left to lose. A directory that still exists is
	// never touched, and the record of a task whose directory exists stays whatever its repository.
	DropGone bool
}

// GC removes the worktrees and branches of closed tasks once that loses nothing: the worktree is clean and the
// branch merged into its base. force also removes a dropped task's unmerged branch and changes. Tasks with nothing
// left are not reported.
func (l *Ledger) GC(dryRun, force bool) ([]Collected, error) {
	return l.GCWith(context.Background(), GCOptions{DryRun: dryRun, Force: force})
}

// GCContext is GC under a deadline, for a caller that cannot wait for git (the ticker's daily sweep). Once ctx is
// done no task is begun and no removal started, a git call that only reads is ended, and what was collected so far
// comes back with ctx's error. A removal already under way is let finish (see worktree.Tree.Ctx).
func (l *Ledger) GCContext(ctx context.Context, dryRun, force bool) ([]Collected, error) {
	return l.GCWith(ctx, GCOptions{DryRun: dryRun, Force: force})
}

// GCWith is GCContext with the options of a collection.
func (l *Ledger) GCWith(ctx context.Context, o GCOptions) ([]Collected, error) {
	all, err := l.Store.All()
	if err != nil {
		return nil, err
	}

	var out []Collected

	for _, t := range all {
		if err := ctx.Err(); err != nil {
			return out, err
		}

		if t.State.Active() || t.Branch == "" {
			continue
		}

		each := o
		each.Force = o.Force && t.State == task.Dropped

		if c, ok := l.collect(ctx, t, each); ok {
			out = append(out, c)
		}
	}

	return out, nil
}

// collect reports what became of one closed task's worktree. A task the deadline cut short is not reported (false):
// what git said before it was cut says nothing of the task.
func (l *Ledger) collect(ctx context.Context, t task.Task, o GCOptions) (Collected, bool) {
	dryRun, force := o.DryRun, o.Force
	tr := treeCtx(ctx, t)
	c := Collected{ID: t.ID, Detail: t.Worktree + " and branch " + t.Branch}

	kept := func(err error) (Collected, bool) {
		if ctx.Err() != nil {
			return c, false
		}

		c.Kept = err.Error()

		return c, true
	}

	// A repository that is not where the task records it (moved, deleted) is no git error per task: the answer is
	// the same for all of its tasks, and git would only say it cannot change to the directory.
	if t.Repo != "" && missing(t.Repo) {
		return l.collectMissingRepo(ctx, t, c, o)
	}

	merged, err := l.Trees.Merged(tr)
	if err != nil {
		return kept(err)
	}

	_, statErr := os.Stat(t.Worktree)
	gone := errors.Is(statErr, os.ErrNotExist)

	if gone && merged {
		// The worktree is gone and the branch merged or gone: at most a merged branch is left.
		if !dryRun {
			if ctx.Err() != nil {
				return c, false
			}

			if err := l.Trees.Remove(tr, false); err != nil {
				c.Kept = err.Error()

				return c, true
			}
		}

		return c, false
	}

	dirty, err := l.Trees.Dirty(tr)
	if err != nil {
		return kept(err)
	}

	switch {
	case dirty != "" && !force:
		c.Kept = "uncommitted changes in " + t.Worktree
	case !merged && !force:
		c.Kept = fmt.Sprintf("branch %s is not merged into %s", t.Branch, t.Base)
	case dryRun:
		c.Removed = true
	case ctx.Err() != nil:
		return c, false
	default:
		if err := l.Trees.Remove(tr, force); err != nil {
			c.Kept = err.Error()
		} else {
			c.Removed = true
			l.log(t.Role, "task", t.ID, "worktree removed: "+c.Detail)
		}
	}

	return c, true
}

// missing reports whether nothing is at path (not: it cannot be read).
func missing(path string) bool {
	_, err := os.Stat(path)

	return errors.Is(err, os.ErrNotExist)
}

// collectMissingRepo is collect for a task whose repository is not at its recorded path. Whatever is left of its
// worktree stays: git cannot say whether its branch is merged, and a directory that exists may hold work. Only a
// done or dropped task with no worktree directory either, and only when asked (GCOptions.DropGone), loses its
// worktree record.
func (l *Ledger) collectMissingRepo(ctx context.Context, t task.Task, c Collected, o GCOptions) (Collected, bool) {
	c.RepoGone = t.Repo
	c.Kept = "repository moved or gone: " + t.Repo
	c.NoTree = missing(t.Worktree)

	if !c.NoTree || !o.DropGone || t.State == task.Parked {
		return c, true
	}

	if o.DryRun {
		c.Kept, c.RepoGone, c.NoTree, c.Dropped = "", "", false, true

		return c, true
	}

	if ctx.Err() != nil {
		return c, false
	}

	t.Worktree, t.Branch, t.Base = "", "", ""

	if err := l.Store.Put(t); err != nil {
		// The error is this task's own: it stands alone, not in its repository's line.
		c.Kept, c.RepoGone, c.NoTree = "worktree record not dropped: "+err.Error(), "", false

		return c, true
	}

	c.Kept, c.RepoGone, c.NoTree, c.Dropped = "", "", false, true
	l.log(t.Role, "task", t.ID, "worktree record dropped: "+c.Detail+" (repository "+t.Repo+" and the directory are gone)")

	return c, true
}

func tree(t task.Task) worktree.Tree {
	return worktree.Tree{Repo: t.Repo, Path: t.Worktree, Branch: t.Branch, Base: t.Base}
}

// treeCtx is tree whose reading git calls end with ctx.
func treeCtx(ctx context.Context, t task.Task) worktree.Tree {
	tr := tree(t)
	tr.Ctx = ctx

	return tr
}

func (l *Ledger) log(role, event, id, detail string) {
	if l.Log != nil {
		l.Log(role, event, id, detail)
	}
}
