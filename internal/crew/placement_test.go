package crew

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// spreadFile is a crew over two nodes: alex's dev runs on beta, everything else on home.
const spreadFile = `name: acme
namespace: /ns
home: alpha
nodes:
  alpha: {prefix: al-, claude: /opt/alpha/claude}
  beta:
    prefix: be-
    claude: /opt/beta/claude
    notify: [push, --x]
notify: [bell]
roles:
  dispatcher: {}
  dev: {place: beta}
  ops: {}
`

func loadFor(t *testing.T, body, node string) *Crew {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	c, err := LoadFor(write(t, body), node)
	require.NoError(t, err)

	return c
}

// A crew file with no nodes: loads as it did before the keys existed: no node is asked, nothing is merged, every role
// is placed here, whoever the machine is.
func TestCrewWithoutNodesIsUnchanged(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	body := "name: acme\nnamespace: /ns\nnotify: [bell]\nroles:\n  dispatcher: {}\n  dev: {}\n"
	path := write(t, body)

	a, err := Load(path)
	require.NoError(t, err)

	b, err := LoadFor(path, "alpha")
	require.NoError(t, err)

	assert.Equal(t, a, b, "the node a machine is changes nothing for a crew that declares none")
	assert.False(t, a.Spread())
	assert.Empty(t, a.Here)
	assert.Empty(t, a.Warnings)
	assert.Equal(t, "acme-", a.Prefix)

	for _, name := range a.Names() {
		assert.True(t, a.PlacedHere(a.Roles[name]), name)
	}

	assert.Empty(t, a.PlacementOf(a.Roles["dev"]))
	assert.Empty(t, a.AwayRoles())
	assert.Empty(t, a.Unplaced())
	assert.Equal(t, SumOf([]byte(body)), a.Sum())
}

// nodes: {} says nothing, and home and place with no nodes mean nothing: warned of, never obeyed.
func TestHomeAndPlaceWithoutNodesAreIgnoredWithAWarning(t *testing.T) {
	c := loadFor(t, "home: alpha\nnodes: {}\nroles:\n  dispatcher: {}\n  dev: {place: beta}\n", "alpha")

	assert.False(t, c.Spread())
	assert.True(t, c.PlacedHere(c.Roles["dev"]))
	require.Len(t, c.Warnings, 2)
	assert.Contains(t, c.Warnings[0], "home: has no meaning without nodes:")
	assert.Contains(t, c.Warnings[1], "role dev: place: has no meaning without nodes:")
}

func TestPlacedHereTable(t *testing.T) {
	for name, c := range map[string]struct {
		node string
		want map[string]bool
	}{
		"home runs the unplaced roles and the dispatcher":     {"alpha", map[string]bool{"dispatcher": true, "ops": true, "dev": false}},
		"the node that holds a role runs it and a dispatcher": {"beta", map[string]bool{"dispatcher": true, "ops": false, "dev": true}},
		"a node the file does not list runs nothing":          {"gamma", map[string]bool{"dispatcher": false, "ops": false, "dev": false}},
		"a machine whose node name is unknown runs nothing":   {"", map[string]bool{"dispatcher": false, "ops": false, "dev": false}},
	} {
		t.Run(name, func(t *testing.T) {
			cr := loadFor(t, spreadFile, c.node)
			require.True(t, cr.Spread())

			for role, want := range c.want {
				assert.Equal(t, want, cr.PlacedHere(cr.Roles[role]), role)
			}
		})
	}
}

// The dispatcher is replicated: with no role of a node's own it runs on home alone, and once a role is placed on another
// node that node runs one too.
func TestDispatcherRunsOnHomeAndOnEveryNodeThatHoldsARole(t *testing.T) {
	c := loadFor(t, "home: alpha\nnodes: {alpha: {}, beta: {}, gamma: {}}\nroles:\n  dispatcher: {}\n  dev: {place: beta}\n", "gamma")

	assert.Equal(t, []string{"alpha", "beta"}, c.DispatcherNodes())
	assert.False(t, c.PlacedHere(c.Roles["dispatcher"]), "gamma holds no role and is not home")
	assert.Equal(t, "alpha, beta", c.WhereIs(c.Roles["dispatcher"]))
	assert.Equal(t, "beta", c.WhereIs(c.Roles["dev"]))
	assert.Equal(t, []string{"dispatcher", "dev"}, c.AwayRoles())
}

