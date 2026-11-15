package crew

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// NodeKeys are the crew-level keys a node may set for itself under nodes.<node>: scalars and lists that name this
// machine's own things (where its claude is, its prefix, its notify command). A node's value is merged over the
// top-level key when this node loads the file. Anything else under a node is a load error: an override that is
// silently ignored is the drift the one file is there to end.
var NodeKeys = []string{"prefix", "claude", "codex", "namespace", "dir", "agents", "notify", "notify_on", "quiet_hours",
	"plugins", "drop_env", "timezone", "courier", "worktree_gc", "spare", "retired", "moved_to"}

// nodeNameRe is what a node's name may be: the mesh's own rule (it goes into addresses and branch names).
var nodeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ThisNodeFile is where the mesh keeps who this machine is; the crew package reads only its name, so it need not know
// the mesh.
func ThisNodeFile() string { return ExpandHome("~/.zmina/mesh/node.json") }

// ThisNode is the name of the node this machine is (zmina node init), "" for a machine that is none.
func ThisNode() string {
	raw, err := os.ReadFile(ThisNodeFile())
	if err != nil {
		return ""
	}

	var nf struct {
		Name string `json:"name"`
	}

	if json.Unmarshal(raw, &nf) != nil || !nodeNameRe.MatchString(nf.Name) {
		return ""
	}

	return nf.Name
}

// Spread reports a crew file that declares nodes: it is one crew over more than one machine, each role placed on a
// node. A file without it is the single-node crew every file was before, and nothing below changes for it.
func (c *Crew) Spread() bool { return len(c.Nodes) > 0 }

// NodeNames lists the nodes the crew file declares, sorted.
func (c *Crew) NodeNames() []string {
	names := make([]string, 0, len(c.Nodes))
	for n := range c.Nodes {
		names = append(names, n)
	}

	sort.Strings(names)

	return names
}

// HasNode reports whether the crew file lists the node.
func (c *Crew) HasNode(node string) bool {
	_, ok := c.Nodes[node]

	return ok
}

// PlacementOf is the node that runs the role: its place, else the crew's home. Empty for a crew that is not spread, and
// for the dispatcher, which runs on every node that holds a role and on home (DispatcherNodes).
func (c *Crew) PlacementOf(r *Role) string {
	if !c.Spread() || r == nil || r.Name == c.Dispatcher {
		return ""
	}

	return or(r.Place, c.Home)
}

// Holds reports whether the node runs at least one role of the crew other than the dispatcher.
func (c *Crew) Holds(node string) bool {
	for _, name := range c.Names() {
		if name != c.Dispatcher && c.PlacementOf(c.Roles[name]) == node {
			return true
		}
	}

	return false
}

// DispatcherNodes are the nodes the dispatcher runs on: home, and every node that holds a role. A node's dispatcher
// coordinates that node's slice of the crew; work for another node goes through zmina tell <node>/<crew>.
func (c *Crew) DispatcherNodes() []string {
	var out []string

	for _, n := range c.NodeNames() {
		if n == c.Home || c.Holds(n) {
			out = append(out, n)
		}
	}

	return out
}

// PlacedHere reports whether this node runs the role. A crew that is not spread runs every role, and so does a machine
// that is no node (doctor warns of it), as before the file could say otherwise. The dispatcher is replicated: it is
// placed on home and on every node that holds a role. A copy (dev#2) is placed where its role is.
func (c *Crew) PlacedHere(r *Role) bool {
	if !c.Spread() || c.Here == "" || r == nil {
		return true
	}

	if r.Name == c.Dispatcher {
		return c.Here == c.Home || c.Holds(c.Here)
	}

	return c.PlacementOf(r) == c.Here
}

// WhereIs says where the role runs, for a line that refuses to start it here: the node it is placed on, or for the
// dispatcher home and the nodes that hold a role.
func (c *Crew) WhereIs(r *Role) string {
	if r != nil && r.Name == c.Dispatcher {
		return strings.Join(c.DispatcherNodes(), ", ")
	}

	return c.PlacementOf(r)
}

// Unplaced lists the roles whose place is no node of the crew file: they run nowhere. The dispatcher is never among
// them.
func (c *Crew) Unplaced() []string {
	var out []string

	for _, name := range c.Names() {
		if name == c.Dispatcher || !c.Spread() {
			continue
		}

		if !c.HasNode(c.PlacementOf(c.Roles[name])) {
			out = append(out, name)
		}
	}

	return out
}

// AwayRoles lists the roles placed on another node than this one, in the crew's order; none for a crew that is not
// spread or a machine that is no node.
func (c *Crew) AwayRoles() []string {
	var out []string

	for _, name := range c.Names() {
		if r := c.Roles[name]; !c.PlacedHere(r) {
			out = append(out, name)
		}
	}

	return out
}

