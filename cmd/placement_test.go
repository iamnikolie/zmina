package cmd

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/shift"
)

// spreadDecl is the declaration of a crew over two nodes, appended after the roles of a test crew.
const spreadDecl = "home: alpha\nnodes:\n  alpha: {}\n  beta: {}\n"

// asNode makes this machine the node of that name for the test: its node.json says who it is.
func asNode(t *testing.T, name string) {
	t.Helper()

	dir := filepath.Dir(crew.ThisNodeFile())
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(crew.ThisNodeFile(), []byte(`{"name":"`+name+`"}`), 0o600))
}

// spreadCrew is a crew of dev (beta's), ops (home's) and the dispatcher, read on the node named.
func spreadCrew(t *testing.T, node string) string {
	t.Helper()

	file := testCrew(t, "  dev:\n    place: beta\n  ops: {}\n"+spreadDecl)
	asNode(t, node)

	return file
}

func needTmux(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
}

// up starts the roles of this node and none placed elsewhere: --all skips them, and naming one is refused with a line that
// says where it runs.
func TestUpStartsOnlyRolesPlacedHere(t *testing.T) {
	needTmux(t)

	file := spreadCrew(t, "alpha")

	_, _, err := zm(t, file, "up", "dev")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dev is placed on beta, not on this node (alpha): run it there, or zmina tell beta/")
	assert.False(t, isUp(t, file, "dev"))

	_, _, err = zm(t, file, "up", "--all")
	require.NoError(t, err)
	assert.True(t, isUp(t, file, "ops"))
	assert.True(t, isUp(t, file, "dispatcher"))
	assert.False(t, isUp(t, file, "dev"))

	// The same file on beta: dev and a dispatcher, not ops.
	file2 := spreadCrew(t, "beta")
	_, _, err = zm(t, file2, "up", "ops")
	require.ErrorContains(t, err, "ops is placed on alpha")
}

// A task or a line for a role placed on another node is refused naming the dispatcher to tell, and writes nothing here.
func TestTaskNewAndSendRefuseARolePlacedElsewhere(t *testing.T) {
	file := spreadCrew(t, "alpha")

	_, _, err := zm(t, file, "task", "new", "dev", "fix the form")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dev is placed on beta, not on this node (alpha)")
	assert.Contains(t, err.Error(), "zmina tell beta/")
	assert.Zero(t, taskCount(t, file), "no task on this node's ledger")

	_, _, err = zm(t, file, "send", "dev", "hello")
	require.ErrorContains(t, err, "dev is placed on beta")

	_, _, err = zm(t, file, "send", "--when-idle", "dev", "hello")
	require.ErrorContains(t, err, "dev is placed on beta")
	assert.Empty(t, inboxOf(t, file, "dev"), "nothing is queued for a role that never runs here")

	// A role of this node is given a task as before.
	out, _, err := zm(t, file, "task", "new", "ops", "fix the form")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · ops\n", out)
}

func TestLsSaysWhereARoleIsPlaced(t *testing.T) {
	file := spreadCrew(t, "alpha")

	out, _, err := zm(t, file, "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "dev (placed: beta)")
	assert.NotContains(t, out, "ops (placed")

	out, _, err = zm(t, file, "ls", "--wide")
	require.NoError(t, err)
	assert.Contains(t, out, "dev (placed: beta)")

	out, _, err = zm(t, file, "ls", "--json")
	require.NoError(t, err)

	var views []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &views))

	by := map[string]map[string]any{}
	for _, v := range views {
		by[v["role"].(string)] = v
	}

	assert.Equal(t, "beta", by["dev"]["place"])
	assert.Equal(t, true, by["dev"]["away"])
	assert.Equal(t, "alpha", by["ops"]["place"])
	assert.NotContains(t, by["ops"], "away")

	// A crew that declares no nodes shows nothing of it, in either form.
	plain := testCrew(t, "  dev: {}\n")
	out, _, err = zm(t, plain, "ls", "--all")
	require.NoError(t, err)
	assert.NotContains(t, out, "placed")

	out, _, err = zm(t, plain, "ls", "--json")
	require.NoError(t, err)
	assert.NotContains(t, out, `"place"`)
	assert.NotContains(t, out, `"away"`)
}

