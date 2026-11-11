package mesh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tapped records what the serving side read, so a test can look at the frames the owner sent.
type tapped struct {
	net.Conn
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *tapped) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	t.mu.Lock()
	t.buf.Write(p[:n])
	t.mu.Unlock()
	return n, err
}

// terminalFrame is the first frame of op the serving side read.
func (t *tapped) terminalFrame(op string) *TerminalRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, line := range bytes.Split(t.buf.Bytes(), []byte("\n")) {
		var w wire
		if json.Unmarshal(line, &w) == nil && w.Op == op && w.Terminal != nil {
			return w.Terminal
		}
	}
	return nil
}

// boundPipes reaches nodes by name over pipes like terminalPipes, with the serving clock under the test's hand and the
// serving side's reads kept.
type boundPipes struct {
	nodes    map[string]*Node
	services map[string]*TerminalServices
	now      func() time.Time
	mu       sync.Mutex
	taps     map[string]*tapped
}

func (p *boundPipes) Dial(ctx context.Context, a Addr) (io.ReadWriteCloser, error) {
	left, right := net.Pipe()
	tap := &tapped{Conn: right}
	p.mu.Lock()
	if p.taps == nil {
		p.taps = map[string]*tapped{}
	}
	p.taps[a.Cmd] = tap
	p.mu.Unlock()
	now := p.now
	if now == nil {
		now = time.Now
	}
	stop := context.AfterFunc(ctx, func() { _ = left.Close() })
	go func() {
		defer stop()
		defer func() { _ = right.Close() }()
		_ = p.nodes[a.Cmd].ServeTerminals(ctx, tap, func(string, string, string, string, time.Time) error { return nil }, now, p.services[a.Cmd], p)
	}()
	return left, nil
}

func (p *boundPipes) tap(node string) *tapped {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.taps[node]
}

func statusService() *TerminalServices {
	return &TerminalServices{
		List:   func() ([]TerminalTarget, error) { return []TerminalTarget{{Crew: "ops", Role: "dev"}}, nil },
		Status: func(kind string) ([]byte, error) { return []byte(`{"ok":true}`), nil },
	}
}

// What the owner signs names the node it is for: the key and the challenge that node gave, and the node's name.
func TestBoundRequestIsSignedForOneNode(t *testing.T) {
	owner := newOwner(t, "owner")
	worker := enroll(t, owner, "worker")
	other := enroll(t, owner, "other")
	key, err := readKey(filepath.Join(owner.Dir, "owner.key"))
	require.NoError(t, err)

	challenge := []byte("a challenge of the server")
	req := TerminalRequest{Crew: "ops", Role: "dev", To: "worker"}
	sig := ed25519.Sign(key, terminalMessageV2("terminal/open", worker.Cert.Key, challenge, req))
	verify := func(op string, server ed25519.PublicKey, challenge []byte, req TerminalRequest) bool {
		return ed25519.Verify(owner.Owner, terminalMessageV2(op, server, challenge, req), sig)
	}

	require.True(t, verify("terminal/open", worker.Cert.Key, challenge, req))
	altered := req
	altered.To = "other"
	assert.False(t, verify("terminal/open", worker.Cert.Key, challenge, altered), "To is inside the signed bytes")
	altered = req
	altered.To = ""
	assert.False(t, verify("terminal/open", worker.Cert.Key, challenge, altered), "a request cannot lose its To")
	altered = req
	altered.Role = "other"
	assert.False(t, verify("terminal/open", worker.Cert.Key, challenge, altered))
	assert.False(t, verify("terminal/list", worker.Cert.Key, challenge, req), "the operation is signed")
	assert.False(t, verify("terminal/open", other.Cert.Key, challenge, req), "the key of the node it is for is signed")
	assert.False(t, verify("terminal/open", worker.Cert.Key, []byte("another challenge"), req), "the challenge is signed")

	// The old form is another message: a signature of one is no signature of the other.
	assert.False(t, ed25519.Verify(owner.Owner, terminalMessage("terminal/open", challenge, req), sig))
	assert.NotEqual(t, terminalMessage("terminal/open", challenge, req), terminalMessageV2("terminal/open", worker.Cert.Key, challenge, req))
}

