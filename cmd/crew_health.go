package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/shift"
)

// openNode is this machine's node, telling its peers how its crews stand in every hello.
func openNode() (*mesh.Node, error) {
	n, err := mesh.Open(meshDir())
	if err != nil {
		return nil, err
	}

	n.Health = crewHealth

	return n, nil
}

// crewHealth is how every crew of this node stands, from its crew file and state; a crew that does not read is left
// out, which says nothing about it.
func crewHealth() []mesh.CrewHealth {
	out := []mesh.CrewHealth{}

	for _, c := range uiCrews("") {
		if h, ok := healthOf(c); ok {
			out = append(out, h)
		}
	}

	return out
}

func healthOf(c uiCrew) (mesh.CrewHealth, bool) {
	cr, err := crew.Load(c.Path)
	if err != nil {
		return mesh.CrewHealth{}, false
	}

	st, err := shift.Read(cr.StateDir())
	if err != nil {
		return mesh.CrewHealth{}, false
	}

	now := time.Now()
	h := mesh.CrewHealth{Crew: c.Key, TickAge: -1, Retired: cr.IsRetired(), MovedTo: cr.MovedTo}

	if !st.LastTick.IsZero() {
		h.TickAge = max(int64(now.Sub(st.LastTick).Seconds()), 0)
	}

	if !st.Stopped.IsZero() {
		h.Stopped = max(int64(now.Sub(st.Stopped).Seconds()), 1)
	}

	// Only a crew whose file declares nodes: tells its file's sum and the roles it has up: for any other crew the hello is
	// what it always was. The file is one for every node there, so a peer's sum that differs is drift, and a role up on two
	// nodes is the split brain placement exists to prevent.
	if cr.Spread() {
		h.Sum = cr.Sum()

		for _, name := range cr.Names() {
			if rs := st.Roles[name]; name != cr.Dispatcher && rs != nil && rs.Up {
				h.Held = append(h.Held, name)
			}
		}
	}

	return h, true
}

// otherCrews names the crews called key this node knows of, each with how it stands, leaving out skip. A node's
// default crew is every node's first crew, so that name says nothing and none are listed for it.
func otherCrews(key string, skip mesh.Address) []string {
	if key == "" || key == "default" {
		return nil
	}

	me, err := mesh.Open(meshDir())
	if err != nil {
		return nil
	}

	var out []string

	// Asked about a crew elsewhere, this node's crew of that name counts; asked about this one, it is the one asked of.
	if here := (mesh.Address{Node: me.Name, Crew: key}); skip.Node != "" && skip != here {
		if c, err := localCrew(key); err == nil {
			if h, ok := healthOf(c); ok {
				out = append(out, here.String()+" ("+h.Why()+")")
			}
		}
	}

	book, _ := me.Peers()
	now := time.Now()

	for _, p := range book {
		for _, h := range p.Crews {
			if h.Crew != key || (mesh.Address{Node: p.Name, Crew: key}) == skip {
				continue
			}

			out = append(out, fmt.Sprintf("%s/%s (%s%s)", p.Name, key, h.Why(), seenAgo(p.CrewsAt, now)))
		}
	}

	return out
}

// seenWithin is how old what a peer told may be before it is said to be old, and before tell asks again.
const seenWithin = 2 * time.Minute

func seenAgo(at, now time.Time) string {
	if d := now.Sub(at); d > seenWithin {
		return ", as of " + age(d) + " ago"
	}

	return ""
}

// warnStanding tells whoever reads a crew's ledger, on stderr, that it is retired or stood still.
func (a *app) warnStanding(s *shift.Shift) {
	if n := s.Standing(); n != "" {
		fmt.Fprintln(a.err, "zmina: warning:", n)
	}
}

// crewStanding is what this node knows of how a crew stands, and how old that is; ok false when it knows nothing (an
// older zmina, a node never met, a crew it did not name).
func crewStanding(me *mesh.Node, to mesh.Address) (mesh.CrewHealth, time.Time, bool) {
	return standingIn(me.Name, peerOf(me, to.Node), to)
}

