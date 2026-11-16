package mesh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
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

// holdingRelay is a certified node of the mesh that carries the owner's session to the worker and does what a relay can do with it: it
// shows the owner the worker's hello as it is, but with the challenge of a session of its own with the worker, drops the owner's auth and
// answers ok itself, and keeps the request the owner then signs. The worker's signatures in its hello are over the owner's nonce and
// say nothing of the challenge, so the owner takes the hello for the worker's.
type holdingRelay struct {
	t      *testing.T
	relay  *Node
	worker *Node
	via    *boundPipes

	mu   sync.Mutex
	kept *wire
	done chan struct{}
	// own is the relay's own session with the worker, whose challenge the owner was shown.
	own *conn
}

func (h *holdingRelay) Dial(_ context.Context, a Addr) (io.ReadWriteCloser, error) {
	left, right := net.Pipe()
	h.done = make(chan struct{})

	// The relay's own sessions are its own: they do not end when the owner's connection does.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	h.t.Cleanup(cancel)

	go func() {
		defer close(h.done)
		defer func() { _ = right.Close() }()

		owner := newConn(right)

		hi, err := owner.expect("hello")
		if err != nil {
			return
		}

		// The worker's hello over the owner's nonce: a session of the relay's, whose hello nonce is the owner's.
		first, err := h.via.Dial(ctx, Addr{Cmd: "worker"})
		if err != nil {
			return
		}

		defer func() { _ = first.Close() }()

		toWorker := newConn(first)
		if err = toWorker.send(wire{Op: "hello", V: protocolVersion, Cert: &h.relay.Cert, Nonce: hi.Nonce}); err != nil {
			return
		}

		told, err := toWorker.expect("hello")
		if err != nil {
			return
		}

		// The relay's own session with the worker, whose challenge is the one the owner is made to sign over.
		second, err := h.via.Dial(ctx, Addr{Cmd: "worker"})
		if err != nil {
			return
		}

		own := newConn(second)
		if _, err = h.relay.open(own, "worker", time.Now); err != nil {
			return
		}

		h.mu.Lock()
		h.own = own
		h.mu.Unlock()

		told.Nonce = own.challenge

		if err = owner.send(told); err != nil {
			return
		}

		if _, err = owner.expect("auth"); err != nil {
			return
		}

		if err = owner.send(wire{Op: "ok"}); err != nil {
			return
		}

		var frame wire
		if err = owner.dec.Decode(&frame); err != nil {
			return
		}

		h.mu.Lock()
		h.kept = &frame
		h.mu.Unlock()

		// What the owner sees is a connection that broke.
	}()

	return left, nil
}

// sendKept sends the request the relay held on its own session with the worker and returns what the worker says.
func (h *holdingRelay) sendKept() wire {
	h.t.Helper()

	<-h.done

	h.mu.Lock()
	kept, own := h.kept, h.own
	h.mu.Unlock()

	require.NotNil(h.t, kept, "the relay held the owner's request")
	require.NotNil(h.t, own)
	require.NoError(h.t, own.send(*kept))

	var w wire
	require.NoError(h.t, own.dec.Decode(&w))

	return w
}

func relayWorld(t *testing.T) (owner, worker, relay *Node, svc *putService, h *holdingRelay) {
	t.Helper()

	owner = newOwner(t, "owner")
	worker = enroll(t, owner, "worker")
	relay = enroll(t, owner, "mallory")
	addPeer(t, owner, "worker")
	addPeer(t, relay, "worker")

	svc = &putService{res: CrewPutResult{Status: PutDone, Sum: strings.Repeat("a", 64)}}
	via := &boundPipes{nodes: map[string]*Node{"worker": worker}, services: map[string]*TerminalServices{"worker": svc.services()}}
	h = &holdingRelay{t: t, relay: relay, worker: worker, via: via}

	return owner, worker, relay, svc, h
}

