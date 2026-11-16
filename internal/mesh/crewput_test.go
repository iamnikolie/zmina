package mesh

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// putCall is one call of the target's crew service.
type putCall struct {
	crew    string
	payload []byte
	base    string
	create  bool
	by      string
}

// putService is a target that records every push and every terminal open, and answers a push with res.
type putService struct {
	mu    sync.Mutex
	puts  []putCall
	opens int
	res   CrewPutResult
	err   error
}

func (p *putService) services() *TerminalServices {
	return &TerminalServices{
		Open: func(context.Context, string, string, uint16, uint16) (Terminal, error) {
			p.mu.Lock()
			p.opens++
			p.mu.Unlock()

			return nil, errors.New("a terminal opened")
		},
		CrewPut: func(crew string, payload []byte, base string, create bool, by string) (CrewPutResult, error) {
			p.mu.Lock()
			p.puts = append(p.puts, putCall{crew, payload, base, create, by})
			p.mu.Unlock()

			return p.res, p.err
		},
	}
}

func (p *putService) calls() []putCall {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]putCall(nil), p.puts...)
}

func (p *putService) opened() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.opens
}

// putWorld is an owner node, a worker that runs the service, and the pipes between them.
func putWorld(t *testing.T) (owner, worker *Node, svc *putService, transport *boundPipes) {
	t.Helper()

	owner = newOwner(t, "owner")
	worker = enroll(t, owner, "worker")
	addPeer(t, owner, "worker")
	svc = &putService{res: CrewPutResult{Status: PutDone, Sum: strings.Repeat("a", 64), Was: strings.Repeat("b", 64)}}
	transport = &boundPipes{nodes: map[string]*Node{"worker": worker}, services: map[string]*TerminalServices{"worker": svc.services()}}

	return owner, worker, svc, transport
}

func putCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	return ctx
}

var someSum = strings.Repeat("b", 64)

// A push reaches the target's service with the file, the base and who sent it, in the bound form, and what the target says
// comes back.
func TestCrewPutReachesTheTargetBoundToIt(t *testing.T) {
	owner, _, svc, transport := putWorld(t)

	res, err := owner.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n"), Base: someSum, Create: false})
	require.NoError(t, err)
	assert.Equal(t, PutDone, res.Status)
	assert.Equal(t, strings.Repeat("a", 64), res.Sum)

	require.Equal(t, []putCall{{crew: "acme", payload: []byte("home: alpha\n"), base: someSum, by: "mesh owner"}}, svc.calls())

	frame := transport.tap("worker").terminalFrame("crew/put")
	require.NotNil(t, frame)
	assert.Equal(t, "worker", frame.To, "the push names the node it is for")
	assert.Equal(t, []byte("home: alpha\n"), frame.Payload, "the file is inside what the owner signed")
	assert.Zero(t, svc.opened())
}

// The target's refusal of a file that is not the one the push was based on comes back as a conflict that carries its file.
func TestCrewPutConflictCarriesTheFileThere(t *testing.T) {
	owner, _, svc, transport := putWorld(t)
	svc.res = CrewPutResult{Status: PutConflict, Sum: strings.Repeat("c", 64), Current: []byte("their file\n")}

	_, err := owner.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Payload: []byte("mine\n"), Base: someSum})

	var conflict *CrewConflict
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "worker", conflict.Node)
	assert.Equal(t, strings.Repeat("c", 64), conflict.Have)
	assert.Equal(t, someSum, conflict.Base)
	assert.Equal(t, []byte("their file\n"), conflict.Current)
	assert.Contains(t, err.Error(), "worker holds a different file")

	// A file too big to send back is not.
	svc.res.Current = make([]byte, CrewFileMax+1)
	_, err = owner.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Payload: []byte("mine\n"), Base: someSum})
	require.ErrorAs(t, err, &conflict)
	assert.Nil(t, conflict.Current)
}

func TestCrewPutErrorOfTheTargetIsTheError(t *testing.T) {
	owner, _, svc, transport := putWorld(t)
	svc.err = errors.New("the file does not load on this node")

	_, err := owner.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Payload: []byte("x\n")})
	require.Error(t, err)
	assert.ErrorContains(t, err, "the file does not load on this node")
}

