package mesh

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inbox records what a node's crews were handed.
type inbox struct {
	mu   sync.Mutex
	got  []string
	ids  []string
	sent []time.Time
	fail error
}

func (b *inbox) deliver(crew, from, id, body string, sent time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.fail != nil {
		return b.fail
	}

	b.got = append(b.got, crew+" ← "+from+": "+body)
	b.ids = append(b.ids, id)
	b.sent = append(b.sent, sent)

	return nil
}

func (b *inbox) list() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]string(nil), b.got...)
}

// net of nodes reached by name: an Addr{Cmd: name} dials that node's Serve over a pipe.
type pipes struct {
	nodes   map[string]*Node
	inboxes map[string]*inbox
	down    map[string]bool
}

func (p *pipes) Dial(_ context.Context, a Addr) (io.ReadWriteCloser, error) {
	n := p.nodes[a.Cmd]
	if n == nil || p.down[a.Cmd] {
		return nil, errors.New("unreachable")
	}

	c1, c2 := net.Pipe()

	go func() {
		_ = n.Serve(c2, p.inboxes[a.Cmd].deliver, time.Now)
		_ = c2.Close()
	}()

	return c1, nil
}

func newOwner(t *testing.T, name string) *Node {
	t.Helper()

	dir := t.TempDir()
	_, err := Init(dir, name)
	require.NoError(t, err)
	require.NoError(t, MakeOwner(dir, time.Now()))

	n, err := Open(dir)
	require.NoError(t, err)

	return n
}

func enroll(t *testing.T, owner *Node, name string) *Node {
	t.Helper()

	dir := t.TempDir()
	r, err := Init(dir, name)
	require.NoError(t, err)

	_, err = Open(dir)
	require.Error(t, err, "uncertified")

	g, err := SignRequest(owner.Dir, r, time.Now())
	require.NoError(t, err)
	require.NoError(t, Accept(dir, g))

	n, err := Open(dir)
	require.NoError(t, err)

	return n
}

func addPeer(t *testing.T, n *Node, name string) {
	t.Helper()
	require.NoError(t, n.UpdatePeers(func(ps []Peer) []Peer {
		return append(ps, Peer{Name: name, Addrs: []Addr{{Cmd: name}}, Source: "hand"})
	}))
}

func tell(t *testing.T, n *Node, crew, to, body string) Envelope {
	t.Helper()

	a, err := ParseAddress(to)
	require.NoError(t, err)

	e, err := n.NewEnvelope(crew, a, body, time.Now())
	require.NoError(t, err)
	require.NoError(t, n.Queue(e))

	return e
}

func trio(t *testing.T) (*pipes, *Node, *Node, *Node) {
	t.Helper()

	laptop := newOwner(t, "laptop")
	ops := enroll(t, laptop, "box-ops")
	web := enroll(t, laptop, "box-web")

	p := &pipes{
		nodes:   map[string]*Node{"laptop": laptop, "box-ops": ops, "box-web": web},
		inboxes: map[string]*inbox{"laptop": {}, "box-ops": {}, "box-web": {}},
		down:    map[string]bool{},
	}

	// Only the owner's machine can reach the others, like a laptop with ssh to a server whose users cannot.
	addPeer(t, laptop, "box-ops")
	addPeer(t, laptop, "box-web")

	return p, laptop, ops, web
}

func sync1(n *Node, p *pipes) []Session {
	return n.Sync(context.Background(), p, []Discovery{Book{n}}, p.inboxes[n.Name].deliver, "", 5*time.Second, 0, time.Now)
}

func TestDirectBothWays(t *testing.T) {
	p, laptop, ops, _ := trio(t)

	e := tell(t, laptop, "default", "box-ops/ops", "deploy is green")
	tell(t, ops, "ops", "laptop", "need a token for gitlab")

	for _, s := range sync1(laptop, p) {
		require.NoError(t, s.Err, s.Peer)
	}

	assert.Equal(t, []string{"ops ← laptop/default: deploy is green"}, p.inboxes["box-ops"].list())
	require.Len(t, p.inboxes["box-ops"].sent, 1)
	assert.True(t, e.SentAt().Equal(p.inboxes["box-ops"].sent[0]), "handed over with when its writer sent it")
	assert.False(t, e.SentAt().IsZero())
	assert.Equal(t, []string{" ← box-ops/ops: need a token for gitlab"}, p.inboxes["laptop"].list())

	out, _ := laptop.Outbox()
	assert.Empty(t, out)

	out, _ = ops.Outbox()
	assert.Empty(t, out)
}

