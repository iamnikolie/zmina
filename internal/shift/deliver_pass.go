package shift

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// Waiting names the roles that have something DeliverTo could give now, in the crew's order (the dispatcher first):
// a role up under zmina, not just given a slash command (slashSettle), with a line queued for it that did not fail
// less than countedGap ago; the dispatcher also with a signal or a note waiting, bar info lines that still ride
// (infoRide), and not one a post or a typed line failed to less than retryGap ago. It reads the loaded state and the
// clock and nothing else: no tmux, no registry, no inbox socket, no task file, and it makes no state of a role that
// has none. A role it names may still not take its line (busy, a draft, a dialog): that is for the pass to find out; a
// role it leaves out has nothing a pass could give. It leaves out, too, a role held after a turn that ended on an API
// error (failureHeld): for a role that has something to give it reads that role's small failure record and Claude Code's
// session file by the role's session name — still no tmux. The hold ends with no change of state.json, as the other
// gates do, so a caller that must see the end of it looks again from time to time (queueGated).
func (s *Shift) Waiting() []string {
	now := s.Now()

	var out []string

	for _, name := range s.Names() {
		rs := s.State.Roles[name]
		if rs == nil || !rs.Up || rs.settling(now) {
			continue
		}

		if name == s.Crew.Dispatcher {
			if !rs.dispatchTriedLately(now) && s.dispatcherWaits(rs, now) && !s.failureHeld(name, now) {
				out = append(out, name)
			}

			continue
		}

		if len(rs.Inbox) > 0 && !rs.triedLately(now) && (!s.failureHeld(name, now) || s.ownerNext(name)) {
			out = append(out, name)
		}
	}

	return out
}

// dispatcherWaits reports whether deliver has something for the dispatcher: a queued line of its own that did not fail
// less than countedGap ago, a signal, a note.
// Info lines alone wait for a line to go with (infoWaits), as deliver does.
func (s *Shift) dispatcherWaits(rs *RoleState, now time.Time) bool {
	if len(rs.Inbox) > 0 && !rs.triedLately(now) {
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

// RoleDelivery is what a pass did for one role it tried (DeliverRoles).
type RoleDelivery struct {
	Role string
	// Changed is whether trying the role altered the state in any way: a line given, a try counted, a record settled.
	// A role that is waiting but cannot take its line (busy, a draft, a dialog) changes nothing.
	Changed bool
	// Took is how long trying the role took, by the wall clock: tmux, the registry, the post or the typing.
	Took time.Duration
}

// slowTry is the time a try may take before the pass ends after it, the roles after it left for the next pass: the
// pass runs under the state lock, and a tmux that answers slowly must not hold it for one role after another.
const slowTry = 2 * time.Second

// wallNow is the wall clock a try is timed by (not the shift's Now, which a pass holds still); a test moves it.
var wallNow = time.Now

// DeliverRoles is DeliverPass for some of the waiting roles only: those of only that Waiting names, in Waiting's order.
// A name in only that Waiting does not name is skipped, so a list gone stale between the look and the call gives a
// role nothing a pass would not. It reports each role it tried, in order, and whether trying it altered the state.
// That is found by comparing the state as it would be saved before and after that role alone (the "after" of one role
// is the "before" of the next, so a pass over n roles serializes the state n+1 times), so a change anywhere in it
// counts: the role's own record, the signals and notes the dispatcher's delivery takes, a notice a failed line adds.
// A caller that repeats the pass uses it to leave a role that changed nothing alone for a while: asking a role that
// cannot take its line yet costs tmux, the registry and a transcript read. Like DeliverPass it starts no role, trims
// nothing, tells nobody again and sweeps no record; call it under the crew state lock. The error is the one DeliverPass
// returns, a state that could not be written after a line went: the roles after the one it came on are not tried and
// not reported. Nor are the roles after one whose try took over slowTry: they are the next pass's.
func (s *Shift) DeliverRoles(only []string) ([]RoleDelivery, error) {
	var names []string

	for _, name := range s.Waiting() {
		if slices.Contains(only, name) {
			names = append(names, name)
		}
	}

	if len(names) == 0 {
		return nil, nil
	}

	before, err := json.Marshal(s.State)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}

	out := make([]RoleDelivery, 0, len(names))

	for _, name := range names {
		if s.flushErr != nil {
			break
		}

		s.mark("deliver", name)

		began := wallNow()
		s.DeliverTo(name)
		took := wallNow().Sub(began)

		after, err := json.Marshal(s.State)
		if err != nil {
			return append(out, RoleDelivery{Role: name, Changed: true, Took: took}), fmt.Errorf("state: %w", err)
		}

		out = append(out, RoleDelivery{Role: name, Changed: !bytes.Equal(before, after), Took: took})
		before = after

		if took > slowTry {
			break
		}
	}

	return out, s.flushErr
}
