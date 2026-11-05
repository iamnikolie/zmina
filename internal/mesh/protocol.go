package mesh

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iamnikolie/zmina/internal/chat"
)

// protocolVersion is the version a node speaks; a node refuses a session with a newer one it does not know.
const protocolVersion = 1

// Deliver hands an envelope addressed to this node to one of its crews ("" for the node's default crew), with its id
// and when its writer sent it (zero when the envelope's time does not read).
type Deliver func(crew, from, id, body string, sent time.Time) error

// wire is one line of the protocol, JSON over the stream. A session is: hello (both ways), auth, ok, then any of
// push → ack, pull → envs → ack → ok, and bye.
type wire struct {
	// TellReceipts, in a hello, says the node sends and takes receipts for plain lines (tell_receipts); its
	// signature, over the other side's nonce, is TellReceiptsSig (in the server's hello, in the client's auth).
	TellReceipts    bool       `json:"tell_receipts,omitempty"`
	TellReceiptsSig []byte     `json:"tell_receipts_sig,omitempty"`
	ReportsV2       bool       `json:"reports_v2,omitempty"`
	PublicationsSig []byte     `json:"publications_sig,omitempty"`
	ReportsV1       bool       `json:"reports_v1,omitempty"`
	CapsSig         []byte     `json:"caps_sig,omitempty"`
	Op              string     `json:"op"`
	V               int        `json:"v,omitempty"`
	Cert            *Cert      `json:"cert,omitempty"`
	Nonce           []byte     `json:"nonce,omitempty"`
	Sig             []byte     `json:"sig,omitempty"`
	Peers           []Peer     `json:"peers,omitempty"`
	Envs            []Envelope `json:"envs,omitempty"`
	IDs             []string   `json:"ids,omitempty"`
	Err             string     `json:"err,omitempty"`
	// Wait, on a pull, is how many seconds the peer may hold it for something to arrive.
	Wait int `json:"wait,omitempty"`
	// BodyMax, in a hello, is the longest body the node takes (BodyMax); an older node sends none.
	BodyMax  int              `json:"body_max,omitempty"`
	Terminal *TerminalRequest `json:"terminal,omitempty"`
	Targets  []TerminalTarget `json:"targets,omitempty"`
	Data     []byte           `json:"data,omitempty"`
	Cols     uint16           `json:"cols,omitempty"`
	Rows     uint16           `json:"rows,omitempty"`
	Node     string           `json:"node,omitempty"`
	Via      []string         `json:"via,omitempty"`
	Text     string           `json:"text,omitempty"`
	Chat     *chat.Snapshot   `json:"chat,omitempty"`
}

type conn struct {
	peer      Peer // Capabilities authenticated on this connection, not a later address-book snapshot.
	enc       *json.Encoder
	dec       *wireDecoder
	challenge []byte
}

func newConn(rw io.ReadWriter) *conn {
	return &conn{enc: json.NewEncoder(rw), dec: &wireDecoder{reader: bufio.NewReader(rw)}}
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
	return n.ServeTerminals(context.Background(), rw, deliver, now, nil, nil)
}

