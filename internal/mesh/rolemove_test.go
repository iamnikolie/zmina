package mesh

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const aMove = "mv-0123456789ab"

// moveService is a node that records every step of a move it is asked and every terminal it is asked to open.
type moveService struct {
	mu    sync.Mutex
	moves []MoveCall
	takes []MoveCall
	opens int
	ans   []byte
	err   error
}

func (m *moveService) services() *TerminalServices {
	return &TerminalServices{
		Open: func(context.Context, string, string, uint16, uint16) (Terminal, error) {
			m.mu.Lock()
			m.opens++
			m.mu.Unlock()

			return nil, errors.New("a terminal opened")
		},
		RoleMove: func(c MoveCall) ([]byte, error) {
			m.mu.Lock()
			m.moves = append(m.moves, c)
			m.mu.Unlock()

			return m.ans, m.err
		},
		RoleTake: func(c MoveCall) ([]byte, error) {
			m.mu.Lock()
			m.takes = append(m.takes, c)
			m.mu.Unlock()

			return m.ans, m.err
		},
	}
}

func (m *moveService) seen() (moves, takes []MoveCall, opens int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]MoveCall(nil), m.moves...), append([]MoveCall(nil), m.takes...), m.opens
}

func moveWorld(t *testing.T) (owner, worker *Node, svc *moveService, transport *boundPipes) {
	t.Helper()

	owner = newOwner(t, "owner")
	worker = enroll(t, owner, "worker")
	addPeer(t, owner, "worker")
	svc = &moveService{ans: []byte(`{"state":"pending"}`)}
	transport = &boundPipes{nodes: map[string]*Node{"worker": worker}, services: map[string]*TerminalServices{"worker": svc.services()}}

	return owner, worker, svc, transport
}

// A step of a move reaches the node's service with its names, its id, the base and who sent it, bound to the node, and what the
// node says comes back.
func TestRoleMoveReachesTheNodeBoundToIt(t *testing.T) {
	owner, _, svc, transport := moveWorld(t)

	data, err := owner.RoleMove(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Role: "dev", Action: "begin", Move: aMove,
		Base: someSum, Force: true, Payload: []byte(`{"to":"beta"}`)})
	require.NoError(t, err)
	assert.JSONEq(t, `{"state":"pending"}`, string(data))

	moves, takes, opens := svc.seen()
	require.Equal(t, []MoveCall{{Crew: "acme", Role: "dev", Action: "begin", Move: aMove, Base: someSum, Force: true,
		Payload: []byte(`{"to":"beta"}`), By: "mesh owner"}}, moves)
	assert.Empty(t, takes)
	assert.Zero(t, opens)

	frame := transport.tap("worker").terminalFrame("role/move")
	require.NotNil(t, frame)
	assert.Equal(t, "worker", frame.To, "the step names the node it is for")
	assert.Equal(t, aMove, frame.Move, "the move is inside what the owner signed")
	assert.True(t, frame.Force)
}

func TestRoleTakeCarriesTheBundleAndReturnsTheAnswer(t *testing.T) {
	owner, _, svc, transport := moveWorld(t)
	svc.ans = []byte(`{"started":true}`)

	data, err := owner.RoleTake(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Role: "dev", Move: aMove, Base: someSum,
		Payload: []byte(`{"version":1}`)})
	require.NoError(t, err)
	assert.JSONEq(t, `{"started":true}`, string(data))

	moves, takes, _ := svc.seen()
	assert.Empty(t, moves)
	require.Len(t, takes, 1)
	assert.Equal(t, []byte(`{"version":1}`), takes[0].Payload)
	assert.Equal(t, "mesh owner", takes[0].By)
	assert.Empty(t, takes[0].Action)
}

func TestRoleMoveErrorOfTheNodeIsTheError(t *testing.T) {
	owner, _, svc, transport := moveWorld(t)
	svc.err = errors.New("dev has 2 queued lines that a move does not carry")

	_, err := owner.RoleMove(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Role: "dev", Action: "begin", Move: aMove})
	require.ErrorContains(t, err, "dev has 2 queued lines")
}

