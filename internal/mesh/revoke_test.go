package mesh

import (
	"crypto/ed25519"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// meets makes n hold the certificate of each of the others, as a session would have left it in its book.
func meets(t *testing.T, n *Node, others ...*Node) {
	t.Helper()

	for _, o := range others {
		require.NoError(t, n.met(o.Cert, nil, BodyMax, time.Now()))
	}
}

// ownerSigned is a list the owner signed by hand, for the tests that offer lists a node must refuse or keep.
func ownerSigned(t *testing.T, owner *Node, version int64, entries ...Revoked) Revocations {
	t.Helper()

	key, err := readKey(filepath.Join(owner.Dir, "owner.key"))
	require.NoError(t, err)

	r := Revocations{Version: version, At: time.Now().UTC().Format(time.RFC3339Nano), Revoked: entries}
	r.Sig = ed25519.Sign(key, r.message())

	return r
}

// The owner revokes by name: the key of the certificate it holds, the next version, signed; revoking twice changes nothing.
func TestOwnerRevokesAKeyAndTheVersionGoesUp(t *testing.T) {
	owner := newOwner(t, "laptop")
	a, b := enroll(t, owner, "a"), enroll(t, owner, "b")
	meets(t, owner, a, b)

	none, err := owner.Revocations()
	require.NoError(t, err)
	assert.Zero(t, none.Version)

	r, added, err := owner.Revoke("a", time.Now())
	require.NoError(t, err)
	assert.True(t, added)
	assert.EqualValues(t, 1, r.Version)
	assert.True(t, r.Has(a.Cert.Key))
	assert.False(t, r.Has(b.Cert.Key))
	require.NoError(t, r.Verify(owner.Owner))

	r, added, err = owner.Revoke("a", time.Now())
	require.NoError(t, err)
	assert.False(t, added, "already on the list")
	assert.EqualValues(t, 1, r.Version)

	r, added, err = owner.Revoke("b", time.Now())
	require.NoError(t, err)
	assert.True(t, added)
	assert.EqualValues(t, 2, r.Version)
	assert.True(t, r.Has(a.Cert.Key) && r.Has(b.Cert.Key), "a list only grows")

	kept, err := owner.Revocations()
	require.NoError(t, err)
	assert.Equal(t, r, kept)

	_, _, err = owner.Revoke("nobody", time.Now())
	require.ErrorContains(t, err, "has not met that node")
	_, _, err = owner.Revoke("laptop", time.Now())
	require.ErrorContains(t, err, "cannot revoke itself")
	_, _, err = a.Revoke("b", time.Now())
	require.ErrorContains(t, err, "does not hold the owner key")
}

// A node never takes a list with a bad signature, a lower version or the same one: it keeps the highest it has seen.
func TestRevocationListsNeverGoBackOrFromAnotherOwner(t *testing.T) {
	owner := newOwner(t, "laptop")
	a, b, c := enroll(t, owner, "a"), enroll(t, owner, "b"), enroll(t, owner, "c")
	meets(t, owner, a, b, c)
	node := enroll(t, owner, "node")

	for _, name := range []string{"a", "b", "c"} {
		_, _, err := owner.Revoke(name, time.Now())
		require.NoError(t, err)
	}
	v3, err := owner.Revocations()
	require.NoError(t, err)
	require.EqualValues(t, 3, v3.Version)

	took, err := node.AcceptRevocations(v3)
	require.NoError(t, err)
	assert.True(t, took)

	// The owner's own earlier list: signed, but lower. Rolled back, a, b and c would be let in again.
	v2 := ownerSigned(t, owner, 2, Revoked{Name: "a", Key: a.Cert.Key})
	took, err = node.AcceptRevocations(v2)
	require.NoError(t, err)
	assert.False(t, took, "a lower version is not taken")

	took, err = node.AcceptRevocations(v3)
	require.NoError(t, err)
	assert.False(t, took, "nor the same one again")

	// A higher version that revokes fewer keys is the owner's word: it is the owner's to give, and is taken.
	v4 := ownerSigned(t, owner, 4, Revoked{Name: "a", Key: a.Cert.Key})
	took, err = node.AcceptRevocations(v4)
	require.NoError(t, err)
	assert.True(t, took)
	held, err := node.Revocations()
	require.NoError(t, err)
	assert.EqualValues(t, 4, held.Version)

	// Not the owner's: another owner's list, a list with an entry added, a version raised.
	stranger := newOwner(t, "stranger")
	forged := ownerSigned(t, stranger, 9, Revoked{Name: "a", Key: a.Cert.Key})
	_, err = node.AcceptRevocations(forged)
	require.ErrorContains(t, err, "not signed by this owner")

	padded := v4
	padded.Revoked = append(append([]Revoked(nil), v4.Revoked...), Revoked{Name: "b", Key: b.Cert.Key, At: v4.At})
	_, err = node.AcceptRevocations(padded)
	require.ErrorContains(t, err, "not signed by this owner")

	raised := v4
	raised.Version = 50
	_, err = node.AcceptRevocations(raised)
	require.ErrorContains(t, err, "not signed by this owner")

	_, err = node.AcceptRevocations(Revocations{Version: 99})
	require.Error(t, err, "an unsigned list")

	held, err = node.Revocations()
	require.NoError(t, err)
	assert.EqualValues(t, 4, held.Version, "nothing that was refused changed what the node holds")
	assert.True(t, held.Has(a.Cert.Key))
	assert.False(t, held.Has(b.Cert.Key))
}

// A revoked key opens no session in either direction, and the list reaches nodes that never met the owner since: it
// rides in every hello.
func TestRevocationSpreadsWithTheHelloAndRefusesThePeer(t *testing.T) {
	owner := newOwner(t, "laptop")
	a, b, c := enroll(t, owner, "a"), enroll(t, owner, "b"), enroll(t, owner, "c")
	p := &pipes{nodes: map[string]*Node{"laptop": owner, "a": a, "b": b, "c": c},
		inboxes: map[string]*inbox{"laptop": {}, "a": {}, "b": {}, "c": {}}, down: map[string]bool{}}
	addPeer(t, owner, "a")
	addPeer(t, a, "b")
	addPeer(t, c, "b")
	addPeer(t, b, "c")
	meets(t, owner, c)

	for _, s := range sync1(c, p) {
		require.NoError(t, s.Err, "c is a node of the mesh until the owner says otherwise")
	}

	_, _, err := owner.Revoke("c", time.Now())
	require.NoError(t, err)

	// laptop -> a: a learns the list from the owner's hello. a -> b: b learns it from a's, never having met the owner.
	for _, s := range sync1(owner, p) {
		require.NoError(t, s.Err)
	}
	held, err := a.Revocations()
	require.NoError(t, err)
	require.EqualValues(t, 1, held.Version)

	for _, s := range sync1(a, p) {
		require.NoError(t, s.Err)
	}
	held, err = b.Revocations()
	require.NoError(t, err)
	require.EqualValues(t, 1, held.Version, "carried by a's hello")
	assert.True(t, held.Has(c.Cert.Key))

	// c calls b: refused; b calls c: refused. The revoked node itself learns it from b's hello and goes quiet.
	ss := sync1(c, p)
	require.Len(t, ss, 1)
	require.Error(t, ss[0].Err)
	assert.Contains(t, ss[0].Err.Error(), "revoked")

	ss = sync1(b, p)
	for _, s := range ss {
		if s.Peer == "c" {
			require.Error(t, s.Err)
			assert.Contains(t, s.Err.Error(), "revoked")
		}
	}

	// Nothing that c had for b, or b for c, moved.
	assert.Empty(t, p.inboxes["b"].list())
	assert.Empty(t, p.inboxes["c"].list())
}

// A revoked node's envelope is not taken, even carried by a node that is not revoked; a told certificate of a revoked
// key is not added to the book; a revoked node opens no session of its own.
func TestRevokedKeyWritesNothingAndIsNotTold(t *testing.T) {
	owner := newOwner(t, "laptop")
	a, c := enroll(t, owner, "a"), enroll(t, owner, "c")
	meets(t, owner, a, c)

	to, _ := ParseAddress("laptop")
	e, err := c.NewEnvelope("ops", to, "from c", time.Now())
	require.NoError(t, err)

	in := &inbox{}
	_, err = owner.receive([]Envelope{e}, in.deliver, time.Now())
	require.NoError(t, err)
	require.Len(t, in.list(), 1, "before the owner revokes, it is taken")

	_, _, err = owner.Revoke("c", time.Now())
	require.NoError(t, err)

	e2, err := c.NewEnvelope("ops", to, "again from c", time.Now())
	require.NoError(t, err)
	ids, err := owner.receive([]Envelope{e2}, in.deliver, time.Now())
	require.ErrorContains(t, err, "revoked key of c")
	assert.Equal(t, []string{e2.ID}, ids, "the giver drops it")
	assert.Len(t, in.list(), 1)

	require.NoError(t, owner.UpdatePeers(func([]Peer) []Peer { return nil }))
	require.NoError(t, owner.met(a.Cert, []Peer{{Name: "c", Cert: &c.Cert}}, BodyMax, time.Now()))
	book, err := owner.Peers()
	require.NoError(t, err)
	require.Len(t, book, 1)
	assert.Equal(t, "a", book[0].Name, "c's certificate, told by a, is not kept")

	// The revoked node, once it learns it, opens no session of its own.
	list, err := owner.Revocations()
	require.NoError(t, err)
	_, err = c.AcceptRevocations(list)
	require.NoError(t, err)
	client, server := net.Pipe()
	defer func() { _ = client.Close(); _ = server.Close() }()
	_, err = c.open(newConn(client), "laptop", time.Now)
	require.ErrorContains(t, err, "was revoked by its owner")
}

// A hello that carries a list the owner did not sign ends the session, and nothing is kept.
func TestHelloWithAForgedRevocationListIsRefused(t *testing.T) {
	owner := newOwner(t, "laptop")
	box := enroll(t, owner, "box")
	other := enroll(t, owner, "other")
	meets(t, owner, box)

	stranger := newOwner(t, "stranger")
	forged := ownerSigned(t, stranger, 7, Revoked{Name: "other", Key: other.Cert.Key})

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
	require.NoError(t, c.send(wire{Op: "hello", V: protocolVersion, Cert: &box.Cert, Nonce: challenge, Revoked: &forged}))
	_, err = c.expect("hello")
	require.ErrorContains(t, err, "did not sign")
	require.Error(t, <-done)

	held, err := owner.Revocations()
	require.NoError(t, err)
	assert.Zero(t, held.Version)
}

// A node's list is kept in revoked.json and survives a reopen; none is version 0.
func TestRevocationListIsKeptWithTheNode(t *testing.T) {
	owner := newOwner(t, "laptop")
	a := enroll(t, owner, "a")
	meets(t, owner, a)
	_, _, err := owner.Revoke("a", time.Now())
	require.NoError(t, err)

	again, err := Open(owner.Dir)
	require.NoError(t, err)
	held, err := again.Revocations()
	require.NoError(t, err)
	assert.EqualValues(t, 1, held.Version)
	assert.True(t, held.Has(a.Cert.Key))
}