// The reviewer's attack: a certified relay holds a push the owner signed and sends it later on its own session. The owner's request
// names the owner's node as the one that sends it, and the session the relay sends it on is the relay's: the worker refuses, and its
// service is not called.
func TestCrewPutHeldByARelayAndSentOnItsOwnSessionIsRefused(t *testing.T) {
	owner, _, _, svc, h := relayWorld(t)

	_, err := owner.CrewPut(putCtx(t), h, "worker", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n"), Base: someSum})
	require.Error(t, err, "the owner is shown a broken connection")
	assert.Empty(t, svc.calls())

	w := h.sendKept()
	assert.Equal(t, "err", w.Op)
	assert.Contains(t, w.Err, "signed for a session of another node")
	assert.Empty(t, svc.calls(), "the push was not done, then or later")
}

// The same for each step of a move.
func TestRoleOpsHeldByARelayAndSentOnItsOwnSessionAreRefused(t *testing.T) {
	for _, op := range []string{"role/move", "role/take"} {
		t.Run(op, func(t *testing.T) {
			owner, worker, _, _, h := relayWorld(t)
			svc := &moveService{ans: []byte(`{"state":"pending"}`)}
			h.via.services["worker"] = svc.services()

			var err error
			if op == "role/take" {
				_, err = owner.RoleTake(putCtx(t), h, "worker", goodStep(op))
			} else {
				_, err = owner.RoleMove(putCtx(t), h, "worker", goodStep(op))
			}

			require.Error(t, err)

			w := h.sendKept()
			assert.Equal(t, "err", w.Op)
			assert.Contains(t, w.Err, "signed for a session of another node")

			moves, takes, _ := svc.seen()
			assert.Empty(t, moves)
			assert.Empty(t, takes)
			_ = worker
		})
	}
}

// The relay cannot put its own key in the request either: the key is inside what the owner signed.
func TestRelayCannotChangeTheCallerKeyOfAHeldRequest(t *testing.T) {
	owner, _, relay, svc, h := relayWorld(t)

	_, err := owner.CrewPut(putCtx(t), h, "worker", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n")})
	require.Error(t, err)

	<-h.done
	h.kept.Terminal.From = relay.Cert.Key

	w := h.sendKept()
	assert.Equal(t, "err", w.Op)
	assert.Contains(t, w.Err, "requires the owner's signature")
	assert.Empty(t, svc.calls())
}

// An honest push still goes: direct, and through a relay's tunnel, where the owner's own hello is with the worker, so the session's
// peer is the owner's node and the key the request names is the one the session proved.
func TestBoundCallerPushAndMoveWorkDirectAndThroughARelay(t *testing.T) {
	for _, relayed := range []bool{false, true} {
		name := map[bool]string{false: "direct", true: "relayed"}[relayed]

		t.Run(name, func(t *testing.T) {
			owner := newOwner(t, "owner")
			jump := enroll(t, owner, "jump")
			worker := enroll(t, owner, "worker")
			puts := &putService{res: CrewPutResult{Status: PutDone, Sum: strings.Repeat("a", 64)}}
			moves := &moveService{ans: []byte(`{"state":"pending"}`)}
			services := puts.services()
			services.RoleMove, services.RoleTake = moves.services().RoleMove, moves.services().RoleTake
			transport := &boundPipes{nodes: map[string]*Node{"jump": jump, "worker": worker}, services: map[string]*TerminalServices{"worker": services}}

			if relayed {
				addRelay(t, owner, "jump")
				addPeer(t, jump, "worker")
				require.NoError(t, owner.UpdatePeers(func(ps []Peer) []Peer { return append(ps, Peer{Name: "worker", Through: []string{"jump"}}) }))
			} else {
				addPeer(t, owner, "worker")
			}

			res, err := owner.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n"), Base: someSum})
			require.NoError(t, err)
			assert.Equal(t, PutDone, res.Status)
			require.Len(t, puts.calls(), 1)
			assert.Equal(t, "mesh owner", puts.calls()[0].by, "the session's peer was the owner's node, not the relay")

			frame := transport.tap("worker").terminalFrame("crew/put")
			require.NotNil(t, frame)
			assert.Equal(t, ed25519.PublicKey(owner.Cert.Key), ed25519.PublicKey(frame.From), "the request names the owner's node")
			assert.NotEqual(t, jump.Cert.Key, ed25519.PublicKey(frame.From))
			assert.InDelta(t, time.Now().Add(requestLife).Unix(), frame.NotAfter, 5)

			_, err = owner.RoleMove(putCtx(t), transport, "worker", goodStep("role/move"))
			require.NoError(t, err)
			_, err = owner.RoleTake(putCtx(t), transport, "worker", goodStep("role/take"))
			require.NoError(t, err)

			seenMoves, seenTakes, _ := moves.seen()
			require.Len(t, seenMoves, 1)
			require.Len(t, seenTakes, 1)
		})
	}
}

// The ops that were on the wire before carry neither field, so what they sign is what it was (a node of an older build verifies them).
func TestOpsOfOlderBuildsSignWithoutTheNewFields(t *testing.T) {
	owner, _, _, transport := putWorld(t)

	_, err := owner.CrewStatus(putCtx(t), transport, "worker", "")
	require.Error(t, err, "the service of putWorld has no status; the frame was sent all the same")

	frame := transport.tap("worker").terminalFrame("crew/status")
	require.NotNil(t, frame)
	assert.Empty(t, frame.From)
	assert.Zero(t, frame.NotAfter)

	raw, err := json.Marshal(TerminalRequest{Crew: "ops", Role: "dev", To: "worker"})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "from")
	assert.NotContains(t, string(raw), "not_after")
}

// The refusals of the caller's key and the time, for each op that runs code on a node: the owner signed a request, but not one that names
// the key of the session it arrives on and a time not past.
func TestBoundCallerRefusals(t *testing.T) {
	now := time.Now()
	owner, worker, svc, transport := putWorld(t)
	moves := &moveService{}
	transport.services["worker"].RoleMove = moves.services().RoleMove
	transport.services["worker"].RoleTake = moves.services().RoleTake

	for _, op := range []string{"crew/put", "role/move", "role/take"} {
		for name, c := range map[string]struct {
			mutate func(req *TerminalRequest)
			says   string
		}{
			"no key of the caller":            {func(r *TerminalRequest) { r.From = nil }, "signed for a session of another node"},
			"the key of another node":         {func(r *TerminalRequest) { r.From = worker.Cert.Key }, "signed for a session of another node"},
			"a key too short":                 {func(r *TerminalRequest) { r.From = owner.Cert.Key[:16] }, "signed for a session of another node"},
			"no time":                         {func(r *TerminalRequest) { r.NotAfter = 0 }, "names no time"},
			"a time a minute and a half past": {func(r *TerminalRequest) { r.NotAfter = now.Add(-90 * time.Second).Unix() }, "out of date"},
			"a time long past":                {func(r *TerminalRequest) { r.NotAfter = now.Add(-time.Hour).Unix() }, "out of date"},
			"a time eleven minutes ahead":     {func(r *TerminalRequest) { r.NotAfter = now.Add(11 * time.Minute).Unix() }, "further ahead"},
			"a time a day ahead":              {func(r *TerminalRequest) { r.NotAfter = now.Add(24 * time.Hour).Unix() }, "further ahead"},
		} {
			t.Run(op+" "+name, func(t *testing.T) {
				req := goodStep(op)
				if op == "crew/put" {
					req = TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n")}
				}

				req.From, req.NotAfter = owner.Cert.Key, now.Add(requestLife).Unix()
				c.mutate(&req)

				w := rawSigned(t, owner, transport, op, req)
				assert.Equal(t, "err", w.Op)
				assert.Contains(t, w.Err, c.says)
			})
		}
	}

	assert.Empty(t, svc.calls())

	seenMoves, seenTakes, _ := moves.seen()
	assert.Empty(t, seenMoves)
	assert.Empty(t, seenTakes)
}

// rawSigned signs req exactly as it is (From and NotAfter as the test set them) for the worker's session and sends it.
func rawSigned(t *testing.T, owner *Node, transport Transport, op string, req TerminalRequest) wire {
	t.Helper()

	rw, c, err := owner.terminalConnect(putCtx(t), transport, "worker")
	require.NoError(t, err)

	defer func() { _ = rw.Close() }()

	key, err := readKey(filepath.Join(owner.Dir, "owner.key"))
	require.NoError(t, err)

	req.To = "worker"
	req.Sig = ed25519.Sign(key, terminalMessageV2(op, c.serverKey, c.challenge, req))
	require.NoError(t, c.send(wire{Op: op, Terminal: &req}))

	var w wire
	require.NoError(t, c.dec.Decode(&w))

	return w
}

// The target's clock decides: a request is taken until its time and a minute more, a frame the owner signed is no good after.
func TestAnExpiredPushIsRefusedAndOneWithinTheSkewIsTaken(t *testing.T) {
	for name, c := range map[string]struct {
		later time.Duration
		says  string
	}{
		"at once":                     {0, ""},
		"a minute in":                 {time.Minute, ""},
		"half a minute past its end":  {2*time.Minute + 30*time.Second, ""},
		"two minutes past its end":    {4 * time.Minute, "out of date"},
		"a day after, a captured one": {24 * time.Hour, "out of date"},
	} {
		t.Run(name, func(t *testing.T) {
			owner, _, svc, transport := putWorld(t)
			transport.now = func() time.Time { return time.Now().Add(c.later) }

			// The target's clock is the one that moved: the owner signed the request for requestLife from now.
			_, err := owner.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n"), Base: someSum})

			if c.says == "" {
				require.NoError(t, err)
				assert.Len(t, svc.calls(), 1)

				return
			}

			require.ErrorContains(t, err, c.says)
			assert.Empty(t, svc.calls())
		})
	}
}

// What a node answers is signed for the session, and what the owner reads is what was signed: a relay that carries the answer cannot
// make one up, change it, or take one from another session.
type answerTamper struct {
	inner Transport
	// change is applied to every line the worker sends the owner, with its op; it may return another line.
	change func(op string, line []byte) []byte
}

func (a answerTamper) Dial(ctx context.Context, addr Addr) (io.ReadWriteCloser, error) {
	rw, err := a.inner.Dial(ctx, addr)
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()

	go func() {
		defer func() { _ = pw.Close() }()

		var pending []byte

		buf := make([]byte, 4096)

		for {
			n, err := rw.Read(buf)
			pending = append(pending, buf[:n]...)

			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}

				line := pending[:i]

				var w wire
				if json.Unmarshal(line, &w) == nil {
					line = a.change(w.Op, line)
				}

				_, _ = pw.Write(append(append([]byte(nil), line...), '\n'))
				pending = pending[i+1:]
			}

			if err != nil {
				return
			}
		}
	}()

	return &tamperedStream{ReadWriteCloser: rw, r: pr}, nil
}