// rawRole signs a step the way sign says and sends it on a fresh session, returning the first reply: what a caller does that is not
// zmina's own client.
func rawRole(t *testing.T, owner *Node, transport Transport, op string, req TerminalRequest, sign func(key ed25519.PrivateKey, c *conn, req *TerminalRequest)) wire {
	t.Helper()

	rw, c, err := owner.terminalConnect(putCtx(t), transport, "worker")
	require.NoError(t, err)

	defer func() { _ = rw.Close() }()

	key, err := readKey(filepath.Join(owner.Dir, "owner.key"))
	require.NoError(t, err)

	req.To = "worker"
	req.From, req.NotAfter = owner.Cert.Key, time.Now().Add(requestLife).Unix()
	sign(key, c, &req)
	require.NoError(t, c.send(wire{Op: op, Terminal: &req}))

	var w wire
	require.NoError(t, c.dec.Decode(&w))

	return w
}

func goodStep(op string) TerminalRequest {
	if op == "role/take" {
		return TerminalRequest{Crew: "acme", Role: "dev", Move: aMove, Base: someSum, Payload: []byte(`{"version":1}`)}
	}

	return TerminalRequest{Crew: "acme", Role: "dev", Action: "begin", Move: aMove, Base: someSum, Payload: []byte(`{"to":"beta"}`)}
}

func signBoundFor(op string) func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
	return func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
		req.Sig = ed25519.Sign(key, terminalMessageV2(op, c.serverKey, c.challenge, *req))
	}
}

// The refusals, for each op: an err frame, and the node's service sees nothing. The old form is refused even with strict requests off.
func TestRoleOpsRefuseWhatTheOwnerDidNotSignForThisNodeAndSession(t *testing.T) {
	for _, op := range []string{"role/move", "role/take"} {
		t.Run(op, func(t *testing.T) {
			owner, worker, svc, transport := moveWorld(t)
			bound := signBoundFor(op)

			for name, c := range map[string]struct {
				sign func(key ed25519.PrivateKey, c *conn, req *TerminalRequest)
				says string
			}{
				"unsigned": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {}, "requires the owner's signature"},
				"signed by another key": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
					_, other, _ := ed25519.GenerateKey(nil)
					req.Sig = ed25519.Sign(other, terminalMessageV2(op, c.serverKey, c.challenge, *req))
				}, "requires the owner's signature"},
				"the old form": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
					req.Sig = ed25519.Sign(key, terminalMessage(op, c.challenge, *req))
				}, op + " takes only an owner request bound to this node"},
				"another node's name": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
					req.To = "other"
					bound(key, c, req)
				}, `this request is for node "other"`},
				"no node's name": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
					req.To = ""
					bound(key, c, req)
				}, "this request is for node"},
				"another operation's signature": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
					req.Sig = ed25519.Sign(key, terminalMessageV2("terminal/list", c.serverKey, c.challenge, *req))
				}, "requires the owner's signature"},
				"the other move op's signature": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
					other := "role/take"
					if op == "role/take" {
						other = "role/move"
					}

					req.Sig = ed25519.Sign(key, terminalMessageV2(other, c.serverKey, c.challenge, *req))
				}, "requires the owner's signature"},
				"another challenge": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
					req.Sig = ed25519.Sign(key, terminalMessageV2(op, c.serverKey, []byte("another challenge"), *req))
				}, "requires the owner's signature"},
				"another node's key": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
					other, _, _ := ed25519.GenerateKey(nil)
					req.Sig = ed25519.Sign(key, terminalMessageV2(op, other, c.challenge, *req))
				}, "requires the owner's signature"},
				"the payload altered after signing": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
					bound(key, c, req)
					req.Payload = []byte(`{"to":"gamma"}`)
				}, "requires the owner's signature"},
				"the move altered after signing": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
					bound(key, c, req)
					req.Move = "mv-ffffffffffff"
				}, "requires the owner's signature"},
				"force added after signing": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
					bound(key, c, req)
					req.Force = true
				}, "requires the owner's signature"},
				"the base altered after signing": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
					bound(key, c, req)
					req.Base = strings.Repeat("d", 64)
				}, "requires the owner's signature"},
				"the role altered after signing": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
					bound(key, c, req)
					req.Role = "ops"
				}, "requires the owner's signature"},
				"the action altered after signing": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
					bound(key, c, req)
					req.Action = "abort"
				}, "requires the owner's signature"},
			} {
				t.Run(name, func(t *testing.T) {
					w := rawRole(t, owner, transport, op, goodStep(op), c.sign)
					assert.Equal(t, "err", w.Op)
					assert.Contains(t, w.Err, c.says)
				})
			}

			require.False(t, worker.StrictRequests, "strict is off, as by default: the old form is refused all the same")

			moves, takes, opens := svc.seen()
			assert.Empty(t, moves)
			assert.Empty(t, takes)
			assert.Zero(t, opens)
		})
	}
}

