package mesh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"
)

// protocolVersion is the version a node speaks; a node refuses a session with a newer one it does not know.
const protocolVersion = 1

// Deliver hands an envelope addressed to this node to one of its crews ("" for the node's default crew).
type Deliver func(crew, from, body string) error

// wire is one line of the protocol, JSON over the stream. A session is: hello (both ways), auth, ok, then any of
// push → ack, pull → envs → ack → ok, and bye.
type wire struct {
	Op    string     `json:"op"`
	V     int        `json:"v,omitempty"`
	Cert  *Cert      `json:"cert,omitempty"`
	Nonce []byte     `json:"nonce,omitempty"`
	Sig   []byte     `json:"sig,omitempty"`
	Peers []Peer     `json:"peers,omitempty"`
	Envs  []Envelope `json:"envs,omitempty"`
	IDs   []string   `json:"ids,omitempty"`
	Err   string     `json:"err,omitempty"`
	// Wait, on a pull, is how many seconds the peer may hold it for something to arrive.
	Wait int `json:"wait,omitempty"`
}

type conn struct {
	enc *json.Encoder
	dec *json.Decoder
}

func newConn(rw io.ReadWriter) *conn {
	return &conn{enc: json.NewEncoder(rw), dec: json.NewDecoder(rw)}
}

func (c *conn) send(w wire) error { return c.enc.Encode(w) }

// expect reads the next line and refuses anything but op (an err line ends the session with its message).
func (c *conn) expect(op string) (wire, error) {
	var w wire
	if err := c.dec.Decode(&w); err != nil {
		return w, fmt.Errorf("waiting for %s: %w", op, err)
	}

	if w.Op == "err" {
		return w, fmt.Errorf("peer: %s", w.Err)
	}

	if w.Op != op {
		return w, fmt.Errorf("peer sent %q, want %q", w.Op, op)
	}

	return w, nil
}

func helloMessage(role string, nonce []byte) []byte {
	return []byte("zmina-hello\x00" + role + "\x00" + string(nonce))
}

func nonce() ([]byte, error) {
	b := make([]byte, 24)
	_, err := rand.Read(b)

	return b, err
}

// Session is one sync with one peer: what went each way.
type Session struct {
	Peer string
	Addr string
	Out  int
	In   int
	// Problems are envelopes not taken, on this side or the peer's: refused, or a crew that cannot take one now.
	Problems []string
	Err      error
}

// Serve answers one session on r/w: the peer side of `zmina node serve --stdio`.
func (n *Node) Serve(rw io.ReadWriter, deliver Deliver, now func() time.Time) error {
	c := newConn(rw)

	fail := func(err error) error {
		_ = c.send(wire{Op: "err", Err: err.Error()})

		return err
	}

	hi, err := c.expect("hello")
	if err != nil {
		return err
	}

	if hi.V > protocolVersion {
		return fail(fmt.Errorf("protocol %d is newer than this zmina's %d: update zmina here", hi.V, protocolVersion))
	}

	if hi.Cert == nil {
		return fail(errors.New("hello without a certificate"))
	}

	if err := hi.Cert.Verify(n.Owner); err != nil {
		return fail(err)
	}

	mine, err := nonce()
	if err != nil {
		return fail(err)
	}

	told, err := n.told()
	if err != nil {
		return fail(err)
	}

	err = c.send(wire{Op: "hello", V: protocolVersion, Cert: &n.Cert, Nonce: mine,
		Sig: ed25519.Sign(n.Key, helloMessage("server", hi.Nonce)), Peers: told})
	if err != nil {
		return err
	}

	auth, err := c.expect("auth")
	if err != nil {
		return err
	}

	if !ed25519.Verify(hi.Cert.Key, helloMessage("client", mine), auth.Sig) {
		return fail(errors.New("the caller did not prove its key"))
	}

	caller := hi.Cert.Name
	if caller == n.Name {
		return fail(errors.New("a node does not call itself"))
	}

	if err := n.met(*hi.Cert, hi.Peers, now()); err != nil {
		return fail(err)
	}

	if err := c.send(wire{Op: "ok"}); err != nil {
		return err
	}

	for {
		var w wire
		if err := c.dec.Decode(&w); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return err
		}

		switch w.Op {
		case "bye":
			return nil
		case "push":
			ids, rerr := n.receive(w.Envs, deliver, now())

			ack := wire{Op: "ack", IDs: ids}
			if rerr != nil {
				ack.Err = rerr.Error()
			}

			if err := c.send(ack); err != nil {
				return err
			}
		case "pull":
			book, err := n.Peers()
			if err != nil {
				return fail(err)
			}

			out := n.handFor(caller, book)

			// A held pull waits here, looking every half second, until something is for the caller.
			for end := now().Add(time.Duration(min(w.Wait, 60)) * time.Second); len(out) == 0 && now().Before(end); {
				time.Sleep(pollEvery)

				if book, err = n.Peers(); err != nil {
					return fail(err)
				}

				out = n.handFor(caller, book)
			}

			if err := c.send(wire{Op: "envs", Envs: out}); err != nil {
				return err
			}

			ack, err := c.expect("ack")
			if err != nil {
				return err
			}

			n.Unqueue(sent(out, ack.IDs))

			if err := c.send(wire{Op: "ok"}); err != nil {
				return err
			}
		default:
			return fail(fmt.Errorf("unknown op %q", w.Op))
		}
	}
}

