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
	// Expires is when the certificate stops counting (RFC 3339), inside the signed bytes. A certificate issued before
	// certificates expired has none and keeps counting; zmina node renew gives it one.
	Expires string `json:"expires,omitempty"`
	Sig     []byte `json:"sig"`
}

// message is what the owner signs. A certificate with an expiry signs it under another prefix, so the one without
// stays what an older zmina reads and an expiry can neither be added to nor taken off a signed certificate.
func (c Cert) message() []byte {
	if c.Expires == "" {
		return []byte("zmina-node\x00" + c.Name + "\x00" + string(c.Key) + "\x00" + c.At)
	}

	return []byte("zmina-node-v2\x00" + c.Name + "\x00" + string(c.Key) + "\x00" + c.At + "\x00" + c.Expires)
}

// Verify checks that owner signed the certificate. It says nothing about the time: see Expired.
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

	if _, _, err := c.Expiry(); err != nil {
		return fmt.Errorf("certificate of %s: %w", c.Name, err)
	}

	return nil
}

// Expiry is when the certificate stops counting; has is false for one issued with no expiry.
func (c Cert) Expiry() (at time.Time, has bool, err error) {
	if c.Expires == "" {
		return time.Time{}, false, nil
	}

	at, err = time.Parse(time.RFC3339, c.Expires)
	if err != nil {
		return time.Time{}, true, fmt.Errorf("bad expiry %q", c.Expires)
	}

	return at, true, nil
}

// ExpiredError is a certificate refused for its age. Its message says what to run.
type ExpiredError struct {
	Name string
	At   time.Time
}

func (e *ExpiredError) Error() string {
	return fmt.Sprintf("the certificate of %s expired on %s: on the owner's node run zmina node renew %s",
		e.Name, e.At.UTC().Format(time.DateOnly), e.Name)
}

// Expired is nil while the certificate counts at now: it has no expiry, or its expiry is not past.
func (c Cert) Expired(now time.Time) error {
	at, has, err := c.Expiry()
	if err != nil {
		return err
	}

	if has && !now.Before(at) {
		return &ExpiredError{Name: c.Name, At: at}
	}

	return nil
}

// CertWarnBefore is how long before its expiry a certificate is said to be running out.
const CertWarnBefore = 30 * 24 * time.Hour

// DefaultCertLife is how long a certificate counts when the owner gives no other.
const DefaultCertLife = 365 * 24 * time.Hour

// CertState is where a certificate stands in time.
type CertState int

// The states of a certificate.
const (
	CertNoExpiry CertState = iota // issued before certificates expired: counts for now, warns
	CertValid                     // expires more than CertWarnBefore from now
	CertExpiring                  // expires within CertWarnBefore
	CertExpired                   // past its expiry: refused
)

// State is where the certificate stands at now, and when it expires (zero for one with no expiry).
func (c Cert) State(now time.Time) (CertState, time.Time) {
	at, has, err := c.Expiry()

	switch {
	case err != nil:
		return CertExpired, time.Time{}
	case !has:
		return CertNoExpiry, time.Time{}
	case !now.Before(at):
		return CertExpired, at
	case at.Sub(now) <= CertWarnBefore:
		return CertExpiring, at
	}

	return CertValid, at
}

// Newer reports whether c was issued after o.
func (c Cert) Newer(o Cert) bool { return c.At > o.At }

// Sign issues a certificate for name and key with the owner's private key, counting for DefaultCertLife from now.
func Sign(owner ed25519.PrivateKey, name string, key ed25519.PublicKey, now time.Time) (Cert, error) {
	return SignFor(owner, name, key, now, DefaultCertLife)
}

// SignFor is Sign for another lifetime; life 0 issues a certificate with no expiry, the form every zmina reads, for a
// mesh some of whose nodes do not read expiries yet.
func SignFor(owner ed25519.PrivateKey, name string, key ed25519.PublicKey, now time.Time, life time.Duration) (Cert, error) {
	if !nameRe.MatchString(name) {
		return Cert{}, fmt.Errorf("node name %q: use letters, digits, '.', '_' and '-'", name)
	}

	if life < 0 {
		return Cert{}, errors.New("a certificate's life cannot be negative")
	}

	c := Cert{Name: name, Key: key, At: now.UTC().Format(time.RFC3339Nano)}
	if life > 0 {
		c.Expires = now.Add(life).UTC().Format(time.RFC3339)
	}

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