// With both nodes new the request goes in the bound form, for every operation, naming the node it is sent to.
func TestOwnerSendsTheBoundFormToANodeThatSaysTerminalV2(t *testing.T) {
	owner := newOwner(t, "owner")
	worker := enroll(t, owner, "worker")
	addPeer(t, owner, "worker")
	transport := &boundPipes{nodes: map[string]*Node{"worker": worker}, services: map[string]*TerminalServices{"worker": statusService()}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := owner.CrewStatus(ctx, transport, "worker", "")
	require.NoError(t, err)
	frame := transport.tap("worker").terminalFrame("crew/status")
	require.NotNil(t, frame)
	assert.Equal(t, "worker", frame.To, "a read names its node as well as a write")

	_, err = owner.TerminalList(ctx, transport, "worker")
	require.NoError(t, err)
	frame = transport.tap("worker").terminalFrame("terminal/list")
	require.NotNil(t, frame)
	assert.Equal(t, "worker", frame.To)
}

// A request the owner signed for one node is refused by every other, whatever the operation: the node names itself.
func TestBoundRequestForAnotherNodeIsRefusedForEveryOperation(t *testing.T) {
	owner := newOwner(t, "owner")
	worker := enroll(t, owner, "worker")
	addPeer(t, owner, "worker")
	transport := &boundPipes{nodes: map[string]*Node{"worker": worker}, services: map[string]*TerminalServices{"worker": statusService()}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key, err := readKey(filepath.Join(owner.Dir, "owner.key"))
	require.NoError(t, err)

	ops := []string{"terminal/list", "terminal/open", "terminal/history", "chat/read", "chat/send", "chat/action", "chat/upload",
		"chat/file", "chat/paths", "chat/rolefile", "chat/looked", "crew/status", "item/read", "todo/act", "alarm/act"}
	for _, op := range ops {
		for _, to := range []string{"other", ""} {
			rw, c, err := owner.terminalConnect(ctx, transport, "worker")
			require.NoError(t, err)
			// Signed correctly for this node's key and challenge, but naming another node (or none): what a request
			// made for a node that is not this one looks like to the owner's signature.
			req := TerminalRequest{Crew: "ops", Role: "dev", Lines: 10, Cols: 80, Rows: 24, To: to}
			req.Sig = ed25519.Sign(key, terminalMessageV2(op, worker.Cert.Key, c.challenge, req))
			require.NoError(t, c.send(wire{Op: op, Terminal: &req}))
			var w wire
			require.NoError(t, c.dec.Decode(&w))
			assert.Equal(t, "err", w.Op, op)
			assert.Contains(t, w.Err, "this request is for node", op)
			_ = rw.Close()
		}
	}
}

// A bound request replayed to another node is refused: the owner signed it against the key and the challenge of the
// node it talked to. X is certified and rogue: it answers the owner's hello with the nonce Y gave it and passes the
// owner's request on to Y.
func TestBoundRequestRelayedByAPeerIsRefused(t *testing.T) {
	owner := newOwner(t, "owner")
	x := enroll(t, owner, "x")
	y := enroll(t, owner, "y")

	err := relayOwnerRequest(t, owner, x, y, true, "terminal/list")
	require.Error(t, err)
	assert.ErrorContains(t, err, "owner's signature", "Y did not take what the owner signed for X")

	err = relayOwnerRequest(t, owner, x, y, true, "crew/status")
	require.Error(t, err)
	assert.ErrorContains(t, err, "owner's signature")
}

// A rogue X that does not say terminal_v2 gets the old form signed and relays it: until strict_requests is on at Y,
// that still works. This is the rollout's known gap, shut by the switch.
func TestOldFormRelayedByAPeerIsTakenOnlyWhileStrictIsOff(t *testing.T) {
	owner := newOwner(t, "owner")
	x := enroll(t, owner, "x")
	y := enroll(t, owner, "y")

	require.NoError(t, relayOwnerRequest(t, owner, x, y, false, "terminal/list"), "the gap: old form is still taken")

	y.StrictRequests = true
	err := relayOwnerRequest(t, owner, x, y, false, "terminal/list")
	require.Error(t, err)
	assert.ErrorContains(t, err, "strict_requests")
}

// relayOwnerRequest has the owner talk to X while X, as a caller of Y, uses Y's challenge as its own, and sends what the
// owner signed on to Y. It returns what Y answered (nil when Y did the operation). xSaysV2 is whether X's hello says
// terminal_v2 to the owner.
func relayOwnerRequest(t *testing.T, owner, x, y *Node, xSaysV2 bool, op string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	toY, yServe := net.Pipe()
	go func() {
		defer func() { _ = yServe.Close() }()
		_ = y.ServeTerminals(ctx, yServe, func(string, string, string, string, time.Time) error { return nil }, time.Now, statusService(), nil)
	}()
	defer func() { _ = toY.Close() }()
	cy := newConn(toY)
	_, err := x.open(cy, "y", time.Now)
	require.NoError(t, err)

	ownerEnd, xEnd := net.Pipe()
	result := make(chan error, 1)
	go func() {
		defer func() { _ = xEnd.Close() }()
		cx := newConn(xEnd)
		hi, err := cx.expect("hello")
		if err != nil {
			result <- err
			return
		}
		hello := wire{Op: "hello", V: protocolVersion, Cert: &x.Cert, Nonce: cy.challenge, Sig: ed25519.Sign(x.Key, helloMessage("server", hi.Nonce))}
		if xSaysV2 {
			hello.TerminalV2, hello.TerminalV2Sig = true, ed25519.Sign(x.Key, terminalV2CapabilityMessage("server", hi.Nonce))
		}
		if err = cx.send(hello); err == nil {
			_, err = cx.expect("auth")
		}
		if err == nil {
			err = cx.send(wire{Op: "ok"})
		}
		var frame wire
		if err == nil {
			err = cx.dec.Decode(&frame)
		}
		if err == nil {
			err = cy.send(wire{Op: frame.Op, Terminal: frame.Terminal})
		}
		if err == nil {
			var got wire
			if err = cy.dec.Decode(&got); err == nil && got.Op == "err" {
				err = &relayRefused{got.Err}
			}
		}
		result <- err
	}()
	co := newConn(ownerEnd)
	_, err = owner.open(co, "x", time.Now)
	require.NoError(t, err)
	require.NoError(t, owner.terminalRequest(co, op, TerminalRequest{}))
	err = <-result
	_ = ownerEnd.Close()
	return err
}

type relayRefused struct{ msg string }

func (e *relayRefused) Error() string { return "peer: " + e.msg }

// An old-form request is taken while strict_requests is off, with an event (at most one a day per caller); with it on
// it is refused and the node says what to do.
func TestOldFormIsTakenWithAnEventThenRefusedWhenStrict(t *testing.T) {
	owner := newOwner(t, "owner")
	worker := enroll(t, owner, "worker")
	addPeer(t, owner, "worker")
	clock := time.Date(2027, 1, 10, 9, 0, 0, 0, time.UTC)
	transport := &boundPipes{nodes: map[string]*Node{"worker": worker}, services: map[string]*TerminalServices{"worker": statusService()},
		now: func() time.Time { return clock }}
	var notices []string
	worker.Notice = func(kind, detail string) { notices = append(notices, kind+": "+detail) }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// What an older owner node sends: the old form, no node named.
	old := func() error {
		rw, c, err := owner.terminalConnect(ctx, transport, "worker")
		require.NoError(t, err)
		defer func() { _ = rw.Close() }()
		c.peer.TerminalV2 = false
		require.NoError(t, owner.terminalRequest(c, "crew/status", TerminalRequest{}))
		_, err = c.expect("crew/status-result")
		return err
	}

	require.NoError(t, old(), "an old request is taken while strict is off")
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0], "terminal-unbound: took crew/status from owner signed in the old form")
	frame := transport.tap("worker").terminalFrame("crew/status")
	require.NotNil(t, frame)
	assert.Empty(t, frame.To, "the old form names no node")

	require.NoError(t, old())
	assert.Len(t, notices, 1, "one event a day per caller, not one per request")

	clock = clock.Add(25 * time.Hour)
	require.NoError(t, old())
	assert.Len(t, notices, 2, "the next day it is said again")

	// A bound request is no event.
	_, err := owner.CrewStatus(ctx, transport, "worker", "")
	require.NoError(t, err)
	assert.Len(t, notices, 2)

	worker.StrictRequests = true
	err = old()
	require.Error(t, err)
	assert.ErrorContains(t, err, "strict_requests is on")
	assert.ErrorContains(t, err, "update zmina on the owner's node")

	// The bound form is what strict takes.
	_, err = owner.CrewStatus(ctx, transport, "worker", "")
	require.NoError(t, err)
}

