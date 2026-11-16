package cmd

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/mesh"
)

// pushWorld is the owner node alpha (this test's HOME) with a crew over alpha and beta, and beta, a node of its own that takes
// pushes into dir, reached over pipes.
type pushWorld struct {
	t      *testing.T
	file   string
	key    string
	beta   *mesh.Node
	dir    string // beta's ~/.zmina: its crews are <dir>/<key>/crew.yaml
	puts   []string
	cert   mesh.Cert
	answer func(key string, payload []byte, base string, create bool, by string) (mesh.CrewPutResult, error)
}

const pushCrewBody = `home: alpha
nodes:
  alpha: {}
  beta: {}
`

// Dial serves beta over a pipe: the node's own protocol, with a crew service that puts files into its dir.
func (w *pushWorld) Dial(ctx context.Context, a mesh.Addr) (io.ReadWriteCloser, error) {
	left, right := net.Pipe()
	stop := context.AfterFunc(ctx, func() { _ = left.Close() })

	go func() {
		defer stop()
		defer func() { _ = right.Close() }()

		_ = w.beta.ServeTerminals(ctx, right, func(string, string, string, string, time.Time) error { return nil }, time.Now,
			&mesh.TerminalServices{CrewPut: w.service}, nil)
	}()

	return left, nil
}

func (w *pushWorld) service(key string, payload []byte, base string, create bool, by string) (mesh.CrewPutResult, error) {
	w.puts = append(w.puts, by+"|"+key)

	if w.answer != nil {
		return w.answer(key, payload, base, create, by)
	}

	res, err := crew.Put(filepath.Join(w.dir, key, "crew.yaml"), payload, crew.PutOptions{Node: "beta", Base: base, Create: create, By: by, How: "crew push"})

	return putAnswer(res, err, base)
}

func newPushWorld(t *testing.T) *pushWorld {
	t.Helper()

	uiHome(t)

	// The crew ops of this node, over alpha and beta.
	file := crewPathOf(t, "ops")
	body, err := os.ReadFile(file)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, []byte(strings.Replace(string(body), "  dev:\n", "  dev:\n    place: beta\n", 1)+pushCrewBody), 0o600))

	_, _, err = zm(t, crewPathOf(t, "default"), "node", "init", "--name", "alpha", "--owner")
	require.NoError(t, err)

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)

	// beta: a node of the same owner, in a directory of its own.
	bdir := t.TempDir()
	req, err := mesh.Init(bdir, "beta")
	require.NoError(t, err)

	g, err := mesh.SignRequest(meshDir(), req, time.Now())
	require.NoError(t, err)
	require.NoError(t, mesh.Accept(bdir, g))

	beta, err := mesh.Open(bdir)
	require.NoError(t, err)

	w := &pushWorld{t: t, file: file, key: "ops", beta: beta, dir: t.TempDir(), cert: beta.Cert}

	// beta already has the crew, as a file it was last pushed, and has told its sum.
	require.NoError(t, os.MkdirAll(filepath.Join(w.dir, w.key), 0o700))
	w.setBeta(pushCrewBody + "roles:\n  dispatcher: {}\n")
	w.meet(me, true, w.betaSum())

	return w
}

func (w *pushWorld) betaFile() string { return filepath.Join(w.dir, w.key, "crew.yaml") }

func (w *pushWorld) setBeta(body string) {
	w.t.Helper()
	require.NoError(w.t, os.WriteFile(w.betaFile(), []byte(body), 0o600))
}

func (w *pushWorld) betaSum() string {
	raw, err := os.ReadFile(w.betaFile())
	require.NoError(w.t, err)

	return crew.SumOf(raw)
}

// meet records beta in alpha's book as its last session left it: reached by pipe, saying crew_sync or not, telling sum.
func (w *pushWorld) meet(me *mesh.Node, crewSync bool, sum string) {
	w.t.Helper()

	cert := w.cert
	require.NoError(w.t, me.UpdatePeers(func(ps []mesh.Peer) []mesh.Peer {
		out := ps[:0]
		for _, p := range ps {
			if p.Name != "beta" {
				out = append(out, p)
			}
		}

		p := mesh.Peer{Name: "beta", Addrs: []mesh.Addr{{Cmd: "beta"}}, Source: "hand", Cert: &cert, OK: time.Now(), TerminalV2: true, CrewSync: crewSync}
		if sum != "" {
			p.Crews = []mesh.CrewHealth{{Crew: w.key, TickAge: 5, Sum: sum}}
		}

		return append(out, p)
	}))
}

