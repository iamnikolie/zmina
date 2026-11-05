package shift

import "github.com/iamnikolie/zmina/internal/tmux"

// ancestorsMax bounds the walk up a process's parents.
const ancestorsMax = 64

// Inside reports whether process pid runs inside the role's own session — it descends from the process in the role's
// pane, as a shell a claude runs for its tool does — and that process. A restart run there ends the very process that
// runs it: the role's /exit takes its tools' processes with it, so the down never finishes and the up never runs.
func (s *Shift) Inside(name string, pid int) (int, bool) {
	if s.Parent == nil {
		return 0, false
	}

	p, err := s.probe(name)
	if err != nil || p.State != tmux.Up || p.PID <= 1 {
		return 0, false
	}

	for i := 0; i < ancestorsMax && pid > 1; i++ {
		if pid == p.PID {
			return p.PID, true
		}

		next, err := s.Parent(pid)
		if err != nil || next == pid {
			break
		}

		pid = next
	}

	return 0, false
}

// OwnRestartOptions are the options of a restart a role asked for from its own session: it goes on with the
// conversation it is on — a role restarts itself to take a new build or new hooks, never to lose its place — unless
// fresh asks for a new one, whatever its resume policy says. With no conversation to go on with it starts as its
// policy says.
func (s *Shift) OwnRestartOptions(name string, fresh, requireContinue bool) (UpOptions, error) {
	if fresh || requireContinue {
		return s.RestartOptions(name, fresh, requireContinue)
	}

	if o, err := s.RestartOptions(name, false, true); err == nil {
		return o, nil
	}

	return s.RestartOptions(name, false, false)
}

// OwnTurnOver reports, for a restart a role asked for from its own session, whether the turn that asked is over: the
// role is at its prompt (idle, or its turn ended while a background shell runs, its input line empty). gone: the
// process that asked (pane, the pid in the role's pane then) runs there no more — the role was put down or restarted
// meanwhile, and the restart is not this one's to finish.
func (s *Shift) OwnTurnOver(name string, pane int) (over, gone bool) {
	p, err := s.probe(name)
	if err != nil {
		return false, false
	}

	if p.State != tmux.Up || p.PID != pane || !s.State.Role(name).Up {
		return false, true
	}

	return s.atPrompt(name, s.Now()), false
}
