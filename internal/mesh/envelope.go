package mesh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// maxHops is how many nodes may carry one envelope besides the one that wrote it.
const maxHops = 4

// Envelope is one message from a crew on one node to a crew on another.
type Envelope struct {
	ID   string `json:"id"`
	From string `json:"from"` // node/crew
	To   string `json:"to"`   // node or node/crew
	At   string `json:"at"`
	Body string `json:"body"`
	// Cert is the writer's certificate, so a node that never met the writer can check it.
	Cert Cert   `json:"cert"`
	Sig  []byte `json:"sig"`
	// Via are the nodes that carried it so far; not signed, a carrier adds itself.
	Via []string `json:"via,omitempty"`
}

func (e Envelope) message() []byte {
	return []byte(strings.Join([]string{"zmina-env", e.ID, e.From, e.To, e.At, e.Body}, "\x00"))
}

// Address is a node and, optionally, one of its crews: "box-ops" or "box-ops/ops".
type Address struct{ Node, Crew string }

// ParseAddress reads node[/crew].
func ParseAddress(s string) (Address, error) {
	node, crew, _ := strings.Cut(s, "/")
	if !nameRe.MatchString(node) || (crew != "" && !nameRe.MatchString(crew)) {
		return Address{}, fmt.Errorf("address %q: want <node> or <node>/<crew>", s)
	}

	return Address{Node: node, Crew: crew}, nil
}

func (a Address) String() string {
	if a.Crew == "" {
		return a.Node
	}

	return a.Node + "/" + a.Crew
}

// NewEnvelope writes and signs a message from crew on node n to the address to.
func (n *Node) NewEnvelope(crew string, to Address, body string, now time.Time) (Envelope, error) {
	if strings.TrimSpace(body) == "" {
		return Envelope{}, errors.New("nothing to say")
	}

	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Envelope{}, err
	}

	e := Envelope{
		ID:   n.Name + "-" + now.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:]),
		From: Address{Node: n.Name, Crew: crew}.String(),
		To:   to.String(),
		At:   now.UTC().Format(time.RFC3339Nano),
		Body: body,
		Cert: n.Cert,
	}
	e.Sig = ed25519.Sign(n.Key, e.message())

	return e, nil
}

// Verify checks the envelope's writer: its certificate under owner, that the certificate names the node it comes
// from, and the signature.
func (e Envelope) Verify(owner ed25519.PublicKey) error {
	from, err := ParseAddress(e.From)
	if err != nil {
		return err
	}

	if _, err := ParseAddress(e.To); err != nil {
		return err
	}

	if err := e.Cert.Verify(owner); err != nil {
		return err
	}

	if e.Cert.Name != from.Node {
		return fmt.Errorf("envelope %s: from %s, signed by %s", e.ID, from.Node, e.Cert.Name)
	}

	if !ed25519.Verify(e.Cert.Key, e.message(), e.Sig) {
		return fmt.Errorf("envelope %s: bad signature", e.ID)
	}

	return nil
}

// dest is the node the envelope goes to.
func (e Envelope) dest() string { a, _ := ParseAddress(e.To); return a.Node }

// held reports whether node has held the envelope already: its writer or one of its carriers.
func (e Envelope) held(node string) bool {
	a, _ := ParseAddress(e.From)

	return a.Node == node || slices.Contains(e.Via, node)
}
