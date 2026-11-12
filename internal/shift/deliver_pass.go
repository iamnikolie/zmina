package shift

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Waiting names the roles that have something DeliverTo could give now, in the crew's order (the dispatcher first):
// a role up under zmina, not just given a slash command (slashSettle), with a line queued for it that did not fail
// less than countedGap ago; the dispatcher also with a signal or a note waiting, bar info lines that still ride
// (infoRide), and not one a post or a typed line failed to less than retryGap ago. It reads the loaded state and the
// clock and nothing else: no tmux, no registry, no inbox socket, no task file, and it makes no state of a role that
// has none. A role it names may still not take its line (busy, a draft, a dialog): that is for the pass to find out; a
// role it leaves out has nothing a pass could give.
func (s *Shift) Waiting() []string {
	now := s.Now()

	var out []string

	for _, name := range s.Names() {
		rs := s.State.Roles[name]
		if rs == nil || !rs.Up || rs.settling(now) {
			continue
		}

		if name == s.Crew.Dispatcher {
			if !rs.dispatchTriedLately(now) && s.dispatcherWaits(rs, now) {
				out = append(out, name)
			}

			continue
		}

		if len(rs.Inbox) > 0 && !rs.triedLately(now) {
			out = append(out, name)
		}
	}

	return out
}

// dispatcherWaits reports whether deliver has something for the dispatcher: its own queued lines, a signal, a note.
// Info lines alone wait for a line to go with (infoWaits), as deliver does.
func (s *Shift) dispatcherWaits(rs *RoleState, now time.Time) bool {
	if len(rs.Inbox) > 0 {
		return true
	}

	var local, remote []string

	for _, sig := range s.State.Signals {
		if IsRemote(signalBody(sig)) {
			remote = append(remote, sig)
		} else {
			local = append(local, sig)
		}
	}

	if len(local)+len(remote)+len(s.State.Notes) == 0 {
		return false
	}

	return len(remote) > 0 || !s.infoWaits(local, now)
}

// DeliverPass is DeliverTo for every role that is waiting (Waiting): a delivery and nothing else, so a caller may run
// it as often as it likes between beats. It starts no role, trims nothing, tells nobody again, sweeps and ages no
// record and writes neither LastTick nor Up; what it cannot give stays queued for the beat. Call it under the crew
// state lock. changed reports whether it altered the state in any way (a line given, a try counted, a record settled),
// so a caller that finds nothing changed may let the lock go without a save. The error is a state that could not be
// written after a line went (see flush): the state on disk no longer says what was done. Waiting makes no state of a
// role that has none, but the delivery can, as the beat's does: a note about a role of the crew file that was never
// started makes it an empty one (noticeCurrent asks for its session).
func (s *Shift) DeliverPass() (changed bool, err error) {
	names := s.Waiting()
	if len(names) == 0 {
		return false, nil
	}

	before, err := json.Marshal(s.State)
	if err != nil {
		return false, fmt.Errorf("state: %w", err)
	}

	for _, name := range names {
		if s.flushErr != nil {
			break
		}

		s.mark("deliver", name)
		s.DeliverTo(name)
	}

	after, err := json.Marshal(s.State)
	if err != nil {
		return true, fmt.Errorf("state: %w", err)
	}

	return !bytes.Equal(before, after), s.flushErr
}
