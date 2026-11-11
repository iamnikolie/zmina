package ledger

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/task"
)

// MissingRepo is a repository that is not at the path closed tasks record for it, with those tasks.
type MissingRepo struct {
	Repo  string
	Tasks []string
	// NoTree is how many of them have no worktree directory left either.
	NoTree int
}

// Line is the one plain line GC prints for the repository, whatever the number of its tasks.
func (m MissingRepo) Line() string {
	noun := "closed tasks"
	if len(m.Tasks) == 1 {
		noun = "closed task"
	}

	return fmt.Sprintf("repository moved or gone: %s, %d %s", m.Repo, len(m.Tasks), noun)
}

// GroupMissing splits what GC found: what stands alone, one entry for each task, and the tasks whose repository is
// gone, one entry for each repository (in the order the first of its tasks was met).
func GroupMissing(got []Collected) (rest []Collected, repos []MissingRepo) {
	at := map[string]int{}

	for _, c := range got {
		if c.RepoGone == "" {
			rest = append(rest, c)

			continue
		}

		i, ok := at[c.RepoGone]
		if !ok {
			i = len(repos)
			at[c.RepoGone] = i
			repos = append(repos, MissingRepo{Repo: c.RepoGone})
		}

		repos[i].Tasks = append(repos[i].Tasks, c.ID)

		if c.NoTree {
			repos[i].NoTree++
		}
	}

	return rest, repos
}

// Relocation is what Relocate did, or on a dry run would do.
type Relocation struct {
	// Old and New are the repository's old path, as given, and its new top level.
	Old, New string
	// Tasks are the records that pointed at Old, as they are with New.
	Tasks []task.Task
	// Unreadable is how many task files could not be read: they were not looked at.
	Unreadable int
	// Written is how many records were rewritten; less than len(Tasks) only when a write failed.
	Written int
}

// HasWorktrees reports whether any of the relocated tasks has a worktree of this repository.
func (r Relocation) HasWorktrees() bool {
	for _, t := range r.Tasks {
		if t.Worktree != "" {
			return true
		}
	}

	return false
}

// Relocate points the task records of a repository that moved at its new place: every task, closed or open, whose
// repo is oldRepo gets newRepo, which must be the top level of a git repository. Only the repo field changes (not the
// time the task was last updated, which other rules read), each record written through the task store, which
// replaces a file whole or not at all. It deletes nothing and touches no git state: the worktrees' own links are Trees.Repair's. A dry run writes
// nothing. Callers hold the state lock, as every writer of the tasks does.
func (l *Ledger) Relocate(oldRepo, newRepo string, dryRun bool) (Relocation, error) {
	oldP, err := repoPath(oldRepo)
	if err != nil {
		return Relocation{}, err
	}

	newP, err := repoPath(newRepo)
	if err != nil {
		return Relocation{}, err
	}

	if oldP == newP {
		return Relocation{}, fmt.Errorf("%s is both the old and the new path", oldP)
	}

	top, err := l.Trees.TopLevel(newP)
	if err != nil {
		return Relocation{}, fmt.Errorf("%s is not a git repository", newP)
	}

	if !samePath(top, newP) {
		return Relocation{}, fmt.Errorf("%s is inside the repository %s, not its top level: give %s", newP, top, top)
	}

	all, err := l.Store.All()
	if err != nil {
		return Relocation{}, err
	}

	r := Relocation{Old: oldP, New: top}

	if bad, err := l.Store.Unreadable(); err == nil {
		r.Unreadable = len(bad)
	}

	for _, t := range all {
		if t.Repo != "" && filepath.Clean(t.Repo) == oldP {
			t.Repo = top
			r.Tasks = append(r.Tasks, t)
		}
	}

	if len(r.Tasks) == 0 {
		return r, fmt.Errorf("no task record points at %s%s", oldP, l.otherMissing(all, oldP))
	}

	if dryRun {
		return r, nil
	}

	for _, t := range r.Tasks {
		if err := l.Store.Put(t); err != nil {
			if r.Written > 0 {
				l.log("", "relocate", "", fmt.Sprintf("%d of %d tasks: %s -> %s (stopped: %s)", r.Written, len(r.Tasks),
					oldP, top, err))
			}

			return r, fmt.Errorf("%s: %w (%d of %d records rewritten; run it again for the rest)", t.ID, err,
				r.Written, len(r.Tasks))
		}

		r.Written++
	}

	l.log("", "relocate", "", fmt.Sprintf("%d tasks: %s -> %s", r.Written, oldP, top))

	return r, nil
}

// otherMissing is a hint for an old path that matched nothing: the other repository paths the records hold that
// are not there.
func (l *Ledger) otherMissing(all []task.Task, oldP string) string {
	seen := map[string]bool{}

	var names []string

	for _, t := range all {
		p := filepath.Clean(t.Repo)
		if t.Repo == "" || p == oldP || seen[p] || !missing(p) {
			continue
		}

		seen[p] = true

		names = append(names, p)
	}

	if len(names) == 0 {
		return ""
	}

	sort.Strings(names)

	if len(names) > 5 {
		names = append(names[:5], "...")
	}

	return "; repositories the records hold that are not there: " + strings.Join(names, ", ")
}

// repoPath is a path as the records hold it: absolute and clean. ~ is the home directory.
func repoPath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("a repository path is empty")
	}

	abs, err := filepath.Abs(crew.ExpandHome(p))
	if err != nil {
		return "", err
	}

	return filepath.Clean(abs), nil
}

// samePath reports whether a and b are one place, a symbolic link on either path or not.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}

	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}

	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}

	return ra == rb
}