// A copy of a role is placed where its role is.
func TestACopyIsPlacedWhereItsRoleIs(t *testing.T) {
	c := loadFor(t, spreadFile, "beta")

	assert.True(t, c.PlacedHere(c.Roles["dev"].Instance(2)))
	assert.False(t, c.PlacedHere(c.Roles["ops"].Instance(2)))
}

func TestPlaceThatIsNoNodeRunsNowhereWithAWarning(t *testing.T) {
	c := loadFor(t, "home: alpha\nnodes: {alpha: {}, beta: {}}\nroles:\n  dispatcher: {}\n  dev: {place: gamma}\n  ops: {}\n", "alpha")

	require.Len(t, c.Warnings, 1)
	assert.Contains(t, c.Warnings[0], `role dev: place "gamma" is not one of nodes: (alpha, beta); the role runs on no node`)
	assert.Equal(t, []string{"dev"}, c.Unplaced())

	for _, node := range []string{"alpha", "beta"} {
		assert.False(t, loadFor(t, "home: alpha\nnodes: {alpha: {}, beta: {}}\nroles:\n  dispatcher: {}\n  dev: {place: gamma}\n", node).
			PlacedHere(c.Roles["dev"]), node)
	}
}

func TestThePlaceOfTheDispatcherIsIgnored(t *testing.T) {
	c := loadFor(t, "home: alpha\nnodes: {alpha: {}, beta: {}}\nroles:\n  dispatcher: {place: beta}\n  dev: {}\n", "alpha")

	require.Len(t, c.Warnings, 1)
	assert.Contains(t, c.Warnings[0], "role dispatcher is the dispatcher: it runs on home (alpha)")
	assert.True(t, c.PlacedHere(c.Roles["dispatcher"]))
}

func TestHomeIsRequiredOnceNodesIsSet(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	_, err := LoadFor(write(t, "nodes: {alpha: {}, beta: {}}\nroles:\n  dispatcher: {}\n"), "alpha")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "home: names the node that runs the dispatcher")

	_, err = LoadFor(write(t, "home: gamma\nnodes: {alpha: {}, beta: {}}\nroles:\n  dispatcher: {}\n"), "alpha")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `home "gamma" is not one of nodes: (alpha, beta)`)
}

func TestNodeOverlayIsMergedOverTheTopLevelKeys(t *testing.T) {
	alpha, beta := loadFor(t, spreadFile, "alpha"), loadFor(t, spreadFile, "beta")

	assert.Equal(t, "/opt/alpha/claude", alpha.Claude)
	assert.Equal(t, "/opt/beta/claude", beta.Claude)
	assert.Equal(t, "al-", alpha.Prefix)
	assert.Equal(t, "be-", beta.Prefix)

	// Every default that follows the prefix follows the overlaid one.
	assert.Equal(t, "al-ops", alpha.Roles["ops"].Session)
	assert.Equal(t, "be-dev", beta.Roles["dev"].Session)

	// A key a node names replaces the top-level one; a key it does not name stays.
	assert.Equal(t, []string{"bell"}, alpha.Notify.Argv, "alpha names no notify: the crew's stays")
	assert.Equal(t, []string{"push", "--x"}, beta.Notify.Argv, "beta's own notify replaces it")
	assert.Equal(t, "/ns", beta.Namespace)

	// A node the file does not list, and a machine that is none, get the top-level keys.
	other := loadFor(t, spreadFile, "gamma")
	assert.Equal(t, "acme-", other.Prefix)
	assert.Equal(t, "claude", other.Claude)
	assert.Equal(t, "acme-", loadFor(t, spreadFile, "").Prefix)

	// The declaration itself is the same on every node.
	assert.Equal(t, alpha.Nodes, beta.Nodes)
	assert.Equal(t, "alpha", beta.Home)
}