// retire writes inside the node it ran on: the mark does not travel with the file, and the other node's reading is unchanged.
func TestRetireOnASpreadCrewWritesInsideThisNode(t *testing.T) {
	file := spreadCrew(t, "alpha")

	_, _, err := zm(t, file, "retire", "--moved-to", "beta/ops")
	require.NoError(t, err)

	text := readString(t, file)
	assert.NotRegexp(t, `(?m)^retired:`, text)
	assert.NotRegexp(t, `(?m)^moved_to:`, text)
	assert.Regexp(t, `(?m)^  alpha: \{retired: "[0-9-]+", moved_to: beta/ops\}$`, text, "inside nodes.alpha")
	assert.Contains(t, text, "  beta: {}", "the other node's entry is untouched")

	alpha, err := crew.LoadFor(file, "alpha")
	require.NoError(t, err)
	assert.True(t, alpha.IsRetired())

	beta, err := crew.LoadFor(file, "beta")
	require.NoError(t, err)
	assert.False(t, beta.IsRetired(), "a retire made on alpha is alpha's")

	_, _, err = zm(t, file, "retire", "--off")
	require.NoError(t, err)

	alpha, err = crew.LoadFor(file, "alpha")
	require.NoError(t, err)
	assert.False(t, alpha.IsRetired())
	assert.NotContains(t, readString(t, file), "moved_to")
}

func TestSeatSetPlaceIsRefusedOnASpreadCrew(t *testing.T) {
	file := spreadCrew(t, "alpha")

	_, _, err := zm(t, file, "seat", "set", "dev", "place=alpha")
	require.ErrorContains(t, err, "edit the file on every node until zmina move exists")
	assert.Contains(t, readString(t, file), "place: beta", "the file is as it was")

	_, _, err = zm(t, file, "seat", "set", "dev", "about=builds")
	require.NoError(t, err)
}

// A task branch of a crew that declares nodes: carries the node, so two nodes pushing to one remote never meet.
func TestTaskBranchNamesTheNodeOfASpreadCrew(t *testing.T) {
	file := spreadCrew(t, "alpha")
	repo := filepath.Join(filepath.Dir(file), "api")
	git := func(dir string, args ...string) {
		t.Helper()

		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...).CombinedOutput()
		require.NoError(t, err, string(out))
	}

	require.NoError(t, os.MkdirAll(repo, 0o700))
	git(repo, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a"), []byte("a"), 0o600))
	git(repo, "add", ".")
	git(repo, "commit", "-q", "-m", "a")

	out, _, err := zm(t, file, "task", "new", "ops", "--repo", "api", "--worktree", "fix the login")
	require.NoError(t, err)

	name := mustCrew(t, file).Name
	assert.Contains(t, out, "zmina/alpha-"+name+"-t-1 from main")
	assert.Equal(t, "zmina/alpha-"+name+"-t-1", readTask(t, file, "T-1").Branch)

	// The same crew read on a machine that is no node keeps the old name.
	require.NoError(t, os.Remove(crew.ThisNodeFile()))

	out, _, err = zm(t, file, "task", "new", "ops", "--repo", "api", "--worktree", "fix the footer")
	require.NoError(t, err)
	assert.Contains(t, out, "zmina/"+name+"-t-2 from main")
}

