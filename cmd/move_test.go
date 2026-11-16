package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/move"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// moveWorld is two nodes on one machine, each with a home of its own: alpha and beta hold one crew (the same crew.yaml, byte for byte),
// one repository each at the same place under home, cloned from one bare remote, and a mesh node. The owner's node is the one whose home
// the test runs in; the other is served over pipes by the node's own protocol, and what it does it does under its own home (as()).
type moveWorld struct {
	t     *testing.T
	owner string
	peer  string
	homes map[string]string
	// socks is the directory of each node's tmux server: two nodes on one machine would share one server, and a role of one name is one
	// session on it.
	socks  map[string]string
	nodes  map[string]*mesh.Node
	remote string
	name   string
	cert   mesh.Cert
	// crashAt is the phase at which the next step is cut short, once (fired says it was).
	mu      sync.Mutex
	crashAt string
	fired   bool
	// reach makes the peer unreachable while set.
	reach bool
}

var errCrash = errors.New("the process died here")

const moveCrewBody = `name: %s
namespace: ~/ns
dir: ~/work
claude: %s
remote_control: false
home: alpha
nodes:
  alpha: {prefix: al-}
  beta: {prefix: be-}
roles:
  dispatcher:
    boot: hello
  dev: {keep: always, dir: ~/work/acme}
  ops: {}
`

// newMoveWorld builds the two nodes. The test runs as the owner's node.
func newMoveWorld(t *testing.T, owner string) *moveWorld {
	t.Helper()
	needTmux(t)

	peer := "beta"
	if owner == "beta" {
		peer = "alpha"
	}

	root := t.TempDir()
	w := &moveWorld{t: t, owner: owner, peer: peer, homes: map[string]string{"alpha": filepath.Join(root, "alpha-home"), "beta": filepath.Join(root, "beta-home")},
		socks: map[string]string{}, nodes: map[string]*mesh.Node{}, remote: filepath.Join(root, "origin.git"), name: fmt.Sprintf("mv%d", time.Now().UnixNano()%1_000_000_000), reach: true}

	t.Setenv("ZMINA_ROLE", "")
	t.Setenv("ZMINA_CREW", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "gitconfig"))
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")

	for _, node := range []string{"alpha", "beta"} {
		// A short path: a socket's is at most about a hundred characters.
		dir, err := os.MkdirTemp("/tmp", "zt")
		require.NoError(t, err)

		w.socks[node] = dir
	}

	t.Cleanup(func() {
		for node, dir := range w.socks {
			w.as(node, func() { tmux.KillServer("zmina-" + w.name) })
			_ = os.RemoveAll(dir)
		}
	})

	keep := hasTranscript
	hasTranscript = func(string, string) bool { return true }
	t.Cleanup(func() { hasTranscript = keep })

	keepFree := diskFree
	diskFree = func(string) (diskSpace, error) { return diskSpace{Free: 200 << 30, Total: 500 << 30, ID: "test"}, nil }
	t.Cleanup(func() { diskFree = keepFree })

	keepGrace, keepPoll := moveGrace, movePollEvery
	moveGrace, movePollEvery = 0, 5*time.Millisecond
	t.Cleanup(func() { moveGrace, movePollEvery = keepGrace, keepPoll })

	claude := filepath.Join(root, "bin", "claude")
	require.NoError(t, os.MkdirAll(filepath.Dir(claude), 0o700))
	require.NoError(t, os.WriteFile(claude, []byte(fakeClaude), 0o700))

	gitIn(t, root, "init", "-q", "--bare", "-b", "main", w.remote)

	for i, node := range []string{"alpha", "beta"} {
		home := w.homes[node]
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".zmina", "ops"), 0o700))
		require.NoError(t, os.WriteFile(w.file(node), []byte(fmt.Sprintf(moveCrewBody, w.name, claude)), 0o600))

		repo := filepath.Join(home, "work", "acme")
		require.NoError(t, os.MkdirAll(filepath.Dir(repo), 0o700))

		if i == 0 {
			gitIn(t, root, "init", "-q", "-b", "main", repo)
			require.NoError(t, os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*.log\n"), 0o600))
			gitIn(t, repo, "add", ".")
			gitIn(t, repo, "commit", "-q", "-m", "first")
			gitIn(t, repo, "remote", "add", "origin", w.remote)
			gitIn(t, repo, "push", "-q", "origin", "main")
		} else {
			gitIn(t, root, "clone", "-q", w.remote, repo)
		}
	}

	// The owner's node, and the other node of the same owner.
	t.Setenv("HOME", w.homes[owner])
	t.Setenv("TMUX_TMPDIR", w.socks[owner])

	_, _, err := zm(t, w.file(owner), "node", "init", "--name", owner, "--owner")
	require.NoError(t, err)

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)

	w.nodes[owner] = me

	pdir := filepath.Join(w.homes[peer], ".zmina", "mesh")
	req, err := mesh.Init(pdir, peer)
	require.NoError(t, err)

	g, err := mesh.SignRequest(meshDir(), req, time.Now())
	require.NoError(t, err)
	require.NoError(t, mesh.Accept(pdir, g))

	pn, err := mesh.Open(pdir)
	require.NoError(t, err)

	w.nodes[peer], w.cert = pn, pn.Cert
	w.meet(true)

	return w
}

func (w *moveWorld) file(node string) string {
	return filepath.Join(w.homes[node], ".zmina", "ops", "crew.yaml")
}

func (w *moveWorld) repo(node string) string { return filepath.Join(w.homes[node], "work", "acme") }

// meet records the peer in the owner's book as its last session left it.
func (w *moveWorld) meet(crewSync bool) {
	w.t.Helper()

	cert := w.cert

	require.NoError(w.t, w.nodes[w.owner].UpdatePeers(func(ps []mesh.Peer) []mesh.Peer {
		out := ps[:0]

		for _, p := range ps {
			if p.Name != w.peer {
				out = append(out, p)
			}
		}

		return append(out, mesh.Peer{Name: w.peer, Addrs: []mesh.Addr{{Cmd: w.peer}}, Source: "hand", Cert: &cert, OK: time.Now(), TerminalV2: true,
			CrewSync: crewSync})
	}))
}

// as runs fn with this machine being that node: its home.
func (w *moveWorld) as(node string, fn func()) {
	w.t.Helper()

	prev, prevSock := os.Getenv("HOME"), os.Getenv("TMUX_TMPDIR")

	require.NoError(w.t, os.Setenv("HOME", w.homes[node]))
	require.NoError(w.t, os.Setenv("TMUX_TMPDIR", w.socks[node]))

	defer func() {
		_ = os.Setenv("HOME", prev)
		_ = os.Setenv("TMUX_TMPDIR", prevSock)
	}()

	fn()
}