// A key that is not set per node is a hard error where it matters, in the overlay of the node that reads the file, and a warning
// on every other node (and where the node is not known): one node's typo, or a key a newer zmina added, does not stop the
// nodes it is not for.
func TestAKeyThatIsNotPerNodeIsAnErrorForItsNodeAndAWarningForTheOthers(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	for key, snippet := range map[string]string{
		"roles":      "    roles: {dev: {}}",
		"place":      "    place: beta",
		"home":       "    home: alpha",
		"nodes":      "    nodes: {}",
		"dispatcher": "    dispatcher: dev",
		"stuck":      "    stuck_minutes: 5",
		"typo":       "    prefx: be-",
	} {
		t.Run(key, func(t *testing.T) {
			body := "home: alpha\nnodes:\n  alpha: {}\n  beta:\n" + snippet + "\nroles:\n  dispatcher: {}\n  dev: {}\n"
			word := `"` + strings.SplitN(strings.TrimSpace(snippet), ":", 2)[0] + `"`

			// The node it is under: refused, naming the key and its line.
			_, err := LoadFor(write(t, body), "beta")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "nodes.beta (line 5): key "+word+" is not set per node")
			assert.Contains(t, err.Error(), "per node: prefix, claude")

			// Every other node, and a machine that is none: loads, warned.
			for _, node := range []string{"alpha", "gamma", ""} {
				c, err := LoadFor(write(t, body), node)
				require.NoError(t, err, node)

				found := false

				for _, w := range c.Warnings {
					if strings.Contains(w, "nodes.beta (line 5): key "+word+" is not set per node") && strings.Contains(w, "ignored here") {
						found = true
					}
				}

				assert.True(t, found, "%q: warned of: %v", node, c.Warnings)
			}
		})
	}
}