// The switch is kept in node.json and survives a reopen and the owner's later certificate.
func TestStrictRequestsIsKeptWithTheNode(t *testing.T) {
	owner := newOwner(t, "owner")
	require.False(t, owner.StrictRequests, "off by default")

	require.NoError(t, SetStrictRequests(owner.Dir, true))
	n, err := Open(owner.Dir)
	require.NoError(t, err)
	assert.True(t, n.StrictRequests)

	require.NoError(t, SetStrictRequests(owner.Dir, false))
	n, err = Open(owner.Dir)
	require.NoError(t, err)
	assert.False(t, n.StrictRequests)

	require.ErrorIs(t, SetStrictRequests(t.TempDir(), true), ErrNoNode)
}

// The capability is signed over the other side's nonce, both ways: an unsigned word is refused, like the others.
func TestTerminalV2CapabilityRequiresSignatureBothWays(t *testing.T) {
	t.Run("a caller that says it without proof", func(t *testing.T) {
		owner := newOwner(t, "alpha")
		clientNode := enroll(t, owner, "beta")
		client, server := net.Pipe()
		require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))
		done := make(chan error, 1)
		go func() {
			defer func() { _ = server.Close() }()
			done <- owner.Serve(server, (&inbox{}).deliver, time.Now)
		}()
		c := newConn(client)
		challenge, err := nonce()
		require.NoError(t, err)
		require.NoError(t, c.send(wire{Op: "hello", V: protocolVersion, Cert: &clientNode.Cert, Nonce: challenge, TerminalV2: true}))
		hi, err := c.expect("hello")
		require.NoError(t, err)
		assert.True(t, hi.TerminalV2, "a node that is new says so")
		require.True(t, ed25519.Verify(owner.Cert.Key, terminalV2CapabilityMessage("server", challenge), hi.TerminalV2Sig))
		require.NoError(t, c.send(wire{Op: "auth", Sig: ed25519.Sign(clientNode.Key, helloMessage("client", hi.Nonce))}))
		_, err = c.expect("ok")
		require.ErrorContains(t, err, "unauthenticated terminal_v2")
		require.NoError(t, client.Close())
		require.Error(t, <-done)
	})

	t.Run("a server that says it without proof", func(t *testing.T) {
		owner := newOwner(t, "alpha")
		clientNode := enroll(t, owner, "beta")
		client, server := net.Pipe()
		require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))
		go func() {
			defer func() { _ = server.Close() }()
			c := newConn(server)
			hi, err := c.expect("hello")
			if err != nil {
				return
			}
			_ = c.send(wire{Op: "hello", V: protocolVersion, Cert: &owner.Cert, Nonce: []byte("n"),
				Sig: ed25519.Sign(owner.Key, helloMessage("server", hi.Nonce)), TerminalV2: true})
			_, _ = c.expect("auth")
		}()
		_, err := clientNode.open(newConn(client), "alpha", time.Now)
		require.ErrorContains(t, err, "unauthenticated terminal_v2")
	})
}