// on runs a command line as that node.
func (w *moveWorld) on(node string, args ...string) (out string, err error) {
	w.t.Helper()

	w.as(node, func() {
		var errOut string

		out, errOut, err = zm(w.t, w.file(node), args...)
		if err != nil {
			err = fmt.Errorf("%w: %s", err, errOut)
		}
	})

	return out, err
}

// hook is the fault of the nodes' apps: one crash, at the phase the test named.
func (w *moveWorld) hook(phase string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.crashAt == phase && !w.fired {
		w.fired = true

		return fmt.Errorf("%w (%s)", errCrash, phase)
	}

	return nil
}

func (w *moveWorld) crash(phase string) {
	w.mu.Lock()
	w.crashAt, w.fired = phase, false
	w.mu.Unlock()
}

func (w *moveWorld) peerApp() *app {
	return &app{out: io.Discard, err: io.Discard, pushTransport: w, moveFault: w.hook}
}

// Dial serves the peer over a pipe: the node's own protocol, with the services that do its steps under its own home.
func (w *moveWorld) Dial(ctx context.Context, a mesh.Addr) (io.ReadWriteCloser, error) {
	if !w.reach {
		return nil, errors.New("beta is not reachable")
	}

	left, right := net.Pipe()
	stop := context.AfterFunc(ctx, func() { _ = left.Close() })

	pa := w.peerApp()

	go func() {
		defer stop()
		defer func() { _ = right.Close() }()

		_ = w.nodes[w.peer].ServeTerminals(ctx, right, func(string, string, string, string, time.Time) error { return nil }, time.Now,
			&mesh.TerminalServices{
				RoleMove: func(c mesh.MoveCall) (data []byte, err error) {
					w.as(w.peer, func() { data, err = pa.localRoleMove(c) })

					return
				},
				RoleTake: func(c mesh.MoveCall) (data []byte, err error) {
					w.as(w.peer, func() { data, err = pa.localRoleTake(c) })

					return
				},
			}, nil)
	}()

	return left, nil
}

// ownerApp is the owner's command line over the world's pipes.
func (w *moveWorld) ownerApp(out, errb io.Writer) *app {
	return &app{out: out, err: errb, pushTransport: w, moveFault: w.hook}
}

// move runs zmina move as the owner.
func (w *moveWorld) move(args ...string) (string, string, error) {
	w.t.Helper()

	var out, errb bytes.Buffer

	root := newRoot(w.ownerApp(&out, &errb))
	root.SetArgs(append([]string{"--crew", w.file(w.owner), "move"}, args...))

	var err error

	w.as(w.owner, func() { err = root.Execute() })

	return out.String(), errb.String(), err
}

var moveIDRe = regexp.MustCompile(`mv-[0-9a-f]{12}`)

func (w *moveWorld) idOf(out string) string {
	w.t.Helper()

	id := moveIDRe.FindString(out)
	require.NotEmpty(w.t, id, out)

	return id
}

// work gives dev on alpha a task with a worktree on the repository and starts it there: T-2 (T-1 is ops's).
func (w *moveWorld) work(dirty bool) (id, worktreePath, branch string) {
	w.t.Helper()

	_, err := w.on("alpha", "task", "new", "ops", "something of ops's")
	require.NoError(w.t, err)

	out, err := w.on("alpha", "task", "new", "dev", "--repo", "~/work/acme", "--worktree", "--title", "Form", "--project", "acme", "--criteria", "it saves",
		"fix the form")
	require.NoError(w.t, err)
	require.Contains(w.t, out, "T-2")

	worktreePath = filepath.Join(w.homes["alpha"], ".zmina", "ops", "worktrees", "acme-T-2")
	branch = "zmina/alpha-" + w.name + "-t-2"

	require.NoError(w.t, os.WriteFile(filepath.Join(worktreePath, "form.txt"), []byte("form v1\n"), 0o600))
	gitIn(w.t, worktreePath, "add", ".")
	gitIn(w.t, worktreePath, "commit", "-q", "-m", "the form")

	if dirty {
		require.NoError(w.t, os.WriteFile(filepath.Join(worktreePath, "form.txt"), []byte("form v2, not committed\n"), 0o600))
		require.NoError(w.t, os.WriteFile(filepath.Join(worktreePath, "extra.txt"), []byte("new file\n"), 0o600))
		require.NoError(w.t, os.WriteFile(filepath.Join(worktreePath, "debug.log"), []byte("ignored\n"), 0o600))
	}

	_, err = w.on("alpha", "up", "dev", "--task", "T-2")
	require.NoError(w.t, err)
	require.True(w.t, w.up("alpha", "dev"))

	// The packet's line was delivered to the role (no beat runs here to type it): its queue is empty, as a role at work has it.
	w.as("alpha", func() { withRole(w.t, w.file("alpha"), "dev", func(rs *shift.RoleState) { rs.Inbox = nil }) })

	return "T-2", worktreePath, branch
}

func (w *moveWorld) up(node, role string) (up bool) {
	w.t.Helper()

	w.as(node, func() { up = isUp(w.t, w.file(node), role) })

	return up
}

func (w *moveWorld) tasks(node string) (out []task.Task) {
	w.t.Helper()

	w.as(node, func() {
		all, err := taskStore(mustCrew(w.t, w.file(node))).All()
		require.NoError(w.t, err)

		out = all
	})

	return out
}

func (w *moveWorld) items(node string) (out []later.Item) {
	w.t.Helper()

	w.as(node, func() {
		items, _, err := laterStore(mustCrew(w.t, w.file(node))).Scan()
		require.NoError(w.t, err)

		out = items
	})

	return out
}

func (w *moveWorld) journal(node, role string) (open bool) {
	w.t.Helper()

	w.as(node, func() { open = move.OpenID(mustCrew(w.t, w.file(node)).Layout(), role) != "" })

	return open
}

func (w *moveWorld) sums() (alpha, beta string) {
	w.t.Helper()

	a, err := os.ReadFile(w.file("alpha"))
	require.NoError(w.t, err)

	b, err := os.ReadFile(w.file("beta"))
	require.NoError(w.t, err)

	return crew.SumOf(a), crew.SumOf(b)
}

