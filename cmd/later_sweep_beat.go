package cmd

import (
	"fmt"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// sweepMin is how few stale items the ticker does not make a sweep for.
const sweepMin = 3

// laterSweepBeat is the ticker's automatic sweep (later_sweep in the crew file): a sweep task for the crew's role when no
// sweep is open, the last was made more than every ago, at least sweepMin items are stale, the shift is not stopped and the
// role holds no active task (a scheduled run is skipped on the same terms). Like the daily worktree sweep it runs after the
// beat's locked phases: what is read of the ledger and the parked list is read with no lock, and the lock is taken once, for
// the task itself, which checks again that nobody made a sweep meanwhile. The last sweep is the newest sweep task on the
// ledger, so nothing is kept in state.json. It fails nothing: what goes wrong is a line of the ticker's output.
func (a *app) laterSweepBeat(now time.Time) {
	var (
		cfg   crew.LaterSweep
		items []later.Item
		stale int
	)

	if a.readState(func(s *shift.Shift) error {
		if !s.Crew.LaterSweepOn() || !laterSweepDue(s, *s.Crew.LaterSweep, now) {
			return nil
		}

		cfg = *s.Crew.LaterSweep
		items = laterStale(s.Crew, now, cfg.OlderSpan())

		return nil
	}) != nil || len(items) < sweepMin {
		return
	}

	stale = len(items)
	items = items[:min(len(items), cfg.MaxItems())]

	var line string

	err := a.withState(func(s *shift.Shift) error {
		s.By = "ticker"

		// Nobody else may have made a sweep, or stopped the shift, since it was read.
		if !s.Crew.LaterSweepOn() || !laterSweepDue(s, cfg, now) {
			return nil
		}

		t, pane, err := a.startSweep(s, cfg.Role, items, stale, cfg.OlderSpan())
		if err != nil {
			if t.ID != "" {
				line = fmt.Sprintf("later sweep: %s for %s: %s", t.ID, cfg.Role, err)

				return nil
			}

			return err
		}

		line = fmt.Sprintf("later sweep: %s for %s, %d of %d stale parked items", t.ID, t.Role, len(t.Sweep), stale)
		if pane.State != tmux.Up {
			line += " (" + t.Role + " is down: it is told to the dispatcher if the task stays unaccepted)"
		}

		return nil
	})

	switch {
	case err != nil:
		a.printTick([]string{"later sweep: " + err.Error()})
	case line != "":
		a.printTick([]string{line})
	}
}

// laterSweepDue says whether the ticker may make a sweep now, but for the parked list: the shift runs, no sweep task is
// open, the last one was made more than every ago, and the role holds no active task.
func laterSweepDue(s *shift.Shift, cfg crew.LaterSweep, now time.Time) bool {
	if !s.State.Stopped.IsZero() || s.Crew.IsRetired() {
		return false
	}

	tasks, _ := s.Tasks.All()

	if _, open := later.OpenSweep(tasks); open {
		return false
	}

	if last := later.LastSweep(tasks); !last.IsZero() && now.Sub(last) < cfg.EverySpan() {
		return false
	}

	for _, t := range tasks {
		if t.Role == cfg.Role && t.State.Active() {
			return false
		}
	}

	return true
}