// The hello tells a file's sum and the roles up here only for a crew that declares nodes:; any other crew is told as it always
// was.
func TestCrewHealthTellsSumAndHeldOnlyForASpreadCrew(t *testing.T) {
	uiHome(t)
	asNode(t, "alpha")

	ops := crewPathOf(t, "ops")
	body, err := os.ReadFile(ops)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ops, append(body, []byte("home: alpha\nnodes:\n  alpha: {}\n  beta: {}\n")...), 0o600))

	for _, key := range []string{"default", "ops"} {
		a := &app{out: io.Discard, err: io.Discard, crew: crewPathOf(t, key)}
		require.NoError(t, a.withState(func(s *shift.Shift) error {
			s.State.Role("dev").Up = true
			s.State.Role("dispatcher").Up = true

			return nil
		}))
	}

	byCrew := map[string]mesh.CrewHealth{}
	for _, h := range crewHealth() {
		byCrew[h.Crew] = h
	}

	require.Contains(t, byCrew, "default")
	require.Contains(t, byCrew, "ops")

	raw, err := os.ReadFile(ops)
	require.NoError(t, err)
	assert.Equal(t, crew.SumOf(raw), byCrew["ops"].Sum)
	assert.Equal(t, []string{"dev"}, byCrew["ops"].Held, "the dispatcher runs on every node and is not told")

	assert.Empty(t, byCrew["default"].Sum)
	assert.Empty(t, byCrew["default"].Held)

	told, err := json.Marshal(byCrew["default"])
	require.NoError(t, err)
	assert.NotContains(t, string(told), "sum")
	assert.NotContains(t, string(told), "held")

}

func healthOfCrew(crewKey, sum string, held ...string) mesh.CrewHealth {
	return mesh.CrewHealth{Crew: crewKey, TickAge: 5, Sum: sum, Held: held}
}

func peerWith(name string, h ...mesh.CrewHealth) mesh.Peer { return mesh.Peer{Name: name, Crews: h} }

func notesText(notes []placementNote) []string {
	var out []string

	for _, n := range notes {
		out = append(out, n.Level+": "+n.Text)
	}

	return out
}