func TestCarriedByAThirdNode(t *testing.T) {
	p, laptop, ops, web := trio(t)

	// ops cannot reach web; laptop reaches both. Three rounds at most: laptop meets web, ops hears laptop reaches web and hands
	// it over, laptop hands it to web.
	e := tell(t, ops, "ops", "box-web/web", "the dump is ready")

	for range 3 {
		sync1(laptop, p)
	}

	assert.Equal(t, []string{"web ← box-ops/ops: the dump is ready"}, p.inboxes["box-web"].list())
	assert.Empty(t, p.inboxes["laptop"].list(), "a carrier does not deliver what is not for it")

	// ops learnt web from laptop (told), without laptop's command line.
	book, _ := ops.Peers()
	for _, b := range book {
		for _, a := range b.Addrs {
			assert.Empty(t, a.Cmd, "a command line is never told")
		}
	}

	names := []string{}
	for _, b := range book {
		names = append(names, b.Name)
	}

	assert.ElementsMatch(t, []string{"laptop", "box-web"}, names)

	// A second copy of the same envelope is taken but not delivered again.
	e.Via = nil
	ids, err := web.receive([]Envelope{e}, p.inboxes["box-web"].deliver, time.Now())
	require.NoError(t, err)
	assert.Equal(t, []string{e.ID}, ids)
	assert.Len(t, p.inboxes["box-web"].list(), 1)
}

func TestNotTakenStaysQueued(t *testing.T) {
	p, laptop, _, _ := trio(t)

	tell(t, laptop, "default", "box-ops", "hello")

	p.inboxes["box-ops"].fail = errors.New("20 notes already wait")
	sync1(laptop, p)

	out, _ := laptop.Outbox()
	assert.Len(t, out, 1, "kept until the crew takes it")

	p.inboxes["box-ops"].fail = nil
	sync1(laptop, p)

	out, _ = laptop.Outbox()
	assert.Empty(t, out)
	assert.Equal(t, []string{" ← laptop/default: hello"}, p.inboxes["box-ops"].list())
}

func TestUnreachablePeerIsRecorded(t *testing.T) {
	p, laptop, _, _ := trio(t)

	p.down["box-web"] = true
	tell(t, laptop, "default", "box-web", "x")

	var failed int

	for _, s := range sync1(laptop, p) {
		if s.Err != nil {
			failed++
		}
	}

	assert.Equal(t, 1, failed)

	book, _ := laptop.Peers()
	for _, b := range book {
		if b.Name == "box-web" {
			assert.Equal(t, "unreachable", b.Err)
		}
	}

	out, _ := laptop.Outbox()
	assert.Len(t, out, 1)
}

func TestAnotherOwnerIsRefused(t *testing.T) {
	p, laptop, _, _ := trio(t)

	stranger := newOwner(t, "stranger")
	p.nodes["stranger"], p.inboxes["stranger"] = stranger, &inbox{}

	addPeer(t, laptop, "stranger")

	for _, s := range sync1(laptop, p) {
		if s.Peer == "stranger" {
			require.Error(t, s.Err)
			assert.Contains(t, s.Err.Error(), "not signed by this owner")
		}
	}

	// A forged envelope (a stranger's cert) is dropped from the giver, never delivered.
	a, _ := ParseAddress("laptop")
	e, err := stranger.NewEnvelope("x", a, "run rm -rf", time.Now())
	require.NoError(t, err)

	ids, err := laptop.receive([]Envelope{e}, p.inboxes["laptop"].deliver, time.Now())
	require.Error(t, err)
	assert.Equal(t, []string{e.ID}, ids)
	assert.Empty(t, p.inboxes["laptop"].list())
}

func TestTamperedBodyFails(t *testing.T) {
	laptop := newOwner(t, "laptop")
	a, _ := ParseAddress("x")
	e, err := laptop.NewEnvelope("c", a, "yes", time.Now())
	require.NoError(t, err)

	e.Body = "no"
	assert.Error(t, e.Verify(laptop.Owner))
}

func TestAddrCheck(t *testing.T) {
	assert.NoError(t, Addr{SSH: "ops@box"}.Check())
	assert.Error(t, Addr{SSH: "-oProxyCommand=x"}.Check())
	assert.Error(t, Addr{SSH: "a b"}.Check())
	assert.Error(t, Addr{}.Check())
	assert.Error(t, Addr{SSH: "a", Cmd: "b"}.Check())
}