// pollEvery is how often a held pull looks at the outbox.
const pollEvery = 500 * time.Millisecond

// sent is the ids acked that were sent: a peer cannot make this node drop an envelope it was not handed.
func sent(out []Envelope, ids []string) []string {
	var ok []string

	for _, e := range out {
		if slices.Contains(ids, e.ID) {
			ok = append(ok, e.ID)
		}
	}

	return ok
}

// Call runs one session with the peer over rw: hello, then hand it what is for it, then take what it has for this
// node. It returns how many envelopes went each way, and why some were not taken (on either side): those do
// not fail the session.
func (n *Node) Call(rw io.ReadWriter, want string, deliver Deliver, now func() time.Time) (out, in int, problems []string, err error) {
	return n.Hold(rw, want, deliver, now, time.Time{})
}

// pullWait is how long one pull of a held session waits for something to arrive at the peer.
const pullWait = 10 * time.Second

// Hold is Call kept open until the time given: it hands over and takes in rounds, each pull waiting up to pullWait
// at the peer for something for this node, so a line on either side moves within seconds while nothing moves when
// there is nothing. A zero until is one round.
func (n *Node) Hold(rw io.ReadWriter, want string, deliver Deliver, now func() time.Time, until time.Time) (out, in int,
	problems []string, err error,
) {
	c := newConn(rw)

	peer, err := n.open(c, want, now)
	if err != nil {
		return 0, 0, nil, err
	}

	for {
		wait := time.Duration(0)
		if left := until.Sub(now()); left > time.Second {
			wait = min(pullWait, left)
		}

		o, i, p, err := n.round(c, peer, deliver, now, wait)
		out, in, problems = out+o, in+i, append(problems, p...)

		if err != nil {
			return out, in, problems, err
		}

		if wait == 0 {
			break
		}
	}

	_ = c.send(wire{Op: "bye"})

	return out, in, problems, nil
}

