package mesh

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var day = 24 * time.Hour

// t0 is the clock of the tests that need one: a fixed day, so a certificate's age never depends on the machine's.
var t0 = time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)

// freshOwner is an owner node whose certificate, and every one it issues with SignRequest, is dated at now.
func freshOwner(t *testing.T, name string, now time.Time) *Node {
	t.Helper()

	dir := t.TempDir()
	_, err := Init(dir, name)
	require.NoError(t, err)
	require.NoError(t, MakeOwner(dir, now))

	n, err := Open(dir)
	require.NoError(t, err)

	return n
}

func enrollAt(t *testing.T, owner *Node, name string, now time.Time, life time.Duration) *Node {
	t.Helper()

	dir := t.TempDir()
	r, err := Init(dir, name)
	require.NoError(t, err)

	g, err := SignRequestFor(owner.Dir, r, now, life)
	require.NoError(t, err)
	require.NoError(t, AcceptAt(dir, g, now))

	n, err := Open(dir)
	require.NoError(t, err)

	return n
}

// A certificate counts for a year by default, with the expiry inside what the owner signs.
func TestCertificateExpires(t *testing.T) {
	owner := freshOwner(t, "owner", t0)
	box := enrollAt(t, owner, "box", t0, DefaultCertLife)

	assert.Equal(t, "2028-02-29T12:00:00Z", box.Cert.Expires, "365 days from the day it was issued")
	require.NoError(t, box.Cert.Verify(owner.Owner))

	require.NoError(t, box.Cert.Expired(t0.Add(365*day-time.Second)))

	err := box.Cert.Expired(t0.Add(365 * day))
	var gone *ExpiredError
	require.True(t, errors.As(err, &gone))
	assert.Equal(t, "the certificate of box expired on 2028-02-29: on the owner's node run zmina node renew box", err.Error())

	// The expiry is signed: it can be neither moved nor taken off.
	moved := box.Cert
	moved.Expires = "2099-03-01T12:00:00Z"
	require.ErrorContains(t, moved.Verify(owner.Owner), "not signed by this owner")

	stripped := box.Cert
	stripped.Expires = ""
	require.ErrorContains(t, stripped.Verify(owner.Owner), "not signed by this owner")

	// The one without an expiry is the old message, as an older zmina reads it, and counts for now.
	old := enrollAt(t, owner, "old", t0, 0)
	assert.Empty(t, old.Cert.Expires)
	require.NoError(t, old.Cert.Verify(owner.Owner))
	require.NoError(t, old.Cert.Expired(t0.Add(50*365*day)))
	assert.Equal(t, []byte("zmina-node\x00old\x00"+string(old.Cert.Key)+"\x00"+old.Cert.At), old.Cert.message())
	assert.NotEqual(t, old.Cert.message()[:12], box.Cert.message()[:12])

	_, err = SignFor(nil, "box", box.Cert.Key, t0, -time.Hour)
	require.Error(t, err)
}

// The warning starts 30 days before the expiry; a certificate with no expiry only has its own state.
func TestCertificateStateAndWarningWindow(t *testing.T) {
	owner := freshOwner(t, "owner", t0)
	box := enrollAt(t, owner, "box", t0, DefaultCertLife)
	expires := t0.Add(DefaultCertLife)

	for _, c := range []struct {
		at   time.Time
		want CertState
	}{
		{t0, CertValid},
		{expires.Add(-31 * day), CertValid},
		{expires.Add(-30 * day), CertExpiring},
		{expires.Add(-time.Second), CertExpiring},
		{expires, CertExpired},
		{expires.Add(40 * day), CertExpired},
	} {
		state, at := box.Cert.State(c.at)
		assert.Equal(t, c.want, state, c.at.Format(time.DateOnly))
		assert.True(t, at.Equal(expires))
	}

	old := enrollAt(t, owner, "old", t0, 0)
	state, at := old.Cert.State(t0.Add(10 * 365 * day))
	assert.Equal(t, CertNoExpiry, state)
	assert.True(t, at.IsZero())
}