// rawPut signs a push the way mutate says and sends it on a fresh session with the worker, returning the worker's first
// reply. It is what a caller does that is not zmina's own client.
func rawPut(t *testing.T, owner *Node, transport Transport, sign func(key ed25519.PrivateKey, c *conn, req *TerminalRequest)) wire {
	t.Helper()

	rw, c, err := owner.terminalConnect(putCtx(t), transport, "worker")
	require.NoError(t, err)

	defer func() { _ = rw.Close() }()

	key, err := readKey(filepath.Join(owner.Dir, "owner.key"))
	require.NoError(t, err)

	req := TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n"), To: "worker", From: owner.Cert.Key,
		NotAfter: time.Now().Add(requestLife).Unix()}
	sign(key, c, &req)
	require.NoError(t, c.send(wire{Op: "crew/put", Terminal: &req}))

	var w wire
	require.NoError(t, c.dec.Decode(&w))

	return w
}

func signBound(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
	req.Sig = ed25519.Sign(key, terminalMessageV2("crew/put", c.serverKey, c.challenge, *req))
}

// The refusals: each is an err frame, and the service sees nothing.
func TestCrewPutRefusals(t *testing.T) {
	owner, worker, svc, transport := putWorld(t)

	for name, c := range map[string]struct {
		sign func(key ed25519.PrivateKey, c *conn, req *TerminalRequest)
		says string
	}{
		"unsigned": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {}, "requires the owner's signature"},
		"signed by another key": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
			_, other, _ := ed25519.GenerateKey(nil)
			req.Sig = ed25519.Sign(other, terminalMessageV2("crew/put", c.serverKey, c.challenge, *req))
		}, "requires the owner's signature"},
		"the old form": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
			req.Sig = ed25519.Sign(key, terminalMessage("crew/put", c.challenge, *req))
		}, "takes only an owner request bound to this node"},
		"another node's name": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
			req.To = "other"
			signBound(key, c, req)
		}, `this request is for node "other"`},
		"no node's name": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
			req.To = ""
			signBound(key, c, req)
		}, "this request is for node"},
		"another operation's signature": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
			req.Sig = ed25519.Sign(key, terminalMessageV2("terminal/list", c.serverKey, c.challenge, *req))
		}, "requires the owner's signature"},
		"another challenge": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
			req.Sig = ed25519.Sign(key, terminalMessageV2("crew/put", c.serverKey, []byte("another challenge"), *req))
		}, "requires the owner's signature"},
		"a file altered after signing": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
			signBound(key, c, req)
			req.Payload = []byte("home: gamma\n")
		}, "requires the owner's signature"},
		"a base altered after signing": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
			signBound(key, c, req)
			req.Base = someSum
		}, "requires the owner's signature"},
		"create added after signing": {func(key ed25519.PrivateKey, c *conn, req *TerminalRequest) {
			signBound(key, c, req)
			req.Create = true
		}, "requires the owner's signature"},
	} {
		t.Run(name, func(t *testing.T) {
			w := rawPut(t, owner, transport, c.sign)
			assert.Equal(t, "err", w.Op)
			assert.Contains(t, w.Err, c.says)
		})
	}

	// Strict off is the default: the old form is still refused for a push.
	require.False(t, worker.StrictRequests)
	assert.Empty(t, svc.calls())
	assert.Zero(t, svc.opened())
}

// What the owner signed once is of no use in another session: the challenge is the serving node's, fresh for each.
func TestCrewPutReplayInAnotherSessionIsRefused(t *testing.T) {
	owner, _, svc, transport := putWorld(t)

	_, err := owner.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n"), Base: someSum})
	require.NoError(t, err)
	require.Len(t, svc.calls(), 1)

	captured := transport.tap("worker").terminalFrame("crew/put")
	require.NotNil(t, captured)

	// A certified node that kept the frame opens its own session with the worker and sends it.
	rw, c, err := owner.terminalConnect(putCtx(t), transport, "worker")
	require.NoError(t, err)

	defer func() { _ = rw.Close() }()

	require.NoError(t, c.send(wire{Op: "crew/put", Terminal: captured}))

	var w wire
	require.NoError(t, c.dec.Decode(&w))
	assert.Equal(t, "err", w.Op)
	assert.Contains(t, w.Err, "requires the owner's signature")
	assert.Len(t, svc.calls(), 1, "the target did not do it twice")
}

