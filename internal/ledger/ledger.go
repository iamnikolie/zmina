// Package ledger is what can be done with tasks: record one (with its own git worktree), change it, close it, and
// collect the worktrees of closed tasks. The rules live here, not in the command line: a task with a worktree is
// written before the worktree is made, so a failure leaves a record, never an orphan; a worktree with uncommitted
// work does not close; only merged work is collected.
package ledger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
}

// New records a task. With a worktree, the task is written first — with the worktree's path, branch and base — and
// the worktree made after; if git fails, the task stays, dropped, with the error in its note.
func (l *Ledger) New(n NewTask) (task.Task, error) {
	if n.Worktree && n.Repo == "" {
		return task.Task{}, errors.New("a worktree needs a repo")
	}

	t := task.Task{Role: n.Role, Asked: n.Asked, Note: n.Note, By: n.By, Title: n.Title, Project: n.Project, URL: n.URL}

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

	t, err := l.Store.Add(t, l.Now())
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
	// Force closes a task although its worktree has uncommitted changes.
	Force bool
}

// ErrDirty is returned when a task would close over uncommitted work.
var ErrDirty = errors.New("uncommitted changes")

// Set changes a task. Closing one (done, dropped) whose worktree has uncommitted changes is refused without Force;
// the worktree stays either way until GC.
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

	if len(changes) == 0 {
		return t, errors.New("nothing to change: give a state, --note, --role, --title, --project or --url")
	}

	t.Updated = l.Now()
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
}

// GC removes the worktrees and branches of closed tasks once that loses nothing: the worktree is clean and the
// branch merged into its base. force also removes a dropped task's unmerged branch and changes. Tasks with nothing
// left are not reported.
func (l *Ledger) GC(dryRun, force bool) ([]Collected, error) {
	all, err := l.Store.All()
	if err != nil {
		return nil, err
	}

	var out []Collected

	for _, t := range all {
		if t.State.Active() || t.Branch == "" {
			continue
		}

		if c, ok := l.collect(t, dryRun, force && t.State == task.Dropped); ok {
			out = append(out, c)
		}
	}

	return out, nil
}

func (l *Ledger) collect(t task.Task, dryRun, force bool) (Collected, bool) {
	tr := tree(t)
	c := Collected{ID: t.ID, Detail: t.Worktree + " and branch " + t.Branch}

	merged, err := l.Trees.Merged(tr)
	if err != nil {
		c.Kept = err.Error()

		return c, true
	}

	_, statErr := os.Stat(t.Worktree)
	gone := errors.Is(statErr, os.ErrNotExist)

	if gone && merged {
		// The worktree is gone and the branch merged or gone: at most a merged branch is left.
		if !dryRun {
			if err := l.Trees.Remove(tr, false); err != nil {
				c.Kept = err.Error()

				return c, true
			}
		}

		return c, false
	}

	dirty, err := l.Trees.Dirty(tr)
	if err != nil {
		c.Kept = err.Error()

		return c, true
	}

	switch {
	case dirty != "" && !force:
		c.Kept = "uncommitted changes in " + t.Worktree
	case !merged && !force:
		c.Kept = fmt.Sprintf("branch %s is not merged into %s", t.Branch, t.Base)
	case dryRun:
		c.Removed = true
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

func tree(t task.Task) worktree.Tree {
	return worktree.Tree{Repo: t.Repo, Path: t.Worktree, Branch: t.Branch, Base: t.Base}
}

func (l *Ledger) log(role, event, id, detail string) {
	if l.Log != nil {
		l.Log(role, event, id, detail)
	}
}
