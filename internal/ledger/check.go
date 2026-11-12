package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/donecheck"
	"github.com/iamnikolie/zmina/internal/task"
)

// Hand is who runs a command from the owner's shell: the actor name zmina records for it (no $ZMINA_ROLE).
const Hand = "hand"

var (
	// ErrCheckFailed is returned when a task cannot close as done because its check did not pass: the command failed
	// or timed out, or the commit it asks for is not there.
	ErrCheckFailed = errors.New("check failed")
	// ErrNotChecked is returned when a task with a check would close as done with no passed check to close it on.
	ErrNotChecked = errors.New("has a check that has not been run")
	// ErrCheckMoved is returned when the task or its worktree changed while its check ran: the result is of what was
	// there before.
	ErrCheckMoved = errors.New("changed while its check ran")
	// ErrCheckSetter is returned when a role sets the check of a task: only the owner and the dispatcher do.
	ErrCheckSetter = errors.New("only the owner or the dispatcher sets a task's check")
	// ErrCheckForce is returned when a role other than the dispatcher forces a close past a check.
	ErrCheckForce = errors.New("only the owner or the dispatcher closes a task past its check")
)

// MayCheck reports whether who may set a task's check, or close a task past it: the owner and the dispatcher. A
// worker's own check would be a command of its own choosing that its closing runs. who is what zmina knows of the
// caller, $ZMINA_ROLE or the owner's hand, as everywhere in the ledger: a process that does not say who it is passes as
// the owner.
func (l *Ledger) MayCheck(who string) bool {
	return who == Hand || (l.Dispatcher != "" && who == l.Dispatcher)
}

// CheckFor is the check that closing t as done runs, and whose it is (donecheck.FromTask, FromCrew): the task's own,
// else the crew's done_check for a task that has a worktree. ok is false for a task with none.
func (l *Ledger) CheckFor(t task.Task) (spec donecheck.Spec, source string, ok bool) {
	switch {
	case t.Check != nil:
		return *t.Check, donecheck.FromTask, true
	case l.DoneCheck != nil && t.Worktree != "":
		return *l.DoneCheck, donecheck.FromCrew, true
	default:
		return donecheck.Spec{}, "", false
	}
}

// CheckPlan is a check ready to run, made without the state lock: the task as it was read, and the commit its worktree
// stood at.
type CheckPlan struct {
	Task   task.Task
	Spec   donecheck.Spec
	Source string
	Head   string
}

// Checked is a check that passed, to close the task on: Set closes only the task it was planned against.
type Checked struct {
	// ID is the task it ran for, and Updated its Updated when the check was planned.
	ID      string
	Updated time.Time
	Result  donecheck.Result
}

// PlanCheck says what closing task id as done has to run first: nil when it has no check. It refuses a worktree with
// uncommitted changes (the check is of a commit, and closing refuses them anyway) and, when the check asks for it, a
// branch with no commit beyond its base, so a command that takes minutes is not run for nothing. It reads the task and
// git and holds no lock; the run and the close that follows are checked against what it read (ErrCheckMoved).
func (l *Ledger) PlanCheck(id string) (*CheckPlan, error) {
	t, err := l.Store.Get(id)
	if err != nil {
		return nil, err
	}

	spec, source, ok := l.CheckFor(t)
	if !ok {
		return nil, nil //nolint:nilnil // no check is not an error
	}

	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", t.ID, err)
	}

	if t.Worktree == "" {
		return nil, fmt.Errorf("%s: %w: it has no worktree to run it in", t.ID, ErrCheckFailed)
	}

	if _, err := os.Stat(t.Worktree); err != nil {
		return nil, fmt.Errorf("%s: %w: its worktree %s cannot be read: %w", t.ID, ErrCheckFailed, t.Worktree, err)
	}

	if err := l.refuseDirty(t); err != nil {
		return nil, err
	}

	// What the close would refuse anyway is refused before a command that takes minutes is run.
	if err := refuseNoResult(t); err != nil {
		return nil, err
	}

	tr := tree(t)

	if spec.Commit {
		n, err := l.Trees.Ahead(tr)
		if err != nil {
			return nil, fmt.Errorf("%s: %w: %w", t.ID, ErrCheckFailed, err)
		}

		if n == 0 {
			return nil, fmt.Errorf("%s: %w: it asks for a commit on %s beyond %s, and there is none", t.ID,
				ErrCheckFailed, t.Branch, t.Base)
		}
	}

	head, err := l.Trees.Head(tr)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", t.ID, ErrCheckFailed, err)
	}

	return &CheckPlan{Task: t, Spec: spec, Source: source, Head: head}, nil
}