// An expired certificate opens no session either way, and says what to run; a renewed one does.
func TestExpiredCertificateOpensNoSession(t *testing.T) {
	owner := freshOwner(t, "laptop", t0)
	box := enrollAt(t, owner, "box", t0, DefaultCertLife)
	clock := t0
	p := &pipes{nodes: map[string]*Node{"laptop": owner, "box": box}, inboxes: map[string]*inbox{"laptop": {}, "box": {}},
		down: map[string]bool{}, now: func() time.Time { return clock }}
	addPeer(t, owner, "box")
	addPeer(t, box, "laptop")
	sync := func(n *Node) []Session {
		return n.Sync(t.Context(), p, []Discovery{Book{n}}, p.inboxes[n.Name].deliver, "", 5*time.Second, 0, func() time.Time { return clock })
	}

	for _, s := range sync(owner) {
		require.NoError(t, s.Err, "both certificates count")
	}

	// A year and a day on. The owner's own certificate is a year old too: it renews itself.
	clock = t0.Add(366 * day)
	_, err := Renew(owner.Dir, clock, DefaultCertLife)
	require.NoError(t, err)
	owner, err = Open(owner.Dir)
	require.NoError(t, err)
	p.nodes["laptop"] = owner

	// The peer's certificate ran out: the owner refuses it, and says the command.
	ss := sync(owner)
	require.Len(t, ss, 1)
	require.Error(t, ss[0].Err)
	assert.Contains(t, ss[0].Err.Error(), "the certificate of box expired on 2028-02-29: on the owner's node run zmina node renew box")

	// And from the expired node's side: it does not call with it, whatever the peer's clock says.
	ss = sync(box)
	require.Len(t, ss, 1)
	require.Error(t, ss[0].Err)
	assert.Contains(t, ss[0].Err.Error(), "zmina node renew box")

	// A node whose clock is behind still calls; the peer's clock decides, and the peer says so.
	late := &pipes{nodes: p.nodes, inboxes: p.inboxes, down: p.down, now: func() time.Time { return t0.Add(366 * day) }}
	ss = box.Sync(t.Context(), late, []Discovery{Book{box}}, late.inboxes["box"].deliver, "", 5*time.Second, 0, func() time.Time { return t0 })
	require.Len(t, ss, 1)
	require.Error(t, ss[0].Err)
	assert.Contains(t, ss[0].Err.Error(), "the certificate of box expired on 2028-02-29")

	// Renewed — the same key, a new date — it is met again.
	g, err := SignRequestFor(owner.Dir, Request{Name: "box", Key: box.Cert.Key}, clock, DefaultCertLife)
	require.NoError(t, err)
	require.NoError(t, AcceptAt(box.Dir, g, clock))
	box, err = Open(box.Dir)
	require.NoError(t, err)
	p.nodes["box"] = box
	assert.Equal(t, "2029-03-01T12:00:00Z", box.Cert.Expires)

	for _, s := range sync(owner) {
		require.NoError(t, s.Err, "a renewed certificate counts: %v", s.Err)
	}
	for _, s := range sync(box) {
		require.NoError(t, s.Err)
	}
}

