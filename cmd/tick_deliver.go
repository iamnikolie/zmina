package cmd

import (
	"errors"
	"fmt"

	"github.com/iamnikolie/zmina/internal/shift"
)

// deliverPass is zmina tick --deliver: the beat's delivery step alone, for the lines that wait for a role, cheap enough
// to run many times a minute. It never waits: a lock another zmina holds (the beat, a command) ends it at once, with
// nothing read, written or logged. Under the lock it reads the state; when no role has anything a pass could give
// (Shift.Waiting) it lets go without a tmux call or a save. Otherwise it runs the pass and saves only a state that
// changed. It starts no role and runs none of the beat's other work: no watches, no mesh round, no daily chores, no
// version note. It logs no event of its own: a line given is logged by the delivery as in the beat.
//
// A caller that repeats the pass must back off when it exits non-zero: a state that could not be saved after a line
// went is read again, as it was, by the next pass, which gives the same line again, and so on at every pass.
func (a *app) deliverPass() error {
	s, err := a.newShift()
	if err != nil {
		return err
	}

	l, err := shift.TryLockAs(s.Crew.StateDir(), shift.LockOpts{Verb: a.verb + " --deliver", Log: s.LogEvent})
	if errors.Is(err, shift.ErrLockHeld) {
		return nil
	}

	if err != nil {
		return err
	}

	// withState also sets CommittedMessageStages and calls PruneWatches; the pass leaves both to the beat on purpose:
	// nothing it delivers reads the first, and the second is the beat's trim. A delivery path that starts reading
	// CommittedMessageStages must set it here too.
	s.State, s.Flush, s.Mark, s.By = l.State, l.Flush, l.Mark, "ticker"

	changed, err := s.DeliverPass()
	if err != nil || !changed {
		l.Release()

		return err
	}

	if err := l.Save(); err != nil {
		return fmt.Errorf("not saved: %w", err)
	}

	return nil
}
