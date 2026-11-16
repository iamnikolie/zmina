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
	"github.com/iamnikolie/zmina/internal/shift"
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

	// beta already has the crew, as a file this node last pushed there (and recorded), and has told its sum.
	require.NoError(t, os.MkdirAll(filepath.Join(w.dir, w.key), 0o700))
	w.setBeta(pushCrewBody + "roles:\n  dispatcher: {}\n")
	w.meet(me, true, w.betaSum())
	require.NoError(t, me.MarkPushed("beta", w.key, w.betaSum()))

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

func TestCrewPushSendsTheFileBasedOnWhatWasLastSentThere(t *testing.T) {
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

// forget drops this node's record of what it last sent to beta, as for a node that was never pushed to.
func (w *pushWorld) forget() {
	w.t.Helper()
	require.NoError(w.t, os.Remove(filepath.Join(meshDir(), "pushed.json")))
}

// A node this one has no record of pushing to (never pushed to, or the record is gone) has no file the push is based on: the refusal
// says what it holds, shows the difference, and names --base, whatever sum the node told. --create is the way to make a crew that is
// not there.
func TestCrewPushToANodeWithNoRecordNeedsBase(t *testing.T) {
	w := newPushWorld(t)
	w.forget()

	for _, told := range []string{w.betaSum(), ""} {
		me, err := mesh.Open(meshDir())
		require.NoError(t, err)
		w.meet(me, true, told)

		out, _, err := w.push("beta")
		require.Error(t, err, "told %q", told)
		assert.Contains(t, err.Error(), "--base "+w.betaSum())
		assert.Contains(t, err.Error(), "no record of sending to or confirming a file there")
		assert.Contains(t, out, "beta holds file "+w.betaSum()[:12])
		assert.Contains(t, out, "the push was based on none")
	}

	// Having looked: --base replaces the file there, and the push is recorded.
	_, errOut, err := w.push("beta", "--base", w.betaSum())
	require.NoError(t, err, errOut)

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)

	local, err := os.ReadFile(w.file)
	require.NoError(t, err)
	assert.Equal(t, crew.SumOf(local), me.PushedSum("beta", w.key))

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

// The review's finding: what the node tells is not what the push is based on. A file edited on beta (a seat edit there, a role editing
// crew.yaml), told in the next hello, is refused with the difference, not overwritten because it was the sum last told.
func TestCrewPushDoesNotOverwriteAnEditTheNodeAlreadyTold(t *testing.T) {
	w := newPushWorld(t)

	w.setBeta(pushCrewBody + "roles:\n  dispatcher: {}\n  hand-edited: {}\n")

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	w.meet(me, true, w.betaSum()) // the hello after the edit

	out, _, err := w.push("beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "an edit there since this node last sent or confirmed its file")
	assert.Contains(t, out, "-  hand-edited: {}")

	there, _ := os.ReadFile(w.betaFile())
	assert.Contains(t, string(there), "hand-edited", "nothing was written")
}

// A push that was written, and one that found the file in place, are recorded: the next is based on it. A file edited there after is refused.
func TestCrewPushRecordsWhatItSentAndWhatItFoundThere(t *testing.T) {
	w := newPushWorld(t)

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)

	local, err := os.ReadFile(w.file)
	require.NoError(t, err)

	_, errOut, err := w.push("beta")
	require.NoError(t, err, errOut)
	assert.Equal(t, crew.SumOf(local), me.PushedSum("beta", w.key))

	// Again with nothing changed: already there, still recorded.
	out, _, err := w.push("beta")
	require.NoError(t, err)
	assert.Contains(t, out, "already holds this file")
	assert.Equal(t, crew.SumOf(local), me.PushedSum("beta", w.key))

	// A new edit here goes over what was sent last.
	require.NoError(t, os.WriteFile(w.file, append(local, []byte("x-more: 1\n")...), 0o600))

	_, errOut, err = w.push("beta")
	require.NoError(t, err, errOut)

	// An edit there after that is refused.
	w.setBeta("home: alpha\nnodes: {alpha: {}, beta: {}}\nroles: {dispatcher: {}, there: {}}\n")

	_, _, err = w.push("beta")
	require.ErrorContains(t, err, "nothing was written")

	// A refused push records nothing: the record is the file sent before.
	updated, err := os.ReadFile(w.file)
	require.NoError(t, err)
	assert.Equal(t, crew.SumOf(updated), me.PushedSum("beta", w.key))
}

// A node that answers that it wrote another file than the one sent is not believed, and nothing is recorded.
func TestCrewPushDoesNotRecordAnAnswerThatIsNotTheFileSent(t *testing.T) {
	w := newPushWorld(t)
	before := w.betaSum()

	w.answer = func(string, []byte, string, bool, string) (mesh.CrewPutResult, error) {
		return mesh.CrewPutResult{Status: mesh.PutDone, Sum: strings.Repeat("e", 64), Was: before}, nil
	}

	_, _, err := w.push("beta")
	require.ErrorContains(t, err, "which is not the file sent")

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	assert.Equal(t, before, me.PushedSum("beta", w.key))
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

// What a node sends back that goes to the owner's terminal is made plain: the difference of a conflict (its file is the node's), the
// warnings it says loading the file gave.
func TestCrewPushPrintsNothingAPeerCouldDriveTheTerminalWith(t *testing.T) {
	w := newPushWorld(t)

	// The file there has an escape sequence in it, and the node answers a conflict with it.
	w.setBeta(pushCrewBody + "roles:\n  dispatcher: {}\n  \x1b[2J\x1b]0;pwned\x07evil: {}\n")

	out, errOut, err := w.push("beta")
	require.Error(t, err)
	assert.NotContains(t, out+errOut+err.Error(), "\x1b")
	assert.NotContains(t, out+errOut+err.Error(), "\x07")
	assert.Contains(t, out, "evil: {}", "the text is shown, with the control characters taken out")

	// The warnings of a push that was taken.
	w.setBeta(pushCrewBody + "roles:\n  dispatcher: {}\n")
	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	require.NoError(t, me.MarkPushed("beta", w.key, w.betaSum()))

	local, err := os.ReadFile(w.file)
	require.NoError(t, err)

	w.answer = func(string, []byte, string, bool, string) (mesh.CrewPutResult, error) {
		return mesh.CrewPutResult{Status: mesh.PutDone, Sum: crew.SumOf(local), Warnings: []string{"line 3\x1b[31m red \x1b[2Jgone"}}, nil
	}

	_, errOut, err = w.push("beta")
	require.NoError(t, err)
	assert.NotContains(t, errOut, "\x1b")
	assert.Contains(t, errOut, "line 3[31m red [2Jgone")
}

// A file that would not load on one of the nodes it lists is not sent, whichever node it goes to.
func TestCrewPushChecksTheFileForEveryNodeItListsBeforeSending(t *testing.T) {
	w := newPushWorld(t)

	body, err := os.ReadFile(w.file)
	require.NoError(t, err)

	body = []byte(strings.Replace(string(body), "  beta: {}\n", "  beta: {}\n  gamma: {quiet_hours: nonsense}\n", 1))
	require.NoError(t, os.WriteFile(w.file, body, 0o600))

	_, _, err = w.push("beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not sent")
	assert.Contains(t, err.Error(), "gamma")
	assert.Empty(t, w.puts, "nothing left this node")
}

// --create says that the crew has no ticker there and how to install one.
func TestCrewPushCreateSaysThereIsNoTickerThere(t *testing.T) {
	w := newPushWorld(t)
	require.NoError(t, os.RemoveAll(filepath.Join(w.dir, w.key)))

	out, errOut, err := w.push("beta", "--create")
	require.NoError(t, err, errOut)
	assert.Contains(t, out, "crew made")
	assert.Contains(t, out, "no ticker is installed on beta for "+w.key)
	assert.Contains(t, out, "zmina --config "+w.key+" ticker install")

	// A replacement says no such thing.
	out, _, err = w.push("beta")
	require.NoError(t, err)
	assert.NotContains(t, out, "no ticker")
}

// A push that cannot finish a crew it made leaves nothing, and the name is free again.
func TestLocalCrewPutThatFailsAfterMakingTheCrewLeavesNothing(t *testing.T) {
	home := uiHome(t)

	_, _, err := zm(t, crewPathOf(t, "default"), "node", "init", "--name", "beta", "--owner")
	require.NoError(t, err)

	a := &app{out: io.Discard, err: io.Discard}
	body := []byte("name: acme\nnamespace: /ns\nhome: alpha\nnodes:\n  alpha: {}\n  beta: {}\nroles:\n  dispatcher: {}\n")

	// A file that loads on beta and not on gamma: refused after the directory was made, so that is taken away again.
	bad := []byte(strings.Replace(string(body), "  beta: {}\n", "  beta: {}\n  gamma: {timezone: Mars/Olympus}\n", 1))

	_, err = a.localCrewPut("acme", bad, "", true, "mesh alpha")
	require.ErrorContains(t, err, "would not load on gamma")
	assert.NoDirExists(t, filepath.Join(home, ".zmina", "acme"))

	res, err := a.localCrewPut("acme", body, "", true, "mesh alpha")
	require.NoError(t, err)
	assert.Equal(t, mesh.PutDone, res.Status)
}

// A push and a crew made by a push write nothing through a link that leaves the zmina home.
func TestLocalCrewPutRefusesALinkOutOfTheHome(t *testing.T) {
	home := uiHome(t)

	_, _, err := zm(t, crewPathOf(t, "default"), "node", "init", "--name", "beta", "--owner")
	require.NoError(t, err)

	a := &app{out: io.Discard, err: io.Discard}
	body := []byte("name: acme\nnamespace: /ns\nhome: alpha\nnodes:\n  alpha: {}\n  beta: {}\nroles:\n  dispatcher: {}\n")
	there := []byte("name: acme\nnamespace: /ns\nroles:\n  dispatcher: {}\n")

	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "crew.yaml"), there, 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(home, ".zmina", "acme")))

	// A crew whose directory is a link to somewhere else: found as one of the node's crews, and refused.
	_, err = a.localCrewPut("acme", body, crew.SumOf(there), false, "mesh alpha")
	require.ErrorContains(t, err, "leaves")
	assert.Equal(t, string(there), readString(t, filepath.Join(outside, "crew.yaml")))

	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "nothing was made there: no history, no temp file")

	// A new crew under a name that is a link is not made through it.
	linked := t.TempDir()
	require.NoError(t, os.Symlink(linked, filepath.Join(home, ".zmina", "newcrew")))

	_, err = a.localCrewPut("newcrew", body, "", true, "mesh alpha")
	require.Error(t, err)

	entries, err = os.ReadDir(linked)
	require.NoError(t, err)
	assert.Empty(t, entries)

	// A crew file that is a link to a file elsewhere (kept in a dotfiles repository, say) is refused too.
	other := filepath.Join(home, ".zmina", "dots")
	require.NoError(t, os.MkdirAll(other, 0o700))

	target := filepath.Join(t.TempDir(), "dots-crew.yaml")
	require.NoError(t, os.WriteFile(target, there, 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(other, "crew.yaml")))

	_, err = a.localCrewPut("dots", body, crew.SumOf(there), false, "mesh alpha")
	require.ErrorContains(t, err, "leaves")
	assert.Equal(t, string(there), readString(t, target))
}

