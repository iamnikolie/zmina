package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/iamnikolie/zmina/internal/shift"
)

// worktreeSweepTimeout is the most one sweep may take. What it did not reach waits for the next day's: a sweep that
// keeps hitting a slow disk or a hung git costs the ticker this long once a day, not at every beat.
var worktreeSweepTimeout = time.Minute

// worktreeBeat is the ticker's daily sweep (worktree_gc: daily): what plain `zmina task gc` does, never --force, so
// only closed tasks whose branch is merged into its base and whose worktree is clean lose theirs. It runs after the
// beat's locked phases, like meshTick, and holds no state lock while git works: the lock is taken twice for a moment,
// to see whether the sweep is due and to write that it began. The sweep's own end is not written: a day is counted
// from its start, and a beat that overlaps one that sweeps finds it claimed.
//
// It fails nothing: what goes wrong is a line in the ticker's output and the next sweep is tomorrow's. Each removal
// is an event in the log, as `task gc` makes it.
func (a *app) worktreeBeat(now time.Time) {
	due := false

	// A crew that does not sweep takes no lock for it; a crew file that does not load sweeps nothing.
	if a.readState(func(s *shift.Shift) error {
		due = s.Crew.WorktreeGCOn() && s.State.WorktreeGCDue(now)

		return nil
	}) != nil || !due {
		return
	}

	claimed := false

	err := a.withState(func(s *shift.Shift) error {
		if s.Crew.WorktreeGCOn() && s.State.WorktreeGCDue(now) {
			s.State.WorktreeGCAt, claimed = now, true
		}

		return nil
	})
	if err != nil || !claimed {
		return
	}

	// No lock from here on.
	s, err := a.newShift()
	if err != nil {
		a.printTick([]string{"worktree sweep: " + err.Error()})

		return
	}

	s.By = "ticker"

	ctx, cancel := context.WithTimeout(context.Background(), worktreeSweepTimeout)
	defer cancel()

	got, err := a.ledger(s).GCContext(ctx, false, false)

	removed := 0

	for _, c := range got {
		if c.Removed {
			removed++
		}
	}

	var lines []string

	if removed > 0 {
		lines = append(lines, fmt.Sprintf("worktree sweep: removed %d", removed))
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		lines = append(lines, fmt.Sprintf("worktree sweep: stopped at its %s deadline, the rest waits for the next sweep",
			worktreeSweepTimeout))
	case err != nil:
		lines = append(lines, "worktree sweep: "+err.Error())
	}

	a.printTick(lines)
}