func loadSpread(t *testing.T, body, node string) *crew.Crew {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	path := filepath.Join(t.TempDir(), "crew.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	c, err := crew.LoadFor(path, node)
	require.NoError(t, err)

	return c
}

func TestPlacementNotesSaySyncDriftAndWhatIsHeld(t *testing.T) {
	c := loadSpread(t, "home: alpha\nnodes: {alpha: {}, beta: {}, gamma: {}}\nroles:\n  dispatcher: {}\n  dev: {place: beta}\n  ops: {}\n  qa: {place: nowhere}\n", "alpha")
	local := healthOfCrew("acme", "aa", "ops")

	got := notesText(placementNotes(c, local, []mesh.Peer{
		peerWith("beta", healthOfCrew("acme", "aa", "dev")),
		peerWith("gamma", healthOfCrew("acme", "bb")),
		peerWith("delta", healthOfCrew("acme", "")),
		peerWith("other", healthOfCrew("elsewhere", "cc")),
	}, nil))

	assert.Equal(t, []string{
		"ok: nodes: alpha (home): ops · beta: dev · gamma · this node: alpha",
		`warn: qa is held by no node: its place "nowhere" is not one of nodes: (alpha, beta, gamma)`,
		"ok: in sync with beta",
		"warn: differs from gamma: the file is not the same on every node; copy one file to every node (crew.yaml, byte for byte), or compare them by hand",
		"warn: delta tells no sum for this crew: an older zmina that reads no nodes:, or a file that declares none; it may run every role",
	}, got)
}

func TestPlacementNotesFlagARoleUpOnTwoNodesAndOneUpWhereItIsNotPlaced(t *testing.T) {
	c := loadSpread(t, "home: alpha\nnodes: {alpha: {}, beta: {}}\nroles:\n  dispatcher: {}\n  dev: {place: beta}\n  ops: {}\n", "alpha")

	got := notesText(placementNotes(c, healthOfCrew("acme", "aa", "ops", "dev"),
		[]mesh.Peer{peerWith("beta", healthOfCrew("acme", "aa", "dev"))}, []string{"ops", "dev"}))

	assert.Contains(t, got, "warn: dev is up on this node, but the crew file places it on beta: zmina down dev here, or fix its place in the file on every node")
	assert.Contains(t, got, "warn: dev is up on this node and on beta: one role on two nodes; zmina down it on the one the file does not place it on")
	assert.NotContains(t, strings.Join(got, "\n"), "ops is up")

	// The dispatcher is on every node that holds a role: its being up on both is the design.
	got = notesText(placementNotes(c, healthOfCrew("acme", "aa"),
		[]mesh.Peer{peerWith("beta", healthOfCrew("acme", "aa", "dispatcher"))}, nil))
	assert.NotContains(t, strings.Join(got, "\n"), "dispatcher is up")
}

func TestPlacementNotesSayWhenThisMachineIsNoNodeOrNotListed(t *testing.T) {
	body := "home: alpha\nnodes: {alpha: {}, beta: {}}\nroles:\n  dispatcher: {}\n  dev: {place: beta}\n"

	got := notesText(placementNotes(loadSpread(t, body, ""), healthOfCrew("acme", "aa"), nil, nil))
	assert.Contains(t, got, "warn: this machine is no node (zmina node init), so it runs every role of the file; a node runs only its own")

	got = notesText(placementNotes(loadSpread(t, body, "gamma"), healthOfCrew("acme", "aa"), nil, nil))
	assert.Contains(t, got, "warn: this node (gamma) is not one of nodes: (alpha, beta): it runs nothing of this crew, and its dispatcher does not run")

	plain := loadSpread(t, "roles:\n  dispatcher: {}\n", "alpha")
	assert.Empty(t, placementNotes(plain, healthOfCrew("acme", ""), nil, nil), "a crew that declares no nodes has nothing to say")
}

// doctor and node ls show it: in sync with a peer that tells the same sum, differs from one that tells another.
func TestDoctorAndNodeLsShowPlacement(t *testing.T) {
	uiHome(t)

	ops := crewPathOf(t, "ops")
	body, err := os.ReadFile(ops)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ops, append(body, []byte("home: alpha\nnodes:\n  alpha: {}\n  beta: {}\n  gamma: {}\n")...), 0o600))

	_, _, err = zm(t, crewPathOf(t, "default"), "node", "init", "--name", "alpha", "--owner")
	require.NoError(t, err)

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)

	sum := crew.SumOf(mustRead(t, ops))
	require.NoError(t, me.UpdatePeers(func(ps []mesh.Peer) []mesh.Peer {
		return append(ps,
			mesh.Peer{Name: "beta", Source: "test", Crews: []mesh.CrewHealth{healthOfCrew("ops", sum)}},
			mesh.Peer{Name: "gamma", Source: "test", Crews: []mesh.CrewHealth{healthOfCrew("ops", strings.Repeat("f", 64))}})
	}))

	out, _, err := zm(t, ops, "node", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "  ops: in sync with beta")
	assert.Contains(t, out, "  ops: differs from gamma")
	assert.NotContains(t, out, "default:", "a crew that declares no nodes is not compared")

	out, _, _ = zm(t, ops, "doctor") // a machine of a test has no claude: doctor fails on that, and says the rest
	assert.Regexp(t, `ok +placement +in sync with beta`, out)
	assert.Regexp(t, `warn +placement +differs from gamma`, out)
	assert.Regexp(t, `ok +placement +nodes: alpha \(home\): dev`, out)
}

// zmina route answers tell for a seat placed on another node, and the pool never picks one.
func TestRouteSendsWorkForAnAwaySeatToItsCrew(t *testing.T) {
	file := testCrew(t, "  designer:\n    place: beta\n  dev: {}\n  dev2:\n    place: beta\n"+
		"routes:\n  - kind: {design: designer}\n  - pool: {seats: [dev, dev2]}\n"+spreadDecl)
	asNode(t, "alpha")

	name := mustCrew(t, file).Name

	out, _, err := zm(t, file, "route", "--kind", "design", "a new header")
	require.NoError(t, err)
	assert.Contains(t, out, "tell beta/"+name+" · rule 4 kind: kind design goes to designer; designer is placed on beta/"+name+", not on this node")
	assert.Contains(t, out, "zmina --crew "+file+" tell beta/"+name+" 'a new header'")

	out, _, err = zm(t, file, "route", "--kind", "code", "fix it")
	require.NoError(t, err)
	assert.Contains(t, out, "dev · rule 6 pool")
	assert.NotContains(t, out, "dev2 ·")

	_, _, err = zm(t, file, "task", "new", "--route", "--kind", "design", "a new header")
	require.ErrorContains(t, err, "tell beta/"+name)
}