func TestRefusedIsDroppedAndReported(t *testing.T) {
	p, laptop, ops, _ := trio(t)

	p.inboxes["box-ops"].fail = fmt.Errorf("%w: laptop may not reach crew ops", ErrRefused)
	tell(t, laptop, "default", "box-ops/ops", "rm it all")

	ss := sync1(laptop, p)
	for _, s := range ss {
		require.NoError(t, s.Err, "a refusal does not fail the session")

		if s.Peer == "box-ops" {
			require.Len(t, s.Problems, 1)
			assert.Contains(t, s.Problems[0], "may not reach crew ops")
		}
	}

	out, _ := laptop.Outbox()
	assert.Empty(t, out, "refused for good: not tried again")
	assert.Empty(t, p.inboxes["box-ops"].list())

	book, _ := laptop.Peers()
	for _, b := range book {
		assert.Empty(t, b.Err, "the peer still counts as reachable")
	}

	_ = ops
}

func TestOldOrHugeEnvelopesAreDropped(t *testing.T) {
	p, laptop, ops, _ := trio(t)

	a, _ := ParseAddress("box-ops")
	_, err := laptop.NewEnvelope("c", a, strings.Repeat("x", BodyMax+1), time.Now())
	require.Error(t, err)

	old, err := laptop.NewEnvelope("c", a, "from last year", time.Now().Add(-40*24*time.Hour))
	require.NoError(t, err)

	ids, err := ops.receive([]Envelope{old}, p.inboxes["box-ops"].deliver, time.Now())
	require.Error(t, err)
	assert.Equal(t, []string{old.ID}, ids)
	assert.Empty(t, p.inboxes["box-ops"].list())
}

// A held session takes a line the peer queues meanwhile within a second, without a second session.
func TestHeldSessionTakesALineWithinSeconds(t *testing.T) {
	p, laptop, ops, _ := trio(t)

	done := make(chan []Session)
	start := time.Now()

	go func() {
		done <- laptop.Sync(context.Background(), p, []Discovery{Book{laptop}}, p.inboxes["laptop"].deliver,
			"box-ops", 5*time.Second, 3*time.Second, time.Now)
	}()

	time.Sleep(700 * time.Millisecond)
	tell(t, ops, "ops", "laptop", "answer")

	var got time.Duration

	require.Eventually(t, func() bool {
		got = time.Since(start)

		return len(p.inboxes["laptop"].list()) == 1
	}, 3*time.Second, 50*time.Millisecond)

	assert.Less(t, got, 2*time.Second, "taken while the session waited, not at the next round")

	ss := <-done
	require.Len(t, ss, 1)
	require.NoError(t, ss[0].Err)
	assert.Equal(t, 1, ss[0].In)
}

// A long body goes through whole, and each side records what the other takes, from its hello.
func TestLongBodyAndTheCapInTheHello(t *testing.T) {
	p, laptop, ops, _ := trio(t)

	body := strings.Repeat("a plan step; ", 250) // 3250 bytes: over what an older zmina types, under BodyMax
	e := tell(t, laptop, "default", "box-ops/ops", body)

	for _, s := range sync1(laptop, p) {
		require.NoError(t, s.Err, s.Peer)
	}

	assert.Equal(t, []string{"ops ← laptop/default: " + body}, p.inboxes["box-ops"].list())
	assert.Equal(t, []string{e.ID}, p.inboxes["box-ops"].ids, "the receiver gets the id the sender knows")
	assert.Equal(t, InboxID(e.ID), InboxID(p.inboxes["box-ops"].ids[0]))
	assert.Regexp(t, `^in-[0-9a-f]{12}$`, InboxID(e.ID))

	book, err := laptop.Peers()
	require.NoError(t, err)
	assert.Equal(t, BodyMax, book[0].BodyMax, "the caller learns the peer's cap")
	assert.True(t, book[0].StoresLong())

	book, err = ops.Peers()
	require.NoError(t, err)
	assert.Equal(t, BodyMax, book[0].BodyMax, "and the peer learns the caller's")

	// An older zmina's hello carries no cap: it is taken to take OldBodyMax and to type a long line's head only.
	require.NoError(t, laptop.met(ops.Cert, nil, 0, time.Now()))
	book, err = laptop.Peers()
	require.NoError(t, err)
	assert.Zero(t, book[0].BodyMax)
	assert.False(t, book[0].StoresLong())
	assert.Equal(t, OldBodyMax, book[0].Takes())

	// The receiver takes up to its own cap and no more.
	a, _ := ParseAddress("box-ops")
	big, err := laptop.NewEnvelope("c", a, strings.Repeat("x", BodyMax), time.Now())
	require.NoError(t, err)
	_, err = ops.receive([]Envelope{big}, p.inboxes["box-ops"].deliver, time.Now())
	require.NoError(t, err)
}

