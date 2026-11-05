package shift

import (
	"errors"
	"time"

	"github.com/iamnikolie/zmina/internal/tmux"
)

// ancestorsMax bounds the walk up a process's parents.
const ancestorsMax = 64

// Own is a restart a role asked for from its own session: the process in its pane then and that start's time, so
// the detached half restarts that very session, never one that replaced it.
type Own struct {
	Pane    int
	Started int64 // the start's time, unix nanoseconds
}

var (
	// ErrOwnGone: the session that asked for its restart was put down, restarted or is being put down meanwhile.
	ErrOwnGone = errors.New("its session ended or is being put down by another command")
	// ErrOwnTurnRuns: the turn that asked, or one after it, still runs.
	ErrOwnTurnRuns = errors.New("its turn still runs")
)

// Inside reports whether a command runs inside the role's own session, so that putting the role down would end the
// command with it: session is the conversation the command runs in (CLAUDE_CODE_SESSION_ID of a claude's tool,
// CODEX_THREAD_ID of a Codex tool, also one the shared App Server daemon runs) and is the role's current one, or
// process pid descends from the process in the role's pane (a runtime that names no session).
func (s *Shift) Inside(name string, pid int, session string) (Own, bool) {
	p, err := s.probe(name)
	if err != nil || p.State != tmux.Up || p.PID <= 1 || !s.State.Role(name).Up {
		return Own{}, false
	}

	own := Own{Pane: p.PID, Started: s.State.Role(name).Started.UnixNano()}

	if session != "" {
		if sess := s.session(name); sess != nil && sess.SessionID == session {
			return own, true
		}
	}

	if s.Parent == nil {
		return Own{}, false
	}

	for i := 0; i < ancestorsMax && pid > 1; i++ {
		if pid == p.PID {
			return own, true
		}

		next, err := s.Parent(pid)
		if err != nil || next == pid {
			break
		}

		pid = next
	}

	return Own{}, false
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
// role is at its prompt (idle, or its turn ended while a background shell runs, its input line empty). gone: that
// session is not the role's any more, or a down of it has begun — the restart is not this one's to finish.
func (s *Shift) OwnTurnOver(name string, own Own) (over, gone bool) {
	p, err := s.probe(name)
	if err != nil {
		return false, false
	}

	rs := s.State.Role(name)
	if p.State != tmux.Up || p.PID != own.Pane || !rs.Up || !rs.Downing.IsZero() || rs.Started.UnixNano() != own.Started {
		return false, true
	}

	return s.atPrompt(name, s.Now()), false
}

// ClaimOwnRestart begins the down of a restart a role asked for from its own session, under the lock the caller
// holds, once that session is still the role's and between turns: so a session that replaced it, a turn that began
// meanwhile, or another restart that claimed it first is never put down by this one. It returns the options the role
// comes back with, whether to wait for it to exit, and the claim FinishOwnRestart checks.
func (s *Shift) ClaimOwnRestart(name string, own Own, fresh, requireContinue, clear bool) (UpOptions, bool, time.Time, error) {
	over, gone := s.OwnTurnOver(name, own)
	if gone {
		return UpOptions{}, false, time.Time{}, ErrOwnGone
	}

	if !over {
		return UpOptions{}, false, time.Time{}, ErrOwnTurnRuns
	}

	opts, err := s.OwnRestartOptions(name, fresh, requireContinue)
	if err != nil {
		return UpOptions{}, false, time.Time{}, err
	}

	wait, err := s.BeginDown(name, DownOptions{Force: true, Clear: clear})
	if err != nil {
		return UpOptions{}, false, time.Time{}, err
	}

	return opts, wait, s.State.Role(name).Downing, nil
}

// FinishOwnRestart ends the down ClaimOwnRestart began and starts the role, in one hold of the lock (the ticker starts
// no role in between), unless another command took the role over meanwhile (its down is not this claim).
func (s *Shift) FinishOwnRestart(name string, claim time.Time, opts UpOptions) error {
	if rs := s.State.Role(name); claim.IsZero() || !rs.Downing.Equal(claim) {
		return ErrOwnGone
	}

	if err := s.FinishDown(name); err != nil {
		return err
	}

	return s.UpWith(name, opts)
}