func (w *moveWorld) placed(node, role string) (place string) {
	w.t.Helper()

	w.as(node, func() {
		c := mustCrew(w.t, w.file(node))
		place = c.PlacementOf(c.Roles[role])
	})

	return place
}

// assertMoved is the end of a move of dev's T-2 from alpha to beta: the work is on beta under a new id with its origin, the role is placed
// and up there and down on alpha, the item on alpha is closed as taken by it, no journal is open, and the two files are one file again.
func (w *moveWorld) assertMoved(branch string) task.Task {
	w.t.Helper()

	t := w.t

	a, b := w.sums()
	assert.Equal(t, a, b, "the two nodes' crew files are byte-identical again")
	assert.Equal(t, "beta", w.placed("alpha", "dev"))
	assert.Equal(t, "beta", w.placed("beta", "dev"))
	assert.Equal(t, "alpha", w.placed("alpha", "ops"), "the rest of the crew stays where it was")

	assert.False(t, w.journal("alpha", "dev"), "the journal is closed")
	assert.False(t, w.up("alpha", "dev"), "dev is down on alpha")
	assert.True(t, w.up("beta", "dev"), "dev is up on beta")

	var moved task.Task

	for _, bt := range w.tasks("beta") {
		if bt.Origin == "alpha/T-2" {
			moved = bt
		}
	}

	require.NotEmpty(t, moved.ID, "the task is on beta's ledger, with its origin")
	assert.Equal(t, "dev", moved.Role)
	assert.Equal(t, task.Open, moved.State)
	assert.Equal(t, "fix the form", moved.Asked)
	assert.Equal(t, "Form", moved.Title)
	assert.Equal(t, "acme", moved.Project)
	assert.Equal(t, []string{"it saves"}, moved.Criteria)
	assert.Equal(t, branch, moved.Branch, "a carried branch keeps its name")
	assert.Equal(t, filepath.Join(w.homes["beta"], ".zmina", "ops", "worktrees", "acme-"+moved.ID), moved.Worktree, "the worktree is beta's own, by the new id")
	assert.Equal(t, w.repo("beta"), moved.Repo, "the repository is beta's own, found by the same place under its home")
	assert.Contains(t, moved.Note, "moved from alpha/T-2")

	var srcTask task.Task

	for _, st := range w.tasks("alpha") {
		if st.ID == "T-2" {
			srcTask = st
		}
	}

	assert.Equal(t, task.Parked, srcTask.State, "the task on alpha stays parked: no new task state")

	var item later.Item

	for _, it := range w.items("alpha") {
		if it.From == "T-2" {
			item = it
		}
	}

	assert.Equal(t, later.Taken, item.State)
	assert.Equal(t, "beta/"+moved.ID, item.Task, "the item is closed as taken by the task there")

	return moved
}

// A move of a role that is up, with a task, a branch and nothing uncommitted: every step, both ends, the two files one again.
func TestMoveCarriesARoleWithItsTaskToTheOtherNode(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, wt, branch := w.work(false)

	// A checkpoint the role wrote, which travels (saved again under the new id).
	var ckpt record.Ref

	w.as("alpha", func() {
		c := mustCrew(t, w.file("alpha"))
		shape := recordShape(c.HandoffFor(c.Roles["dev"]))

		values := map[string]string{}
		for _, s := range shape.Sections {
			values[s.Name] = "section " + s.Name
		}

		ref, err := record.Save(c.StateDir(), record.Document{Version: 1, Kind: "checkpoint", Task: "T-2", Role: "dev", By: "dev", At: time.Now(), Shape: shape, Values: values})
		require.NoError(t, err)

		ckpt = ref

		st := taskStore(c)
		tk, err := st.Get("T-2")
		require.NoError(t, err)

		tk.Checkpoint, tk.CheckpointTaskUpdated = &ckpt, tk.Updated
		require.NoError(t, st.Put(tk))
	})

	out, errOut, err := w.move("dev", "beta")
	require.NoError(t, err, out+errOut)

	assert.Contains(t, out, "dev: alpha to beta (move mv-")
	assert.Contains(t, out, "T-2 handed over: branch "+branch)
	assert.Contains(t, out, "beta took T-2 as T-1")
	assert.Contains(t, out, "dev is up on beta")
	assert.Contains(t, out, "crew files equal on alpha and beta")

	moved := w.assertMoved(branch)
	assert.Equal(t, "T-1", moved.ID, "a new local id: beta's ledger is its own")

	// The work: the branch is on beta, its worktree has the commit, and the tip is the one alpha pushed.
	assert.Equal(t, gitIn(t, wt, "rev-parse", "HEAD"), gitIn(t, moved.Worktree, "rev-parse", "HEAD"))
	assert.Equal(t, "form v1\n", readFile(t, filepath.Join(moved.Worktree, "form.txt")))
	assert.Equal(t, branch, gitIn(t, moved.Worktree, "rev-parse", "--abbrev-ref", "HEAD"))
	assert.Empty(t, gitIn(t, moved.Worktree, "status", "--short"))

	// The checkpoint was saved again under the new task's id, its provenance in By.
	require.NotNil(t, moved.Checkpoint)
	assert.Equal(t, moved.ID, moved.Checkpoint.Task)

	w.as("beta", func() {
		d, err := record.Load(mustCrew(t, w.file("beta")).StateDir(), "checkpoint", moved.ID, moved.Checkpoint.Revision)
		require.NoError(t, err)
		assert.Equal(t, "dev", d.Role)
		assert.Contains(t, d.By, "moved from alpha/T-2")
		assert.Equal(t, "section "+d.Shape.Sections[0].Name, d.Values[d.Shape.Sections[0].Name])
	})

	// beta's role is handed the task: its queue holds the packet's line, and the dispatcher there was told.
	w.as("beta", func() {
		var all []string

		all = append(all, inboxOf(t, w.file("beta"), "dev")...)
		assert.NotEmpty(t, all, "the packet's line waits for dev there, or was typed to it")
		assert.Contains(t, strings.Join(dispatcherLines(t, w.file("beta")), "\n"), "dev moved here from alpha")
	})

	// alpha's dispatcher was told the role went.
	w.as("alpha", func() {
		assert.Contains(t, strings.Join(dispatcherLines(t, w.file("alpha")), "\n"), "dev moved to beta")
	})

	// The two files are the file place: dev placed on beta, in a file history of both nodes.
	for _, node := range []string{"alpha", "beta"} {
		raw := readFile(t, w.file(node))
		assert.Contains(t, raw, "place: beta")
	}

	// The first branch's private refs are gone on beta.
	assert.Empty(t, gitIn(t, w.repo("beta"), "for-each-ref", "refs/zmina-move"))
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(raw)
}