// What the owner signed once is of no use in another session: the challenge is the serving node's, fresh for each.
func TestRoleOpsReplayInAnotherSessionIsRefused(t *testing.T) {
	for _, op := range []string{"role/move", "role/take"} {
		t.Run(op, func(t *testing.T) {
			owner, _, svc, transport := moveWorld(t)

			req := goodStep(op)
			if op == "role/take" {
				_, err := owner.RoleTake(putCtx(t), transport, "worker", req)
				require.NoError(t, err)
			} else {
				_, err := owner.RoleMove(putCtx(t), transport, "worker", req)
				require.NoError(t, err)
			}

			captured := transport.tap("worker").terminalFrame(op)
			require.NotNil(t, captured)

			rw, c, err := owner.terminalConnect(putCtx(t), transport, "worker")
			require.NoError(t, err)

			defer func() { _ = rw.Close() }()

			require.NoError(t, c.send(wire{Op: op, Terminal: captured}))

			var w wire
			require.NoError(t, c.dec.Decode(&w))
			assert.Equal(t, "err", w.Op)
			assert.Contains(t, w.Err, "requires the owner's signature")

			moves, takes, _ := svc.seen()
			if op == "role/take" {
				assert.Empty(t, moves)
				assert.Len(t, takes, 1, "the node did not do it twice")
			} else {
				assert.Len(t, moves, 1, "the node did not do it twice")
				assert.Empty(t, takes)
			}
		})
	}
}

// The mesh checks what it can before it asks the service: the names, the id, the action, the base, the sizes. A path, an option or a
// command never reaches it.
func TestRoleOpsBadRequestsNeverReachTheService(t *testing.T) {
	type bad struct {
		op     string
		mutate func(r *TerminalRequest)
		says   string
	}

	for name, c := range map[string]bad{
		"a crew that is a path":         {"role/move", func(r *TerminalRequest) { r.Crew = "../ops" }, "a crew's name, no path"},
		"a crew that is absolute":       {"role/take", func(r *TerminalRequest) { r.Crew = "/etc" }, "a crew's name, no path"},
		"no crew":                       {"role/move", func(r *TerminalRequest) { r.Crew = "" }, "a crew's name, no path"},
		"a role that is a path":         {"role/move", func(r *TerminalRequest) { r.Role = "../../x" }, "a role's name"},
		"a role with a space":           {"role/take", func(r *TerminalRequest) { r.Role = "d ev" }, "a role's name"},
		"a copy of a role":              {"role/move", func(r *TerminalRequest) { r.Role = "dev#2" }, "a copy, dev#2, does not move"},
		"no role":                       {"role/take", func(r *TerminalRequest) { r.Role = "" }, "a role's name"},
		"an upper-case role":            {"role/move", func(r *TerminalRequest) { r.Role = "Dev" }, "a role's name"},
		"an action that is none":        {"role/move", func(r *TerminalRequest) { r.Action = "restart" }, "check, begin, poll, commit or abort"},
		"no action":                     {"role/move", func(r *TerminalRequest) { r.Action = "" }, "check, begin, poll, commit or abort"},
		"an action on a take":           {"role/take", func(r *TerminalRequest) { r.Action = "begin" }, "a take has no action"},
		"a move id that is a path":      {"role/move", func(r *TerminalRequest) { r.Move = "../../etc/passwd" }, "the id of a move"},
		"a move id with a command":      {"role/take", func(r *TerminalRequest) { r.Move = "mv-$(id)" }, "the id of a move"},
		"a move id too short":           {"role/move", func(r *TerminalRequest) { r.Move = "mv-ab" }, "the id of a move"},
		"upper-case hex in a move id":   {"role/move", func(r *TerminalRequest) { r.Move = "mv-0123456789AB" }, "the id of a move"},
		"no move id on a poll":          {"role/move", func(r *TerminalRequest) { r.Action, r.Move = "poll", "" }, "the id of a move"},
		"no move id on a take":          {"role/take", func(r *TerminalRequest) { r.Move = "" }, "the id of a move"},
		"a bad move id on a check":      {"role/move", func(r *TerminalRequest) { r.Action, r.Move = "check", "x" }, "the id of a move"},
		"a base that is no hash":        {"role/move", func(r *TerminalRequest) { r.Base = "not-a-sum" }, "the hash of the crew file"},
		"an upper-case base":            {"role/take", func(r *TerminalRequest) { r.Base = strings.ToUpper(someSum) }, "the hash of the crew file"},
		"an empty take":                 {"role/take", func(r *TerminalRequest) { r.Payload = nil }, "this one is empty"},
		"a body over the cap":           {"role/move", func(r *TerminalRequest) { r.Payload = make([]byte, RoleMoveBodyMax+1) }, "carries at most"},
		"a bundle over the cap":         {"role/take", func(r *TerminalRequest) { r.Payload = make([]byte, RoleTakeMax+1) }, "carries at most"},
		"a body of a take as a bundle":  {"role/move", func(r *TerminalRequest) { r.Payload = make([]byte, RoleMoveBodyMax+1) }, "carries at most"},
		"a long role name":              {"role/take", func(r *TerminalRequest) { r.Role = strings.Repeat("a", 32) }, "a role's name"},
		"a line break in the crew name": {"role/move", func(r *TerminalRequest) { r.Crew = "ops\nx" }, "a crew's name, no path"},
	} {
		t.Run(name, func(t *testing.T) {
			owner, _, svc, transport := moveWorld(t)

			req := goodStep(c.op)
			c.mutate(&req)

			w := rawRole(t, owner, transport, c.op, req, signBoundFor(c.op))
			assert.Equal(t, "err", w.Op)
			assert.Contains(t, w.Err, c.says)

			moves, takes, opens := svc.seen()
			assert.Empty(t, moves)
			assert.Empty(t, takes)
			assert.Zero(t, opens)
		})
	}
}