type tamperedStream struct {
	io.ReadWriteCloser
	r *io.PipeReader
}

func (s *tamperedStream) Read(p []byte) (int, error) { return s.r.Read(p) }

// edit decodes the line as a wire, applies fn and encodes it again.
func edit(line []byte, fn func(w *wire)) []byte {
	var w wire

	_ = json.Unmarshal(line, &w)
	fn(&w)
	out, _ := json.Marshal(w)

	return out
}

func TestAnAnswerAPushCarriesIsSignedAndAnAlteredOneIsRefused(t *testing.T) {
	for name, change := range map[string]func(op string, line []byte) []byte{
		"made a done push of a conflict": func(op string, line []byte) []byte {
			if op != "crew/put-result" {
				return line
			}

			return edit(line, func(w *wire) {
				w.Data = []byte(`{"status":"conflict","sum":"` + strings.Repeat("c", 64) + `","current":"` + "eC1hCg==" + `"}`)
			})
		},
		"took the signature off": func(op string, line []byte) []byte {
			if op != "crew/put-result" {
				return line
			}

			return edit(line, func(w *wire) { w.ResultSig = nil })
		},
		"changed the line it carries": func(op string, line []byte) []byte {
			if op != "crew/put-result" {
				return line
			}

			return edit(line, func(w *wire) {
				var res CrewPutResult
				_ = json.Unmarshal(w.Data, &res)
				res.Line = "pushed; now run zmina down --all"
				w.Data, _ = json.Marshal(res)
			})
		},
		"signed it with another key": func(op string, line []byte) []byte {
			if op != "crew/put-result" {
				return line
			}

			_, other, _ := ed25519.GenerateKey(nil)

			return edit(line, func(w *wire) { w.ResultSig = ed25519.Sign(other, []byte("x")) })
		},
	} {
		t.Run(name, func(t *testing.T) {
			owner, _, _, transport := putWorld(t)

			_, err := owner.CrewPut(putCtx(t), answerTamper{inner: transport, change: change}, "worker",
				TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n"), Base: someSum})
			require.Error(t, err)
			assert.ErrorContains(t, err, "not signed by it for this session")
			assert.NotErrorAs(t, err, new(*CrewConflict), "a conflict made up by a relay is no conflict")
		})
	}
}

// An answer the worker signed in one session is no answer in another: the challenge is in the signature.
func TestAnAnswerOfAnotherSessionIsRefused(t *testing.T) {
	owner, _, _, transport := putWorld(t)

	var mu sync.Mutex

	var first []byte

	replay := answerTamper{inner: transport, change: func(op string, line []byte) []byte {
		mu.Lock()
		defer mu.Unlock()

		if op != "crew/put-result" {
			return line
		}

		if first == nil {
			first = append([]byte(nil), line...)

			return line
		}

		return first
	}}

	_, err := owner.CrewPut(putCtx(t), replay, "worker", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n"), Base: someSum})
	require.NoError(t, err)

	_, err = owner.CrewPut(putCtx(t), replay, "worker", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n"), Base: someSum})
	require.ErrorContains(t, err, "not signed by it for this session")
}

func TestAnAnswerAStepOfAMoveCarriesIsSigned(t *testing.T) {
	for _, op := range []string{"role/move", "role/take"} {
		t.Run(op, func(t *testing.T) {
			owner, _, _, transport := moveWorld(t)

			forge := answerTamper{inner: transport, change: func(o string, line []byte) []byte {
				if o != op+"-result" {
					return line
				}

				return edit(line, func(w *wire) { w.Data = []byte(`{"state":"ready","bundle":"e30="}`) })
			}}

			var err error
			if op == "role/take" {
				_, err = owner.RoleTake(putCtx(t), forge, "worker", goodStep(op))
			} else {
				_, err = owner.RoleMove(putCtx(t), forge, "worker", goodStep(op))
			}

			require.ErrorContains(t, err, "not signed by it for this session")

			unsigned := answerTamper{inner: transport, change: func(o string, line []byte) []byte {
				if o != op+"-result" {
					return line
				}

				return edit(line, func(w *wire) { w.ResultSig = nil })
			}}

			if op == "role/take" {
				_, err = owner.RoleTake(putCtx(t), unsigned, "worker", goodStep(op))
			} else {
				_, err = owner.RoleMove(putCtx(t), unsigned, "worker", goodStep(op))
			}

			require.ErrorContains(t, err, "not signed by it for this session")
		})
	}
}

// What a node signs but is no honest answer is shaped before the owner reads it: another status, a hash that is none, a line with an
// escape sequence in it. The line and the warnings come out Plain.
func TestAnAnswerOfAPushIsCheckedAndMadePlain(t *testing.T) {
	owner, _, svc, transport := putWorld(t)

	svc.res = CrewPutResult{Status: PutDone, Sum: strings.Repeat("a", 64), Was: strings.Repeat("b", 64),
		Line: "replaced\x1b[2J the file", Warnings: []string{"crew.yaml line 3\x1b]0;title\x07: x"}}

	res, err := owner.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n"), Base: someSum})
	require.NoError(t, err)
	assert.Equal(t, "replaced[2J the file", res.Line)
	assert.Equal(t, []string{"crew.yaml line 3]0;title: x"}, res.Warnings)

	svc.res = CrewPutResult{Status: "done-ish", Sum: strings.Repeat("a", 64)}
	_, err = owner.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n")})
	require.ErrorContains(t, err, "status")

	svc.res = CrewPutResult{Status: PutConflict, Sum: "\x1b[2Jnot-a-sum"}
	_, err = owner.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n")})
	require.ErrorContains(t, err, "a hash that is none")
	assert.NotContains(t, err.Error(), "\x1b")

	many := make([]string, 200)
	for i := range many {
		many[i] = strings.Repeat("w", 5000)
	}

	svc.res = CrewPutResult{Status: PutDone, Sum: strings.Repeat("a", 64), Warnings: many, Line: strings.Repeat("l", 9000)}
	res, err = owner.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n"), Base: someSum})
	require.NoError(t, err)
	assert.Len(t, res.Warnings, putWarningsMax)
	assert.Len(t, res.Warnings[0], putTextMax)
	assert.Len(t, res.Line, putTextMax)
}

// What a peer says in an err line is text, and comes out Plain.
func TestAPeersErrorLineHasNoEscapeSequence(t *testing.T) {
	owner, _, svc, transport := putWorld(t)
	svc.err = errors.New("no such crew\x1b[2J\x1b[Hdone\r\nzmina: pushed everything")

	_, err := owner.CrewPut(putCtx(t), transport, "worker", TerminalRequest{Crew: "acme", Payload: []byte("home: alpha\n")})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "\x1b")
	assert.NotContains(t, err.Error(), "\n")
	assert.NotContains(t, err.Error(), "\r")
	assert.Contains(t, err.Error(), "no such crew")
}

// The owner's node keeps, for each node and crew, the sum it last pushed or confirmed there, in a file of the mesh directory.
func TestPushedSumIsKeptPerNodeAndCrew(t *testing.T) {
	owner := newOwner(t, "owner")

	assert.Empty(t, owner.PushedSum("beta", "ops"), "no record")

	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	require.NoError(t, owner.MarkPushed("beta", "ops", a))
	require.NoError(t, owner.MarkPushed("beta", "web", b))
	require.NoError(t, owner.MarkPushed("gamma", "ops", b))

	assert.Equal(t, a, owner.PushedSum("beta", "ops"))
	assert.Equal(t, b, owner.PushedSum("beta", "web"))
	assert.Equal(t, b, owner.PushedSum("gamma", "ops"))
	assert.Empty(t, owner.PushedSum("gamma", "web"))

	require.NoError(t, owner.MarkPushed("beta", "ops", b))
	assert.Equal(t, b, owner.PushedSum("beta", "ops"), "the last word")

	assert.FileExists(t, filepath.Join(owner.Dir, "pushed.json"))

	require.Error(t, owner.MarkPushed("beta", "ops", "not a sum"))
	require.Error(t, owner.MarkPushed("be ta", "ops", a))
	require.Error(t, owner.MarkPushed("beta", "../ops", a))
	assert.Equal(t, b, owner.PushedSum("beta", "ops"))
}
