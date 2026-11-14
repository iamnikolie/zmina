package ledger

import (
	"errors"
	"fmt"
	"strings"

	"github.com/iamnikolie/zmina/internal/task"
)

// ErrNoVerdict is returned when a review task would close as done with no verdict: its reviewer says accept or reject.
var ErrNoVerdict = errors.New("is a review and has no verdict")

// ErrVerdict is returned for a verdict that does not fit: not accept or reject, on a task that is no review, or one that
// differs from the verdict the review was closed with.
var ErrVerdict = errors.New("verdict refused")

// closeReview is what closing t as done asks of a review task: its verdict. One closed with none is refused, unless the
// owner or the dispatcher forces it, which comes back as the Unchecked the closing keeps. A task that is no review takes none.
func (l *Ledger) closeReview(t task.Task, c Change) (*task.Unchecked, error) {
	if err := checkVerdict(t, c.Verdict); err != nil {
		return nil, err
	}

	if t.Review == nil {
		return nil, nil //nolint:nilnil // nothing to wait for is not an error
	}

	if c.Verdict == task.VerdictReject && strings.TrimSpace(c.Note) == "" && t.Result == nil {
		return nil, fmt.Errorf("%s: %w: a reject says what is wrong: save a result (--result <json>) or give --note", t.ID, ErrVerdict)
	}

	if c.Verdict != "" {
		return nil, nil //nolint:nilnil // it has its verdict
	}

	run := "zmina task done " + t.ID + " --verdict accept|reject"

	if c.Force {
		if err := l.refuseSetter(c.By, ErrCheckForce); err != nil {
			return nil, fmt.Errorf("%s: %w", t.ID, err)
		}

		return &task.Unchecked{By: c.By, Run: run}, nil
	}

	return nil, fmt.Errorf("%s: %w: close it with --verdict accept or --verdict reject (--force closes it without one, for the "+
		"owner and the dispatcher)", t.ID, ErrNoVerdict)
}

// checkVerdict is the error that says a verdict is no good for t, nil for none and for one that fits.
func checkVerdict(t task.Task, verdict string) error {
	switch {
	case verdict == "":
		return nil
	case verdict != task.VerdictAccept && verdict != task.VerdictReject:
		return fmt.Errorf("%w: --verdict is %s or %s, not %q", ErrVerdict, task.VerdictAccept, task.VerdictReject, verdict)
	case t.Review == nil:
		return fmt.Errorf("%s: %w: --verdict is for a review task, and this is none", t.ID, ErrVerdict)
	}

	return nil
}

// sameVerdict is the error that says a review task already closed as done takes no other verdict than it has: nil for none,
// for the same, and for a task that is no review (which checkVerdict refuses).
func sameVerdict(t task.Task, c Change) error {
	if err := checkVerdict(t, c.Verdict); err != nil || c.Verdict == "" || c.Verdict == t.Review.Verdict {
		return err
	}

	if t.Review.Verdict == "" {
		return fmt.Errorf("%s: %w: it was closed without a verdict", t.ID, ErrVerdict)
	}

	return fmt.Errorf("%s: %w: it was closed with %s already", t.ID, ErrVerdict, t.Review.Verdict)
}

// reviewDue is the review that closing t as done makes due, nil when none is. It is due when the crew has auto_review, a role
// closes the task (not the owner at the keyboard, not the dispatcher), the task has a worktree to read and is no review
// itself, and the rounds of its reviews are not used up.
func (l *Ledger) reviewDue(t task.Task, c Change) (*task.ReviewDue, error) {
	if l.AutoReview == 0 || t.Worktree == "" || t.Review != nil || c.By == Hand || c.By == l.Dispatcher {
		return nil, nil //nolint:nilnil // no review due is not an error
	}

	round, err := l.reviewRound(t.ID)
	if err != nil || round > l.AutoReview {
		return nil, err
	}

	return &task.ReviewDue{Round: round}, nil
}

// reviewRound is the round the next review of task id is: one over the highest round of the reviews made of it, whatever
// became of them, so that no two stamps have the same round and the review a closing is due is found by it. A review taken
// up again from the later list keeps the round it had.
func (l *Ledger) reviewRound(id string) (int, error) {
	all, err := l.Store.All()
	if err != nil {
		return 0, err
	}

	round := 1

	for _, t := range all {
		if r := t.Review; r != nil && r.Of == id {
			round = max(round, r.Round+1)
		}
	}

	return round, nil
}

// reviewPending reports whether t is a done task whose review is yet to be made or still under way: its worktree is what the
// reviewer reads, so a collection leaves it. A review that is closed, with a verdict or without, is no longer waited for.
func (l *Ledger) reviewPending(t task.Task, all map[string]task.Task) bool {
	if l.AutoReview == 0 || t.State != task.Done || t.Closed == nil || t.Closed.Review == nil {
		return false
	}

	if t.Closed.Review.Task == "" {
		return true
	}

	r, ok := all[t.Closed.Review.Task]

	return ok && r.State.Active()
}