// The size of what a step carries is judged before its signature is read: an unsigned request over the cap is refused for its size.
func TestRoleOpsSizeIsCheckedBeforeTheSignature(t *testing.T) {
	owner, _, svc, transport := moveWorld(t)

	req := goodStep("role/take")
	req.Payload = make([]byte, RoleTakeMax+1)

	w := rawRole(t, owner, transport, "role/take", req, func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {})
	assert.Equal(t, "err", w.Op)
	assert.Contains(t, w.Err, "carries at most")
	assert.NotContains(t, w.Err, "signature")

	_, takes, _ := svc.seen()
	assert.Empty(t, takes)
}

// The client does not send what the node would refuse for its size, and the answer it reads is bounded too.
func TestRoleOpsClientChecksSizeFirst(t *testing.T) {
	owner, _, svc, transport := moveWorld(t)

	_, err := owner.RoleMove(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Role: "dev", Action: "begin", Move: aMove,
		Payload: make([]byte, RoleMoveBodyMax+1)})
	require.ErrorContains(t, err, "carries at most")

	_, err = owner.RoleTake(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Role: "dev", Move: aMove,
		Payload: make([]byte, RoleTakeMax+1)})
	require.ErrorContains(t, err, "carries at most")

	assert.Nil(t, transport.tap("worker"), "nothing was sent: not even a session was opened")

	// An answer over the cap is refused at the node, never sent.
	svc.ans = make([]byte, RoleTakeMax+1)
	_, err = owner.RoleMove(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Role: "dev", Action: "poll", Move: aMove})
	require.ErrorContains(t, err, "at most")
}

// A node of a zmina from before zmina move did not say crew_sync: the owner's node sends it nothing, and says what to do.
func TestRoleOpsToAPeerThatDidNotSayCrewSyncAreRefusedBeforeAnythingIsSent(t *testing.T) {
	for _, op := range []string{"role/move", "role/take"} {
		t.Run(op, func(t *testing.T) {
			owner := newOwner(t, "owner")
			old := enroll(t, owner, "old")
			addPeer(t, owner, "old")

			transport, sent := oldNode(t, old, nil)

			var err error
			if op == "role/take" {
				_, err = owner.RoleTake(putCtx(t), transport, "old", goodStep(op))
			} else {
				_, err = owner.RoleMove(putCtx(t), transport, "old", goodStep(op))
			}

			require.Error(t, err)
			assert.ErrorContains(t, err, "old did not say crew_sync")
			assert.ErrorContains(t, err, "update it")
			assert.Empty(t, sent(), "not a frame of the step left this node")
		})
	}
}