// Uncommitted work: a worktree with changes, a new file and an ignored file. The changes travel as one commit with the move's trailer,
// which beta undoes (the work is uncommitted again there, on the branch at its parent); the ignored file does not travel; alpha's branch has
// the commit, and alpha's worktree reads clean at it.
func TestMoveCarriesUncommittedWorkAsOneCommitThatTheOtherNodeUndoes(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, wt, branch := w.work(true)

	before := gitIn(t, wt, "rev-parse", "HEAD")

	out, errOut, err := w.move("dev", "beta")
	require.NoError(t, err, out+errOut)
	assert.Contains(t, out, "with its uncommitted work as one commit")

	moved := w.assertMoved(branch)
	id := w.idOf(out)

	// alpha: the branch moved on by one commit that carries the trailer; its worktree is clean at it.
	tip := gitIn(t, wt, "rev-parse", "HEAD")
	assert.NotEqual(t, before, tip)
	assert.Equal(t, before, gitIn(t, wt, "rev-parse", "HEAD~1"))
	assert.Contains(t, gitIn(t, wt, "log", "-1", "--format=%B"), "Zmina-Move: "+id)
	assert.Empty(t, gitIn(t, wt, "status", "--short"))
	assert.Equal(t, tip, gitIn(t, w.remote, "rev-parse", "refs/heads/"+branch), "the commit is on origin")

	// beta: at the parent, the work in the files, uncommitted; the ignored file did not come.
	assert.Equal(t, before, gitIn(t, moved.Worktree, "rev-parse", "HEAD"), "the commit that carried the uncommitted work is undone")
	assert.Equal(t, "form v2, not committed\n", readFile(t, filepath.Join(moved.Worktree, "form.txt")))
	assert.Equal(t, "new file\n", readFile(t, filepath.Join(moved.Worktree, "extra.txt")))
	assert.NoFileExists(t, filepath.Join(moved.Worktree, "debug.log"))

	status := gitIn(t, moved.Worktree, "status", "--short")
	assert.Contains(t, status, "form.txt")
	assert.Contains(t, status, "extra.txt")
	assert.NotContains(t, status, "debug.log")
}

// The owner's node can be the one the role goes to: its own steps are the target's, and the other node is asked for the source's.
func TestMoveWhenTheOwnerNodeIsTheTarget(t *testing.T) {
	w := newMoveWorld(t, "beta")
	_, _, branch := w.work(false)

	out, errOut, err := w.move("dev", "beta")
	require.NoError(t, err, out+errOut)
	assert.Contains(t, out, "beta took T-2 as T-1")

	moved := w.assertMoved(branch)
	assert.Equal(t, "T-1", moved.ID)
}

// A role that holds no task has nothing to carry but its place: the two files change, nothing is started.
func TestMoveOfARoleThatHoldsNothingOnlyPlacesIt(t *testing.T) {
	w := newMoveWorld(t, "alpha")

	out, errOut, err := w.move("dev", "beta")
	require.NoError(t, err, out+errOut)
	assert.Contains(t, out, "dev holds no task, so it was not started")

	a, b := w.sums()
	assert.Equal(t, a, b)
	assert.Equal(t, "beta", w.placed("alpha", "dev"))
	assert.Equal(t, "beta", w.placed("beta", "dev"))
	assert.False(t, w.up("beta", "dev"))
	assert.Empty(t, w.tasks("beta"))
	assert.False(t, w.journal("alpha", "dev"))
}

// The crash tests: the process dies at a phase (the step ends there with what was written left as it is), and --resume goes on from
// where the move stood, to the same end as an uninterrupted one.
func TestMoveCrashAtEachPhaseThenResume(t *testing.T) {
	for _, phase := range []string{"begun", "asked", "released", "ready", "owner-before-take", "taken-git", "taken-import", "taken-flip", "taken",
		"owner-after-take", "commit-items", "commit-flip"} {
		t.Run(phase, func(t *testing.T) {
			w := newMoveWorld(t, "alpha")
			_, _, branch := w.work(true)

			w.crash(phase)

			out, _, err := w.move("dev", "beta")
			require.ErrorContains(t, err, "the process died here", "the move was cut short at "+phase)

			id := w.idOf(out)

			assert.True(t, w.journal("alpha", "dev"), "the journal is open, whatever phase it stopped at: the role stays refused")

			// The role is not started on alpha meanwhile, by any road.
			_, uperr := w.on("alpha", "up", "dev")
			require.Error(t, uperr)
			assert.Contains(t, uperr.Error(), "its move to beta is open ("+id)

			out, errOut, err := w.move("dev", "beta", "--resume", id)
			require.NoError(t, err, out+errOut)

			w.assertMoved(branch)
			assert.Contains(t, out, "crew files equal on alpha and beta")
		})
	}
}

// --abort puts the role back: its tasks are open on their own ids, its item closed, the journal gone, and the role is started on its work
// again on the node it left. beta, which was never asked to take, is as it was.
func TestMoveAbortAtEachPhaseRestartsTheRoleHere(t *testing.T) {
	for _, phase := range []string{"begun", "asked", "released", "ready"} {
		t.Run(phase, func(t *testing.T) {
			w := newMoveWorld(t, "alpha")
			_, wt, branch := w.work(true)

			alphaBefore, betaBefore := w.sums()

			w.crash(phase)

			out, _, err := w.move("dev", "beta")
			require.ErrorContains(t, err, "the process died here")

			id := w.idOf(out)

			out, errOut, err := w.move("dev", "beta", "--abort", id)
			require.NoError(t, err, out+errOut)
			assert.Contains(t, out, "called off on alpha")

			assert.False(t, w.journal("alpha", "dev"))
			assert.True(t, w.up("alpha", "dev"), "dev is up on alpha again, on its task")
			assert.False(t, w.up("beta", "dev"))
			assert.Empty(t, w.tasks("beta"), "beta never took anything")

			var back task.Task

			for _, tk := range w.tasks("alpha") {
				if tk.ID == "T-2" {
					back = tk
				}
			}

			assert.Equal(t, task.Open, back.State, "the task is open again on its own id")
			assert.Empty(t, back.Later)

			for _, it := range w.items("alpha") {
				assert.NotEqual(t, later.Parked, it.State, "no parked item is left over")
			}

			a, b := w.sums()
			assert.Equal(t, alphaBefore, a, "the file did not change")
			assert.Equal(t, betaBefore, b)

			// The uncommitted work is where it was: in the files, uncommitted, unless it was pushed (then the commit stays).
			assert.Equal(t, "form v2, not committed\n", readFile(t, filepath.Join(wt, "form.txt")))

			if phase == "ready" {
				assert.Contains(t, gitIn(t, wt, "log", "-1", "--format=%B"), "Zmina-Move: "+id, "a commit that is on origin stays")
			} else {
				assert.NotContains(t, gitIn(t, wt, "log", "-1", "--format=%B"), "Zmina-Move", "a commit that was not pushed is undone")
				assert.Contains(t, gitIn(t, wt, "status", "--short"), "form.txt")
			}

			_ = branch
		})
	}
}