// ServeTerminals adds live terminal services to the ordinary mesh session. Terminal requests require
// proof of the owner key in addition to the usual node authentication.
func (n *Node) ServeTerminals(ctx context.Context, rw io.ReadWriter, deliver Deliver, now func() time.Time,
	services *TerminalServices, transport Transport,
) error {
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
	c.challenge = mine

	told, err := n.told()
	if err != nil {
		return fail(err)
	}

	err = c.send(wire{Op: "hello", V: protocolVersion, Cert: &n.Cert, Nonce: mine,
		Sig: ed25519.Sign(n.Key, helloMessage("server", hi.Nonce)), Peers: told, BodyMax: BodyMax, ReportsV1: true, ReportsV2: true, PublicationsSig: ed25519.Sign(n.Key, publicationCapabilityMessage("server", hi.Nonce, BodyMax, true)), CapsSig: ed25519.Sign(n.Key, capabilityMessage("server", hi.Nonce, BodyMax, true)),
		TellReceipts: true, TellReceiptsSig: ed25519.Sign(n.Key, tellReceiptCapabilityMessage("server", hi.Nonce, BodyMax))})
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

	if hi.ReportsV1 && !ed25519.Verify(hi.Cert.Key, capabilityMessage("client", mine, hi.BodyMax, true), auth.CapsSig) {
		return fail(errors.New("unauthenticated reports_v1 capability"))
	}
	if hi.ReportsV2 && (!hi.ReportsV1 || !ed25519.Verify(hi.Cert.Key, publicationCapabilityMessage("client", mine, hi.BodyMax, hi.ReportsV1), auth.PublicationsSig)) {
		return fail(errors.New("unauthenticated reports_v2 capability"))
	}
	if hi.TellReceipts && !ed25519.Verify(hi.Cert.Key, tellReceiptCapabilityMessage("client", mine, hi.BodyMax), auth.TellReceiptsSig) {
		return fail(errors.New("unauthenticated tell_receipts capability"))
	}
	c.peer = Peer{Name: caller, BodyMax: hi.BodyMax, ReportsV1: hi.ReportsV1, ReportsV2: hi.ReportsV2, TellReceipts: hi.TellReceipts}
	if err := n.metPeer(*hi.Cert, hi.Peers, c.peer, now()); err != nil {
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
		case "terminal/list", "terminal/open", "terminal/history", "chat/read", "chat/send", "chat/action", "chat/upload", "chat/file",
			"crew/status":
			return n.serveTerminal(ctx, c, w, services)
		case "terminal/tunnel":
			return n.serveTunnel(ctx, rw, c, w, transport)
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

			out, blocked := n.handFor(c.peer, book)

			// A held pull waits here, looking every half second, until something is for the caller.
			for end := now().Add(time.Duration(min(w.Wait, 60)) * time.Second); len(out) == 0 && now().Before(end); {
				time.Sleep(pollEvery)

				if book, err = n.Peers(); err != nil {
					return fail(err)
				}

				out, blocked = n.handFor(c.peer, book)
			}

			if err := c.send(wire{Op: "envs", Envs: out, Err: strings.Join(blocked, "; ")}); err != nil {
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

	if err := c.send(wire{Op: "hello", V: protocolVersion, Cert: &n.Cert, Nonce: mine, Peers: told,
		BodyMax: BodyMax, ReportsV1: true, ReportsV2: true, TellReceipts: true}); err != nil {
		return "", err
	}

	hi, err := c.expect("hello")
	if err != nil {
		return "", err
	}
	c.challenge = hi.Nonce

	if hi.Cert == nil {
		return "", errors.New("the peer sent no certificate")
	}

	if err := hi.Cert.Verify(n.Owner); err != nil {
		return "", err
	}

	if !ed25519.Verify(hi.Cert.Key, helloMessage("server", mine), hi.Sig) {
		return "", errors.New("the peer did not prove its key")
	}

	if hi.ReportsV1 && !ed25519.Verify(hi.Cert.Key, capabilityMessage("server", mine, hi.BodyMax, true), hi.CapsSig) {
		return "", errors.New("unauthenticated reports_v1 capability")
	}
	if hi.ReportsV2 && (!hi.ReportsV1 || !ed25519.Verify(hi.Cert.Key, publicationCapabilityMessage("server", mine, hi.BodyMax, hi.ReportsV1), hi.PublicationsSig)) {
		return "", errors.New("unauthenticated reports_v2 capability")
	}
	if hi.TellReceipts && !ed25519.Verify(hi.Cert.Key, tellReceiptCapabilityMessage("server", mine, hi.BodyMax), hi.TellReceiptsSig) {
		return "", errors.New("unauthenticated tell_receipts capability")
	}
	if want != "" && hi.Cert.Name != want {
		return "", fmt.Errorf("this address reaches %s, not %s", hi.Cert.Name, want)
	}

	if err := c.send(wire{Op: "auth", Sig: ed25519.Sign(n.Key, helloMessage("client", hi.Nonce)), CapsSig: ed25519.Sign(n.Key, capabilityMessage("client", hi.Nonce, BodyMax, true)), PublicationsSig: ed25519.Sign(n.Key, publicationCapabilityMessage("client", hi.Nonce, BodyMax, true)),
		TellReceiptsSig: ed25519.Sign(n.Key, tellReceiptCapabilityMessage("client", hi.Nonce, BodyMax))}); err != nil {
		return "", err
	}

	if _, err := c.expect("ok"); err != nil {
		return "", err
	}

	c.peer = Peer{Name: hi.Cert.Name, BodyMax: hi.BodyMax, ReportsV1: hi.ReportsV1, ReportsV2: hi.ReportsV2, TellReceipts: hi.TellReceipts}
	return hi.Cert.Name, n.metPeer(*hi.Cert, hi.Peers, c.peer, now())
}

// round hands the peer what is for it, then takes what it has for this node, the pull waiting up to wait at the
// peer for something to come.
func (n *Node) round(c *conn, peer string, deliver Deliver, now func() time.Time, wait time.Duration) (out, in int,
	problems []string, err error,
) {
	book, err := n.Peers()
	if err != nil {
		return 0, 0, problems, err
	}

	hand, blocked := n.handFor(c.peer, book)
	problems = append(problems, blocked...)
	if len(hand) > 0 {
		if err := c.send(wire{Op: "push", Envs: hand}); err != nil {
			return 0, 0, problems, err
		}

		ack, err := c.expect("ack")
		if err != nil {
			return 0, 0, problems, err
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

	if got.Err != "" {
		problems = append(problems, peer+": "+got.Err)
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

// met records a peer that proved its certificate, the longest body it takes (0: it told none), and the nodes it told
// of: a told node is kept only with a certificate this owner signed, and only its ssh addresses. Which told nodes
// the peer reaches itself decides what this node lets it carry.
func (n *Node) met(cert Cert, told []Peer, bodyMax int, now time.Time) error {
	return n.metCapabilities(cert, told, bodyMax, false, now)
}
func capabilityMessage(side string, nonce []byte, max int, reports bool) []byte {
	return []byte(fmt.Sprintf("zmina-caps\x00%s\x00%x\x00%d\x00%t", side, nonce, max, reports))
}
func publicationCapabilityMessage(side string, nonce []byte, max int, legacy bool) []byte {
	return append([]byte("zmina-reports-v2\x00"), capabilityMessage(side, nonce, max, legacy)...)
}

// tellReceiptCapabilityMessage is what a node signs to say it sends and takes receipts for plain lines.
func tellReceiptCapabilityMessage(side string, nonce []byte, max int) []byte {
	return append([]byte("zmina-tell-receipts-v1\x00"), capabilityMessage(side, nonce, max, true)...)
}

func (n *Node) metCapabilities(cert Cert, told []Peer, bodyMax int, reports bool, now time.Time, publications ...bool) error {
	return n.metPeer(cert, told, Peer{BodyMax: bodyMax, ReportsV1: reports,
		ReportsV2: reports && len(publications) > 0 && publications[0]}, now)
}

// metPeer records a peer met in a session: what it authenticated on it (caps) replaces what the book held.
func (n *Node) metPeer(cert Cert, told []Peer, caps Peer, now time.Time) error {
	return n.UpdatePeers(func(ps []Peer) []Peer {
		ps = upsert(ps, cert, "called", func(p *Peer) {
			p.OK, p.Err, p.BodyMax = now, "", caps.BodyMax
			p.ReportsV1 = caps.ReportsV1
			p.ReportsV2 = caps.ReportsV1 && caps.ReportsV2
			p.TellReceipts = caps.TellReceipts
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
func (n *Node) handFor(peer Peer, book []Peer) ([]Envelope, []string) {
	all, err := n.Outbox()
	if err != nil {
		return nil, []string{err.Error()}
	}
	byName := map[string]Peer{}
	for _, p := range book {
		byName[p.Name] = p
	}
	var out []Envelope
	var problems []string
	for _, e := range all {
		d := byName[e.dest()]
		if e.dest() != peer.Name && (d.direct() || !slices.Contains(d.Through, peer.Name) || e.held(peer.Name) || len(e.Via) >= maxHops) {
			continue
		}
		if IsTellReceipt(e.Body) && !peer.TellReceipts {
			problem := fmt.Sprintf("tell receipt %s pending: %s has no authenticated tell_receipts on this connection; update the peer and sync (plain notes remain compatible)", e.ID, peer.Name)
			if err := n.setPendingError(e.ID, problem); err != nil {
				problems = append(problems, err.Error())
			}
			problems = append(problems, problem)
			continue
		}
		if version := ReportWireVersion(e.Body); version > 0 {
			problem := ""
			switch {
			case version > 2:
				problem = fmt.Sprintf("report %s pending: unsupported reports_v%d format; update this node and the peer", e.ID, version)
			case version == 2 && !peer.ReportsV2:
				problem = fmt.Sprintf("report %s pending: %s has no authenticated reports_v2 on this connection; update the peer for publication reports (plain notes remain compatible)", e.ID, peer.Name)
			case !peer.ReportsV1:
				problem = fmt.Sprintf("report %s pending: %s has no authenticated reports_v1 on this connection; update the peer and sync (plain notes remain compatible)", e.ID, peer.Name)
			case len(e.Body) > peer.Takes():
				problem = fmt.Sprintf("report %s pending: %d bytes exceeds %s negotiated cap %d; shorten the report or update the peer", e.ID, len(e.Body), peer.Name, peer.Takes())
			}
			if err := n.setPendingError(e.ID, problem); err != nil {
				problems = append(problems, err.Error())
			}
			if problem != "" {
				problems = append(problems, problem)
				continue
			}
		}
		out = append(out, e)
	}
	return out, problems
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
				err := deliver(to.Crew, e.From, e.ID, e.Body, e.SentAt())
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

// DeliverLocal applies the same signature, replay and acknowledgement rules to self-addressed envelopes.
func (n *Node) DeliverLocal(e Envelope, deliver Deliver, now time.Time) ([]string, error) {
	to, err := ParseAddress(e.To)
	if err != nil || to.Node != n.Name {
		return nil, fmt.Errorf("not a local envelope")
	}
	return n.receive([]Envelope{e}, deliver, now)
}
