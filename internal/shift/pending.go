package shift

import (
	"fmt"
	"time"
)

// Kinds of a pending down.
const (
	pendingRestart = "restart"
	pendingRelease = "release"
	// pendingAbort is a zmina rotation abort between its two holds of the lock: Notes is the conversation it goes back to, Dir
	// where it ran.
	pendingAbort = "abort"
)

// PendingDown is what a begun restart or release still has to do once the role has ended: its claim (the Downing it
// set) and, for a restart, how to start the role again and what to hand it.
type PendingDown struct {
	Kind  string    `json:"kind"`
	Claim time.Time `json:"claim"`
	// Dir is where a restart starts the role fresh; Tasks, Why, Notes and Done what it is handed (Restart).
	Dir   string   `json:"dir,omitempty"`
	Tasks []string `json:"tasks,omitempty"`
	Why   string   `json:"why,omitempty"`
	Notes string   `json:"notes,omitempty"`
	Done  string   `json:"done,omitempty"`
}

// pendingOf is a restart job as the state keeps it.
func pendingOf(job Restart) *PendingDown {
	return &PendingDown{Kind: pendingRestart, Claim: job.Claim, Dir: job.Opts.Dir, Tasks: job.Tasks, Why: job.Why,
		Notes: job.Notes, Done: job.Done}
}

// restartOf is the restart job a pending restart saved.
func (p *PendingDown) restartOf(name string) Restart {
	return Restart{Name: name, Claim: p.Claim, Opts: UpOptions{Fresh: true, Dir: p.Dir}, Tasks: p.Tasks, Why: p.Why,
		Notes: p.Notes, Done: p.Done}
}

// staleDown is how long a down with nothing saved to finish it (a zmina down or sleep whose process died during the
// wait) may stay begun before the ticker stops treating the role as going down and tells the dispatcher.
const staleDown = 2 * downingFor

// finishStale ends a down its own process never finished. A restart or release the ticker began is finished from
// what it saved, once its beat had time to finish it itself; any other down stops muting the role's notes after
// staleDown, and the dispatcher is told the role stayed down. It returns a line for the log, empty when it did
// nothing.
func (s *Shift) finishStale(name string, rs *RoleState, now time.Time) string {
	if p := rs.Pending; p != nil && p.Claim.Equal(rs.Downing) {
		switch p.Kind {
		case pendingRestart:
			s.log(name, "restart-late", "", "its beat ended before it finished; finishing it now")

			return s.FinishRestart(p.restartOf(name))
		case pendingAbort:
			// The command that began the abort died during its wait. The role is put down for good and left not held, so that a
			// keep: always role is opened again by the ticker as after a crash; the dispatcher hears it once.
			if err := s.FinishDown(name); err != nil {
				return "abort " + name + ": " + err.Error()
			}

			rs.Held = false
			s.log(name, "rotation-abort-late", "", p.Why+": its command ended before it finished; the role is down and not held")
			s.addNotes([]string{fmt.Sprintf("%s: %s ended before it finished (its command died during the wait): the role is down and "+
				"not held, a keep: always role is opened again by the ticker; zmina --crew %s rotation abort %s again, or %s", name, p.Why,
				shellWord(s.Crew.Path), shellWord(name), s.resumeCommand(name, p.Notes, false))})

			return name + ": abort finished after its command ended"
		case pendingRelease:
			if err := s.FinishDown(name); err != nil {
				return "release " + name + ": " + err.Error()
			}

			s.log(name, "released-late", "", "its beat ended before it finished the release")

			return name + ": release finished after its beat ended"
		}
	}

	if now.Sub(rs.Downing) < staleDown {
		return ""
	}

	rs.Downing, rs.Pending = time.Time{}, nil
	s.log(name, "down-stale", "", "a down that its command never finished")
	s.addNotes([]string{name + ": went down halfway (the command that put it down ended during the wait) and stays " +
		"down; its tasks: zmina task ls --role " + name + "; start it with zmina up " + name})

	return name + ": a down left halfway; told the dispatcher"
}
