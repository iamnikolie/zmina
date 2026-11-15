package cmd

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/mesh"
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
		add("warn", "this machine is no node (zmina node init), so it runs every role of the file; a node runs only its own")
	case !c.HasNode(c.Here):
		add("warn", "this node (%s) is not one of nodes: (%s): it runs nothing of this crew, and its dispatcher does not run",
			c.Here, strings.Join(c.NodeNames(), ", "))
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

	for _, p := range peers {
		for _, h := range p.Crews {
			if h.Crew != local.Crew {
				continue
			}

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
