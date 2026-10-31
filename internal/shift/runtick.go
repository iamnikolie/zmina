package shift

import "github.com/iamnikolie/zmina/internal/watch"

// WithLock runs fn on a shift holding the state lock and saves after; it is how RunTick takes the lock twice.
type WithLock func(fn func(s *Shift) error) error

// RunTick is one whole beat: the roles under the lock, the watch sources and the wait for released roles to end
// outside it (a slow source never blocks another zmina command), their results under the lock again, and the
// dispatcher's queue typed once. run runs the sources; out receives the lines for the log.
func RunTick(locked WithLock, run func([]WatchRun) []watch.Result, out func([]string)) error {
	var (
		runs     []WatchRun
		released []string
		tm       Tmux
		told     bool
	)

	err := locked(func(s *Shift) error {
		s.By = "ticker"
		out(s.Tick())

		told, released, tm = s.Told, s.Released, s.Tmux
		runs = s.DueWatches()

		return nil
	})
	if err != nil || len(runs)+len(released) == 0 {
		return err
	}

	if len(released) > 0 {
		WaitGone(tm, released, ExitWait)
	}

	var results []watch.Result
	if len(runs) > 0 {
		results = run(runs)
	}

	return locked(func(s *Shift) error {
		s.By = "ticker"

		for _, name := range released {
			// Started again meanwhile (zmina up): that session is not this down's to end.
			if rs := s.State.Role(name); rs.Up || rs.Downing.IsZero() {
				continue
			}

			if err := s.FinishDown(name); err != nil {
				out([]string{"release " + name + ": " + err.Error()})
			}
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