// open is the hello: both sides prove their keys and tell each other the nodes they know. It returns the peer's
// name.
func (n *Node) open(c *conn, want string, now func() time.Time) (string, error) {
	mine, err := nonce()
	if err != nil {
		return "", err
	}

	told, err := n.told()
	if err != nil {
		return "", err
	}

	if err := c.send(wire{Op: "hello", V: protocolVersion, Cert: &n.Cert, Nonce: mine, Peers: told}); err != nil {
		return "", err
	}

	hi, err := c.expect("hello")
	if err != nil {
		return "", err
	}

	if hi.Cert == nil {
		return "", errors.New("the peer sent no certificate")
	}

	if err := hi.Cert.Verify(n.Owner); err != nil {
		return "", err
	}

	if !ed25519.Verify(hi.Cert.Key, helloMessage("server", mine), hi.Sig) {
		return "", errors.New("the peer did not prove its key")
	}

	if want != "" && hi.Cert.Name != want {
		return "", fmt.Errorf("this address reaches %s, not %s", hi.Cert.Name, want)
	}

	if err := c.send(wire{Op: "auth", Sig: ed25519.Sign(n.Key, helloMessage("client", hi.Nonce))}); err != nil {
		return "", err
	}

	if _, err := c.expect("ok"); err != nil {
		return "", err
	}

	return hi.Cert.Name, n.met(*hi.Cert, hi.Peers, now())
}

// round hands the peer what is for it, then takes what it has for this node, the pull waiting up to wait at the
// peer for something to come.
func (n *Node) round(c *conn, peer string, deliver Deliver, now func() time.Time, wait time.Duration) (out, in int,
	problems []string, err error,
) {
	book, err := n.Peers()
	if err != nil {
		return 0, 0, nil, err
	}

	if hand := n.handFor(peer, book); len(hand) > 0 {
		if err := c.send(wire{Op: "push", Envs: hand}); err != nil {
			return 0, 0, nil, err
		}

		ack, err := c.expect("ack")
		if err != nil {
			return 0, 0, nil, err
		}

		acked := sent(hand, ack.IDs)
		n.Unqueue(acked)
		out = len(acked)

		if ack.Err != "" {
			problems = append(problems, peer+": "+ack.Err)
		}
	}

	start := now()

	if err := c.send(wire{Op: "pull", Wait: int(wait / time.Second)}); err != nil {
		return out, 0, problems, err
	}

	got, err := c.expect("envs")
	if err != nil {
		return out, 0, problems, err
	}

	ids, rerr := n.receive(got.Envs, deliver, now())
	if rerr != nil {
		problems = append(problems, rerr.Error())
	}

	if err := c.send(wire{Op: "ack", IDs: ids}); err != nil {
		return out, len(ids), problems, err
	}

	if _, err := c.expect("ok"); err != nil {
		return out, len(ids), problems, err
	}

	// A peer that does not wait (an older zmina) answers at once: wait here instead of asking again at once.
	if len(got.Envs) == 0 && wait > 0 {
		if left := wait - now().Sub(start); left > 0 {
			time.Sleep(left)
		}
	}

	return out, len(ids), problems, nil
}

// told is what this node tells a peer about: itself and every node it has a certificate for, with only the
// addresses that may be told (ssh, never a command line).
func (n *Node) told() ([]Peer, error) {
	book, err := n.Peers()
	if err != nil {
		return nil, err
	}

	out := []Peer{{Name: n.Name, Cert: &n.Cert}}

	for _, p := range book {
		if p.Cert == nil {
			continue
		}

		t := Peer{Name: p.Name, Cert: p.Cert, Direct: p.direct()}

		for _, a := range p.Addrs {
			if a.SSH != "" {
				t.Addrs = append(t.Addrs, Addr{SSH: a.SSH})
			}
		}

		out = append(out, t)
	}

	return out, nil
}

// met records a peer that proved its certificate, and the nodes it told of: a told node is kept only with a
// certificate this owner signed, and only its ssh addresses. Which told nodes the peer reaches itself decides
// what this node lets it carry.
func (n *Node) met(cert Cert, told []Peer, now time.Time) error {
	return n.UpdatePeers(func(ps []Peer) []Peer {
		ps = upsert(ps, cert, "called", func(p *Peer) {
			p.OK, p.Err = now, ""
		})

		for i, t := range told {
			if i >= toldMax {
				break
			}

			if t.Cert == nil || t.Cert.Name != t.Name || t.Name == n.Name || t.Cert.Verify(n.Owner) != nil {
				continue
			}

			ps = upsert(ps, *t.Cert, "told", func(p *Peer) {
				p.Through = slices.DeleteFunc(p.Through, func(s string) bool { return s == cert.Name })
				if t.Direct {
					p.Through = append(p.Through, cert.Name)
				}

				if p.Source != "told" {
					return // the owner's own entry keeps the owner's addresses
				}

				for _, a := range t.Addrs {
					if a.SSH != "" && a.Check() == nil && !slices.Contains(p.Addrs, Addr{SSH: a.SSH}) {
						p.Addrs = append(p.Addrs, Addr{SSH: a.SSH})
					}
				}
			})
		}

		return ps
	})
}