// A node that has not said terminal_v2 (an older zmina) gets the old form; one that has gets the bound one; and each
// side keeps what the other said.
func TestTerminalV2IsNegotiatedBothWays(t *testing.T) {
	owner := newOwner(t, "owner")
	worker := enroll(t, owner, "worker")
	old := enroll(t, owner, "old")
	addPeer(t, owner, "worker")
	addPeer(t, owner, "old")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A server of an older zmina: its hello carries no terminal_v2.
	oldServer := func(sniffed chan<- *TerminalRequest) Transport {
		return transportFunc(func(ctx context.Context, a Addr) (io.ReadWriteCloser, error) {
			left, right := net.Pipe()
			go func() {
				defer func() { _ = right.Close() }()
				c := newConn(right)
				hi, err := c.expect("hello")
				if err != nil {
					return
				}
				c.challenge = []byte("older challenge")
				_ = c.send(wire{Op: "hello", V: protocolVersion, Cert: &old.Cert, Nonce: c.challenge,
					Sig: ed25519.Sign(old.Key, helloMessage("server", hi.Nonce))})
				if _, err = c.expect("auth"); err != nil {
					return
				}
				_ = c.send(wire{Op: "ok"})
				var frame wire
				if err = c.dec.Decode(&frame); err != nil {
					return
				}
				sniffed <- frame.Terminal
				_ = c.send(wire{Op: "crew/status-result", Data: []byte(`{}`)})
			}()
			return left, nil
		})
	}

	sniffed := make(chan *TerminalRequest, 1)
	_, err := owner.CrewStatus(ctx, oldServer(sniffed), "old", "")
	require.NoError(t, err)
	got := <-sniffed
	require.NotNil(t, got)
	assert.Empty(t, got.To, "an older node is sent the old form: it would not know the other")
	assert.True(t, ed25519.Verify(owner.Owner, terminalMessage("crew/status", []byte("older challenge"), *got), got.Sig))

	transport := &boundPipes{nodes: map[string]*Node{"worker": worker}, services: map[string]*TerminalServices{"worker": statusService()}}
	_, err = owner.CrewStatus(ctx, transport, "worker", "")
	require.NoError(t, err)
	assert.Equal(t, "worker", transport.tap("worker").terminalFrame("crew/status").To)

	// Both sides kept what the other authenticated.
	ownerBook, err := owner.Peers()
	require.NoError(t, err)
	for _, p := range ownerBook {
		assert.Equal(t, p.Name == "worker", p.TerminalV2, "owner's book: %s", p.Name)
	}
	var workerSees Peer
	book, err := worker.Peers()
	require.NoError(t, err)
	for _, p := range book {
		if p.Name == "owner" {
			workerSees = p
		}
	}
	assert.True(t, workerSees.TerminalV2, "the server kept that its caller said terminal_v2")
}

type transportFunc func(ctx context.Context, a Addr) (io.ReadWriteCloser, error)

func (f transportFunc) Dial(ctx context.Context, a Addr) (io.ReadWriteCloser, error) {
	return f(ctx, a)
}

// The bound form goes through a relay's tunnel too: the owner's hello is with the destination, so it is addressed to it.
func TestBoundRequestThroughARelayIsForTheDestination(t *testing.T) {
	owner := newOwner(t, "owner")
	jump := enroll(t, owner, "jump")
	worker := enroll(t, owner, "worker")
	addRelay(t, owner, "jump")
	addPeer(t, jump, "worker")
	require.NoError(t, owner.UpdatePeers(func(ps []Peer) []Peer { return append(ps, Peer{Name: "worker", Through: []string{"jump"}}) }))
	transport := &boundPipes{nodes: map[string]*Node{"jump": jump, "worker": worker}, services: map[string]*TerminalServices{"worker": statusService()}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := owner.CrewStatus(ctx, transport, "worker", "")
	require.NoError(t, err)
	frame := transport.tap("worker").terminalFrame("crew/status")
	require.NotNil(t, frame)
	assert.Equal(t, "worker", frame.To)
}