// A relay that carries a push cannot change it: the file, the base and the crew are in the owner's signature.
func TestCrewPutAlteredByACarrierIsRefused(t *testing.T) {
	owner, _, svc, transport := putWorld(t)

	rw, c, err := owner.terminalConnect(putCtx(t), transport, "worker")
	require.NoError(t, err)

	defer func() { _ = rw.Close() }()

	key, err := readKey(filepath.Join(owner.Dir, "owner.key"))
	require.NoError(t, err)

	for name, change := range map[string]func(*TerminalRequest){
		"the file": func(r *TerminalRequest) { r.Payload = append([]byte("x: 1\n"), r.Payload...) },
		"the crew": func(r *TerminalRequest) { r.Crew = "ops" },
		"the base": func(r *TerminalRequest) { r.Base = strings.Repeat("d", 64) },
		"the make": func(r *TerminalRequest) { r.Create = true },
		"the node": func(r *TerminalRequest) { r.To = "owner" },
		"the whole": func(r *TerminalRequest) {
			*r = TerminalRequest{Crew: "ops", Payload: []byte("a\n"), To: "worker", Sig: r.Sig}
		},
	} {
		req := TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n"), Base: someSum, To: "worker"}
		signBound(key, c, &req)
		change(&req)
		require.NoError(t, c.send(wire{Op: "crew/put", Terminal: &req}), name)

		var w wire
		require.NoError(t, c.dec.Decode(&w), name)
		assert.Equal(t, "err", w.Op, name)

		// The session ends at a refusal: a new one for the next.
		_ = rw.Close()
		rw, c, err = owner.terminalConnect(putCtx(t), transport, "worker")
		require.NoError(t, err)
	}

	assert.Empty(t, svc.calls())
}

// The mesh checks what it can before it asks the service: size, crew name, base, an empty file; the service sees none of them.
func TestCrewPutBadRequestsNeverReachTheService(t *testing.T) {
	owner, _, svc, transport := putWorld(t)

	for name, c := range map[string]struct {
		mutate func(req *TerminalRequest)
		says   string
	}{
		"over the size cap":  {func(r *TerminalRequest) { r.Payload = make([]byte, CrewFileMax+1) }, "a node takes at most"},
		"empty":              {func(r *TerminalRequest) { r.Payload = nil }, "this one is empty"},
		"a path":             {func(r *TerminalRequest) { r.Crew = "../ops" }, "a crew's name, no path"},
		"a path down":        {func(r *TerminalRequest) { r.Crew = "ops/x" }, "a crew's name, no path"},
		"an absolute path":   {func(r *TerminalRequest) { r.Crew = "/etc" }, "a crew's name, no path"},
		"a hidden name":      {func(r *TerminalRequest) { r.Crew = ".ssh" }, "a crew's name, no path"},
		"no name":            {func(r *TerminalRequest) { r.Crew = "" }, "a crew's name, no path"},
		"a space":            {func(r *TerminalRequest) { r.Crew = "my crew" }, "a crew's name, no path"},
		"a line break":       {func(r *TerminalRequest) { r.Crew = "ops\nx" }, "a crew's name, no path"},
		"a long name":        {func(r *TerminalRequest) { r.Crew = strings.Repeat("a", 65) }, "a crew's name, no path"},
		"a bad base":         {func(r *TerminalRequest) { r.Base = "not-a-sum" }, "the hash of the file replaced"},
		"a short base":       {func(r *TerminalRequest) { r.Base = "abcd" }, "the hash of the file replaced"},
		"an upper-case base": {func(r *TerminalRequest) { r.Base = strings.ToUpper(someSum) }, "the hash of the file replaced"},
	} {
		t.Run(name, func(t *testing.T) {
			w := rawPut(t, owner, transport, func(key ed25519.PrivateKey, conn *conn, req *TerminalRequest) {
				c.mutate(req)
				signBound(key, conn, req)
			})
			assert.Equal(t, "err", w.Op)
			assert.Contains(t, w.Err, c.says)
		})
	}

	assert.Empty(t, svc.calls())
}