// Once beta took the work (its answer was lost) the role runs there: --abort is refused, naming --resume; and --abort when beta cannot be
// asked is refused unless forced.
func TestMoveAbortIsRefusedWhenTheOtherNodeTook(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, _, branch := w.work(false)

	w.crash("taken")

	out, _, err := w.move("dev", "beta")
	require.ErrorContains(t, err, "the process died here")

	id := w.idOf(out)

	_, _, err = w.move("dev", "beta", "--abort", id)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "beta took the work already")
	assert.Contains(t, err.Error(), "--resume "+id)
	assert.True(t, w.journal("alpha", "dev"), "nothing was called off")

	// The take is idempotent: a second answer is the first.
	out, errOut, err := w.move("dev", "beta", "--resume", id)
	require.NoError(t, err, out+errOut)
	w.assertMoved(branch)
	assert.Equal(t, 1, countOrigin(w.tasks("beta"), "alpha/T-2"), "one task on beta, not two")
}

func countOrigin(ts []task.Task, origin string) int {
	n := 0

	for _, t := range ts {
		if t.Origin == origin {
			n++
		}
	}

	return n
}

func TestMoveAbortWhenTheOtherNodeCannotBeAskedNeedsForce(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	w.work(false)

	w.crash("released")

	out, _, err := w.move("dev", "beta")
	require.ErrorContains(t, err, "the process died here")

	id := w.idOf(out)

	w.reach = false

	_, _, err = w.move("dev", "beta", "--abort", id)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not be asked whether it took the work")
	assert.True(t, w.journal("alpha", "dev"))

	out, errOut, err := w.move("dev", "beta", "--abort", id, "--force")
	require.NoError(t, err, out+errOut)
	assert.False(t, w.journal("alpha", "dev"))
	assert.True(t, w.up("alpha", "dev"))
}

// A target that cannot be reached when the work is ready: the role is down and parked on the source (durable), the journal stays open, and
// the owner's ticker takes the move on when the node is back.
func TestMoveTargetUnreachableLeavesTheJournalOpenAndTheTickerFinishesIt(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, _, branch := w.work(false)

	// The preflight needs the target; so it is reachable then, and not for the take.
	w.crash("ready")

	out, _, err := w.move("dev", "beta")
	require.ErrorContains(t, err, "the process died here")

	id := w.idOf(out)

	w.reach = false

	_, _, err = w.move("dev", "beta", "--resume", id)
	require.Error(t, err)
	assert.True(t, w.journal("alpha", "dev"), "the journal stays open")
	assert.False(t, w.up("alpha", "dev"))
	assert.Equal(t, "alpha", w.placed("alpha", "dev"), "nothing is flipped")

	// The ticker of the owner's node: nothing while it is away, said once...
	var lines bytes.Buffer

	beat := func() {
		w.as("alpha", func() {
			a := w.ownerApp(&lines, io.Discard)
			a.crew = w.file("alpha")
			a.moveBeat()
		})
	}

	beat()
	beat()
	assert.Equal(t, 1, strings.Count(lines.String(), "beta is not reachable"), "an error is said when it changes, not every beat")

	// ...and the move is finished by it once the node is back.
	w.reach = true
	beat()

	w.assertMoved(branch)
	assert.Contains(t, lines.String(), "beta took T-2 as T-1")

	_, ok := func() (move.Drive, bool) {
		var d move.Drive
		var ok bool

		w.as("alpha", func() { d, ok, _ = move.OpenDrive(mustCrew(t, w.file("alpha")).Layout(), "dev") })

		return d, ok
	}()
	assert.False(t, ok, "the owner's record is gone with the move")
}

// A second begin with another id is refused naming the open one; the same id again is the same move.
func TestMoveDoubleBegin(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	w.work(false)

	w.crash("asked")

	out, _, err := w.move("dev", "beta")
	require.ErrorContains(t, err, "the process died here")

	id := w.idOf(out)

	_, _, err = w.move("dev", "beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dev already has a move open on alpha ("+id+")")
	assert.Contains(t, err.Error(), "--resume "+id)

	// The source itself, asked to begin another: refused naming the open one; the same id: ok.
	body, err := json.Marshal(move.BeginBody{To: "beta"})
	require.NoError(t, err)

	call := mesh.MoveCall{Crew: "ops", Role: "dev", Action: "begin", Move: "mv-aaaaaaaaaaaa", Payload: body, By: "hand"}

	w.as("alpha", func() {
		a := w.ownerApp(io.Discard, io.Discard)
		a.crew = w.file("alpha")

		_, err = a.localRoleMove(call)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dev has a move open already ("+id+" to beta)")

		call.Move = id
		_, err = a.localRoleMove(call)
		require.NoError(t, err, "the same id is the same move")
	})
}

// While the journal is open the ticker does not start a keep: always role, nor does task new, send or later take give it anything.
func TestMovingRoleGetsNoWorkAndIsNotStartedByTheTicker(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	w.work(false)

	w.crash("asked")

	_, _, err := w.move("dev", "beta")
	require.ErrorContains(t, err, "the process died here")

	for _, args := range [][]string{{"task", "new", "dev", "more work"}, {"send", "dev", "hello"}} {
		_, err := w.on("alpha", args...)
		require.Error(t, err, args)
		assert.Contains(t, err.Error(), "its move to beta is open", args)
	}

	// zmina doctor says where the owner looks that a move is open, and how to end it.
	out, _ := w.on("alpha", "doctor")
	assert.Contains(t, out, "dev has a move to beta open (mv-")
	assert.Contains(t, out, "--abort mv-")

	// The ticker moves the move on (the role parks and goes down, the branch is pushed) and does not start the keep: always role again.
	_, err = w.on("alpha", "tick")
	require.NoError(t, err)
	assert.False(t, w.up("alpha", "dev"), "put down by the move, not started again by the beat")
	assert.True(t, w.journal("alpha", "dev"))

	_, err = w.on("alpha", "tick")
	require.NoError(t, err)
	assert.False(t, w.up("alpha", "dev"))

	var open move.Journal

	w.as("alpha", func() {
		j, ok, err := move.Open(mustCrew(t, w.file("alpha")).Layout(), "dev")
		require.NoError(t, err)
		require.True(t, ok)

		open = j
	})

	assert.Equal(t, move.PhaseReady, open.Phase, "the source's own beat handed the work over")
	assert.NotEmpty(t, open.Bundle)
}