// zmina seat on a crew over nodes says the edit is made here only, and what it says is true: the node it edited is the owner's, and a push
// goes over what was last sent to each node, so a node edited there meanwhile is refused with the difference.
func TestSeatEditOfACrewOverNodesWarnsAndTheWarningIsTrue(t *testing.T) {
	w := newPushWorld(t)

	_, errOut, err := zm(t, w.file, "seat", "set", "dev", "about=builds the form")
	require.NoError(t, err, errOut)
	assert.Contains(t, errOut, "this edit changed it here only")
	assert.Contains(t, errOut, "a node whose file was edited there since is refused, with the difference shown")

	// The same edit made on beta by hand: the push of the file with this node's edit does not overwrite it unseen.
	w.setBeta(pushCrewBody + "roles:\n  dispatcher: {}\n  dev:\n    about: beta's own words\n")

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	w.meet(me, true, w.betaSum())

	out, _, err := w.push("beta")
	require.Error(t, err)
	assert.Contains(t, out, "beta's own words")
	assert.Contains(t, err.Error(), "nothing was written")
}

// A seat edit that read the file before a push replaced it fails and says to run it again: the push is not written over.
func TestSeatEditThatLostARaceToAPushFailsInsteadOfWinning(t *testing.T) {
	w := newPushWorld(t)

	a := &app{out: io.Discard, err: io.Discard, crew: w.file}

	before, err := os.ReadFile(w.file)
	require.NoError(t, err)

	err = a.editSeat("dev", "", func(s *shift.Shift, e *crew.Edit) (string, error) {
		// What a push on this node does between the read of the file and the write of the edit (the target side of a push).
		pushed := append(append([]byte(nil), before...), []byte("x-pushed: 1\n")...)
		_, perr := crew.Put(w.file, pushed, crew.PutOptions{Node: "alpha", Base: crew.SumOf(before), By: "mesh owner", How: "crew push"})
		require.NoError(t, perr)

		return "set about", e.Set("dev", map[string]string{"about": "stale"})
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the crew file changed meanwhile, run it again")

	got, err := os.ReadFile(w.file)
	require.NoError(t, err)
	assert.Contains(t, string(got), "x-pushed: 1", "the push stays")
	assert.NotContains(t, string(got), "stale")
}
