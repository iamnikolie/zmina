// Package mesh lets crews on different machines (or under different users) reach each other without a hub.
//
// A node is one ~/.zmina: its own key, and a certificate the owner's key signed. A node is known by that key,
// never by where it lives, so the network under it can change. Messages are envelopes signed by the node that
// wrote them; any node may carry one towards its destination, and the destination checks the signature, not the
// carrier. Nodes meet over a Transport (today: a command that reaches the peer's `zmina node serve --stdio`, such
// as ssh) and tell each other the nodes they know, so knowing one peer is enough to learn the rest.
package mesh

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Cert binds a node's name to its key, signed by the owner's key.
type Cert struct {
	Name string            `json:"name"`
	Key  ed25519.PublicKey `json:"key"`
	At   string            `json:"at"`
	Sig  []byte            `json:"sig"`
}

func (c Cert) message() []byte {
	return []byte("zmina-node\x00" + c.Name + "\x00" + string(c.Key) + "\x00" + c.At)
}

// Verify checks that owner signed the certificate.
func (c Cert) Verify(owner ed25519.PublicKey) error {
	if !nameRe.MatchString(c.Name) {
		return fmt.Errorf("node name %q: use letters, digits, '.', '_' and '-'", c.Name)
	}

	if len(c.Key) != ed25519.PublicKeySize || len(owner) != ed25519.PublicKeySize {
		return errors.New("certificate: bad key")
	}

	if !ed25519.Verify(owner, c.message(), c.Sig) {
		return fmt.Errorf("certificate of %s is not signed by this owner", c.Name)
	}

	return nil
}

// Newer reports whether c was issued after o.
func (c Cert) Newer(o Cert) bool { return c.At > o.At }

// Sign issues a certificate for name and key with the owner's private key.
func Sign(owner ed25519.PrivateKey, name string, key ed25519.PublicKey, now time.Time) (Cert, error) {
	if !nameRe.MatchString(name) {
		return Cert{}, fmt.Errorf("node name %q: use letters, digits, '.', '_' and '-'", name)
	}

	c := Cert{Name: name, Key: key, At: now.UTC().Format(time.RFC3339Nano)}
	c.Sig = ed25519.Sign(owner, c.message())

	return c, nil
}

// NewKey makes a fresh key pair.
func NewKey() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)

	return priv, err
}

// nameRe is a node's or a crew's name: it goes into addresses and file names.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidName reports whether s can name a node.
func ValidName(s string) bool { return nameRe.MatchString(s) }
