package cmd

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/move"
	"github.com/iamnikolie/zmina/internal/shift"
)

// placementNote is one thing placement has to say of a crew whose file declares nodes: for doctor (with its level) and
// zmina node ls (the text). Header notes say how the crew is spread; the rest are what to look at.
type placementNote struct {
	Level  string
	Text   string
	Header bool
}

// placementNotes says how a crew whose file declares nodes: stands: how it is spread, whether this node's file is the one
// its peers hold, which roles no node holds, and which roles are up on two nodes. local is how this node tells the crew
// (its sum and the roles it has up); peers are the nodes met, each with what it last told. up are the roles of the crew
// up on this node by its own state, which says more than the hello when it is not yet told; nil when not known. A crew
// that is not spread has nothing to say.
func placementNotes(c *crew.Crew, local mesh.CrewHealth, peers []mesh.Peer, up []string) []placementNote {
	if !c.Spread() {
		return nil
	}

	var out []placementNote

	add := func(level, format string, args ...any) {
		out = append(out, placementNote{Level: level, Text: fmt.Sprintf(format, args...)})
	}

	out = append(out, placementNote{Level: "ok", Header: true, Text: spreadLine(c)})

	switch {
	case c.Here == "":
		add("fail", "this machine's node name cannot be read (~/.zmina/mesh/node.json: zmina node init), so it runs nothing of this crew: "+
			"not a role, the dispatcher or a job of the crew")
	case !c.HasNode(c.Here):
		add("warn", "this node (%s) is not one of nodes: (%s), and a node's name is matched exactly: it runs nothing of this crew, "+
			"not a role, the dispatcher or a job of the crew", c.Here, strings.Join(c.NodeNames(), ", "))
	}

	for _, name := range c.Unplaced() {
		add("warn", "%s is held by no node: its place %q is not one of nodes: (%s)", name, c.PlacementOf(c.Roles[name]),
			strings.Join(c.NodeNames(), ", "))
	}

	for _, name := range up {
		if r, ok := c.Roles[name]; ok && !c.PlacedHere(r) {
			add("warn", "%s is up on this node, but the crew file places it on %s: zmina down %s here, or fix its place in the "+
				"file on every node", name, c.WhereIs(r), name)
		}
	}

	var same, differs, silent []string

	held := local.Held
	if up != nil {
		held = up
	}

	heard := map[string]bool{}
	met := map[string]bool{}

	for _, p := range peers {
		// Only the nodes the file lists are this crew's: another peer's crew of the same name (two default crews) is another crew.
		if !c.HasNode(p.Name) || p.Name == c.Here {
			continue
		}

		met[p.Name] = p.Cert != nil

		for _, h := range p.Crews {
			if h.Crew != local.Crew {
				continue
			}

			heard[p.Name] = true

			switch h.Sum {
			case "":
				silent = append(silent, p.Name)
			case local.Sum:
				same = append(same, p.Name)
			default:
				differs = append(differs, p.Name)
			}

			for _, role := range h.Held {
				if role != c.Dispatcher && slices.Contains(held, role) {
					add("warn", "%s is up on this node and on %s: one role on two nodes; zmina down it on the one the file "+
						"does not place it on", role, p.Name)
				}
			}
		}
	}

	if len(same) > 0 {
		add("ok", "in sync with %s", strings.Join(same, ", "))
	}

	if len(differs) > 0 {
		add("warn", "differs from %s: the file is not the same on every node; copy one file to every node (crew.yaml, byte "+
			"for byte), or compare them by hand", strings.Join(differs, ", "))
	}

	if len(silent) > 0 {
		add("warn", "%s tells no sum for this crew: an older zmina that reads no nodes:, or a file that declares none; "+
			"it may run every role", strings.Join(silent, ", "))
	}

	// A node the file lists and that has said nothing of this crew: never met, or met and not running a crew of this name.
	for _, n := range c.NodeNames() {
		switch {
		case n == c.Here || heard[n]:
		case met[n]:
			add("warn", "no word from %s about this crew: it runs no crew of this name yet (zmina crew push %s --create), or tells "+
				"nothing of its crews", n, n)
		default:
			add("warn", "no word from %s: this node has not met it (zmina node ls; zmina node peer add or enroll)", n)
		}
	}

	return out
}

