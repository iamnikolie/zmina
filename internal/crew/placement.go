package crew

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	"plugins", "drop_env", "timezone", "courier", "worktree_gc", "lessons", "spare", "retired", "moved_to"}

// nodeNameRe is what a node's name may be: the mesh's own rule (it goes into addresses and branch names).
var nodeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ThisNodeFile is where the mesh keeps who this machine is; the crew package reads only its name, so it need not know
// the mesh.
func ThisNodeFile() string { return ExpandHome("~/.zmina/mesh/node.json") }

// ThisNode is the name of the node this machine is (zmina node init), "" for a machine whose name cannot be read.
func ThisNode() string {
	name, _ := thisNode()

	return name
}

// thisNode is ThisNode with why it is none: no file, a file that does not read, or a name that is no node's. A crew over nodes
// runs nothing on a machine that has no name, so the reason is what its warning says.
func thisNode() (name, why string) {
	raw, err := os.ReadFile(ThisNodeFile())
	if err != nil {
		return "", err.Error()
	}

	var nf struct {
		Name string `json:"name"`
	}

	if err := json.Unmarshal(raw, &nf); err != nil {
		return "", ThisNodeFile() + ": " + err.Error()
	}

	if !nodeNameRe.MatchString(nf.Name) {
		return "", fmt.Sprintf("%s: %q is no node's name", ThisNodeFile(), nf.Name)
	}

	return nf.Name, ""
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

// Runs reports whether this machine runs anything of the crew: a crew that declares no nodes always does, one that does only
// on a node it lists. A machine whose node name cannot be read (no node.json, one that does not read, a name that is no
// node's) is none, and a node the file does not list is none: neither runs a role, the dispatcher or a job of the crew.
func (c *Crew) Runs() bool { return !c.Spread() || (c.Here != "" && c.HasNode(c.Here)) }

// PlacedHere reports whether this node runs the role. A crew that is not spread runs every role, as before the file could say
// otherwise. A crew that is, on a machine whose node name is not known, runs none: guessing that a machine is a node that
// runs everything would start every role twice. The dispatcher is replicated: it is placed on home and on every node that
// holds a role. A copy (dev#2) is placed where its role is.
func (c *Crew) PlacedHere(r *Role) bool {
	if !c.Spread() || r == nil {
		return true
	}

	if c.Here == "" {
		return false
	}

	if r.Name == c.Dispatcher {
		return c.Here == c.Home || c.Holds(c.Here)
	}

	return c.PlacementOf(r) == c.Here
}

// placedElsewhere reports whether the role is another node's to run: this machine is a node of the file, and the role is not placed on
// it. A crew that is not spread says no, and so does a machine that is no node of the file, which can tell nothing of where a role runs.
func (c *Crew) placedElsewhere(r *Role) bool { return c.Spread() && c.Runs() && !c.PlacedHere(r) }

// WatchPlace is the node that runs the watch: its place, else the crew's home. Empty for a crew that is not spread.
func (c *Crew) WatchPlace(w *Watch) string {
	if !c.Spread() || w == nil {
		return ""
	}

	return or(w.Place, c.Home)
}

// WatchHere reports whether this node runs the watch. A crew that is not spread runs every watch, as before; one that
// is runs each watch on one node only: a listed node that runs a dispatcher (a watch's notes go to its node's
// dispatcher), and on a machine that is no node of the file on none.
func (c *Crew) WatchHere(w *Watch) bool {
	if !c.Spread() || w == nil {
		return true
	}

	return c.Runs() && c.WatchPlace(w) == c.Here && c.WatchHeard(w)
}

// WatchHeard reports whether the node the watch is placed on runs a dispatcher: home, or a node that holds a role. A
// watch on a node with none would queue its notes for nobody.
func (c *Crew) WatchHeard(w *Watch) bool {
	if !c.Spread() || w == nil {
		return true
	}

	return slices.Contains(c.DispatcherNodes(), c.WatchPlace(w))
}

// WatchAway reports whether the watch is another node's to run: this machine is a node of the file, and the watch is
// placed on a different one that can run it. Only then is what this node kept of the watch no longer its own.
func (c *Crew) WatchAway(w *Watch) bool {
	return c.Spread() && w != nil && c.Runs() && c.WatchPlace(w) != c.Here && c.WatchHeard(w)
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
func (c *Crew) placement(here, why string, warn func(string)) error {
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

		for _, name := range c.WatchNames() {
			if c.Watches[name] != nil && c.Watches[name].Place != "" {
				warn(fmt.Sprintf("watch %s: place: has no meaning without nodes:, ignored", name))
			}
		}

		return nil
	}

	names := c.NodeNames()
	c.Here = here

	for _, n := range names {
		if !nodeNameRe.MatchString(n) {
			return fmt.Errorf("nodes: %q is not a node name (letters, digits, '.', '_' and '-')", n)
		}

		// A key that is not per node is a hard error where it matters (this node's own overlay: the file is refused here) and a
		// warning everywhere else, so one node's typo, or a key of a newer zmina, does not stop the nodes it is not for.
		if err := checkOverlay(n, c.Nodes[n]); err != nil {
			if n == here {
				return err
			}

			warn(err.Error() + "; ignored here (the node it is for refuses the file)")
		}
	}

	if c.Home == "" {
		return errors.New("home: names the node that runs the dispatcher; it is required once nodes: is set " +
			"(nodes: " + strings.Join(names, ", ") + ")")
	}

	if !c.HasNode(c.Home) {
		return fmt.Errorf("home %q is not one of nodes: (%s)", c.Home, strings.Join(names, ", "))
	}

	// What follows asks which node holds which role, and the answer reads the dispatcher's name and each role's own, which are not there
	// before the defaults are completed (a file with no dispatcher: has none, and a role has no name yet). A dispatcher left as "" is
	// the name of every role then, so no node would hold any and a watch on a node with roles would be warned of as unheard. They are put
	// in here as complete puts them in, with the same value.
	c.Dispatcher = or(c.Dispatcher, defaultDispatcher)

	for name, r := range c.Roles {
		if r != nil {
			r.Name = name
		}
	}

	dispatcher := c.Dispatcher

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

	for _, name := range c.WatchNames() {
		w := c.Watches[name]
		if w == nil || w.Place == "" {
			continue
		}

		switch {
		case !c.HasNode(w.Place):
			warn(fmt.Sprintf("watch %s: place %q is not one of nodes: (%s); the watch runs on no node", name, w.Place,
				strings.Join(names, ", ")))
		case !c.WatchHeard(w):
			warn(fmt.Sprintf("watch %s: place %s holds no role of the crew, so no dispatcher runs there to hear it; "+
				"the watch runs on no node", name, w.Place))
		}
	}

	switch {
	case here == "":
		warn("this machine's node name cannot be read (" + why + "): a crew over nodes runs nothing here, not a role or the " +
			"dispatcher, until it is; zmina node init, or mend ~/.zmina/mesh/node.json")

		return nil
	case !c.HasNode(here):
		warn(fmt.Sprintf("this node (%s) is not one of nodes: (%s), and a node's name is matched exactly: it runs nothing of "+
			"this crew, not a role or the dispatcher, until the file lists it", here, strings.Join(names, ", ")))

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

// ownDirs are the names zmina keeps under a crew's directory, and under ~/.zmina where the default crew keeps its files, that no
// method of Layout names: the stores that live beside the layout (the codex adapter's registry, the report store, the tells' receipts,
// chat files, lessons, a tick's pruned log, the recovery context, the records of results and checkpoints, the packets of a handoff, a
// release and a restart, the retention archive), the agents' boot documents, the mesh's directory and the node's own. A test walks
// Layout and says that none of its names is missing from ReservedKeys; this list is for the rest, and a name added to it is a name a crew
// made by a push can no longer take.
var ownDirs = []string{"mesh", "mesh-ui", "agents", "archive", "bin", "codex", "messages", "tells", "chat-files", "lessons", "pruned",
	"context", "results", "checkpoints", "handoffs", "releases", "restarts"}

// ReservedKeys are the names a crew made by a push (zmina crew push --create) may not take: every name zmina keeps in a crew's directory
// or under ~/.zmina, where the default crew keeps its files and the other crews' directories sit beside them. They are those of
// Layout (the first part of each of its paths, as a crew's directory holds them) and ownDirs.
func ReservedKeys() []string {
	l := Layout{Dir: "x"}
	keys := slices.Clone(ownDirs)

	for _, p := range []string{l.State(), l.History(), l.Lock(), l.CourierLock(), l.LockHolder(), l.Tasks(), l.Later(), l.Moves(), l.Alarms(),
		l.Asks(), l.Inbox(), l.Events(), l.Reads(), l.ReadsRate(), l.Limits(), l.Screens(), l.ScreenRules(), l.ScreenRulesTold(),
		l.LaterSweep(), l.Hooks(), l.Delivery(), l.RotationsDir(), l.RotationsFailed(), l.Worktrees(), l.Watch("x"), l.CourierLog(),
		l.TickLog(), l.Plugins()} {
		keys = append(keys, strings.Split(filepath.ToSlash(strings.TrimPrefix(p, "x"+string(filepath.Separator))), "/")[0])
	}

	slices.Sort(keys)

	return slices.Compact(keys)
}

// NewCrewKey says whether a name may be the key of a crew made by a push: lower-case words joined by dashes, as a crew's
// name is, and none of the ReservedKeys. "default" is the node's default crew, which a push may make as well.
func NewCrewKey(key string) bool {
	if key == "default" {
		return true
	}

	return roleRe.MatchString(key) && !slices.Contains(ReservedKeys(), key)
}
