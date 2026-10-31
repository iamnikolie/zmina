package mesh

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Node is this ~/.zmina's member of the mesh: its directory, name, key, certificate and the owner's public key.
type Node struct {
	Dir   string
	Name  string
	Key   ed25519.PrivateKey
	Cert  Cert
	Owner ed25519.PublicKey
}

// ErrNoNode is returned while this ~/.zmina has no node yet (`zmina node init`).
var ErrNoNode = errors.New("this machine is not a node yet: zmina node init")

// nodeFile is node.json: who this node is. The private key sits in node.key beside it, the owner's (on the owner's
// node only) in owner.key.
type nodeFile struct {
	Name  string            `json:"name"`
	Owner ed25519.PublicKey `json:"owner,omitempty"`
	Cert  *Cert             `json:"cert,omitempty"`
}

// Request is what a new node hands the owner to sign: its name and public key.
type Request struct {
	Name string            `json:"name"`
	Key  ed25519.PublicKey `json:"key"`
}

// Grant is what the owner hands back: the certificate and the owner's public key.
type Grant struct {
	Cert  Cert              `json:"cert"`
	Owner ed25519.PublicKey `json:"owner"`
}

// Init makes this node's key under dir (once; a second call keeps the key and may rename an uncertified node) and
// returns the request for the owner to sign.
func Init(dir, name string) (Request, error) {
	if !ValidName(name) {
		return Request{}, fmt.Errorf("node name %q: use letters, digits, '.', '_' and '-'", name)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Request{}, err
	}

	key, err := readKey(filepath.Join(dir, "node.key"))
	if errors.Is(err, os.ErrNotExist) {
		if key, err = NewKey(); err == nil {
			err = writeKey(filepath.Join(dir, "node.key"), key)
		}
	}

	if err != nil {
		return Request{}, err
	}

	var nf nodeFile
	if err := readJSON(filepath.Join(dir, "node.json"), &nf); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Request{}, err
	}

	if nf.Cert != nil && nf.Name != name {
		return Request{}, fmt.Errorf("this node is %s already", nf.Name)
	}

	nf.Name = name
	if err := writeJSON(filepath.Join(dir, "node.json"), nf); err != nil {
		return Request{}, err
	}

	return Request{Name: name, Key: key.Public().(ed25519.PublicKey)}, nil
}

// MakeOwner gives the node under dir an owner key and certifies the node with it: the first node of a mesh.
func MakeOwner(dir string, now time.Time) error {
	path := filepath.Join(dir, "owner.key")
	if _, err := os.Stat(path); err == nil {
		return errors.New("this node holds the owner key already")
	}

	var nf nodeFile
	if err := readJSON(filepath.Join(dir, "node.json"), &nf); err != nil {
		return err
	}

	if nf.Cert != nil {
		return errors.New("this node is certified by another owner")
	}

	key, err := readKey(filepath.Join(dir, "node.key"))
	if err != nil {
		return err
	}

	owner, err := NewKey()
	if err != nil {
		return err
	}

	if err := writeKey(path, owner); err != nil {
		return err
	}

	g, err := grant(owner, Request{Name: nf.Name, Key: key.Public().(ed25519.PublicKey)}, now)
	if err != nil {
		return err
	}

	return Accept(dir, g)
}

// SignRequest certifies another node with the owner key under dir.
func SignRequest(dir string, r Request, now time.Time) (Grant, error) {
	owner, err := readKey(filepath.Join(dir, "owner.key"))
	if errors.Is(err, os.ErrNotExist) {
		return Grant{}, errors.New("this node does not hold the owner key")
	}

	if err != nil {
		return Grant{}, err
	}

	return grant(owner, r, now)
}

func grant(owner ed25519.PrivateKey, r Request, now time.Time) (Grant, error) {
	if len(r.Key) != ed25519.PublicKeySize {
		return Grant{}, errors.New("request: bad key")
	}

	c, err := Sign(owner, r.Name, r.Key, now)
	if err != nil {
		return Grant{}, err
	}

	return Grant{Cert: c, Owner: owner.Public().(ed25519.PublicKey)}, nil
}

// Accept stores the owner's grant for the node under dir: it must certify this node's own key and name.
func Accept(dir string, g Grant) error {
	key, err := readKey(filepath.Join(dir, "node.key"))
	if err != nil {
		return fmt.Errorf("no node key here: %w", err)
	}

	var nf nodeFile
	if err := readJSON(filepath.Join(dir, "node.json"), &nf); err != nil {
		return err
	}

	if err := g.Cert.Verify(g.Owner); err != nil {
		return err
	}

	if !g.Cert.Key.Equal(key.Public()) || g.Cert.Name != nf.Name {
		return fmt.Errorf("the certificate is for %s, not this node (%s)", g.Cert.Name, nf.Name)
	}

	if nf.Owner != nil && !nf.Owner.Equal(g.Owner) {
		return errors.New("this node already has another owner")
	}

	nf.Owner, nf.Cert = g.Owner, &g.Cert

	return writeJSON(filepath.Join(dir, "node.json"), nf)
}

// Open reads the certified node under dir.
func Open(dir string) (*Node, error) {
	var nf nodeFile
	if err := readJSON(filepath.Join(dir, "node.json"), &nf); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoNode
	} else if err != nil {
		return nil, err
	}

	if nf.Cert == nil {
		return nil, fmt.Errorf("node %s waits for the owner's certificate: zmina node sign on the owner's node, "+
			"then zmina node accept here", nf.Name)
	}

	key, err := readKey(filepath.Join(dir, "node.key"))
	if err != nil {
		return nil, err
	}

	return &Node{Dir: dir, Name: nf.Name, Key: key, Cert: *nf.Cert, Owner: nf.Owner}, nil
}

