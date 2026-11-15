package cmd

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// A machine that cannot say which node it is writes no retire mark: it would land at the top of the file, for every node.
func TestRetireNeedsToKnowTheNode(t *testing.T) {
	file := spreadCrew(t, "alpha")
	require.NoError(t, os.Remove(crew.ThisNodeFile()))

	_, _, err := zm(t, file, "retire")
	require.ErrorContains(t, err, "node name cannot be read")
	assert.NotContains(t, readString(t, file), "retired")
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

	// A machine whose node name cannot be read runs nothing of the crew, and makes no task for it: no branch of the old name either.
	require.NoError(t, os.Remove(crew.ThisNodeFile()))

	_, _, err = zm(t, file, "task", "new", "ops", "--repo", "api", "--worktree", "fix the footer")
	require.ErrorContains(t, err, "node name cannot be read")
	assert.Equal(t, 1, taskCount(t, file))
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
		// Not nodes of the file: another crew of the same name there is none of this one's business.
		peerWith("delta", healthOfCrew("acme", "")),
		peerWith("other", healthOfCrew("elsewhere", "cc")),
	}, nil))

	assert.Equal(t, []string{
		"ok: nodes: alpha (home): ops · beta: dev · gamma · this node: alpha",
		`warn: qa is held by no node: its place "nowhere" is not one of nodes: (alpha, beta, gamma)`,
		"ok: in sync with beta",
		"warn: differs from gamma: the file is not the same on every node; copy one file to every node (crew.yaml, byte for byte), or compare them by hand",
	}, got)

	// A peer of the file that tells the crew with no sum: an older zmina, or a file that declares none.
	got = notesText(placementNotes(c, local, []mesh.Peer{peerWith("beta", healthOfCrew("acme", "")), peerWith("gamma", healthOfCrew("acme", "aa"))}, nil))
	assert.Contains(t, got, "warn: beta tells no sum for this crew: an older zmina that reads no nodes:, or a file that declares none; it may run every role")
}