// A grant that is already past is not stored; an expired writer's envelope is not taken, nor a told node's certificate.
func TestExpiredCertificateIsNotAccepted(t *testing.T) {
	owner := freshOwner(t, "laptop", t0)
	dir := t.TempDir()
	r, err := Init(dir, "box")
	require.NoError(t, err)

	g, err := SignRequestFor(owner.Dir, r, t0, 30*day)
	require.NoError(t, err)
	require.ErrorContains(t, AcceptAt(dir, g, t0.Add(31*day)), "zmina node renew box")
	_, err = Open(dir)
	require.Error(t, err, "nothing was stored")
	require.NoError(t, AcceptAt(dir, g, t0.Add(29*day)))

	box, err := Open(dir)
	require.NoError(t, err)

	to, _ := ParseAddress("laptop")
	e, err := box.NewEnvelope("ops", to, "hello", t0.Add(40*day))
	require.NoError(t, err)

	in := &inbox{}
	ids, err := owner.receive([]Envelope{e}, in.deliver, t0.Add(40*day))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the certificate of box expired")
	assert.Equal(t, []string{e.ID}, ids, "the giver drops what will not be taken")
	assert.Empty(t, in.list())

	e, err = box.NewEnvelope("ops", to, "hello", t0.Add(20*day))
	require.NoError(t, err)
	_, err = owner.receive([]Envelope{e}, in.deliver, t0.Add(20*day))
	require.NoError(t, err)
	assert.Len(t, in.list(), 1)

	// A node told of a certificate that ran out does not keep it.
	late := t0.Add(60 * day)
	other := enrollAt(t, owner, "other", t0, 30*day)
	require.NoError(t, owner.met(other.Cert, []Peer{{Name: "box", Cert: &box.Cert}}, BodyMax, late))
	book, err := owner.Peers()
	require.NoError(t, err)
	names := []string{}
	for _, p := range book {
		names = append(names, p.Name)
	}
	assert.Equal(t, []string{"other"}, names, "box's certificate had run out when it was told")
}

// Renewing keeps the key and the name and gives a later date and expiry; a node that is not the owner cannot.
func TestRenewKeepsTheKeyAndMovesTheExpiry(t *testing.T) {
	owner := freshOwner(t, "laptop", t0)
	was := owner.Cert
	later := t0.Add(300 * day)

	c, err := Renew(owner.Dir, later, DefaultCertLife)
	require.NoError(t, err)

	assert.True(t, c.Key.Equal(was.Key))
	assert.Equal(t, "laptop", c.Name)
	assert.True(t, c.Newer(was))
	assert.Equal(t, later.Add(DefaultCertLife).UTC().Format(time.RFC3339), c.Expires)
	require.NoError(t, c.Verify(owner.Owner))

	again, err := Open(owner.Dir)
	require.NoError(t, err)
	assert.Equal(t, c.Expires, again.Cert.Expires, "kept in node.json")

	// A renewed certificate is not a replaced one: its key is the same.
	book := []Peer{{Name: "laptop", Cert: &c}}
	assert.False(t, replaced(book, was))

	// A node without the owner key cannot issue one.
	box := enrollAt(t, owner, "box", t0, DefaultCertLife)
	_, err = Renew(box.Dir, later, DefaultCertLife)
	require.ErrorContains(t, err, "does not hold the owner key")

	_, err = Renew(t.TempDir(), later, DefaultCertLife)
	require.ErrorIs(t, err, ErrNoNode)
}

// No expiry on a certificate issued before: it counts, and renewing gives it one.
func TestCertificateWithoutExpiryCountsAndRenewsIntoOne(t *testing.T) {
	owner := freshOwner(t, "laptop", t0)
	box := enrollAt(t, owner, "box", t0, 0)
	require.Empty(t, box.Cert.Expires)

	clock := t0.Add(3 * 365 * day)
	require.NoError(t, box.Cert.Expired(clock))

	g, err := SignRequestFor(owner.Dir, Request{Name: "box", Key: box.Cert.Key}, clock, DefaultCertLife)
	require.NoError(t, err)
	require.NoError(t, AcceptAt(box.Dir, g, clock))

	box, err = Open(box.Dir)
	require.NoError(t, err)
	assert.NotEmpty(t, box.Cert.Expires)
	require.NoError(t, box.Cert.Verify(owner.Owner))
}

// The certificate a node gets for another node's key or name is not taken, expiry or not.
func TestAcceptRefusesAnotherNodesGrant(t *testing.T) {
	owner := freshOwner(t, "laptop", t0)
	dir := t.TempDir()
	_, err := Init(dir, "box")
	require.NoError(t, err)

	other, err := NewKey()
	require.NoError(t, err)
	g, err := SignRequestFor(owner.Dir, Request{Name: "box", Key: other.Public().(ed25519.PublicKey)}, t0, DefaultCertLife)
	require.NoError(t, err)
	require.ErrorContains(t, AcceptAt(dir, g, t0), "not this node")
}