// The client does not send what the target would refuse for its size, and does not send to a node that did not say crew_sync.
func TestCrewPutClientChecksSizeFirst(t *testing.T) {
	owner, _, svc, transport := putWorld(t)

	_, err := owner.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Payload: make([]byte, CrewFileMax+1)})
	require.ErrorContains(t, err, "1 to")
	_, err = owner.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme"})
	require.ErrorContains(t, err, "1 to")
	assert.Nil(t, transport.tap("worker").terminalFrame("crew/put"), "nothing was sent")
	assert.Empty(t, svc.calls())
}

// oldNode is a node of a zmina from before crew push: its hello says terminal_v2 (as an older one may) and no crew_sync, and it
// refuses an op it does not know the way every older server does, with unknown op. It records the ops it was sent.
func oldNode(t *testing.T, old *Node, says func(*wire)) (Transport, func() []string) {
	t.Helper()

	var mu sync.Mutex

	var ops []string

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
			hello := wire{Op: "hello", V: protocolVersion, Cert: &old.Cert, Nonce: c.challenge,
				Sig:        ed25519.Sign(old.Key, helloMessage("server", hi.Nonce)),
				TerminalV2: true, TerminalV2Sig: ed25519.Sign(old.Key, terminalV2CapabilityMessage("server", hi.Nonce))}
			if says != nil {
				says(&hello)
			}

			_ = c.send(hello)

			if _, err = c.expect("auth"); err != nil {
				return
			}

			_ = c.send(wire{Op: "ok"})

			var frame wire
			if err = c.dec.Decode(&frame); err != nil {
				return
			}

			mu.Lock()
			ops = append(ops, frame.Op)
			mu.Unlock()

			// What the loop of an older zmina does with an op that is none of its own.
			_ = c.send(wire{Op: "err", Err: `unknown op "` + frame.Op + `"`})
		}()

		return left, nil
	}), func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), ops...) }
}