// standingIn is crewStanding from this node's name and the book's entry for the destination's node.
func standingIn(node string, p mesh.Peer, to mesh.Address) (mesh.CrewHealth, time.Time, bool) {
	if to.Node == node {
		c, err := localCrew(to.Crew)
		if err != nil {
			return mesh.CrewHealth{}, time.Time{}, false
		}

		h, ok := healthOf(c)

		return h, time.Now(), ok
	}

	key := to.Crew
	if key == "" {
		key = "default"
		if len(p.Crews) == 1 {
			key = p.Crews[0].Crew
		}
	}

	i := slices.IndexFunc(p.Crews, func(h mesh.CrewHealth) bool { return h.Crew == key })
	if i < 0 {
		return mesh.CrewHealth{}, time.Time{}, false
	}

	return p.Crews[i], p.CrewsAt, true
}

// freshStanding asks the far node how its crews stand when what this node holds is old (its own ticker keeps it fresh
// otherwise): one session. It reports false when that failed, so the caller does not wait for a dead peer twice.
func (a *app) freshStanding(ctx context.Context, me *mesh.Node, to mesh.Address) bool {
	p := peerOf(me, to.Node)
	if to.Node == me.Name || len(p.Addrs) == 0 || time.Since(p.CrewsAt) <= seenWithin {
		return true
	}

	ss := me.Sync(ctx, mesh.Exec{}, []mesh.Discovery{mesh.Book{Node: me}}, a.deliverFrom(), to.Node, peerTimeout, 0, time.Now)

	return !slices.ContainsFunc(ss, func(s mesh.Session) bool { return s.Err != nil })
}

// notRunning is plain tell's refusal, and report's warning: where the crew stands, where a live one is if the owner
// said or a crew of its name is known, and how to queue on purpose.
func notRunning(to mesh.Address, h mesh.CrewHealth, at time.Time) string {
	line := fmt.Sprintf("%s is not running: %s%s; a line would wait there until someone starts it",
		to, h.Why(), seenAgo(at, time.Now()))

	if h.MovedTo != "" {
		line += ". Its live crew: " + h.MovedTo
	}

	if others := otherCrews(h.Crew, to); len(others) > 0 {
		line += ". Other crews of that name: " + strings.Join(others, ", ") + " (one name does not prove one crew)"
	}

	return line
}

// noCrewFile says which crews exist here when the one asked for does not: on a node whose only crews are named, the
// plain command finds no default crew.
func noCrewFile(none *crew.NoFileError) error {
	crews := uiCrews("")
	if len(crews) == 0 {
		return none
	}

	var names []string
	for _, c := range crews {
		if c.Key == "default" {
			names = append(names, "default (no flag)")
		} else {
			names = append(names, c.Key+" (--config "+c.Key+")")
		}
	}

	return fmt.Errorf("crew: %s does not exist; the crews here: %s", none.Path, strings.Join(names, ", "))
}

// heldWithin is how old what a peer told of the roles it has up may be before it is no evidence of what runs there now: the
// ticker syncs every minute, so a peer silent this long is not known to run anything.
const heldWithin = 10 * time.Minute

// peersHeld is which other nodes lately told the roles of the crew called key up, role -> node names, from the address book.
func peersHeld(key string, now time.Time) map[string][]string {
	me, err := mesh.Open(meshDir())
	if err != nil {
		return nil
	}

	book, _ := me.Peers()
	out := map[string][]string{}

	for _, p := range book {
		if p.CrewsAt.IsZero() || now.Sub(p.CrewsAt) > heldWithin {
			continue
		}

		for _, h := range p.Crews {
			if h.Crew != key {
				continue
			}

			for _, role := range h.Held {
				out[role] = append(out[role], p.Name)
			}
		}
	}

	return out
}