func TestAnOverlayThatIsNoMappingOrDoesNotDecodeIsALoadError(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	_, err := LoadFor(write(t, "home: alpha\nnodes:\n  alpha: [prefix]\nroles:\n  dispatcher: {}\n"), "alpha")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nodes.alpha (line 3): a mapping")

	_, err = LoadFor(write(t, "home: alpha\nnodes:\n  alpha: {notify_on: 3}\nroles:\n  dispatcher: {}\n"), "alpha")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nodes.alpha:")

	_, err = LoadFor(write(t, "home: alpha\nnodes:\n  'a b': {}\n  alpha: {}\nroles:\n  dispatcher: {}\n"), "alpha")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"a b" is not a node name`)

	// A node with nothing under it is a member with nothing of its own.
	c := loadFor(t, "home: alpha\nnodes:\n  alpha:\n  beta: {}\nroles:\n  dispatcher: {}\n", "alpha")
	assert.True(t, c.HasNode("alpha"))
}

// Load asks the machine who it is only for a file that declares nodes:. A machine whose name cannot be read, for any reason,
// runs nothing of such a crew (guessing that it is a node that runs everything would start every role twice), and says so
// on every load.
func TestLoadReadsTheNodeNameOfThisMachineAndFailsClosed(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", t.TempDir())

	path := write(t, spreadFile)

	failsClosed := func(why string) {
		t.Helper()

		c, err := Load(path)
		require.NoError(t, err, why)
		assert.Empty(t, c.Here, why)
		assert.False(t, c.Runs(), why)

		for _, name := range c.Names() {
			assert.False(t, c.PlacedHere(c.Roles[name]), why+": "+name)
		}

		require.NotEmpty(t, c.Warnings, why)
		assert.Contains(t, c.Warnings[len(c.Warnings)-1], "node name cannot be read", why)
		assert.Contains(t, c.Warnings[len(c.Warnings)-1], "runs nothing here", why)
		assert.Contains(t, c.Warnings[len(c.Warnings)-1], "zmina node init", why)
	}

	failsClosed("no node.json")

	require.NoError(t, os.MkdirAll(filepath.Dir(ThisNodeFile()), 0o700))

	for why, body := range map[string]string{
		"bad json":       `{"name":`,
		"no name":        `{}`,
		"a bad name":     `{"name":"b/../x"}`,
		"a name of path": `{"name":".ssh"}`,
		"a number":       `{"name":3}`,
	} {
		require.NoError(t, os.WriteFile(ThisNodeFile(), []byte(body), 0o600))
		failsClosed(why)
		assert.Empty(t, ThisNode(), why)
	}

	// Unreadable: a directory where the file should be.
	require.NoError(t, os.Remove(ThisNodeFile()))
	require.NoError(t, os.Mkdir(ThisNodeFile(), 0o700))
	failsClosed("unreadable")
	require.NoError(t, os.Remove(ThisNodeFile()))

	require.NoError(t, os.WriteFile(ThisNodeFile(), []byte(`{"name":"beta"}`), 0o600))

	c, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "beta", c.Here)
	assert.True(t, c.Runs())
	assert.Equal(t, "be-", c.Prefix)
	assert.True(t, c.PlacedHere(c.Roles["dev"]))
	assert.False(t, c.PlacedHere(c.Roles["ops"]))
	assert.Empty(t, c.Warnings)
}

// A node the file does not list runs nothing, says so on every load, and is not told to be a node by its name differing in case.
func TestANodeThatIsNotListedRunsNothingAndSaysSo(t *testing.T) {
	c := loadFor(t, spreadFile, "Alpha")

	assert.False(t, c.Runs())

	for _, name := range c.Names() {
		assert.False(t, c.PlacedHere(c.Roles[name]), name)
	}

	require.Len(t, c.Warnings, 1)
	assert.Contains(t, c.Warnings[0], "this node (Alpha) is not one of nodes: (alpha, beta)")
	assert.Contains(t, c.Warnings[0], "matched exactly")
	assert.Contains(t, c.Warnings[0], "runs nothing of this crew, not a role or the dispatcher")

	// A listed node runs, and a crew that declares no nodes always does.
	assert.True(t, loadFor(t, spreadFile, "alpha").Runs())
	assert.True(t, loadFor(t, "roles:\n  dispatcher: {}\n", "").Runs())
	assert.True(t, loadFor(t, "roles:\n  dispatcher: {}\n", "anything").Runs())
}

// A node's own notify replaces the crew's whatever form either has, and "" or [] says none; a key with nothing under it
// changes nothing. lessons and plugins are per node too, so a job can be kept to one node.
func TestNodeOverlayReplacesNotifyLessonsAndPluginsWhole(t *testing.T) {
	file := func(node string) string {
		return `home: alpha
notify: [push, --all]
lessons: on
plugins:
  - {name: pager, on: [ask]}
nodes:
  alpha:
` + node + `
  beta: {}
roles:
  dispatcher: {}
`
	}

	for name, c := range map[string]struct {
		overlay string
		check   func(t *testing.T, c *Crew)
	}{
		"a string over a list": {"    notify: 'sh -c \"echo $1\"'", func(t *testing.T, c *Crew) {
			assert.Equal(t, `sh -c "echo $1"`, c.Notify.Shell)
			assert.Nil(t, c.Notify.Argv, "the list is gone, so Line is the string's")
			assert.Equal(t, []string{"/bin/sh", "-c", `sh -c "echo $1"`}, c.Notify.Line())
		}},
		"a list over a list": {"    notify: [other]", func(t *testing.T, c *Crew) { assert.Equal(t, []string{"other"}, c.Notify.Argv) }},
		"an empty string says none": {"    notify: ''", func(t *testing.T, c *Crew) {
			assert.True(t, c.Notify.Empty())
		}},
		"an empty list says none": {"    notify: []", func(t *testing.T, c *Crew) { assert.True(t, c.Notify.Empty()) }},
		"a key with nothing under it changes nothing": {"    notify:", func(t *testing.T, c *Crew) {
			assert.Equal(t, []string{"push", "--all"}, c.Notify.Argv)
		}},
		"lessons off for this node": {"    lessons: off", func(t *testing.T, c *Crew) { assert.False(t, c.Lessons.On) }},
		"no plugin for this node":   {"    plugins: []", func(t *testing.T, c *Crew) { assert.Empty(t, c.Plugins) }},
		"nothing named: the crew's": {"    prefix: al-", func(t *testing.T, c *Crew) {
			assert.True(t, c.Lessons.On)
			require.Len(t, c.Plugins, 1)
			assert.Equal(t, []string{"push", "--all"}, c.Notify.Argv)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			c.check(t, loadFor(t, file(c.overlay), "alpha"))

			// The other node keeps the crew's.
			other := loadFor(t, file(c.overlay), "beta")
			assert.True(t, other.Lessons.On)
			assert.Equal(t, []string{"push", "--all"}, other.Notify.Argv)
			assert.Len(t, other.Plugins, 1)
		})
	}
}

func TestRoleTellNamesTheNodeAndTheCrew(t *testing.T) {
	named := loadFor(t, spreadFile, "alpha")
	assert.Equal(t, "beta/acme", named.RoleTell(named.Roles["dev"]))
	assert.Equal(t, "alpha/acme", named.RoleTell(named.Roles["dispatcher"]), "the dispatcher: home")

	def := loadFor(t, "home: alpha\nnodes: {alpha: {}, beta: {}}\nroles:\n  dispatcher: {}\n  dev: {place: beta}\n", "alpha")
	assert.Equal(t, "beta", def.RoleTell(def.Roles["dev"]), "the default crew is the node alone")
}

// retire writes inside nodes.<this node> when the file declares nodes:, so it does not travel with the file; a file with no
// nodes: gets the top-level keys, as before.
func TestRetireWritesInsideTheNodeOfAFileThatDeclaresNodes(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	path := write(t, "# crew\nhome: alpha\nnodes:\n  alpha: {prefix: al-}\n  beta: {}\nroles:\n  dispatcher: {}\n  dev: {}\n")

	e, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, e.SetNodeKey("beta", "retired", "2026-11-05"))
	require.NoError(t, e.SetNodeKey("beta", "moved_to", "box/acme"))
	require.NoError(t, e.SaveCrewKeys())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "# crew")
	assert.NotRegexp(t, `(?m)^retired:`, string(raw), "no top-level mark")

	beta, err := LoadFor(path, "beta")
	require.NoError(t, err)
	assert.True(t, beta.IsRetired())
	assert.Equal(t, "box/acme", beta.MovedTo)

	alpha, err := LoadFor(path, "alpha")
	require.NoError(t, err)
	assert.False(t, alpha.IsRetired(), "the mark is beta's")
	assert.Equal(t, "al-", alpha.Prefix, "alpha's own overlay is untouched")

	// Off: the node's keys go.
	e, err = Open(path)
	require.NoError(t, err)
	require.NoError(t, e.SetNodeKey("beta", "retired", ""))
	require.NoError(t, e.SetNodeKey("beta", "moved_to", ""))
	require.NoError(t, e.SaveCrewKeys())

	beta, err = LoadFor(path, "beta")
	require.NoError(t, err)
	assert.False(t, beta.IsRetired())

	// A node the file does not list cannot keep a key.
	e, err = Open(path)
	require.NoError(t, err)
	assert.ErrorContains(t, e.SetNodeKey("gamma", "retired", "2026-11-05"), "not one of nodes: (alpha, beta)")

	// A machine that is no node, and a crew that declares no nodes, write at the top, as before.
	e, err = Open(path)
	require.NoError(t, err)
	require.NoError(t, e.SetNodeKey("", "retired", "2026-11-06"))
	require.NoError(t, e.SaveCrewKeys())

	c, err := LoadFor(path, "alpha")
	require.NoError(t, err)
	assert.True(t, c.IsRetired())

	plain := write(t, "roles:\n  dispatcher: {}\n")
	e, err = Open(plain)
	require.NoError(t, err)
	require.NoError(t, e.SetNodeKey("alpha", "retired", "2026-11-05"))
	require.NoError(t, e.SaveCrewKeys())

	c, err = Load(plain)
	require.NoError(t, err)
	assert.Equal(t, "2026-11-05", c.Retired)
}

// A top-level retired mark is no node's to unset by deleting a key: this node says it is not retired.
func TestNodeCanClearATopLevelMark(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	path := write(t, "home: alpha\nretired: 2026-11-01\nnodes:\n  alpha: {}\n  beta: {}\nroles:\n  dispatcher: {}\n")

	e, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, e.SetNodeKey("alpha", "retired", ""))
	require.NoError(t, e.SaveCrewKeys())

	alpha, err := LoadFor(path, "alpha")
	require.NoError(t, err)
	assert.False(t, alpha.IsRetired())

	beta, err := LoadFor(path, "beta")
	require.NoError(t, err)
	assert.True(t, beta.IsRetired(), "the mark stays for the node that did not clear it")
}

func TestSeatSetPlaceIsRefused(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	spread := write(t, spreadFile)
	e, err := Open(spread)
	require.NoError(t, err)
	assert.ErrorContains(t, e.Set("dev", map[string]string{"place": "alpha"}),
		"use zmina move <role> <node>")
	assert.ErrorContains(t, e.Add("qa", map[string]string{"place": "alpha"}), "use zmina move <role> <node>")

	// Not a seat key either on a crew that declares no nodes: say what to do.
	plain := write(t, "roles:\n  dispatcher: {}\n  dev: {}\n")
	e, err = Open(plain)
	require.NoError(t, err)
	assert.ErrorContains(t, e.Set("dev", map[string]string{"place": "alpha"}), "declare nodes: and home:")

	// Other keys of a seat of a spread crew still go.
	e, err = Open(spread)
	require.NoError(t, err)
	require.NoError(t, e.Set("dev", map[string]string{"model": "opus"}))
}

// A detector is a listener: in a crew over nodes it runs on one node, home unless it names another, and on a machine
// whose name is not known on none. A crew that is not spread runs every watch, and a place there is warned of.
func TestAWatchRunsOnOneNode(t *testing.T) {
	body := "home: alpha\nnodes: {alpha: {}, beta: {}}\nroles:\n  dispatcher: {}\n  dev: {place: beta}\n" +
		"watch:\n  mrs: {kind: exec, every: 1m, run: [x]}\n  cards: {kind: exec, every: 1m, run: [x], place: beta}\n" +
		"  lost: {kind: exec, every: 1m, run: [x], place: gamma}\n"

	alpha, beta, none := loadFor(t, body, "alpha"), loadFor(t, body, "beta"), loadFor(t, body, "")

	assert.True(t, alpha.WatchHere(alpha.Watches["mrs"]))
	assert.False(t, alpha.WatchHere(alpha.Watches["cards"]))
	assert.False(t, beta.WatchHere(beta.Watches["mrs"]))
	assert.True(t, beta.WatchHere(beta.Watches["cards"]))
	assert.Equal(t, "alpha", beta.WatchPlace(beta.Watches["mrs"]))
	assert.Equal(t, "beta", alpha.WatchPlace(alpha.Watches["cards"]))

	for _, c := range []*Crew{alpha, beta, none} {
		assert.False(t, c.WatchHere(c.Watches["lost"]))
		assert.Contains(t, strings.Join(c.Warnings, "\n"),
			`watch lost: place "gamma" is not one of nodes: (alpha, beta); the watch runs on no node`)
	}

	for _, name := range none.WatchNames() {
		assert.False(t, none.WatchHere(none.Watches[name]), name)
	}

	flat := loadFor(t, "roles:\n  dispatcher: {}\nwatch:\n  mrs: {kind: exec, every: 1m, run: [x], place: beta}\n", "alpha")
	assert.True(t, flat.WatchHere(flat.Watches["mrs"]))
	assert.Empty(t, flat.WatchPlace(flat.Watches["mrs"]))
	assert.Contains(t, strings.Join(flat.Warnings, "\n"), "watch mrs: place: has no meaning without nodes:, ignored")
}

// A watch placed on a node that holds a role is heard there, so loading the file says nothing of it, whether or not the file names its
// dispatcher: the load asks which node holds which role before the defaults are completed, and must give the answer complete's do. A node
// that holds no role is still warned of, with the reason, and a role placed on it silences that.
func TestAValidWatchPlaceIsNotWarnedOfAtLoad(t *testing.T) {
	const watches = "watch:\n  cards: {kind: exec, every: 1m, run: [x], place: beta}\n"

	for name, c := range map[string]struct{ head, roles string }{
		"no dispatcher key":      {"", "  dispatcher: {}\n  dev: {place: beta}\n"},
		"the default's own name": {"dispatcher: dispatcher\n", "  dispatcher: {}\n  dev: {place: beta}\n"},
		"another dispatcher":     {"dispatcher: lead\n", "  lead: {}\n  dev: {place: beta}\n"},
	} {
		t.Run(name, func(t *testing.T) {
			body := c.head + "home: alpha\nnodes: {alpha: {}, beta: {}}\nroles:\n" + c.roles + watches

			for _, node := range []string{"alpha", "beta", ""} {
				got := loadFor(t, body, node)

				assert.NotContains(t, strings.Join(got.Warnings, "\n"), "holds no role", node)
				assert.NotContains(t, strings.Join(got.Warnings, "\n"), "runs on no node", node)
				assert.True(t, got.WatchHeard(got.Watches["cards"]), node)
			}
		})
	}

	// The same watch with no role on beta is warned of, for that reason: beta holds nothing, while home holds the role.
	body := "home: alpha\nnodes: {alpha: {}, beta: {}}\nroles:\n  dispatcher: {}\n  dev: {}\n" + watches
	got := loadFor(t, body, "alpha")

	assert.Contains(t, strings.Join(got.Warnings, "\n"), "watch cards: place beta holds no role of the crew, so no dispatcher runs there to hear it")
	assert.False(t, got.Holds("beta"))
	assert.True(t, got.Holds("alpha"))
	assert.False(t, got.WatchHeard(got.Watches["cards"]))

	// The dispatcher's own place is ignored, so it does not make beta hold a role, with the dispatcher's name left to the default or not.
	for _, head := range []string{"", "dispatcher: dispatcher\n"} {
		body = head + "home: alpha\nnodes: {alpha: {}, beta: {}}\nroles:\n  dispatcher: {place: beta}\n  dev: {}\n" + watches
		got = loadFor(t, body, "alpha")

		assert.Contains(t, strings.Join(got.Warnings, "\n"), "watch cards: place beta holds no role of the crew", head)
		assert.Contains(t, strings.Join(got.Warnings, "\n"), "role dispatcher is the dispatcher: it runs on home (alpha)", head)
	}

	home := loadFor(t, "home: alpha\nnodes: {alpha: {}, beta: {}}\nroles:\n  dispatcher: {}\n  dev: {}\nwatch:\n  mrs: {kind: exec, every: 1m, run: [x]}\n", "alpha")
	assert.NotContains(t, strings.Join(home.Warnings, "\n"), "holds no role", "a watch on home is heard there")
}

// A role placed on another node starts there and never here: a directory it names that is not there is that node's to have, so this node
// does not call it a problem (the check of a seat edit leaves it alone too). A role placed here, and any role of a crew on one node, are
// held to theirs. A machine that is no node of the file can tell nothing of where a role runs, and holds every role to its directory.
func TestADirOfARolePlacedElsewhereIsNotAProblemOfThisNode(t *testing.T) {
	root := t.TempDir()
	devDir, opsDir := filepath.Join(root, "only-on-beta"), filepath.Join(root, "only-on-alpha")

	body := "home: alpha\nnodes: {alpha: {}, beta: {}}\nroles:\n  dispatcher: {}\n  dev: {place: beta, dir: " + devDir + "}\n  ops: {dir: " + opsDir + "}\n"

	problem := func(c *Crew, role string) bool { return c.Roles[role].DirErr() != nil }

	alpha, beta, none := loadFor(t, body, "alpha"), loadFor(t, body, "beta"), loadFor(t, body, "")

	assert.False(t, problem(alpha, "dev"), "dev is beta's")
	assert.True(t, problem(alpha, "ops"), "ops is placed here")
	assert.True(t, problem(beta, "dev"))
	assert.False(t, problem(beta, "ops"), "ops is alpha's")
	assert.True(t, problem(none, "dev"), "a machine that is no node holds every role to its directory")
	assert.True(t, problem(none, "ops"))
	assert.ErrorContains(t, alpha.Roles["ops"].DirErr(), "role ops: dir "+opsDir+" is not a directory")

	flat := loadFor(t, "roles:\n  dispatcher: {}\n  dev: {dir: "+devDir+"}\n", "alpha")
	assert.True(t, problem(flat, "dev"), "a crew on one node")

	// The directory made, the role's problem is gone where it was one.
	require.NoError(t, os.Mkdir(opsDir, 0o700))
	assert.False(t, problem(loadFor(t, body, "alpha"), "ops"))
}

// A place on a watch that has none keeps the watch's fingerprint: an upgrade does not look like a changed watch.
func TestAWatchWithoutPlaceKeepsItsFingerprint(t *testing.T) {
	c := loadFor(t, "roles:\n  dispatcher: {}\nwatch:\n  mrs: {kind: exec, every: 1m, run: [x]}\n", "")

	raw, err := yaml.Marshal(c.Watches["mrs"])
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "place")
}