// push runs zmina crew push over the world's pipes.
func (w *pushWorld) push(args ...string) (string, string, error) {
	w.t.Helper()

	var out, errb bytes.Buffer

	root := newRoot(&app{out: &out, err: &errb, pushTransport: w})
	root.SetArgs(append([]string{"--crew", w.file, "crew", "push"}, args...))
	err := root.Execute()

	return out.String(), errb.String(), err
}

func TestCrewPushSendsTheFileBasedOnWhatTheNodeTold(t *testing.T) {
	w := newPushWorld(t)
	before := w.betaSum()

	out, errOut, err := w.push("beta")
	require.NoError(t, err, errOut)
	assert.Contains(t, out, "pushed "+w.key+" to beta: file ")
	assert.Contains(t, out, "which replaced "+before[:12])

	local, err := os.ReadFile(w.file)
	require.NoError(t, err)

	there, err := os.ReadFile(w.betaFile())
	require.NoError(t, err)
	assert.Equal(t, string(local), string(there), "beta now holds this node's file, byte for byte")
	assert.Equal(t, []string{"mesh alpha|" + w.key}, w.puts, "it was sent by the owner's node")

	vs, err := crew.HistoryOf(w.betaFile()).List()
	require.NoError(t, err)
	require.NotEmpty(t, vs)
	assert.Equal(t, "mesh alpha", vs[0].By)
	assert.Equal(t, "crew push", vs[0].How)

	// Again: it already holds it.
	out, _, err = w.push("beta")
	require.NoError(t, err)
	assert.Contains(t, out, "beta already holds this file")
}

// The node holds another file than the one it last told (an edit there): nothing is written, the difference is shown, and the
// way on names the hash of the file seen.
func TestCrewPushRefusedWhenTheNodeHoldsAnotherFileAndShowsTheDifference(t *testing.T) {
	w := newPushWorld(t)
	told := w.betaSum()

	w.setBeta(pushCrewBody + "roles:\n  dispatcher: {}\n  hand-edited: {}\n")
	edited := w.betaSum()
	require.NotEqual(t, told, edited)

	out, _, err := w.push("beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing was written")
	assert.Contains(t, err.Error(), "zmina crew push beta --base "+edited)
	assert.Contains(t, out, "beta holds file "+edited[:12])
	assert.Contains(t, out, "the push was based on "+told[:12])
	assert.Contains(t, out, "-  hand-edited: {}", "what is there and not here")
	assert.Contains(t, out, "--- beta/"+w.key+" crew.yaml")

	there, _ := os.ReadFile(w.betaFile())
	assert.Contains(t, string(there), "hand-edited", "the file there is as it was")

	// Having looked: the owner says which file it replaces.
	out, errOut, err := w.push("beta", "--base", edited)
	require.NoError(t, err, errOut)
	assert.Contains(t, out, "pushed "+w.key+" to beta")

	there, _ = os.ReadFile(w.betaFile())
	assert.NotContains(t, string(there), "hand-edited")
}

// A node that told no sum (the crew is a plain one there, or not there at all) has nothing for the push to be based on: the
// refusal says what it holds, and --create is the way to make a crew that is not there.
func TestCrewPushToANodeThatToldNoSum(t *testing.T) {
	w := newPushWorld(t)

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	w.meet(me, true, "")

	out, _, err := w.push("beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--base "+w.betaSum())
	assert.Contains(t, out, "beta holds file "+w.betaSum()[:12])

	// No crew there at all: refused, and --create makes it.
	require.NoError(t, os.RemoveAll(filepath.Join(w.dir, w.key)))

	_, _, err = w.push("beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--create")
	assert.NoDirExists(t, filepath.Join(w.dir, w.key))

	out, errOut, err := w.push("beta", "--create")
	require.NoError(t, err, errOut)
	assert.Contains(t, out, "pushed "+w.key+" to beta: crew made")
	assert.FileExists(t, w.betaFile())

	// And --create with --base is neither: one makes, the other replaces.
	_, _, err = w.push("beta", "--create", "--base", w.betaSum())
	require.ErrorContains(t, err, "not both")
}