// IsOwner reports whether this node holds the owner key.
func (n *Node) IsOwner() bool {
	_, err := os.Stat(filepath.Join(n.Dir, "owner.key"))

	return err == nil
}

// Peer is one other node this node knows: from the owner's hand (`zmina node peer add`, `enroll`) or told by
// another node.
type Peer struct {
	Name   string `json:"name"`
	Cert   *Cert  `json:"cert,omitempty"`
	Addrs  []Addr `json:"addrs,omitempty"`
	Source string `json:"source,omitempty"` // hand, told, called
	// OK is the last time a session with it went through, Tried the last attempt, Err why that attempt failed.
	OK    time.Time `json:"ok,omitzero"`
	Tried time.Time `json:"tried,omitzero"`
	Err   string    `json:"err,omitempty"`
	// Through are the peers that told this node they reach it directly: who may carry an envelope for it.
	Through []string `json:"through,omitempty"`
	// Allow are this node's crews the peer may reach (default, a config name, or * for all); none by default.
	Allow []string `json:"allow,omitempty"`
	// Direct is what a node tells a peer: it reaches this one itself (only on the wire).
	Direct bool `json:"direct,omitempty"`
}

// direct reports whether this node reaches the peer itself: it has an address and the last try did not fail.
func (p Peer) direct() bool { return len(p.Addrs) > 0 && p.Err == "" }

// Peers reads the address book.
func (n *Node) Peers() ([]Peer, error) {
	var ps []Peer
	if err := readJSON(n.path("peers.json"), &ps); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	return ps, nil
}

// UpdatePeers changes the address book under the node's lock.
func (n *Node) UpdatePeers(fn func(ps []Peer) []Peer) error {
	return n.locked(func() error {
		ps, err := n.Peers()
		if err != nil {
			return err
		}

		ps = fn(ps)
		sort.Slice(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })

		return writeJSON(n.path("peers.json"), ps)
	})
}

// Outbox lists the envelopes waiting to leave this node, oldest first.
func (n *Node) Outbox() ([]Envelope, error) {
	files, err := filepath.Glob(n.path("outbox", "*.json"))
	if err != nil {
		return nil, err
	}

	sort.Strings(files)

	var out []Envelope

	for _, f := range files {
		var e Envelope
		if err := readJSON(f, &e); err != nil {
			continue // half-written or removed meanwhile
		}

		out = append(out, e)
	}

	return out, nil
}

// Queue puts an envelope in the outbox.
func (n *Node) Queue(e Envelope) error {
	if err := os.MkdirAll(n.path("outbox"), 0o700); err != nil {
		return err
	}

	return writeJSON(n.outboxPath(e.ID), e)
}

// Unqueue removes envelopes from the outbox once another node holds them.
func (n *Node) Unqueue(ids []string) {
	for _, id := range ids {
		if ValidID(id) {
			_ = os.Remove(n.outboxPath(id))
		}
	}
}

func (n *Node) outboxPath(id string) string { return n.path("outbox", id+".json") }

// ValidID reports whether id can name an outbox file.
func ValidID(id string) bool {
	return id != "" && !strings.ContainsAny(id, `/\`) && !strings.HasPrefix(id, ".")
}

// seenKeep is how long a delivered or carried envelope's id is remembered against a second copy.
const seenKeep = 30 * 24 * time.Hour

// seen and markSeen read and write seen.json, the ids already taken; callers hold the lock.
func (n *Node) seen() (map[string]time.Time, error) {
	m := map[string]time.Time{}
	if err := readJSON(n.path("seen.json"), &m); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	return m, nil
}

func (n *Node) markSeen(m map[string]time.Time, now time.Time) error {
	for id, at := range m {
		if now.Sub(at) > seenKeep {
			delete(m, id)
		}
	}

	return writeJSON(n.path("seen.json"), m)
}

func (n *Node) path(parts ...string) string {
	return filepath.Join(append([]string{n.Dir}, parts...)...)
}

// locked runs fn holding the node's lock (lock next to node.json), waiting for it up to a minute.
func (n *Node) locked(fn func() error) error {
	f, err := os.OpenFile(n.path("lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	deadline := time.Now().Add(time.Minute)

	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}

		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("%s is held by another zmina", n.path("lock"))
		}

		time.Sleep(50 * time.Millisecond)
	}

	return fn()
}

// TrySync takes the sync lock without waiting: false when another zmina is syncing this node already. The returned
// func releases it.
func (n *Node) TrySync() (func(), bool) {
	f, err := os.OpenFile(n.path("sync.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}

	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		_ = f.Close()

		return nil, false
	}

	return func() { _ = f.Close() }, true
}

// LastSync is when this node last ran a sync round (the mtime of its synced stamp).
func (n *Node) LastSync() time.Time {
	fi, err := os.Stat(n.path("synced"))
	if err != nil {
		return time.Time{}
	}

	return fi.ModTime()
}

// MarkSync records that a sync round ran now.
func (n *Node) MarkSync() { _ = os.WriteFile(n.path("synced"), nil, 0o600) }

func readKey(path string) (ed25519.PrivateKey, error) {
	var seed []byte
	if err := readJSON(path, &seed); err != nil {
		return nil, err
	}

	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s: not a key", path)
	}

	return ed25519.NewKeyFromSeed(seed), nil
}

func writeKey(path string, k ed25519.PrivateKey) error { return writeJSON(path, k.Seed()) }

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	return nil
}

// writeJSON replaces path whole (temp file, then rename), owner-only.
func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, path)
}