func TestCrewPutToAPeerThatDidNotSayCrewSyncIsRefusedBeforeAnythingIsSent(t *testing.T) {
	owner := newOwner(t, "owner")
	old := enroll(t, owner, "old")
	addPeer(t, owner, "old")

	transport, sent := oldNode(t, old, nil)

	_, err := owner.CrewPut(putCtx(t), transport, "old", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n")})
	require.Error(t, err)
	assert.ErrorContains(t, err, "old did not say crew_sync")
	assert.ErrorContains(t, err, "update it")
	assert.Empty(t, sent(), "not a frame of the push left this node")
}

// Were the capability word lacking its proof, the push is refused too: crew_sync is a signed word like terminal_v2.
func TestCrewPutToAPeerThatSaysCrewSyncWithoutProofIsRefused(t *testing.T) {
	owner := newOwner(t, "owner")
	old := enroll(t, owner, "old")
	addPeer(t, owner, "old")

	transport, sent := oldNode(t, old, func(h *wire) { h.CrewSync = true })

	_, err := owner.CrewPut(putCtx(t), transport, "old", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n")})
	require.ErrorContains(t, err, "unauthenticated crew_sync")
	assert.Empty(t, sent())
}

// What an older node does with a push that were sent to it all the same: refuses, with unknown op. It never opens a terminal. The
// same line of the serving loop answers a new node asked for an op that is none of its own.
func TestAnOpNoServerKnowsIsRefusedNotOpenedAsATerminal(t *testing.T) {
	owner, _, svc, transport := putWorld(t)

	for _, op := range []string{"crew/put-now", "crew/nonesuch", "terminal/open2", "crew/"} {
		rw, c, err := owner.terminalConnect(putCtx(t), transport, "worker")
		require.NoError(t, err)

		key, err := readKey(filepath.Join(owner.Dir, "owner.key"))
		require.NoError(t, err)

		// Even a request the owner signed, with a terminal size: an op the loop does not name is not served as a terminal.
		req := TerminalRequest{Crew: "acme", Role: "dev", Cols: 80, Rows: 24, To: "worker"}
		req.Sig = ed25519.Sign(key, terminalMessageV2(op, c.serverKey, c.challenge, req))
		require.NoError(t, c.send(wire{Op: op, Terminal: &req}))

		var w wire
		require.NoError(t, c.dec.Decode(&w))
		assert.Equal(t, "err", w.Op, op)
		assert.Contains(t, w.Err, "unknown op", op)

		_ = rw.Close()
	}

	assert.Zero(t, svc.opened())
	assert.Empty(t, svc.calls())
}

// A node that takes the op but has no service for it says so; it is not a terminal either.
func TestCrewPutWithNoServiceIsRefused(t *testing.T) {
	owner, _, svc, transport := putWorld(t)

	transport.services["worker"] = &TerminalServices{Open: svc.services().Open}

	_, err := owner.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n")})
	require.ErrorContains(t, err, "crew push unavailable")
	assert.Zero(t, svc.opened())

	transport.services["worker"] = nil

	_, err = owner.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n")})
	require.ErrorContains(t, err, "no terminal service")
}

// A node that is not the owner holds no owner.key, so it cannot sign a push at all.
func TestCrewPutNeedsTheOwnerKey(t *testing.T) {
	owner, worker, _, _ := putWorld(t)
	other := enroll(t, owner, "other")
	addPeer(t, other, "worker")

	transport := &boundPipes{nodes: map[string]*Node{"worker": worker}, services: map[string]*TerminalServices{"worker": (&putService{}).services()}}

	_, err := other.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n")})
	require.ErrorContains(t, err, "owner node")
}

// crew_sync is a word signed over the other side's nonce, both ways, like terminal_v2: a hello that says it without the
// proof is refused.
func TestCrewSyncCapabilityRequiresSignatureBothWays(t *testing.T) {
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
		require.NoError(t, c.send(wire{Op: "hello", V: protocolVersion, Cert: &clientNode.Cert, Nonce: challenge, CrewSync: true}))

		hi, err := c.expect("hello")
		require.NoError(t, err)
		assert.True(t, hi.CrewSync, "a node that is new says so")
		require.True(t, ed25519.Verify(owner.Cert.Key, crewSyncCapabilityMessage("server", challenge), hi.CrewSyncSig))
		require.NoError(t, c.send(wire{Op: "auth", Sig: ed25519.Sign(clientNode.Key, helloMessage("client", hi.Nonce))}))

		_, err = c.expect("ok")
		require.ErrorContains(t, err, "unauthenticated crew_sync")
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
				Sig: ed25519.Sign(owner.Key, helloMessage("server", hi.Nonce)), CrewSync: true})
			_, _ = c.expect("auth")
		}()

		_, err := clientNode.open(newConn(client), "alpha", time.Now)
		require.ErrorContains(t, err, "unauthenticated crew_sync")
	})

	t.Run("a word signed for the other side", func(t *testing.T) {
		assert.NotEqual(t, crewSyncCapabilityMessage("server", []byte{1}), crewSyncCapabilityMessage("client", []byte{1}))
		assert.NotEqual(t, crewSyncCapabilityMessage("server", []byte{1}), crewSyncCapabilityMessage("server", []byte{2}))
		assert.NotEqual(t, crewSyncCapabilityMessage("server", []byte{1}), terminalV2CapabilityMessage("server", []byte{1}),
			"not the word of another capability")
	})
}

// Each side keeps what the other authenticated: a new node's book entry says crew_sync, an older node's does not.
func TestCrewSyncIsNegotiatedBothWays(t *testing.T) {
	owner, worker, _, _ := putWorld(t)
	old := enroll(t, owner, "old")
	addPeer(t, owner, "old")

	net1 := &pipes{nodes: map[string]*Node{"worker": worker}, inboxes: map[string]*inbox{"worker": {}}, down: map[string]bool{}}

	ss := owner.Sync(context.Background(), net1, []Discovery{Book{Node: owner}}, (&inbox{}).deliver, "worker", time.Second, 0, time.Now)
	require.Len(t, ss, 1)
	require.NoError(t, ss[0].Err)

	// A session with the older node, so that the book keeps what it said.
	oldTransport, _ := oldNode(t, old, nil)
	rw, _, err := owner.terminalConnect(putCtx(t), oldTransport, "old")
	require.NoError(t, err)
	_ = rw.Close()

	book, err := owner.Peers()
	require.NoError(t, err)

	for _, p := range book {
		assert.Equal(t, p.Name == "worker", p.CrewSync, "owner's book: %s", p.Name)
	}

	seen, err := worker.Peers()
	require.NoError(t, err)

	for _, p := range seen {
		if p.Name == "owner" {
			assert.True(t, p.CrewSync, "the server kept that its caller said crew_sync")
		}
	}
}
