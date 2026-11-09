package shift

import (
	"time"
)

// Kinds of a pending down.
const (
	pendingRestart = "restart"
	pendingRelease = "release"
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