// Preflight: each mismatch is a refusal that names the field; nothing is begun.
func TestMovePreflightRefusalsNameTheField(t *testing.T) {
	// The order a refusal is raised in does not matter to the owner: each, alone, names its field.
	src := move.Facts{Node: "alpha", Sum: strings.Repeat("a", 64), Dir: "~/work", Namespace: "~/ns", Listed: true, Spread: true, Known: true, Placed: "alpha",
		Repos: []move.RepoFact{{Top: "~/work/acme", Found: true, Origin: "git@example.com:acme/app.git"}}}
	dst := move.Facts{Node: "beta", Sum: src.Sum, Dir: "~/work", Namespace: "~/ns", Listed: true, Spread: true, Known: true, Placed: "alpha",
		Repos: []move.RepoFact{{Top: "~/work/acme", Found: true, Origin: "git@example.com:acme/app.git"}}}

	require.NoError(t, compareFacts("dev", "beta", src, dst, false))

	for name, c := range map[string]struct {
		mutate func(s, d *move.Facts)
		force  bool
		says   string
	}{
		"the files differ":              {func(s, d *move.Facts) { d.Sum = strings.Repeat("b", 64) }, false, "crew file differs: alpha holds aaaaaaaaaaaa, beta holds bbbbbbbbbbbb"},
		"the crew dir differs":          {func(s, d *move.Facts) { d.Dir = "~/other" }, false, "crew dir differs: ~/work on alpha, ~/other on beta"},
		"the namespace differs":         {func(s, d *move.Facts) { d.Namespace = "~/ns2" }, false, "namespace differs: ~/ns on alpha, ~/ns2 on beta"},
		"the repo is missing there":     {func(s, d *move.Facts) { d.Repos[0].Found = false }, false, "repo ~/work/acme is no repository's top level on beta"},
		"the repo is not listed there":  {func(s, d *move.Facts) { d.Repos = nil }, false, "repo ~/work/acme is no repository's top level on beta"},
		"the origin differs":            {func(s, d *move.Facts) { d.Repos[0].Origin = "git@example.com:other/app.git" }, false, "repo ~/work/acme: origin differs: git@example.com:acme/app.git on alpha, git@example.com:other/app.git on beta"},
		"no origin here":                {func(s, d *move.Facts) { s.Repos[0].Origin = "" }, false, "repo ~/work/acme has no origin on alpha"},
		"the target is not listed":      {func(s, d *move.Facts) { d.Listed = false }, false, "nodes: the crew file on beta does not list beta"},
		"the target has no nodes":       {func(s, d *move.Facts) { d.Spread = false }, false, "declares no nodes:"},
		"the role is not placed here":   {func(s, d *move.Facts) { s.Placed = "beta" }, false, "placed: the file on alpha places dev on beta, not on alpha"},
		"the target places it so":       {func(s, d *move.Facts) { d.Placed = "beta" }, false, "placed: the file on beta places dev on beta"},
		"a move is open":                {func(s, d *move.Facts) { s.Moving = "mv-0123456789ab" }, false, "already has a move open on alpha (mv-0123456789ab)"},
		"a move is open there":          {func(s, d *move.Facts) { d.Moving = "mv-0123456789ab" }, false, "has a move open on beta"},
		"the role is not in the target": {func(s, d *move.Facts) { d.Known = false }, false, `no role "dev" in the crew file on beta`},
		"a queue":                       {func(s, d *move.Facts) { s.Queued = 2 }, false, "dev has 2 queued lines that a move does not carry"},
		"a dirty directory":             {func(s, d *move.Facts) { s.Dirty = []string{"~/work/acme (the role's directory)"} }, false, "holds uncommitted work that a move does not carry: ~/work/acme (the role's directory)"},
	} {
		t.Run(name, func(t *testing.T) {
			s, d := src, dst
			s.Repos = append([]move.RepoFact(nil), src.Repos...)
			d.Repos = append([]move.RepoFact(nil), dst.Repos...)
			c.mutate(&s, &d)

			err := compareFacts("dev", "beta", s, d, c.force)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.says)
		})
	}

	// --force lets the queue and the directory go.
	s := src
	s.Queued, s.Dirty = 2, []string{"~/work/acme (the role's directory)"}
	require.NoError(t, compareFacts("dev", "beta", s, dst, true))
}

// The preflight on the real nodes: an origin that differs, a file that differs, no crew_sync: refused with nothing begun on alpha.
func TestMoveRefusedBeforeAnythingIsBegunWhenTheNodesDiffer(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	w.work(false)

	// The origin of the repository on beta is another one.
	gitIn(t, w.repo("beta"), "remote", "set-url", "origin", filepath.Join(t.TempDir(), "other.git"))

	_, _, err := w.move("dev", "beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repo ~/work/acme: origin differs")
	assert.False(t, w.journal("alpha", "dev"), "nothing was begun")
	assert.True(t, w.up("alpha", "dev"), "the role was not touched")

	gitIn(t, w.repo("beta"), "remote", "set-url", "origin", w.remote)

	// The file on beta differs (an edit there).
	require.NoError(t, os.WriteFile(w.file("beta"), []byte(readFile(t, w.file("beta"))+"lessons: off\n"), 0o600))

	_, _, err = w.move("dev", "beta")
	require.ErrorContains(t, err, "crew file differs")
	assert.False(t, w.journal("alpha", "dev"))

	require.NoError(t, os.WriteFile(w.file("beta"), []byte(readFile(t, w.file("alpha"))), 0o600))

	// beta did not say crew_sync: nothing is sent to it.
	w.meet(false)

	_, _, err = w.move("dev", "beta")
	require.ErrorContains(t, err, "beta did not say crew_sync in its last session")
	assert.False(t, w.journal("alpha", "dev"))

	// A role that does not exist, the dispatcher, a copy, a node that is not listed, itself.
	w.meet(true)

	for args, says := range map[string]string{
		"nope beta":         `no role "nope"`,
		"dispatcher beta":   "is the dispatcher",
		"dev#2 beta":        "is no role a move carries",
		"dev gamma":         "gamma is not one of nodes: (alpha, beta)",
		"dev alpha":         "dev is placed on alpha already",
		"ops alpha":         "ops is placed on alpha already",
		"dev beta --wait 0": "--wait is how long",
	} {
		_, _, err = w.move(strings.Fields(args)...)
		require.Error(t, err, args)
		assert.Contains(t, err.Error(), says, args)
	}
}

