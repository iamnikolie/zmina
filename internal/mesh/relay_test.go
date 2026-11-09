package mesh

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A certified peer that tells it reaches a node directly carries nothing for it unless the owner named it a relay.
func TestDirectClaimFromAPeerNotARelayCarriesNothing(t *testing.T) {
	a := newOwner(t, "alpha")
	m := enroll(t, a, "mallory")
	y := enroll(t, a, "yankee")

	// mallory calls in and says it reaches yankee; the told entry also says relay, which the wire never sets.
	require.NoError(t, a.met(m.Cert, []Peer{{Name: y.Name, Cert: &y.Cert, Direct: true, Relay: true}}, BodyMax, time.Now()))

	book, err := a.Peers()
	require.NoError(t, err)
	for _, p := range book {
		assert.False(t, p.Relay, "%s: what a peer tells never makes a relay", p.Name)
		assert.Empty(t, p.Through, "%s: a claim from a peer that is no relay is not kept", p.Name)
	}

	tell(t, a, "ops", "yankee/ops", "the deploy key")
	hand, problems := a.handFor(Peer{Name: m.Name, BodyMax: BodyMax}, book)
	assert.Empty(t, problems)
	assert.Empty(t, hand, "yankee's envelope stays here")

	// A book written before relays were the owner's word still lists the claim: it is not used either.
	book = []Peer{{Name: m.Name, Cert: &m.Cert}, {Name: y.Name, Cert: &y.Cert, Through: []string{m.Name}}}
	hand, _ = a.handFor(Peer{Name: m.Name, BodyMax: BodyMax}, book)
	assert.Empty(t, hand)

	// The owner names mallory a relay: now the claim counts.
	markRelay(t, a, m.Name)
	require.NoError(t, a.met(m.Cert, []Peer{{Name: y.Name, Cert: &y.Cert, Direct: true}}, BodyMax, time.Now()))
	book, err = a.Peers()
	require.NoError(t, err)
	hand, _ = a.handFor(Peer{Name: m.Name, BodyMax: BodyMax}, book)
	assert.Len(t, hand, 1)
}

func TestCarrierNotNamedARelayCarriesNothing(t *testing.T) {
	p, laptop, ops, _ := trio(t)

	tell(t, ops, "ops", "box-web/web", "the dump is ready")

	for range 3 {
		sync1(laptop, p)
	}

	assert.Empty(t, p.inboxes["box-web"].list(), "ops's owner never let laptop carry")

	out, err := ops.Outbox()
	require.NoError(t, err)
	assert.Len(t, out, 1, "the envelope waits on ops")
}

func TestTerminalRouteSkipsAPeerNotARelay(t *testing.T) {
	owner := newOwner(t, "owner")
	jump := enroll(t, owner, "jump")
	worker := enroll(t, owner, "worker")
	addPeer(t, owner, "jump")
	addPeer(t, jump, "worker")
	require.NoError(t, owner.UpdatePeers(func(ps []Peer) []Peer {
		return append(ps, Peer{Name: "worker", Through: []string{"jump"}})
	}))

	transport := &terminalPipes{nodes: map[string]*Node{"jump": jump, "worker": worker}}
	_, err := owner.TerminalList(context.Background(), transport, "worker")
	require.ErrorContains(t, err, "no address or relay")
}

// TestReplacedCertificateOpensNoSession: once the owner re-certified a node with a new key and a node learnt the
// newer certificate, the old key completes no hello there, either side of it: it neither pulls the envelopes
// addressed to that node's name nor is called as that node.
func TestReplacedCertificateOpensNoSession(t *testing.T) {
	p, laptop, old, _ := trio(t)

	tell(t, laptop, "default", "box-ops/ops", "the token for box-ops")

	time.Sleep(2 * time.Millisecond) // the new certificate is issued later
	fresh := enroll(t, laptop, "box-ops")
	require.NoError(t, laptop.met(fresh.Cert, nil, BodyMax, time.Now()))

	// The old key calls in.
	addPeer(t, old, "laptop")
	ss := sync1(old, p)
	require.Len(t, ss, 1)
	require.Error(t, ss[0].Err)
	assert.Contains(t, ss[0].Err.Error(), "replaced")

	// The old key answers when this node calls box-ops.
	called := false
	for _, s := range sync1(laptop, p) {
		if strings.Contains(s.Addr, "box-ops") {
			called = true
			require.Error(t, s.Err)
			assert.Contains(t, s.Err.Error(), "replaced")
		}
	}
	require.True(t, called)

	assert.Empty(t, p.inboxes["box-ops"].list(), "the old key took nothing")

	out, err := laptop.Outbox()
	require.NoError(t, err)
	assert.Len(t, out, 1, "the envelope waits for the new key")

	// The new key gets it.
	p.nodes["box-ops"] = fresh
	sync1(laptop, p)
	assert.Equal(t, []string{"ops ← laptop/default: the token for box-ops"}, p.inboxes["box-ops"].list())
}