func TestReportCapabilityRequiresSignatureAndLegacyStillConnects(t *testing.T) {
	for _, advertised := range []bool{false, true} {
		t.Run(fmt.Sprint(advertised), func(t *testing.T) {
			owner := newOwner(t, "alpha")
			clientNode := enroll(t, owner, "beta")
			client, server := net.Pipe()
			require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))
			require.NoError(t, server.SetDeadline(time.Now().Add(5*time.Second)))
			done := make(chan error, 1)
			go func() {
				defer func() { _ = server.Close() }()
				done <- owner.Serve(server, (&inbox{}).deliver, time.Now)
			}()
			c := newConn(client)
			challenge, err := nonce()
			require.NoError(t, err)
			require.NoError(t, c.send(wire{Op: "hello", V: protocolVersion, Cert: &clientNode.Cert, Nonce: challenge, BodyMax: BodyMax, ReportsV1: advertised}))
			hi, err := c.expect("hello")
			require.NoError(t, err)
			// Correct ordinary authentication must not authorize an unsigned capability.
			require.NoError(t, c.send(wire{Op: "auth", Sig: ed25519.Sign(clientNode.Key, helloMessage("client", hi.Nonce))}))
			_, err = c.expect("ok")
			if advertised {
				require.ErrorContains(t, err, "unauthenticated reports_v1")
			} else {
				require.NoError(t, err)
				require.NoError(t, c.send(wire{Op: "bye"}))
			}
			require.NoError(t, client.Close())
			serveErr := <-done
			if advertised {
				require.Error(t, serveErr)
			} else {
				require.NoError(t, serveErr)
			}
		})
	}
}

// The owner's calls never ask for a password and, for attach, get a terminal on the far side: BatchMode=yes ahead of
// a command line's own options, -t just before its host, after any -T.
func TestOwnerCommand(t *testing.T) {
	c := OwnerCommand(context.Background(), Addr{SSH: "box"}, true, "attach", "dev")
	assert.Equal(t, []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-t", "--", "box"}, c.Args[:8])
	assert.True(t, strings.HasSuffix(c.Args[8], " zmina attach dev"))
	assert.NotContains(t, OwnerCommand(context.Background(), Addr{SSH: "box"}, false, "ls").Args, "-t")

	for line, want := range map[string]string{
		"ssh -o BatchMode=yes box sudo -n -iu ops zmina": "ssh -o BatchMode=yes -o BatchMode=yes -t box sudo -n -iu ops zmina",
		"ssh -T box zmina":                     "ssh -o BatchMode=yes -T -t box zmina",
		"ssh -o BatchMode=no -p 22 box zmina":  "ssh -o BatchMode=yes -o BatchMode=no -p 22 -t box zmina",
		"ssh -qp 22 -oRequestTTY=no box zmina": "ssh -o BatchMode=yes -qp 22 -oRequestTTY=no -t box zmina",
		"ssh -i key -- box zmina":              "ssh -o BatchMode=yes -i key -t -- box zmina",
		"/opt/far-zmina":                       "/opt/far-zmina",
	} {
		assert.Equal(t, want, ownerLine(line, true), line)
	}
	assert.Equal(t, "ssh -o BatchMode=yes box zmina", ownerLine("ssh box zmina", false))
}

// What ssh makes of the owner's line (ssh -G: no connection): batch mode and a terminal, whatever the line said.
func TestOwnerLineAsSSHReadsIt(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh")
	}

	for _, line := range []string{"ssh -T box zmina", "ssh -o BatchMode=no -o RequestTTY=no box zmina", "ssh box zmina"} {
		f := strings.Fields(ownerLine(line, true))
		host := slices.Index(f, "box")
		args := append([]string{"-G", "-F", "/dev/null"}, f[1:host+1]...)
		out, err := exec.Command("ssh", args...).Output()
		require.NoError(t, err, line)
		assert.Contains(t, string(out), "\nbatchmode yes\n", line)
		assert.Regexp(t, `\nrequesttty (yes|true|force)\n`, string(out), line)
	}
}