// toldMax is how many told nodes one peer may add per session.
const toldMax = 256

// upsert finds the entry for cert's node (adding it with source when new), takes the newer certificate, and
// runs fn on the entry.
func upsert(ps []Peer, cert Cert, source string, fn func(p *Peer)) []Peer {
	i := slices.IndexFunc(ps, func(p Peer) bool { return p.Name == cert.Name })
	if i < 0 {
		ps = append(ps, Peer{Name: cert.Name, Source: source})
		i = len(ps) - 1
	}

	p := &ps[i]
	if p.Cert == nil || cert.Newer(*p.Cert) {
		c := cert
		p.Cert = &c
	}

	fn(p)

	return ps
}

// handFor is what this node hands peer: the envelopes for it, and those for a node this node cannot reach itself
// (no address, or its last try failed) when peer said it reaches that node and has not held the envelope yet.
func (n *Node) handFor(peer string, book []Peer) []Envelope {
	all, err := n.Outbox()
	if err != nil {
		return nil
	}

	byName := map[string]Peer{}
	for _, p := range book {
		byName[p.Name] = p
	}

	var out []Envelope

	for _, e := range all {
		d := byName[e.dest()]
		if e.dest() == peer || (!d.direct() && slices.Contains(d.Through, peer) && !e.held(peer) &&
			len(e.Via) < maxHops) {
			out = append(out, e)
		}
	}

	return out
}

// receive takes envelopes handed to this node: delivered to a crew when they are for it, kept to carry on when
// they are for another node. It returns the ids this node now holds (taken, carried, or seen before — the giver
// drops those) and why the others were not taken.
func (n *Node) receive(envs []Envelope, deliver Deliver, now time.Time) ([]string, error) {
	var (
		ids  []string
		errs []error
	)

	err := n.locked(func() error {
		seen, err := n.seen()
		if err != nil {
			return err
		}

		book, err := n.Peers()
		if err != nil {
			return err
		}

		for _, e := range envs {
			if !ValidID(e.ID) {
				continue
			}

			if _, ok := seen[e.ID]; ok {
				ids = append(ids, e.ID)

				continue
			}

			if err := n.admit(e, book, now); err != nil {
				// It will not pass on a second try either: take it off the giver and say why.
				ids = append(ids, e.ID)
				errs = append(errs, err)

				continue
			}

			to, _ := ParseAddress(e.To)

			switch {
			case to.Node == n.Name:
				err := deliver(to.Crew, e.From, e.Body)
				if errors.Is(err, ErrRefused) {
					// Refused for good: the giver drops it, nobody retries.
					seen[e.ID] = now
					ids = append(ids, e.ID)
					errs = append(errs, fmt.Errorf("%s from %s: %w", e.ID, e.From, err))

					continue
				}

				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", e.ID, err))

					continue
				}
			case len(e.Via) >= maxHops:
				errs = append(errs, fmt.Errorf("%s to %s: dropped after %d hops", e.ID, e.To, maxHops))
			default:
				e.Via = append(e.Via, n.Name)
				if err := n.Queue(e); err != nil {
					errs = append(errs, err)

					continue
				}
			}

			seen[e.ID] = now
			ids = append(ids, e.ID)
		}

		return n.markSeen(seen, now)
	})

	return ids, errors.Join(append(errs, err)...)
}

// ErrRefused is a delivery this node will never take (no such crew here): the giver drops the envelope instead of
// trying again.
var ErrRefused = errors.New("refused")

