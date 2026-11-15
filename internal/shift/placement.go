package shift

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// awayPrefix opens the RecordNotified key of the daily note that a role placed on another node is up here: awayPrefix +
// role + "/" + the day.
const awayPrefix = "placement-up/"

// PlacedAway says where the crew file runs the role when that is not this node, "" when the role is placed here (or the
// crew is not spread over nodes, or this machine is no node). The one line names the node, and how to reach the crew
// there.
func (s *Shift) PlacedAway(name string) string {
	r, err := s.Role(name)
	if err != nil || s.Crew.PlacedHere(r) {
		return ""
	}

	if r.Name == s.Crew.Dispatcher {
		return fmt.Sprintf("%s runs on %s (home and the nodes that hold a role), not on this node (%s)", name,
			strings.Join(s.Crew.DispatcherNodes(), ", "), s.Crew.Here)
	}

	return fmt.Sprintf("%s is placed on %s, not on this node (%s): run it there, or zmina tell %s \"<text>\" to its dispatcher",
		name, s.Crew.PlacementOf(r), s.Crew.Here, s.Crew.RoleTell(r))
}

// errPlacedAway is the refusal of a start of a role the crew file places on another node.
func (s *Shift) errPlacedAway(name string) error {
	if line := s.PlacedAway(name); line != "" {
		return errors.New(line)
	}

	return nil
}

// placedAwayShort is placedAway as a reason in a sentence: "it is placed on beta, not on this node".
func (s *Shift) placedAwayShort(name string) string {
	r, err := s.Role(name)
	if err != nil || s.Crew.PlacedHere(r) {
		return ""
	}

	if r.Name == s.Crew.Dispatcher {
		return "it runs on " + strings.Join(s.Crew.DispatcherNodes(), ", ") + ", not on this node"
	}

	return "it is placed on " + s.Crew.PlacementOf(r) + ", not on this node"
}

// awayUp is the beat's look at a role the crew file places on another node: it is skipped, never started, never put down.
// A session of it that is up here is two of one role if the other node runs it too, so the dispatcher is told, once a
// day, and nothing is taken down: that is the owner's to decide. It returns a line for the log and notes.
func (s *Shift) awayUp(name string, r *crew.Role, now time.Time) (did []string) {
	p, err := s.probe(name)
	if err != nil || p.State != tmux.Up {
		return nil
	}

	day := s.Now().In(s.Crew.Location()).Format(time.DateOnly)
	key := awayPrefix + name + "/" + day

	text := fmt.Sprintf("%s is up on this node (%s), but the crew file places it on %s: one role must not run on two nodes; "+
		"zmina down %s here, or change its place in the crew file on every node", name, s.Crew.Here, s.Crew.WhereIs(r), name)

	if told, _ := s.NoteOnce(key, "zmina", text, false); told {
		s.log(name, "placed-elsewhere", "", text)

		did = append(did, name+": up here, placed on "+s.Crew.WhereIs(r))
	}

	return did
}

// twicePrefix opens the RecordNotified key of the daily note that a role is up on this node and on another: twicePrefix +
// role + "/" + the day.
const twicePrefix = "role-twice/"

// sweepAway drops the keys of the daily notes of past days (a role placed elsewhere that is up here, a role up on two nodes).
func (s *Shift) sweepAway(now time.Time) {
	if len(s.State.RecordNotified) == 0 {
		return
	}

	day := now.In(s.Crew.Location()).Format(time.DateOnly)

	for key := range s.State.RecordNotified {
		for _, prefix := range []string{awayPrefix, twicePrefix} {
			if rest, ok := strings.CutPrefix(key, prefix); ok && !strings.HasSuffix(rest, "/"+day) {
				delete(s.State.RecordNotified, key)
			}
		}
	}
}

// roleTwice is the backstop of the placement: a role up on this node that another node lately told up too is one role on two
// nodes, whatever the file says (a hand start, a file not yet copied, a node of an older zmina that runs every role). Each
// such role is an event (role-twice) and one note to the dispatcher, once a day; nothing is taken down: which of the two
// goes is the owner's to say. It asks tmux only for the roles a peer says it holds.
func (s *Shift) roleTwice(now time.Time) (did []string) {
	if s.PeerHeld == nil || !s.Crew.Spread() {
		return nil
	}

	held := s.PeerHeld()
	if len(held) == 0 {
		return nil
	}

	day := now.In(s.Crew.Location()).Format(time.DateOnly)

	for _, name := range s.Crew.Names() {
		nodes := held[name]
		if name == s.Crew.Dispatcher || len(nodes) == 0 {
			continue
		}

		if p, err := s.probe(name); err != nil || p.State != tmux.Up {
			continue
		}

		text := fmt.Sprintf("%s is up on this node (%s) and on %s: one role on two nodes; zmina down %s on the one the crew file "+
			"does not place it on (it places it on %s)", name, s.Crew.Here, strings.Join(nodes, ", "), name, s.Crew.WhereIs(s.Crew.Roles[name]))

		if told, _ := s.NoteOnce(twicePrefix+name+"/"+day, "zmina", text, false); told {
			s.log(name, "role-twice", strings.Join(nodes, ","), text)

			did = append(did, name+": up here and on "+strings.Join(nodes, ", "))
		}
	}

	return did
}