// A node that did not say crew_sync in its last session is not sent a push at all: it would not know the op.
func TestCrewPushToAnOlderNodeIsRefusedBeforeAnythingIsSent(t *testing.T) {
	w := newPushWorld(t)

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	w.meet(me, false, w.betaSum())

	_, _, err = w.push("beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "beta did not say crew_sync in its last session")
	assert.Contains(t, err.Error(), "zmina node sync")
	assert.Empty(t, w.puts, "no session was opened, no frame sent")

	// Never met: the same, and says why.
	require.NoError(t, me.UpdatePeers(func(ps []mesh.Peer) []mesh.Peer {
		for i := range ps {
			if ps[i].Name == "beta" {
				ps[i].OK = time.Time{}
			}
		}

		return ps
	}))

	_, _, err = w.push("beta")
	require.ErrorContains(t, err, "no session with it yet")
}

func TestCrewPushRefusals(t *testing.T) {
	w := newPushWorld(t)

	_, _, err := w.push("alpha")
	require.ErrorContains(t, err, "alpha is this node")

	_, _, err = w.push("gamma")
	require.ErrorContains(t, err, "gamma is not one of nodes: (alpha, beta)")

	// A crew that declares no nodes has nothing to push.
	plain := crewPathOf(t, "default")
	var out, errb bytes.Buffer

	root := newRoot(&app{out: &out, err: &errb, pushTransport: w})
	root.SetArgs([]string{"--crew", plain, "crew", "push", "beta"})
	require.ErrorContains(t, root.Execute(), "declares no nodes:")

	// A crew file that is none of this node's crews has no name over there.
	elsewhere := filepath.Join(t.TempDir(), "crew.yaml")
	require.NoError(t, os.WriteFile(elsewhere, []byte("roles:\n  dispatcher: {}\n"+pushCrewBody), 0o600))

	root = newRoot(&app{out: &out, err: &errb, pushTransport: w})
	root.SetArgs([]string{"--crew", elsewhere, "crew", "push", "beta"})
	require.ErrorContains(t, root.Execute(), "is not one of this node's crews")

	assert.Empty(t, w.puts)
}

// A push is the owner's hand, never a role's: a command with $ZMINA_ROLE (or a Codex session no seat owns) is refused before anything is
// read or sent, though it runs on the owner's node where owner.key is within reach.
func TestCrewPushIsNotARolesCommand(t *testing.T) {
	w := newPushWorld(t)

	t.Setenv("ZMINA_ROLE", "dev")

	_, _, err := w.push("beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zmina crew push is the owner's; a role (dev) does not run it")
	assert.Empty(t, w.puts)

	t.Setenv("ZMINA_ROLE", "dispatcher")

	_, _, err = w.push("beta")
	require.ErrorContains(t, err, "a role (dispatcher) does not run it")
	assert.Empty(t, w.puts)

	// The owner's hand (no role in the environment) pushes as before.
	t.Setenv("ZMINA_ROLE", "")

	_, errOut, err := w.push("beta")
	require.NoError(t, err, errOut)
	assert.NotEmpty(t, w.puts)
}

// Only the owner's node can push: the request is signed with owner.key, which no other node holds.
func TestCrewPushIsTheOwnersNodeOnly(t *testing.T) {
	w := newPushWorld(t)
	require.NoError(t, os.Remove(filepath.Join(meshDir(), "owner.key")))

	_, _, err := w.push("beta")
	require.ErrorContains(t, err, "owner.key")
	assert.Empty(t, w.puts)
}

// A file bigger than a node takes is not sent.
func TestCrewPushRefusesAFileOverTheCap(t *testing.T) {
	w := newPushWorld(t)

	body, err := os.ReadFile(w.file)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(w.file, append(body, []byte("x-pad: "+strings.Repeat("a", mesh.CrewFileMax)+"\n")...), 0o600))

	_, _, err = w.push("beta")
	require.ErrorContains(t, err, "a push carries at most")
	assert.Empty(t, w.puts)
}