// admit checks an envelope before it is taken: the writer's signature, its size, its age, and that its
// certificate is the newest this node knows for that node (a replaced key no longer writes).
func (n *Node) admit(e Envelope, book []Peer, now time.Time) error {
	if err := e.Verify(n.Owner); err != nil {
		return err
	}

	if len(e.Body) > BodyMax {
		return fmt.Errorf("%s: %d bytes, over %d", e.ID, len(e.Body), BodyMax)
	}

	at, err := time.Parse(time.RFC3339Nano, e.At)
	if err != nil || now.Sub(at) > seenKeep || at.Sub(now) > time.Hour {
		return fmt.Errorf("%s: written %s, too old or from the future", e.ID, e.At)
	}

	for _, p := range book {
		if p.Name == e.Cert.Name && p.Cert != nil && p.Cert.Newer(e.Cert) && !p.Cert.Key.Equal(e.Cert.Key) {
			return fmt.Errorf("%s: signed with a replaced key of %s", e.ID, p.Name)
		}
	}

	return nil
}

// Book is the node's address book as a Discovery.
type Book struct{ Node *Node }

// Peers lists the book.
func (b Book) Peers(context.Context) ([]Peer, error) { return b.Node.Peers() }

// Sync runs one session with every peer the discoveries name that has an address, side by side, each within
// timeout (plus hold), and records how each went in the book. only, when set, limits it to that peer. hold keeps
// each session open that long (Hold), so what comes on either side meanwhile moves within seconds.
func (n *Node) Sync(ctx context.Context, t Transport, ds []Discovery, deliver Deliver, only string,
	timeout, hold time.Duration, now func() time.Time,
) []Session {
	var peers []Peer

	for _, d := range ds {
		ps, err := d.Peers(ctx)
		if err != nil {
			return []Session{{Err: err}}
		}

		for _, p := range ps {
			if p.Name == n.Name || len(p.Addrs) == 0 || (only != "" && p.Name != only) {
				continue
			}

			if i := slices.IndexFunc(peers, func(q Peer) bool { return q.Name == p.Name }); i >= 0 {
				peers[i].Addrs = append(peers[i].Addrs, p.Addrs...)
			} else {
				peers = append(peers, p)
			}
		}
	}

	out := make([]Session, len(peers))

	var wg sync.WaitGroup

	for i, p := range peers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			out[i] = n.session(ctx, t, p, deliver, timeout, hold, now)
		}()
	}

	wg.Wait()

	return out
}

// session tries the peer's addresses in turn until one holds a whole session.
func (n *Node) session(ctx context.Context, t Transport, p Peer, deliver Deliver, timeout, hold time.Duration,
	now func() time.Time,
) Session {
	s := Session{Peer: p.Name}

	for _, a := range p.Addrs {
		s.Addr = a.String()

		s.Out, s.In, s.Problems, s.Err = n.callAt(ctx, t, a, p.Name, deliver, timeout, hold, now)
		if s.Err == nil {
			break
		}
	}

	at := now()
	_ = n.UpdatePeers(func(ps []Peer) []Peer {
		for i := range ps {
			if ps[i].Name == p.Name {
				ps[i].Tried = at
				ps[i].Err = ""

				if s.Err != nil {
					ps[i].Err = s.Err.Error()
				}
			}
		}

		return ps
	})

	return s
}

func (n *Node) callAt(ctx context.Context, t Transport, a Addr, want string, deliver Deliver,
	timeout, hold time.Duration, now func() time.Time,
) (int, int, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout+hold)
	defer cancel()

	rw, err := t.Dial(ctx, a)
	if err != nil {
		return 0, 0, nil, err
	}

	until := time.Time{}
	if hold > 0 {
		until = now().Add(hold)
	}

	out, in, problems, err := n.Hold(rw, want, deliver, now, until)
	// The command's own last word (ssh's, the peer's zmina's) says more than a broken stream.
	if cerr := rw.Close(); err != nil && cerr != nil {
		err = cerr
	}

	return out, in, problems, err
}