// spreadLine is how the crew file spreads the crew, from this node's side.
func spreadLine(c *crew.Crew) string {
	var parts []string

	for _, n := range c.NodeNames() {
		var roles []string

		for _, name := range c.Names() {
			if name != c.Dispatcher && c.PlacementOf(c.Roles[name]) == n {
				roles = append(roles, name)
			}
		}

		word := n
		if n == c.Home {
			word += " (home)"
		}

		if len(roles) > 0 {
			word += ": " + strings.Join(roles, ", ")
		}

		parts = append(parts, word)
	}

	line := "nodes: " + strings.Join(parts, " · ")
	if c.Here != "" {
		line += " · this node: " + c.Here
	}

	return line
}

// checkPlacement is doctor's look at a crew whose file declares nodes:.
func (a *app) checkPlacement(c *crew.Crew, st *shift.State, add func(name, level, format string, args ...any)) {
	if !c.Spread() {
		// A move journal left in a crew that declares no nodes holds its role down, and only --abort ends it.
		for _, name := range c.Names() {
			if j, ok, err := move.Open(c.Layout(), name); err != nil {
				add("placement", "fail", "the move journal of %s does not read, so the role is not started: %v", name, err)
			} else if ok {
				add("placement", "fail", "%s has a move journal (%s to %s) but this crew's file declares no nodes:, so it cannot go on and the role is held down: zmina move %s %s --abort %s",
					name, j.ID, j.To, name, j.To, j.ID)
			}
		}

		return
	}

	local, ok := healthOf(uiCrew{Key: a.crewKey(), Path: c.Path})
	if !ok {
		// A state that does not read keeps no health, not the file's sum.
		local = mesh.CrewHealth{Crew: a.crewKey(), TickAge: -1, Sum: c.Sum()}
	}

	var peers []mesh.Peer

	if me, err := openNode(); err == nil {
		peers, _ = me.Peers()
	}

	var up []string

	if st != nil {
		up = []string{}

		for _, name := range c.Names() {
			if rs := st.Roles[name]; name != c.Dispatcher && rs != nil && rs.Up {
				up = append(up, name)
			}
		}
	}

	for _, n := range placementNotes(c, local, peers, up) {
		add("placement", n.Level, "%s", n.Text)
	}

	// A move that is open: its role is held down on this node until it ends, so it is said where the owner looks.
	for _, name := range c.Names() {
		if j, ok, err := move.Open(c.Layout(), name); err != nil {
			add("placement", "fail", "the move journal of %s does not read, so the role is not started: %v", name, err)
		} else if ok {
			add("placement", "warn", "%s has a move to %s open (%s, %s): zmina move %s %s --resume %s finishes it, --abort %s calls it off",
				name, j.To, j.ID, j.Phase, name, j.To, j.ID, j.ID)
		}
	}

	// A take of a move that was cut short on this node: the next take of it, or the abort, takes away what it made.
	for _, name := range c.Names() {
		if m, ok, err := move.OpenTaking(c.Layout(), name); err != nil {
			add("placement", "fail", "the marker of a take of %s does not read: %v", name, err)
		} else if ok {
			add("placement", "warn", "a take of %s from %s (%s) is running or was cut short on this node: zmina move %s %s --resume %s takes it again, --abort %s takes away what it made",
				name, m.From, m.ID, name, c.Here, m.ID, m.ID)
		}
	}

	for _, d := range move.Drives(c.Layout()) {
		add("placement", "warn", "this node drives the move %s of %s from %s to %s: its ticker takes it on each beat; zmina move %s %s --resume %s",
			d.ID, d.Role, d.From, d.To, d.Role, d.To, d.ID)
	}
}

// nodeLsPlacement are the lines zmina node ls adds for each crew of this node whose file declares nodes:.
func nodeLsPlacement(files map[string]string, health []mesh.CrewHealth, peers []mesh.Peer) []string {
	var out []string

	for _, key := range slices.Sorted(maps.Keys(files)) {
		c, err := crew.Load(files[key])
		if err != nil || !c.Spread() {
			continue
		}

		i := slices.IndexFunc(health, func(h mesh.CrewHealth) bool { return h.Crew == key })
		if i < 0 {
			continue
		}

		for _, n := range placementNotes(c, health[i], peers, health[i].Held) {
			if !n.Header {
				out = append(out, fmt.Sprintf("  %s: %s", key, n.Text))
			}
		}
	}

	return out
}