// A queue is refused unless forced; forced, the lines are dropped and the source's dispatcher is told which. A role's own directory that
// holds uncommitted work is refused the same way.
func TestMoveQueueAndDirtyDirectoryNeedForce(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	w.work(false)

	w.as("alpha", func() {
		withRole(t, w.file("alpha"), "dev", func(rs *shift.RoleState) { rs.Inbox = []string{"check the pricing page"} })
	})

	_, _, err := w.move("dev", "beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dev has 1 queued lines that a move does not carry")
	assert.False(t, w.journal("alpha", "dev"))

	// A stray file in the role's own directory (the main checkout, which the move does not carry): refused too, and left where it is with --force.
	w.as("alpha", func() { withRole(t, w.file("alpha"), "dev", func(rs *shift.RoleState) { rs.Inbox = nil }) })
	require.NoError(t, os.WriteFile(filepath.Join(w.repo("alpha"), "stray.txt"), []byte("x\n"), 0o600))

	_, _, err = w.move("dev", "beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "holds uncommitted work that a move does not carry: ~/work/acme (the role's directory)")
	assert.False(t, w.journal("alpha", "dev"))

	w.as("alpha", func() {
		withRole(t, w.file("alpha"), "dev", func(rs *shift.RoleState) { rs.Inbox = []string{"check the pricing page"} })
	})

	out, errOut, err := w.move("dev", "beta", "--force")
	require.NoError(t, err, out+errOut)
	assert.Contains(t, strings.Join(dispatcherLinesOn(t, w, "alpha"), "\n"), "check the pricing page")
	assert.Contains(t, strings.Join(dispatcherLinesOn(t, w, "alpha"), "\n"), "dropped 1")
	assert.FileExists(t, filepath.Join(w.repo("alpha"), "stray.txt"), "left where it was")
}

func dispatcherLinesOn(t *testing.T, w *moveWorld, node string) (out []string) {
	t.Helper()

	w.as(node, func() { out = dispatcherLines(t, w.file(node)) })

	return out
}

// The take is idempotent by move id: the same bundle again is the same answer, and no second task.
func TestTakeIsIdempotentByMoveID(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, _, branch := w.work(true)

	w.crash("ready")

	out, _, err := w.move("dev", "beta")
	require.ErrorContains(t, err, "the process died here")

	id := w.idOf(out)

	// The bundle, as the source says it.
	var ans move.PollAnswer

	w.as("alpha", func() {
		a := w.ownerApp(io.Discard, io.Discard)
		a.crew = w.file("alpha")

		raw, err := a.localRoleMove(mesh.MoveCall{Crew: "ops", Role: "dev", Action: "poll", Move: id, By: "hand"})
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &ans))
	})

	require.Equal(t, move.StateReady, ans.State)

	take := func() move.TakeAnswer {
		var got move.TakeAnswer

		w.as("beta", func() {
			pa := w.peerApp()
			raw, err := pa.localRoleTake(mesh.MoveCall{Crew: "ops", Role: "dev", Move: id, Payload: ans.Bundle, By: "mesh alpha"})
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &got))
		})

		return got
	}

	first := take()
	second := take()

	assert.Equal(t, first, second, "the same answer")
	assert.Equal(t, 1, countOrigin(w.tasks("beta"), "alpha/T-2"))
	assert.True(t, first.Started)
	assert.Equal(t, 1, len(first.Tasks))

	// The source commits, once, and again: the same.
	commit := func() move.PollAnswer {
		var got move.PollAnswer

		w.as("alpha", func() {
			a := w.ownerApp(io.Discard, io.Discard)
			a.crew = w.file("alpha")

			body, _ := json.Marshal(move.CommitBody{To: "beta", Taken: map[string]string{"T-2": "beta/" + first.Tasks[0].To}, Sum: first.Sum})
			raw, err := a.localRoleMove(mesh.MoveCall{Crew: "ops", Role: "dev", Action: "commit", Move: id, Payload: body, By: "hand"})
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &got))
		})

		return got
	}

	assert.Equal(t, move.StateCommitted, commit().State)
	assert.Equal(t, move.StateCommitted, commit().State, "a commit made again is the same commit")

	w.assertMoved(branch)
}

