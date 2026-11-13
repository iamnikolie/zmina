package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/donecheck"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/worktree"
)

// Hand is who runs a command from the owner's shell: the actor name zmina records for it (no $ZMINA_ROLE).
const Hand = "hand"

var (
	// ErrCheckFailed is returned when a task cannot close as done because its check did not pass: the command failed
	// or timed out, it left files behind, or the commit it asks for is not there.
	ErrCheckFailed = errors.New("check failed")
	// ErrCheckEnvironment is returned (as an *EnvFault) when the check could not run at all: the shell did not find
	// the command or could not execute it, did not start, or the command exited with the spec's env_fault_rc. The task
	// stays open: nothing was judged, so nothing failed.
	ErrCheckEnvironment = errors.New("check could not run")
	// ErrCheckInterrupted is returned when the check was stopped before it ended (a signal to zmina): it did not fail,
	// and nothing is known of it.
	ErrCheckInterrupted = errors.New("check interrupted")
	// ErrNotChecked is returned when a task with a check would close as done with no passed check to close it on.
	ErrNotChecked = errors.New("has a check that has not been run")
	// ErrCheckMoved is returned when the task, its worktree or its check changed while the check ran: the result is of
	// what was there before.
	ErrCheckMoved = errors.New("changed while its check ran")
	// ErrCheckSetter is returned when a role sets the check of a task: only the owner and the dispatcher do.
	ErrCheckSetter = errors.New("only the owner or the dispatcher sets a task's check")
	// ErrCheckForce is returned when a role other than the dispatcher forces a close past a check.
	ErrCheckForce = errors.New("only the owner or the dispatcher closes a task past its check")
	// ErrUnswept is returned when a sweep task would close as done while an item it covers has no verdict: it is still
	// parked and nobody kept, closed or took it since the sweep was made (zmina later swept).
	ErrUnswept = errors.New("has parked items without a verdict")
	// ErrInCheck is returned when a zmina that a check started would set a check or force past one.
	ErrInCheck = errors.New("a zmina run by a check acts for no one: it sets no task's check and forces past none")
)

// EnvFault is the refusal of a check that ran into a broken environment (ErrCheckEnvironment): what ran, and what came of
// it. The work was not judged, and the task is as it was.
type EnvFault struct {
	ID     string
	Role   string
	Result donecheck.Result
	// Spec is the check that ran.
	Spec donecheck.Spec
}

func (e *EnvFault) Unwrap() error { return ErrCheckEnvironment }

func (e *EnvFault) Error() string {
	res := e.Result

	return fmt.Sprintf("%s: %s: the check could not run, so the work was not judged: %s (%s, at %s); either the environment is "+
		"broken (a tool the check needs is missing, or the seat it runs on) or a file it runs is missing or not executable in "+
		"the worktree; the task stays open and no failed check is recorded; mend that and run it again\n%s\n--force closes it "+
		"anyway, for the owner and the dispatcher", e.ID, ErrCheckEnvironment, donecheck.Line(e.Spec.Run),
		donecheck.Line(res.Fault()), res.Commit[:min(len(res.Commit), 10)], res.Tail())
}

// MayCheck reports whether who may set a task's check, or close a task past it: the owner and the dispatcher, and
// neither from inside a check. A worker's own check would be a command of its own choosing that its closing runs. who is
// what zmina knows of the caller, $ZMINA_ROLE or the owner's hand, as everywhere in the ledger: a process that does not
// say who it is passes as the owner.
func (l *Ledger) MayCheck(who string) bool {
	return !l.InCheck && (who == Hand || (l.Dispatcher != "" && who == l.Dispatcher))
}

