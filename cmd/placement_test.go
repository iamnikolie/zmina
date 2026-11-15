package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
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
