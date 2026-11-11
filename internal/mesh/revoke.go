package mesh

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Revoked is the owner's word that one key no longer speaks for a node: a certificate with that key opens no session
// and writes no envelope, whatever its date. Name is for people; the key is what counts.
type Revoked struct {
	Name string            `json:"name"`
	Key  ed25519.PublicKey `json:"key"`
	At   string            `json:"at"`
}

// Revocations is the owner's signed list of revoked keys. Version only goes up: each node keeps the highest list it
// has seen (revoked.json), takes no other with a lower or equal version, and passes it on in every hello, so the list
// spreads with the syncs that already run. A list grows by the owner's hand (zmina node revoke).
type Revocations struct {
	Version int64     `json:"version"`
	At      string    `json:"at"`
	Revoked []Revoked `json:"revoked"`
	Sig     []byte    `json:"sig"`
}

func (r Revocations) message() []byte {
	var b strings.Builder

	fmt.Fprintf(&b, "zmina-revoked\x00%d\x00%s", r.Version, r.At)

	for _, e := range r.Revoked {
		fmt.Fprintf(&b, "\x00%q\x00%x\x00%s", e.Name, []byte(e.Key), e.At)
	}

	return []byte(b.String())
}

// Verify checks that owner signed the list.
func (r Revocations) Verify(owner ed25519.PublicKey) error {
	if r.Version < 1 {
		return errors.New("revocation list: no version")
	}

	if len(owner) != ed25519.PublicKeySize {
		return errors.New("revocation list: bad owner key")
	}

	for _, e := range r.Revoked {
		if len(e.Key) != ed25519.PublicKeySize {
			return errors.New("revocation list: bad key")
		}
	}

	if !ed25519.Verify(owner, r.message(), r.Sig) {
		return errors.New("revocation list is not signed by this owner")
	}

	return nil
}

// Has reports whether key is revoked.
func (r Revocations) Has(key ed25519.PublicKey) bool {
	return slices.ContainsFunc(r.Revoked, func(e Revoked) bool { return e.Key.Equal(key) })
}

// Revocations reads the list this node holds: the highest it has seen. None yet is the empty list, version 0.
func (n *Node) Revocations() (Revocations, error) {
	var r Revocations
	if err := readJSON(n.path("revoked.json"), &r); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Revocations{}, err
	}

	return r, nil
}

// AcceptRevocations keeps r when the owner signed it and its version is above the one this node holds. A list that
// is not the owner's is an error; one that is not higher is left alone (took false): a node never goes back to a
// list with fewer revoked keys.
func (n *Node) AcceptRevocations(r Revocations) (took bool, err error) {
	if err := r.Verify(n.Owner); err != nil {
		return false, err
	}

	err = n.locked(func() error {
		have, err := n.Revocations()
		if err != nil {
			return err
		}

		if r.Version <= have.Version {
			return nil
		}

		took = true

		return writeJSON(n.path("revoked.json"), r)
	})

	return took && err == nil, err
}

// Revoke, on the owner's node, adds the key of the certificate this node holds for name to the list and signs it as
// the next version. It returns the new list, and false when the key was revoked already (nothing changes then).
func (n *Node) Revoke(name string, now time.Time) (Revocations, bool, error) {
	owner, err := readKey(filepath.Join(n.Dir, "owner.key"))
	if errors.Is(err, os.ErrNotExist) {
		return Revocations{}, false, errors.New("this node does not hold the owner key: revoke from the owner's node")
	}

	if err != nil {
		return Revocations{}, false, err
	}

	if !owner.Public().(ed25519.PublicKey).Equal(n.Owner) {
		return Revocations{}, false, errors.New("this node's owner.key is not its owner's")
	}

	if name == n.Name {
		return Revocations{}, false, errors.New("that is this node: it cannot revoke itself")
	}

	var out Revocations

	added := false

	err = n.locked(func() error {
		book, err := n.Peers()
		if err != nil {
			return err
		}

		i := slices.IndexFunc(book, func(p Peer) bool { return p.Name == name && p.Cert != nil })
		if i < 0 {
			return fmt.Errorf("no certificate of %s in this node's book: it has not met that node", name)
		}

		cert := *book[i].Cert

		have, err := n.Revocations()
		if err != nil {
			return err
		}

		if have.Has(cert.Key) {
			out = have

			return nil
		}

		out = Revocations{
			Version: have.Version + 1,
			At:      now.UTC().Format(time.RFC3339Nano),
			Revoked: append(slices.Clone(have.Revoked), Revoked{Name: name, Key: cert.Key, At: now.UTC().Format(time.RFC3339Nano)}),
		}
		out.Sig = ed25519.Sign(owner, out.message())
		added = true

		return writeJSON(n.path("revoked.json"), out)
	})

	return out, added, err
}
