package crew

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		"a machine that is no node runs every role":           {"", map[string]bool{"dispatcher": true, "ops": true, "dev": true}},
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

func TestAKeyThatIsNotPerNodeIsALoadError(t *testing.T) {
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

			_, err := LoadFor(write(t, body), "beta")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "nodes.beta (line 5): key "+`"`+strings.SplitN(strings.TrimSpace(snippet), ":", 2)[0]+`"`+" is not set per node")
			assert.Contains(t, err.Error(), "per node: prefix, claude")

			// The same file is refused on every node: it is one declaration.
			_, err = LoadFor(write(t, body), "alpha")
			assert.Error(t, err)
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

// Load asks the machine who it is only for a file that declares nodes:.
func TestLoadReadsTheNodeNameOfThisMachine(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", t.TempDir())

	path := write(t, spreadFile)

	c, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, c.Here, "a machine that is no node")
	assert.True(t, c.PlacedHere(c.Roles["dev"]))

	require.NoError(t, os.MkdirAll(filepath.Dir(ThisNodeFile()), 0o700))
	require.NoError(t, os.WriteFile(ThisNodeFile(), []byte(`{"name":"beta"}`), 0o600))

	c, err = Load(path)
	require.NoError(t, err)
	assert.Equal(t, "beta", c.Here)
	assert.Equal(t, "be-", c.Prefix)
	assert.True(t, c.PlacedHere(c.Roles["dev"]))
	assert.False(t, c.PlacedHere(c.Roles["ops"]))

	// A node.json that does not say a plain name is no node.
	require.NoError(t, os.WriteFile(ThisNodeFile(), []byte(`{"name":"b/../x"}`), 0o600))
	assert.Empty(t, ThisNode())
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
		"edit the file on every node until zmina move exists")
	assert.ErrorContains(t, e.Add("qa", map[string]string{"place": "alpha"}), "until zmina move exists")

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