// The target's side as this node runs it (zmina node serve): crews are found by key among its own, made only under ~/.zmina and
// only when asked, and the file must load here.
func TestLocalCrewPutOnTheTargetNode(t *testing.T) {
	home := uiHome(t)

	_, _, err := zm(t, crewPathOf(t, "default"), "node", "init", "--name", "beta", "--owner")
	require.NoError(t, err)

	a := &app{out: io.Discard, err: io.Discard}
	ops := crewPathOf(t, "ops")

	body := func(extra string) []byte {
		return []byte("name: ops\nnamespace: /ns\nhome: alpha\nnodes:\n  alpha: {}\n  beta: {}\nroles:\n  dispatcher: {}\n" + extra)
	}

	old, err := os.ReadFile(ops)
	require.NoError(t, err)

	// An existing crew by its key: replaced when the base is its file, kept in its history, one event.
	res, err := a.localCrewPut("ops", body(""), crew.SumOf(old), false, "mesh alpha")
	require.NoError(t, err)
	assert.Equal(t, mesh.PutDone, res.Status)
	assert.Equal(t, crew.SumOf(body("")), res.Sum)
	assert.Equal(t, crew.SumOf(old), res.Was)
	assert.Equal(t, string(body("")), readString(t, ops))

	vs, err := crew.HistoryOf(ops).List()
	require.NoError(t, err)
	assert.Equal(t, "mesh alpha", vs[0].By)
	assert.Equal(t, "crew push", vs[0].How)
	require.Len(t, eventsNamed(t, ops, "crew-changed"), 1)

	// The same file: nothing written. Another base: a conflict that carries the file there, nothing written.
	res, err = a.localCrewPut("ops", body(""), "", false, "mesh alpha")
	require.NoError(t, err)
	assert.Equal(t, mesh.PutSame, res.Status)

	res, err = a.localCrewPut("ops", body("  dev: {}\n"), crew.SumOf(old), false, "mesh alpha")
	require.NoError(t, err)
	assert.Equal(t, mesh.PutConflict, res.Status)
	assert.Equal(t, string(body("")), string(res.Current))
	assert.Equal(t, crew.SumOf(body("")), res.Sum)
	assert.Equal(t, string(body("")), readString(t, ops))

	// A file that does not load here, or declares no nodes, replaces nothing.
	for _, bad := range []string{"roles: [a\n b: {", "name: ops\nroles:\n  dispatcher: {}\n"} {
		_, err = a.localCrewPut("ops", []byte(bad), crew.SumOf(body("")), false, "mesh alpha")
		require.Error(t, err, bad)
		assert.Equal(t, string(body("")), readString(t, ops))
	}

	// A crew that is not there is not made unless asked to.
	_, err = a.localCrewPut("newcrew", body(""), "", false, "mesh alpha")
	require.ErrorContains(t, err, "no crew \"newcrew\" on beta")
	assert.NoDirExists(t, filepath.Join(home, ".zmina", "newcrew"))

	res, err = a.localCrewPut("newcrew", body(""), "", true, "mesh alpha")
	require.NoError(t, err)
	assert.Equal(t, mesh.PutDone, res.Status)
	assert.Equal(t, string(body("")), readString(t, filepath.Join(home, ".zmina", "newcrew", "crew.yaml")))

	// Made only under the zmina home, by a plain name that is no directory zmina keeps there, and in none that is there.
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".zmina", "stuff"), 0o700))

	for _, key := range []string{"../escape", "a/b", "/etc", ".hidden", "Mesh", "tasks", "mesh", "crew.d", "stuff", ""} {
		_, err = a.localCrewPut(key, body(""), "", true, "mesh alpha")
		require.Error(t, err, key)
	}

	assert.NoFileExists(t, filepath.Join(home, "escape", "crew.yaml"))
	assert.NoFileExists(t, filepath.Join(home, ".zmina", "stuff", "crew.yaml"))
	assert.NoFileExists(t, filepath.Join(home, ".zmina", "tasks", "crew.yaml"))

	// The default crew is addressed as default.
	def := crewPathOf(t, "default")
	oldDef, err := os.ReadFile(def)
	require.NoError(t, err)

	res, err = a.localCrewPut("default", body(""), crew.SumOf(oldDef), false, "mesh alpha")
	require.NoError(t, err)
	assert.Equal(t, mesh.PutDone, res.Status)
	assert.Equal(t, string(body("")), readString(t, def))
}

// zmina node ls says whether a peer takes a push.
func TestNodeLsSaysCrewSync(t *testing.T) {
	w := newPushWorld(t)

	out, _, err := zm(t, w.file, "node", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "terminal_v2 · crew_sync")

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	w.meet(me, false, "")

	out, _, err = zm(t, w.file, "node", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "terminal_v2 · no crew_sync")
}