// A node that takes the op but has no service for it says so; it is not a terminal either.
func TestRoleOpsWithNoServiceAreRefused(t *testing.T) {
	owner, _, svc, transport := moveWorld(t)

	transport.services["worker"] = &TerminalServices{Open: svc.services().Open}

	_, err := owner.RoleMove(putCtx(t), transport, "worker", goodStep("role/move"))
	require.ErrorContains(t, err, "role/move unavailable")

	_, err = owner.RoleTake(putCtx(t), transport, "worker", goodStep("role/take"))
	require.ErrorContains(t, err, "role/take unavailable")

	transport.services["worker"] = nil

	_, err = owner.RoleMove(putCtx(t), transport, "worker", goodStep("role/move"))
	require.ErrorContains(t, err, "no terminal service")

	_, _, opens := svc.seen()
	assert.Zero(t, opens)
}

// A node that is not the owner holds no owner.key, so it cannot sign a step at all.
func TestRoleOpsNeedTheOwnerKey(t *testing.T) {
	owner, worker, svc, _ := moveWorld(t)
	other := enroll(t, owner, "other")
	addPeer(t, other, "worker")

	transport := &boundPipes{nodes: map[string]*Node{"worker": worker}, services: map[string]*TerminalServices{"worker": svc.services()}}

	_, err := other.RoleMove(putCtx(t), transport, "worker", goodStep("role/move"))
	require.ErrorContains(t, err, "owner node")
	_, err = other.RoleTake(putCtx(t), transport, "worker", goodStep("role/take"))
	require.ErrorContains(t, err, "owner node")
}

// The server's own side of the word: a caller whose hello did not say crew_sync (an owner node from before zmina move, which is bound to
// terminal_v2 already) is refused the step, though its signature is the owner's and bound to this node.
func TestRoleOpsFromACallerThatDidNotSayCrewSyncAreRefusedByTheNode(t *testing.T) {
	for _, op := range []string{"role/move", "role/take"} {
		t.Run(op, func(t *testing.T) {
			owner := newOwner(t, "owner")
			worker := enroll(t, owner, "worker")
			svc := &moveService{}

			client, server := net.Pipe()
			require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))

			go func() {
				defer func() { _ = server.Close() }()
				_ = worker.ServeTerminals(context.Background(), server, (&inbox{}).deliver, time.Now, svc.services(), nil)
			}()

			c := newConn(client)
			challenge, err := nonce()
			require.NoError(t, err)
			require.NoError(t, c.send(wire{Op: "hello", V: protocolVersion, Cert: &owner.Cert, Nonce: challenge, TerminalV2: true}))

			hi, err := c.expect("hello")
			require.NoError(t, err)
			require.NoError(t, c.send(wire{Op: "auth", Sig: ed25519.Sign(owner.Key, helloMessage("client", hi.Nonce)),
				TerminalV2Sig: ed25519.Sign(owner.Key, terminalV2CapabilityMessage("client", hi.Nonce))}))
			_, err = c.expect("ok")
			require.NoError(t, err)

			key, err := readKey(filepath.Join(owner.Dir, "owner.key"))
			require.NoError(t, err)

			req := goodStep(op)
			req.To, req.From, req.NotAfter = "worker", owner.Cert.Key, time.Now().Add(requestLife).Unix()
			req.Sig = ed25519.Sign(key, terminalMessageV2(op, hi.Cert.Key, hi.Nonce, req))
			require.NoError(t, c.send(wire{Op: op, Terminal: &req}))

			var w wire
			require.NoError(t, c.dec.Decode(&w))
			assert.Equal(t, "err", w.Op)
			assert.Contains(t, w.Err, "needs both nodes to say crew_sync")

			moves, takes, _ := svc.seen()
			assert.Empty(t, moves)
			assert.Empty(t, takes)
		})
	}
}
