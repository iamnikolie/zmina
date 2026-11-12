package shift

import (
	"slices"

	"github.com/iamnikolie/zmina/internal/watch"
)

// WithLock runs fn on a shift holding the state lock and saves after; it is how RunTick takes the lock twice.
type WithLock func(fn func(s *Shift) error) error

// RunTick is one whole beat: the roles under the lock; the alarms' notify commands and tells, the watch sources
// and the wait for released roles to end outside it (a slow source never blocks another zmina command); their
// results under the lock again, and the dispatcher's queue typed once. run runs the sources, rang does what one
// rung alarm left (it returns a line for the log and how it ended); out receives the lines for the log.
func RunTick(locked WithLock, run func([]WatchRun) []watch.Result, rang func(Rung) (string, error),
	out func([]string)) error {
	var (
		runs     []WatchRun
		released []string
		restarts []Restart
		rung     []Rung
		tm       Tmux
		told     bool
	)

	err := locked(func(s *Shift) error {
		s.By = "ticker"
		out(s.Tick())

		told, released, restarts, rung, tm = s.Told, s.Released, s.Restarts, s.Rang, s.Tmux
		runs = s.DueWatches()

		return nil
	})
	if err != nil {
		return err
	}

	// Each part outside the lock is recorded as soon as it ran, in a lock of its own: an alarm is rung only when it
	// went out, and a beat that dies after a part and before its record repeats that one part (RungDone).
	if rang != nil {
		for _, r := range rung {
			line, err := rang(r)
			if line != "" {
				out([]string{line})
			}

			if lerr := locked(func(s *Shift) error {
				s.By = "ticker"
				out(s.RungDone(r.ID, err))

				return nil
			}); lerr != nil {
				// The lock is not to be had: the parts after this one wait for a beat that gets it, rather than
				// run with nowhere to be recorded.
				out([]string{r.ID + ": not recorded, the state lock was not taken: " + lerr.Error()})

				break
			}
		}
	}

	if len(runs)+len(released)+len(restarts) == 0 {
		return nil
	}

	gone := slices.Clone(released)
	for _, r := range restarts {
		gone = append(gone, r.Name)
	}

	if len(gone) > 0 {
		WaitGone(tm, gone, ExitWait)
	}

	var results []watch.Result
	if len(runs) > 0 {
		results = run(runs)
	}

	return locked(func(s *Shift) error {
		s.By = "ticker"
		s.mark("finish", "")

		for _, name := range released {
			// Started again meanwhile (zmina up): that session is not this down's to end.
			if rs := s.State.Role(name); rs.Up || rs.Downing.IsZero() {
				continue
			}

			if err := s.FinishDown(name); err != nil {
				out([]string{"release " + name + ": " + err.Error()})
			}
		}

		for _, r := range restarts {
			out([]string{s.FinishRestart(r)})
		}

		for i, r := range runs {
			out(s.FinishWatch(r, results[i], false))
		}

		// The first phase typed the queue already if it could; news from the watches goes now, once.
		if !told {
			if line := s.Deliver(); line != "" {
				out([]string{"told the dispatcher: " + line})
			}
		}

		return nil
	})
}
