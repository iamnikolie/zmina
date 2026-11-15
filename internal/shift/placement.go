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

// sweepAway drops the keys of the daily notes of past days.
func (s *Shift) sweepAway(now time.Time) {
	if len(s.State.RecordNotified) == 0 {
		return
	}

	day := now.In(s.Crew.Location()).Format(time.DateOnly)

	for key := range s.State.RecordNotified {
		if rest, ok := strings.CutPrefix(key, awayPrefix); ok && !strings.HasSuffix(rest, "/"+day) {
			delete(s.State.RecordNotified, key)
		}
	}
}
