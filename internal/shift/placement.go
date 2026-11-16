package shift

import (
	"errors"
	"fmt"
	"slices"
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

	// A machine that is no node, or that the file does not list, runs none of the crew: which node runs the role is not the answer.
	if !s.Crew.Runs() {
		return name + ": " + s.notRunningWhy()
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

	if !s.Crew.Runs() {
		return "this node runs nothing of the crew (zmina doctor)"
	}

	if r.Name == s.Crew.Dispatcher {
		return "it runs on " + strings.Join(s.Crew.DispatcherNodes(), ", ") + ", not on this node"
	}

	return "it is placed on " + s.Crew.PlacementOf(r) + ", not on this node"
}

// notRunningWhy says why this machine runs nothing of a crew over nodes: its node name cannot be read, or the file does not
// list it.
func (s *Shift) notRunningWhy() string {
	if s.Crew.Here == "" {
		return "this machine's node name cannot be read, so a crew over nodes runs nothing here (not a role, the dispatcher or a job) " +
			"until it can: zmina node init, or mend ~/.zmina/mesh/node.json"
	}

	return fmt.Sprintf("this node (%s) is not one of nodes: (%s), so it runs nothing of this crew (not a role, the dispatcher or a job) "+
		"until the file lists it; a node's name is matched exactly", s.Crew.Here, strings.Join(s.Crew.NodeNames(), ", "))
}

// notRunningPrefix opens the RecordNotified key of the daily log line of a machine that runs nothing of the crew.
const notRunningPrefix = "node-not-running/"

// notRunning is the whole beat of a machine that runs nothing of the crew (its node name cannot be read, or the file does
// not list it): it has no role, no dispatcher and no job to run, so it only shows that the ticker beat and says once a day, in
// the log, why nothing runs. Every command says the same on every load.
func (s *Shift) notRunning(now time.Time) []string {
	s.State.LastTick = now
	s.sweepAway(now)

	key := notRunningPrefix + now.In(s.Crew.Location()).Format(time.DateOnly)
	if s.State.RecordNotified[key] {
		return nil
	}

	s.MarkNoted(key)

	why := s.notRunningWhy()
	s.log("", "node-not-running", "", why)

	return []string{why}
}

// workPrefix opens the RecordNotified key of the note that a role placed on another node holds work here: workPrefix + role.
// It is kept while the role holds something, and goes when it holds nothing.
const workPrefix = "placement-work/"

// awayUp is the beat's look at a role the crew file places on another node: it is skipped, never started, never put down.
// Its Up in the state follows the probe, both ways, because that is what the hello tells as held: a session that is up here is
// told, and one that ended is not still told. A session that is up is two of one role if the other node runs it too, so the
// dispatcher is told, once a day, and nothing is taken down: that is the owner's to decide. It returns lines for the log.
func (s *Shift) awayUp(name string, r *crew.Role, now time.Time) (did []string) {
	p, err := s.probe(name)
	if err != nil {
		return nil
	}

	up := p.State == tmux.Up

	if rs := s.State.Roles[name]; rs != nil {
		rs.Up = up
	} else if up {
		s.State.Role(name).Up = true
	}

	did = append(did, s.awayWork(name, r)...)

	if !up {
		return did
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

// awayWork tells the dispatcher once that a role the crew file places on another node still holds work on this node's ledger
// or lines in its queue: the beat does nothing for it here, so they would sit. A role moved with zmina move takes its work along;
// this one was placed by an edit of the file, so the way on is to park what it holds and give the work to the node it is placed
// on. The note is made again only after the role has held nothing.
func (s *Shift) awayWork(name string, r *crew.Role) []string {
	key := workPrefix + name

	if name == s.Crew.Dispatcher {
		return nil
	}

	holds := s.heldBy(name)

	lines := 0
	if rs := s.State.Roles[name]; rs != nil {
		lines = len(rs.Inbox)
	}

	if len(holds) == 0 && lines == 0 {
		delete(s.State.RecordNotified, key)

		return nil
	}

	var what []string

	if len(holds) > 0 {
		what = append(what, strings.Join(holds, ", "))
	}

	if lines > 0 {
		noun := "lines"
		if lines == 1 {
			noun = "line"
		}

		what = append(what, fmt.Sprintf("%d queued %s", lines, noun))
	}

	text := fmt.Sprintf("%s holds %s on this node (%s), but the crew file places it on %s, so nothing here works on it: park what it "+
		"holds (zmina task park <id>) and give the work to %s (zmina tell %s \"<what remains>\"). A role moved with zmina move takes its "+
		"work along; that runs while the file still places the role on the node that holds it", name, strings.Join(what, " and "),
		s.Crew.Here, s.Crew.WhereIs(r), s.Crew.WhereIs(r), s.Crew.RoleTell(r))

	if told, _ := s.NoteOnce(key, "zmina", text, false); told {
		s.log(name, "placed-elsewhere-work", strings.Join(holds, ","), text)

		return []string{name + ": holds " + strings.Join(what, " and ") + ", placed on " + s.Crew.WhereIs(r)}
	}

	return nil
}

// listedNodes is the nodes among names that the crew file lists and that are not this node, sorted, each once.
func (s *Shift) listedNodes(names []string) []string {
	var out []string

	for _, n := range names {
		if s.Crew.HasNode(n) && n != s.Crew.Here {
			out = append(out, n)
		}
	}

	slices.Sort(out)

	return slices.Compact(out)
}

// twicePrefix opens the RecordNotified key of the daily note that a role is up on this node and on another: twicePrefix +
// role + "/" + the nodes it is up on, sorted and joined by commas + "/" + the day.
const twicePrefix = "role-twice/"

// sweepAway drops the keys of the daily notes of past days (a role placed elsewhere that is up here, a role up on two nodes).
func (s *Shift) sweepAway(now time.Time) {
	if len(s.State.RecordNotified) == 0 {
		return
	}

	day := now.In(s.Crew.Location()).Format(time.DateOnly)

	for key := range s.State.RecordNotified {
		for _, prefix := range []string{awayPrefix, twicePrefix, notRunningPrefix} {
			if rest, ok := strings.CutPrefix(key, prefix); ok && rest != day && !strings.HasSuffix(rest, "/"+day) {
				delete(s.State.RecordNotified, key)
			}
		}
	}
}

// roleTwice is the backstop of the placement: a role up on this node that another node lately told up too is one role on two
// nodes, whatever the file says (a hand start, a file not yet copied, a node of an older zmina that runs every role). Each
// such role is an event (role-twice) and one note to the dispatcher, once a day for each set of nodes it is up on; nothing is taken down:
// which of the two goes is the owner's to say. It asks tmux only for the roles a peer says it holds. Only the nodes the crew file lists
// are witnesses: a peer of the mesh that is no node of it is not believed, so that what it claims neither makes a note nor uses up the
// day's key for a real duplicate. A node that says nothing (it is down, or runs a zmina that tells no roles) cannot be seen at all.
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
		nodes := s.listedNodes(held[name])
		if name == s.Crew.Dispatcher || len(nodes) == 0 {
			continue
		}

		if p, err := s.probe(name); err != nil || p.State != tmux.Up {
			continue
		}

		text := fmt.Sprintf("%s is up on this node (%s) and on %s: one role on two nodes; zmina down %s on the one the crew file "+
			"does not place it on (it places it on %s)", name, s.Crew.Here, strings.Join(nodes, ", "), name, s.Crew.WhereIs(s.Crew.Roles[name]))

		if told, _ := s.NoteOnce(twicePrefix+name+"/"+strings.Join(nodes, ",")+"/"+day, "zmina", text, false); told {
			s.log(name, "role-twice", strings.Join(nodes, ","), text)

			did = append(did, name+": up here and on "+strings.Join(nodes, ", "))
		}
	}

	return did
}