// A node the file lists and that has said nothing of the crew gets a line: never met, or met and not telling this crew.
func TestPlacementNotesSayWhenAListedNodeHasToldNothing(t *testing.T) {
	c := loadSpread(t, "home: alpha\nnodes: {alpha: {}, beta: {}, gamma: {}, delta: {}}\nroles:\n  dispatcher: {}\n  dev: {place: beta}\n", "alpha")
	cert := mesh.Cert{Name: "x"}

	got := notesText(placementNotes(c, healthOfCrew("acme", "aa"), []mesh.Peer{
		peerWith("beta", healthOfCrew("acme", "aa")),
		{Name: "gamma", Cert: &cert, Crews: []mesh.CrewHealth{healthOfCrew("another", "aa")}}, // met, tells another crew
		peerWith("unrelated", healthOfCrew("acme", "bb")),                                     // not a node of the file
	}, nil))

	assert.Equal(t, []string{
		"ok: nodes: alpha (home) · beta: dev · delta · gamma · this node: alpha",
		"ok: in sync with beta",
		"warn: no word from delta: this node has not met it (zmina node ls; zmina node peer add or enroll)",
		"warn: no word from gamma about this crew: it runs no crew of this name yet (zmina crew push gamma --create), or tells nothing of its crews",
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
	assert.Contains(t, got, "fail: this machine's node name cannot be read (~/.zmina/mesh/node.json: zmina node init), so it runs nothing of this crew: not a role, the dispatcher or a job of the crew",
		"doctor fails: a machine that runs nothing is not what the owner meant")

	got = notesText(placementNotes(loadSpread(t, body, "gamma"), healthOfCrew("acme", "aa"), nil, nil))
	assert.Contains(t, got, "warn: this node (gamma) is not one of nodes: (alpha, beta), and a node's name is matched exactly: it runs nothing of this crew, not a role, the dispatcher or a job of the crew")

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

// worktreeSweptAt is when the beat's daily worktree sweep began on the crew (the job's mark in the state), zero when it never did.
func worktreeSweptAt(t *testing.T, file string) time.Time {
	t.Helper()

	var at time.Time

	require.NoError(t, (&app{out: io.Discard, err: io.Discard, crew: file}).readState(func(s *shift.Shift) error {
		at = s.State.WorktreeGCAt

		return nil
	}))

	return at
}

// The crew's own jobs of the beat run on a node that runs the crew and on none else: not on a node the file does not list, and not
// on a machine whose node name cannot be read. The daily worktree sweep stands for them here.
func TestTheBeatsJobsRunOnlyWhereTheCrewRuns(t *testing.T) {
	needTmux(t)

	body := "  dev:\n    place: beta\n  ops: {}\n" + spreadDecl + "worktree_gc: daily\n"

	for name, c := range map[string]struct {
		node string // "" removes node.json
		runs bool
	}{
		"a node the file lists":       {"alpha", true},
		"a node the file does not":    {"gamma", false},
		"a machine with no node name": {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			file := testCrew(t, body)
			if c.node != "" {
				asNode(t, c.node)
			}

			_, errOut, err := zm(t, file, "tick")
			require.NoError(t, err, errOut)
			assert.Equal(t, c.runs, !worktreeSweptAt(t, file).IsZero(), "the daily sweep ran")
			assert.Equal(t, c.runs, isUp(t, file, "dispatcher"), "the dispatcher is up")
			assert.False(t, isUp(t, file, "dev"), "dev is beta's")

			if !c.runs {
				assert.Contains(t, errOut, "warning:", "and every command says why")
			}
		})
	}
}

// Every command of a crew over nodes says, on a machine that runs nothing of it, why and what to do; doctor fails.
func TestAMachineThatRunsNothingSaysSoOnEveryCommandAndDoctorFails(t *testing.T) {
	file := spreadCrew(t, "alpha")
	require.NoError(t, os.Remove(crew.ThisNodeFile()))

	_, errOut, err := zm(t, file, "ls")
	require.NoError(t, err)
	assert.Contains(t, errOut, "this machine's node name cannot be read")
	assert.Contains(t, errOut, "runs nothing here, not a role or the dispatcher, until it is; zmina node init")

	out, _, derr := zm(t, file, "doctor")
	require.Error(t, derr, "doctor fails")
	assert.Regexp(t, `fail +placement +this machine's node name cannot be read`, out)

	// A node the file does not list: a warning on every command, and doctor says it.
	asNode(t, "Alpha")

	_, errOut, err = zm(t, file, "ls")
	require.NoError(t, err)
	assert.Contains(t, errOut, "this node (Alpha) is not one of nodes: (alpha, beta), and a node's name is matched exactly")

	out, _, _ = zm(t, file, "doctor")
	assert.Regexp(t, `warn +placement +this node \(Alpha\) is not one of nodes:`, out)
}

// A restart of a role placed elsewhere that is up here is refused up front, with nothing put down; down works on it, and so does
// the sleep's refusal. A later take for it is refused too, since it would make a task on this ledger for a role that never runs here.
func TestRestartOfARolePlacedElsewhereIsRefusedAndDownWorks(t *testing.T) {
	needTmux(t)

	file := spreadCrew(t, "beta")

	// dev is beta's: started there.
	_, _, err := zm(t, file, "up", "dev")
	require.NoError(t, err)
	require.True(t, isUp(t, file, "dev"))

	// The same file read on alpha: dev is up here but placed on beta.
	asNode(t, "alpha")

	_, _, err = zm(t, file, "restart", "dev")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dev is placed on beta, not on this node (alpha)")
	assert.True(t, isUp(t, file, "dev"), "nothing was put down")

	_, _, err = zm(t, file, "sleep", "dev", "--until", "tomorrow")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dev is placed on beta")
	assert.True(t, isUp(t, file, "dev"))

	_, _, err = zm(t, file, "later", "add", "look at the footer")
	require.NoError(t, err)

	_, _, err = zm(t, file, "later", "take", "L-1", "dev")
	require.ErrorContains(t, err, "dev is placed on beta")
	assert.Zero(t, taskCount(t, file))

	_, _, err = zm(t, file, "down", "--clear", "dev")
	require.NoError(t, err)
	assert.False(t, isUp(t, file, "dev"), "the owner's way out of a double run")
}

// The review loop does not die when the review rule's first seat runs on another node: a seat of this node that the rule lists next
// takes it; with none, the dispatcher gets one note that names the seat and the crew to tell, and nothing is told for it.
func TestReviewWithTheFirstSeatOnAnotherNode(t *testing.T) {
	file := testCrew(t, "  dev:\n  reviewer:\n    place: beta\n  second:\n"+
		"routes:\n  - review: {first: reviewer, again: [second, dev]}\n  - pool: {seats: [dev, second]}\n"+spreadDecl)
	asNode(t, "alpha")

	// dev did T-1: it may not review it.
	_, _, err := zm(t, file, "task", "new", "dev", "the work")
	require.NoError(t, err)

	out, _, err := zm(t, file, "route", "--kind", "review", "--of", "T-1")
	require.NoError(t, err)
	assert.Contains(t, out, "second", "the next seat it lists takes the review")
	assert.Contains(t, out, "the first review seat reviewer is placed on")
	assert.NotContains(t, out, "tell ")
}

// The loop that makes a task's review: a first seat that runs on another node is passed over for the seat of this node the rule lists
// next.
func TestAutoReviewTakesTheNextSeatWhenTheFirstRunsElsewhere(t *testing.T) {
	file := reviewCrew(t, "  dev: {}\n  dev2: {}\n  reviewer:\n    place: beta\n", "auto_review: on\n"+spreadDecl)
	asNode(t, "alpha")

	id := workTask(t, file, "dev")
	out := closeAs(t, file, "dev", id)

	assert.Contains(t, out, "review T-2 of "+id+" for dev2")
	assert.Equal(t, "dev2", readTask(t, file, "T-2").Role)
	assert.Empty(t, dispatcherLines(t, file))
}

// With no seat of this node to take it, the dispatcher gets one clear note naming the seat and the crew to tell, and nothing is told for
// it; the loop does not die and asks again each beat.
func TestAutoReviewWithOnlyAnAwaySeatSaysWhichCrewToTell(t *testing.T) {
	file := testCrew(t, "  dev: {}\n  reviewer:\n    place: beta\nroutes:\n  - review: {first: reviewer}\n  - pool: {seats: [dev]}\n"+
		"auto_review: on\n"+spreadDecl)
	asNode(t, "alpha")

	repo := filepath.Join(filepath.Dir(file), "api")
	require.NoError(t, os.MkdirAll(repo, 0o700))
	gitIn(t, repo, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a"), []byte("a"), 0o600))
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "a")

	id := workTask(t, file, "dev")
	out := closeAs(t, file, "dev", id)

	name := mustCrew(t, file).Name
	assert.Contains(t, out, "review of "+id+" waits for a seat: review seat reviewer is placed on beta/"+name+": tell beta/"+name+
		" with what is to be reviewed (zmina does not tell for you)")

	lines := dispatcherLines(t, file)
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "review seat reviewer is placed on beta/"+name)
	assert.NotContains(t, lines[0], "another crew's work")
	assert.Equal(t, 1, taskCount(t, file), "no review task was made here")

	// Once, however often the beat asks.
	assert.Empty(t, reviewBeatOutput(t, file))
	assert.Len(t, dispatcherLines(t, file), 1)
}