// The bundle is data from another node: a take refuses what does not fit this node, each refusal naming what, and writes nothing.
func TestTakeRefusesABundleThatDoesNotFit(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	w.work(false)

	w.crash("ready")

	out, _, err := w.move("dev", "beta")
	require.ErrorContains(t, err, "the process died here")

	id := w.idOf(out)

	var ans move.PollAnswer

	w.as("alpha", func() {
		a := w.ownerApp(io.Discard, io.Discard)
		a.crew = w.file("alpha")

		raw, err := a.localRoleMove(mesh.MoveCall{Crew: "ops", Role: "dev", Action: "poll", Move: id, By: "hand"})
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &ans))
	})

	good, err := move.ParseBundle(ans.Bundle)
	require.NoError(t, err)

	for name, c := range map[string]struct {
		mutate func(b *move.Bundle)
		says   string
	}{
		"a repo that is none of the crew's": {func(b *move.Bundle) { b.Tasks[0].Repo = "/etc" }, "is no repository's top level"},
		"a repo that is a path elsewhere":   {func(b *move.Bundle) { b.Tasks[0].Repo = "~/.ssh" }, "is no repository's top level"},
		"an origin that differs":            {func(b *move.Bundle) { b.Tasks[0].Origin = "git@example.com:x/y.git" }, "origin differs"},
		"a tip that is not the branch's":    {func(b *move.Bundle) { b.Tasks[0].Tip = strings.Repeat("a", 40) }, "the branch moved since"},
		"a branch that is an option":        {func(b *move.Bundle) { b.Tasks[0].Branch = "--upload-pack=touch x" }, "not a plain branch name"},
		"another crew dir":                  {func(b *move.Bundle) { b.Dir = "~/elsewhere" }, "crew dir differs"},
		"another namespace":                 {func(b *move.Bundle) { b.Namespace = "~/other" }, "namespace differs"},
		"a base that is not this file's":    {func(b *move.Bundle) { b.Base = strings.Repeat("c", 64) }, "was made from another crew file"},
		"another node":                      {func(b *move.Bundle) { b.To = "gamma" }, "the bundle is for gamma"},
		"another role":                      {func(b *move.Bundle) { b.Role = "ops" }, "another move or another role"},
		"a move id that is a path":          {func(b *move.Bundle) { b.Move = "../../x" }, "move id"},
		"the dispatcher":                    {func(b *move.Bundle) { b.Role = "dispatcher" }, "another move or another role"},
	} {
		t.Run(name, func(t *testing.T) {
			b := good
			b.Tasks = append([]move.BTask(nil), good.Tasks...)
			c.mutate(&b)

			raw, err := json.Marshal(b)
			require.NoError(t, err)

			var gotErr error

			w.as("beta", func() {
				pa := w.peerApp()
				_, gotErr = pa.localRoleTake(mesh.MoveCall{Crew: "ops", Role: "dev", Move: id, Base: good.Base, Payload: raw, By: "mesh alpha"})
			})

			require.Error(t, gotErr)
			assert.Contains(t, gotErr.Error(), c.says)
			assert.Empty(t, w.tasks("beta"), "nothing was written")
			assert.Equal(t, "alpha", w.placed("beta", "dev"))
			assert.False(t, w.up("beta", "dev"))
		})
	}
}

// Only the owner's hand runs zmina move, and zmina crew push: a role (ZMINA_ROLE) is refused, whatever else it holds.
func TestMoveAndCrewPushAreTheOwnersHandNotARoles(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	w.work(false)

	t.Setenv("ZMINA_ROLE", "dev")

	_, _, err := w.move("dev", "beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zmina move is the owner's; a role (dev) does not run it")
	assert.False(t, w.journal("alpha", "dev"))

	var out, errb bytes.Buffer

	root := newRoot(w.ownerApp(&out, &errb))
	root.SetArgs([]string{"--crew", w.file("alpha"), "crew", "push", "beta"})

	var perr error

	w.as("alpha", func() { perr = root.Execute() })
	require.Error(t, perr)
	assert.Contains(t, perr.Error(), "zmina crew push is the owner's; a role (dev) does not run it")

	// The dispatcher is a role too.
	t.Setenv("ZMINA_ROLE", "dispatcher")

	_, _, err = w.move("dev", "beta")
	require.ErrorContains(t, err, "a role (dispatcher) does not run it")

	// And the owner's hand, with no role in the environment, is not refused for it.
	t.Setenv("ZMINA_ROLE", "")

	out2, errOut, err := w.move("dev", "beta")
	require.NoError(t, err, out2+errOut)
}

// A node that is not the owner's holds no owner.key: it cannot sign a move.
func TestMoveIsTheOwnersNodeOnly(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	w.work(false)

	require.NoError(t, os.Remove(filepath.Join(w.homes["alpha"], ".zmina", "mesh", "owner.key")))

	_, _, err := w.move("dev", "beta")
	require.ErrorContains(t, err, "owner.key")
	assert.False(t, w.journal("alpha", "dev"))
}

// seat set place= keeps refusing, and names zmina move.
func TestSeatSetPlaceRefusalNamesMove(t *testing.T) {
	w := newMoveWorld(t, "alpha")

	_, err := w.on("alpha", "seat", "set", "dev", "place=beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "use zmina move")
}

// A role that went away comes back: the same move the other way round, with the work the role did there. The branch goes forward on the
// node it left, an earlier hold on the role is lifted, and the ids and origins are each node's own.
func TestMoveThereAndBackAgain(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, wt, branch := w.work(false)

	out, errOut, err := w.move("dev", "beta")
	require.NoError(t, err, out+errOut)

	moved := w.assertMoved(branch)

	// dev works on beta: a commit, and what it was handed is delivered.
	w.as("beta", func() { withRole(t, w.file("beta"), "dev", func(rs *shift.RoleState) { rs.Inbox = nil }) })
	require.NoError(t, os.WriteFile(filepath.Join(moved.Worktree, "more.txt"), []byte("done on beta\n"), 0o600))
	gitIn(t, moved.Worktree, "add", ".")
	gitIn(t, moved.Worktree, "commit", "-q", "-m", "more")

	// Back: the earlier task's worktree on alpha, which has the branch checked out, is in the way and is said so.
	_, _, err = w.move("dev", "alpha")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is checked out in")
	assert.Contains(t, err.Error(), "an earlier task's worktree")

	// With it gone, the branch goes forward and the role is placed back and started.
	gitIn(t, w.repo("alpha"), "worktree", "remove", "--force", wt)

	id := w.idOf(readJournalID(t, w))

	out, errOut, err = w.move("dev", "alpha", "--resume", id)
	require.NoError(t, err, out+errOut)
	assert.Contains(t, out, "alpha took T-1 as T-3")

	a, b := w.sums()
	assert.Equal(t, a, b)
	assert.Equal(t, "alpha", w.placed("alpha", "dev"))
	assert.Equal(t, "alpha", w.placed("beta", "dev"))
	assert.True(t, w.up("alpha", "dev"), "an earlier hold on dev here did not keep it down")
	assert.False(t, w.up("beta", "dev"))

	var back task.Task

	for _, tk := range w.tasks("alpha") {
		if tk.Origin == "beta/"+moved.ID {
			back = tk
		}
	}

	require.NotEmpty(t, back.ID)
	assert.Equal(t, "T-3", back.ID)
	assert.Equal(t, "done on beta\n", readFile(t, filepath.Join(back.Worktree, "more.txt")), "the work done on beta is on alpha")
	assert.Equal(t, branch, back.Branch)
}

// readJournalID is the id of the move open on whichever node holds it, as the text of a line.
func readJournalID(t *testing.T, w *moveWorld) string {
	t.Helper()

	for _, node := range []string{"beta", "alpha"} {
		var id string

		w.as(node, func() { id = move.OpenID(mustCrew(t, w.file(node)).Layout(), "dev") })

		if id != "" {
			return "move " + id
		}
	}

	t.Fatal("no open move")

	return ""
}