// RoleTell is the address to tell the dispatcher where a role that is placed elsewhere runs: <node>/<crew> for a crew
// named, <node> for the default crew (its key there is "default").
func (c *Crew) RoleTell(r *Role) string {
	node := c.PlacementOf(r)
	if node == "" && r != nil && r.Name == c.Dispatcher && c.Spread() {
		node = c.Home
	}

	if node == "" {
		return ""
	}

	if c.Name == "" {
		return node
	}

	return node + "/" + c.Name
}

// Sum is the hash of the crew file's bytes as read, the same one the file's history keeps: two nodes whose files
// carry one sum hold one file.
func (c *Crew) Sum() string { return c.sum }

// SumOf is the hash of a crew file's bytes.
func SumOf(raw []byte) string { return sumOf(raw) }

// placement reads what the file says of nodes, checks it and, for the node this machine is, merges its overlay over the
// top-level keys. It runs on the decoded file before the defaults are completed, so an overlaid prefix or namespace
// is the one every default follows. here is the node name, or "" for a machine that is none.
func (c *Crew) placement(here string, warn func(string)) error {
	if !c.Spread() {
		if c.Nodes != nil {
			c.Nodes = nil // nodes: {} says nothing
		}

		if c.Home != "" {
			warn("home: has no meaning without nodes:, ignored")
		}

		for _, name := range c.roleNames() {
			if c.Roles[name] != nil && c.Roles[name].Place != "" {
				warn(fmt.Sprintf("role %s: place: has no meaning without nodes:, ignored", name))
			}
		}

		return nil
	}

	names := c.NodeNames()

	for _, n := range names {
		if !nodeNameRe.MatchString(n) {
			return fmt.Errorf("nodes: %q is not a node name (letters, digits, '.', '_' and '-')", n)
		}

		if err := checkOverlay(n, c.Nodes[n]); err != nil {
			return err
		}
	}

	if c.Home == "" {
		return errors.New("home: names the node that runs the dispatcher; it is required once nodes: is set " +
			"(nodes: " + strings.Join(names, ", ") + ")")
	}

	if !c.HasNode(c.Home) {
		return fmt.Errorf("home %q is not one of nodes: (%s)", c.Home, strings.Join(names, ", "))
	}

	dispatcher := or(c.Dispatcher, defaultDispatcher)

	for _, name := range c.roleNames() {
		r := c.Roles[name]
		if r == nil || r.Place == "" {
			continue
		}

		switch {
		case name == dispatcher:
			warn(fmt.Sprintf("role %s is the dispatcher: it runs on home (%s) and on every node that holds a role; "+
				"place is ignored", name, c.Home))
		case !c.HasNode(r.Place):
			warn(fmt.Sprintf("role %s: place %q is not one of nodes: (%s); the role runs on no node", name, r.Place,
				strings.Join(names, ", ")))
		}
	}

	c.Here = here

	if here == "" || !c.HasNode(here) {
		return nil
	}

	return c.overlay(c.Nodes[here], here)
}

func (c *Crew) roleNames() []string {
	names := make([]string, 0, len(c.Roles))
	for n := range c.Roles {
		names = append(names, n)
	}

	sort.Strings(names)

	return names
}

// checkOverlay refuses a node's value that is no mapping or that carries a key a node may not set.
func checkOverlay(node string, v yaml.Node) error {
	if v.Kind == 0 || (v.Kind == yaml.ScalarNode && v.Tag == "!!null") {
		return nil // "alpha:" with nothing under it
	}

	if v.Kind != yaml.MappingNode {
		return fmt.Errorf("nodes.%s (line %d): a mapping of keys this node sets for itself (%s), or nothing", node, v.Line,
			strings.Join(NodeKeys, ", "))
	}

	for i := 0; i+1 < len(v.Content); i += 2 {
		key := v.Content[i]
		if !slices.Contains(NodeKeys, key.Value) {
			return fmt.Errorf("nodes.%s (line %d): key %q is not set per node; per node: %s. The rest is one declaration "+
				"for every node (a role's place says where it runs)", node, key.Line, key.Value, strings.Join(NodeKeys, ", "))
		}
	}

	return nil
}

// overlay merges the node's keys over the crew's own: a key the node names replaces the top-level one, a key it does
// not name stays.
func (c *Crew) overlay(v yaml.Node, node string) error {
	if v.Kind != yaml.MappingNode || len(v.Content) == 0 {
		return nil
	}

	nodes, home := c.Nodes, c.Home

	if err := v.Decode(c); err != nil {
		var te *yaml.TypeError
		if errors.As(err, &te) {
			return fmt.Errorf("nodes.%s: %s", node, strings.Join(te.Errors, "; "))
		}

		return fmt.Errorf("nodes.%s: %w", node, err)
	}

	c.Nodes, c.Home = nodes, home

	return nil
}