// refuseSetter is the error that says who may not set a check or force past one, nil when who may; no is what is said
// of a caller that is neither the owner nor the dispatcher.
func (l *Ledger) refuseSetter(who string, no error) error {
	switch {
	case l.InCheck:
		return ErrInCheck
	case !l.MayCheck(who):
		return no
	default:
		return nil
	}
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

// Checked is a check that passed, to close the task on: Set closes only the task it was planned against, with the check
// it was planned with.
type Checked struct {
	// ID is the task it ran for, and Updated its Updated and Gen the hand-out it was in when the check was planned.
	ID      string
	Updated time.Time
	Gen     int
	// Spec is the check that ran, and Result is source and commit as well as how it ended.
	Spec   donecheck.Spec
	Result donecheck.Result
}

// PlanCheck says what closing task id as done (the change c) has to run first: nil when it has no check to run, or is
// closed already. It refuses, before a command that takes minutes is run, whatever Set would refuse after it: a change
// that is no good, a worktree with uncommitted changes, a task with criteria and no result, and, when the check asks for
// it, a branch with no commit of its own. It reads the task and git and holds no lock; the run and the close that follows
// are checked against what it read (ErrCheckMoved).
func (l *Ledger) PlanCheck(id string, c Change) (*CheckPlan, error) {
	t, err := l.Store.Get(id)
	if err != nil {
		return nil, err
	}

	// A session that does not hold the task is refused before anything is run for it, as the close would refuse it.
	if c.Caller != nil {
		if st := l.Current(t, *c.Caller); st.Late {
			return nil, st.Err(t.ID)
		}
	}

	spec, source, ok := l.CheckFor(t)
	if !ok || c.Force || c.State != task.Done || t.State == task.Done {
		return nil, nil //nolint:nilnil // no check to run is not an error
	}

	if _, _, err := l.apply(t, c, true); err != nil {
		return nil, err
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

	tr := tree(t)

	if spec.Commit {
		own, err := l.ownCommits(t, tr)
		if err != nil {
			return nil, fmt.Errorf("%s: %w: %w", t.ID, ErrCheckFailed, err)
		}

		if !own {
			return nil, fmt.Errorf("%s: %w: it asks for a commit of the task's own, and %s has none: nothing beyond %s, "+
				"and nothing since the worktree was made", t.ID, ErrCheckFailed, t.Branch, t.Base)
		}
	}

	head, err := l.Trees.Head(tr)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", t.ID, ErrCheckFailed, err)
	}

	return &CheckPlan{Task: t, Spec: spec, Source: source, Head: head}, nil
}

// ownCommits says whether the task's branch has a commit of its own: one beyond its base, or, for a branch whose work its
// base already has, one since the commit the worktree was made at (kept in the task when it was made, else read from the
// branch's reflog). Where that commit is not known at all, a branch its base has is taken as having had commits: nothing
// more can be said of it.
func (l *Ledger) ownCommits(t task.Task, tr worktree.Tree) (bool, error) {
	ahead, err := l.Trees.Ahead(tr)
	if err != nil {
		return false, err
	}

	if ahead > 0 {
		return true, nil
	}

	fork := t.Fork
	if fork == "" {
		fork, _ = l.Trees.Created(tr)
	}

	if fork == "" {
		return true, nil
	}

	since, err := l.Trees.Since(tr, fork)
	if err != nil {
		return true, nil //nolint:nilerr // the commit it was made at is not in the repository: as if not known
	}

	return since > 0, nil
}

// RunCheck runs a plan's check, in its worktree, holding no lock, and says what came of it: the Checked to close the
// task on, or an error that wraps ErrCheckFailed (the end of the output with it), or an *EnvFault (ErrCheckEnvironment)
// when the check could not run, or ErrCheckInterrupted when the run was stopped before it ended. A check that passes and
// leaves files in a clean worktree has failed: the task could not close over them. Either way the run is an event.
func (l *Ledger) RunCheck(ctx context.Context, p *CheckPlan) (*Checked, error) {
	res := donecheck.Run(ctx, p.Spec, p.Task.Worktree, l.Env)
	res.Source, res.Commit = p.Source, p.Head

	short := p.Head[:min(len(p.Head), 10)]
	line := donecheck.Line(p.Spec.Run)
	took := res.Duration().Round(time.Millisecond)

	switch {
	case res.Environment():
		l.log(p.Task.Role, "check", p.Task.ID, fmt.Sprintf("environment fault, the work was not judged: %s (%s, %s after %s, at %s)",
			line, p.Source, donecheck.Line(res.Fault()), took, short))

		return nil, &EnvFault{ID: p.Task.ID, Role: p.Task.Role, Result: res, Spec: p.Spec}
	case res.Interrupted:
		l.log(p.Task.Role, "check", p.Task.ID, fmt.Sprintf("interrupted: %s (%s, after %s, at %s)", line, p.Source, took, short))

		return nil, fmt.Errorf("%s: %w: %s was stopped after %s, before it ended: nothing is known of it, and the task is "+
			"not closed; run it again", p.Task.ID, ErrCheckInterrupted, line, took)
	case !res.Passed():
		why := donecheck.Line(res.Failure(p.Spec.Limit()))

		l.log(p.Task.Role, "check", p.Task.ID, fmt.Sprintf("failed: %s (%s, %s after %s, at %s)", line, p.Source, why, took,
			short))

		return nil, fmt.Errorf("%s: %w: %s (%s after %s, at %s)\n%s\n--force closes it anyway, for the owner and the "+
			"dispatcher", p.Task.ID, ErrCheckFailed, line, why, took, short, res.Tail())
	}

	if err := l.leftFiles(p, line); err != nil {
		return nil, err
	}

	l.log(p.Task.Role, "check", p.Task.ID, fmt.Sprintf("passed: %s (%s, exit 0, %s, at %s)", line, p.Source, took, short))

	return &Checked{ID: p.Task.ID, Updated: p.Task.Updated, Gen: handOutOf(p.Task), Spec: p.Spec, Result: res}, nil
}

// leftFiles refuses a check that passed and left the worktree dirty: it was clean when the check began (PlanCheck refuses
// it otherwise), so it is the check that made the files, and closing would refuse them as uncommitted changes. It names
// the first five, so that they can be removed or ignored.
func (l *Ledger) leftFiles(p *CheckPlan, line string) error {
	dirty, err := l.Trees.Dirty(tree(p.Task))
	if err != nil {
		return fmt.Errorf("%s: %w: it passed, but the worktree cannot be read after it: %w", p.Task.ID, ErrCheckFailed, err)
	}

	if dirty == "" {
		return nil
	}

	files := strings.Split(dirty, "\n")
	shown := files[:min(len(files), 5)]

	for i, f := range shown {
		shown[i] = donecheck.Line(f)
	}

	more := ""
	if len(files) > len(shown) {
		more = fmt.Sprintf(" and %d more", len(files)-len(shown))
	}

	l.log(p.Task.Role, "check", p.Task.ID, fmt.Sprintf("left files: %s (%s, %d in %s)", line, p.Source, len(files), p.Task.Worktree))

	return fmt.Errorf("%s: %w: it passed, but left %d file(s) in the clean worktree %s: %s%s; the task cannot close over "+
		"them: have the check remove them, or git ignore them, and run it again", p.Task.ID, ErrCheckFailed, len(files),
		p.Task.Worktree, strings.Join(shown, ", "), more)
}

// closeCheck is what closing t as done does about its check: nothing for a task with none; for one with a check, the
// closing is forced (Unchecked, for the owner and the dispatcher), or rests on a check that passed on this very task,
// commit and check (Result). A preflight only refuses what is wrong with a force.
func (l *Ledger) closeCheck(t task.Task, c Change, preflight bool) (*donecheck.Result, *task.Unchecked, error) {
	spec, source, ok := l.CheckFor(t)
	if !ok {
		if c.Checked != nil {
			return nil, nil, fmt.Errorf("%s: %w: it has no check now; run it again", t.ID, ErrCheckMoved)
		}

		return nil, nil, nil
	}

	if c.Force {
		if err := l.refuseSetter(c.By, ErrCheckForce); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", t.ID, err)
		}

		return nil, &task.Unchecked{By: c.By, Run: spec.Run}, nil
	}

	if preflight {
		return nil, nil, nil
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

	// A hand-out does not change the task: a check that ran for one is no answer for the next session's.
	if c.Checked.Gen != handOutOf(t) {
		return nil, nil, fmt.Errorf("%s: %w: the task was handed out again (hand-out %d, it ran in %d); run it again", t.ID,
			ErrCheckMoved, handOutOf(t), c.Checked.Gen)
	}

	if c.Checked.Spec != spec || c.Checked.Result.Source != source {
		return nil, nil, fmt.Errorf("%s: %w: its check was changed (it ran %s, %s's; it is now %s, %s's); run it again", t.ID,
			ErrCheckMoved, donecheck.Line(c.Checked.Spec.Run), c.Checked.Result.Source, donecheck.Line(spec.Run), source)
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

// closeSweep is what closing a sweep task as done asks of the parked list: every item it covers is closed, taken or kept
// for this sweep (later.Unswept, the answer of zmina later swept), or the owner or the dispatcher closes it past
// that (c.Force), which comes back as the Unchecked the closing keeps. nil, nil: a task that is no sweep, or no list to ask.
func (l *Ledger) closeSweep(t task.Task, c Change) (*task.Unchecked, error) {
	if len(t.Sweep) == 0 || l.Later == nil {
		return nil, nil //nolint:nilnil // nothing to wait for is not an error
	}

	items, _, err := l.Later.Scan()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.ID, err)
	}

	open := later.Unswept(items, t.Sweep, t.ID)
	if len(open) == 0 {
		return nil, nil //nolint:nilnil // every item has its verdict
	}

	run := "zmina later swept " + t.ID

	if c.Force {
		if err := l.refuseSetter(c.By, ErrCheckForce); err != nil {
			return nil, fmt.Errorf("%s: %w", t.ID, err)
		}

		return &task.Unchecked{By: c.By, Run: run}, nil
	}

	return nil, fmt.Errorf("%s: %w: %d of %d (%s); give each a verdict, %s says which are left (--force closes it anyway, for "+
		"the owner and the dispatcher)", t.ID, ErrUnswept, len(open), len(t.Sweep), strings.Join(open, ", "), run)
}

// cleanCheck is spec as it is kept, once who sets it may, it is a command, and the task has a worktree to run it in.
func (l *Ledger) cleanCheck(spec donecheck.Spec, by string, hasWorktree bool) (*donecheck.Spec, error) {
	if err := l.refuseSetter(by, ErrCheckSetter); err != nil {
		return nil, err
	}

	if !hasWorktree {
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
	}

	if err := l.refuseSetter(c.By, ErrCheckSetter); err != nil {
		return "", fmt.Errorf("%s: %w", t.ID, err)
	}

	// A fault the old check met says nothing of the new one.
	t.CheckEnv, t.CheckEnvTold = nil, nil

	if c.ClearCheck {
		t.Check = nil

		return "check cleared", nil
	}

	spec, err := l.cleanCheck(*c.Check, c.By, t.Worktree != "")
	if err != nil {
		return "", fmt.Errorf("%s: %w", t.ID, err)
	}

	t.Check = spec

	return "check: " + donecheck.Line(spec.Run), nil
}