// RunCheck runs a plan's check, in its worktree, holding no lock, and says what came of it: the Checked to close the
// task on, or an error that wraps ErrCheckFailed and carries the end of the output. Either way the run is an event.
func (l *Ledger) RunCheck(ctx context.Context, p *CheckPlan) (*Checked, error) {
	res := donecheck.Run(ctx, p.Spec, p.Task.Worktree, l.Env)
	res.Source, res.Commit = p.Source, p.Head

	short := p.Head[:min(len(p.Head), 10)]

	if res.Passed() {
		l.log(p.Task.Role, "check", p.Task.ID, fmt.Sprintf("passed: %s (%s, exit 0, %s, at %s)", oneLine(p.Spec.Run),
			p.Source, res.Duration().Round(time.Millisecond), short))

		return &Checked{ID: p.Task.ID, Updated: p.Task.Updated, Result: res}, nil
	}

	why := res.Failure(p.Spec.Limit())

	l.log(p.Task.Role, "check", p.Task.ID, fmt.Sprintf("failed: %s (%s, %s after %s, at %s)", oneLine(p.Spec.Run),
		p.Source, why, res.Duration().Round(time.Millisecond), short))

	return nil, fmt.Errorf("%s: %w: %s (%s after %s, at %s)\n%s\n--force closes it anyway, for the owner and the "+
		"dispatcher", p.Task.ID, ErrCheckFailed, oneLine(p.Spec.Run), why, res.Duration().Round(time.Millisecond), short,
		res.Tail())
}

// closeCheck is what closing t as done does about its check: nothing for a task with none; for one with a check, the
// closing is forced (Unchecked, for the owner and the dispatcher), or rests on a check that passed on this very task
// and commit (Result).
func (l *Ledger) closeCheck(t task.Task, c Change) (*donecheck.Result, *task.Unchecked, error) {
	spec, _, ok := l.CheckFor(t)
	if !ok {
		return nil, nil, nil
	}

	if c.Force {
		if !l.MayCheck(c.By) {
			return nil, nil, fmt.Errorf("%s: %w: ask the dispatcher", t.ID, ErrCheckForce)
		}

		return nil, &task.Unchecked{By: c.By, Run: spec.Run}, nil
	}

	if c.Checked == nil || !c.Checked.Result.Passed() {
		return nil, nil, fmt.Errorf("%s: %w: run zmina task done %s, which runs it", t.ID, ErrNotChecked, t.ID)
	}

	if c.Checked.ID != t.ID {
		return nil, nil, fmt.Errorf("%s: %w: the check that passed was of %s", t.ID, ErrCheckMoved, c.Checked.ID)
	}

	if !c.Checked.Updated.Equal(t.Updated) {
		return nil, nil, fmt.Errorf("%s: %w: the task was changed; run it again", t.ID, ErrCheckMoved)
	}

	head, err := l.Trees.Head(tree(t))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", t.ID, err)
	}

	if head != c.Checked.Result.Commit {
		return nil, nil, fmt.Errorf("%s: %w: its worktree moved from %s to %s; run it again", t.ID, ErrCheckMoved,
			c.Checked.Result.Commit[:min(len(c.Checked.Result.Commit), 10)], head[:min(len(head), 10)])
	}

	res := c.Checked.Result

	return &res, nil, nil
}

// cleanCheck is spec as it is kept, once who sets it may, it is a command, and the task has a worktree to run it in.
func (l *Ledger) cleanCheck(spec donecheck.Spec, by string, hasWorktree bool) (*donecheck.Spec, error) {
	switch {
	case !l.MayCheck(by):
		return nil, ErrCheckSetter
	case !hasWorktree:
		return nil, errors.New("a check runs in the task's worktree, and the task has none")
	}

	if err := spec.Validate(); err != nil {
		return nil, err
	}

	spec.Run = strings.TrimSpace(spec.Run)

	return &spec, nil
}

// setCheck changes t's check as c says and returns what to log of it; "" when c leaves it.
func (l *Ledger) setCheck(t *task.Task, c Change) (string, error) {
	switch {
	case c.Check == nil && !c.ClearCheck:
		return "", nil
	case c.Check != nil && c.ClearCheck:
		return "", errors.New("a check is set or cleared, not both")
	case c.State != "" && !c.State.Active():
		return "", fmt.Errorf("%s: set the check before closing the task, not with it", t.ID)
	case !l.MayCheck(c.By):
		return "", fmt.Errorf("%s: %w", t.ID, ErrCheckSetter)
	case c.ClearCheck:
		t.Check = nil

		return "check cleared", nil
	}

	spec, err := l.cleanCheck(*c.Check, c.By, t.Worktree != "")
	if err != nil {
		return "", fmt.Errorf("%s: %w", t.ID, err)
	}

	t.Check = spec

	return "check: " + oneLine(spec.Run), nil
}

// oneLine is s on one line, cut, for an event.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 80 {
		return string(r[:79]) + "…"
	}

	return s
}
