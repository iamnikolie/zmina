package mesh

import (
	"crypto/ed25519"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// session runs one Call from a to b over a pipe, b answering with Serve.
func session(t *testing.T, a, b *Node) {
	t.Helper()

	client, server := net.Pipe()
	require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, server.SetDeadline(time.Now().Add(5*time.Second)))

	done := make(chan error, 1)
	go func() { defer func() { _ = server.Close() }(); done <- b.Serve(server, (&inbox{}).deliver, time.Now) }()

	_, _, _, err := a.Call(client, b.Name, (&inbox{}).deliver, time.Now)
	require.NoError(t, err)
	require.NoError(t, client.Close())
	require.NoError(t, <-done)
}

func peerNamed(t *testing.T, n *Node, name string) Peer {
	t.Helper()

	ps, err := n.Peers()
	require.NoError(t, err)

	for _, p := range ps {
		if p.Name == name {
			return p
		}
	}

	t.Fatalf("%s does not know %s", n.Name, name)

	return Peer{}
}

func TestTellReceiptsNegotiatedBothWays(t *testing.T) {
	a := newOwner(t, "alpha")
	b := enroll(t, a, "beta")

	session(t, a, b)

	require.True(t, peerNamed(t, a, "beta").TellReceipts, "the caller records the server's signed capability")
	require.True(t, peerNamed(t, b, "alpha").TellReceipts, "the server records the caller's signed capability")
}

// TestTellReceiptHeldForAPeerWithoutTheCapability: an older zmina says nothing of tell_receipts; it would type a
// receipt into its dispatcher as a plain line, so the receipt waits and the plain line goes.
func TestTellReceiptHeldForAPeerWithoutTheCapability(t *testing.T) {
	a := newOwner(t, "alpha")
	b := enroll(t, a, "beta")
	receipt := tell(t, a, "ops", "beta/ops", TellReceiptPrefix+`{"version":1,"kind":"tell_receipt"}`)
	plain := tell(t, a, "ops", "beta/ops", "plain compatible note")

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, server.SetDeadline(time.Now().Add(5*time.Second)))

	done := make(chan []Envelope, 1)
	go func() {
		defer func() { _ = server.Close() }()
		c := newConn(server)
		hi, err := c.expect("hello")
		if err != nil {
			done <- nil
			return
		}
		challenge, _ := nonce()
		// An older zmina: reports, no tell_receipts.
		_ = c.send(wire{Op: "hello", V: protocolVersion, Cert: &b.Cert, Nonce: challenge, Sig: ed25519.Sign(b.Key, helloMessage("server", hi.Nonce)), BodyMax: BodyMax, ReportsV1: true, CapsSig: ed25519.Sign(b.Key, capabilityMessage("server", hi.Nonce, BodyMax, true))})
		_, _ = c.expect("auth")
		_ = c.send(wire{Op: "ok"})
		var got []Envelope
		for {
			var w wire
			if err := c.dec.Decode(&w); err != nil {
				done <- got
				return
			}
			switch w.Op {
			case "push":
				got = append(got, w.Envs...)
				var ids []string
				for _, e := range w.Envs {
					ids = append(ids, e.ID)
				}
				_ = c.send(wire{Op: "ack", IDs: ids})
			case "pull":
				_ = c.send(wire{Op: "envs"})
			case "ack":
				_ = c.send(wire{Op: "ok"})
			case "bye":
				done <- got
				return
			}
		}
	}()

	_, _, problems, err := a.Call(client, "beta", (&inbox{}).deliver, time.Now)
	require.NoError(t, err)

	got := <-done
	require.Len(t, got, 1)
	require.Equal(t, plain.ID, got[0].ID)
	require.Contains(t, strings.Join(problems, ";"), "tell_receipts")
	require.False(t, peerNamed(t, a, "beta").TellReceipts)

	out, err := a.Outbox()
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, receipt.ID, out[0].ID)

	problem, err := a.PendingError(receipt.ID)
	require.NoError(t, err)
	require.Contains(t, problem, "tell_receipts")

	// Updated, the peer takes the same queued receipt.
	session(t, a, b)

	out, err = a.Outbox()
	require.NoError(t, err)
	require.Empty(t, out)
}

func TestTellReceiptsCapabilityNeedsItsOwnSignature(t *testing.T) {
	a := newOwner(t, "alpha")
	b := enroll(t, a, "beta")

	t.Run("client", func(t *testing.T) {
		client, server := net.Pipe()
		defer func() { _ = client.Close() }()
		require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))
		require.NoError(t, server.SetDeadline(time.Now().Add(5*time.Second)))

		done := make(chan error, 1)
		go func() { defer func() { _ = server.Close() }(); done <- a.Serve(server, (&inbox{}).deliver, time.Now) }()

		c := newConn(client)
		challenge, err := nonce()
		require.NoError(t, err)
		require.NoError(t, c.send(wire{Op: "hello", V: protocolVersion, Cert: &b.Cert, Nonce: challenge, BodyMax: BodyMax, ReportsV1: true, TellReceipts: true}))
		hi, err := c.expect("hello")
		require.NoError(t, err)
		// The reports signature, and one over the wrong nonce, do not grant tell_receipts.
		require.NoError(t, c.send(wire{Op: "auth", Sig: ed25519.Sign(b.Key, helloMessage("client", hi.Nonce)),
			CapsSig:         ed25519.Sign(b.Key, capabilityMessage("client", hi.Nonce, BodyMax, true)),
			TellReceiptsSig: ed25519.Sign(b.Key, tellReceiptCapabilityMessage("client", challenge, BodyMax))}))
		_, err = c.expect("ok")
		require.ErrorContains(t, err, "unauthenticated tell_receipts")
		require.Error(t, <-done)
	})

	t.Run("server", func(t *testing.T) {
		client, server := net.Pipe()
		defer func() { _ = client.Close() }()
		require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))
		require.NoError(t, server.SetDeadline(time.Now().Add(5*time.Second)))

		done := make(chan error, 1)
		go func() {
			defer func() { _ = server.Close() }()
			c := newConn(server)
			hi, err := c.expect("hello")
			if err != nil {
				done <- err
				return
			}
			challenge, _ := nonce()
			// Unsigned: the claim alone.
			done <- c.send(wire{Op: "hello", V: protocolVersion, Cert: &b.Cert, Nonce: challenge, BodyMax: BodyMax, ReportsV1: true, TellReceipts: true, Sig: ed25519.Sign(b.Key, helloMessage("server", hi.Nonce)), CapsSig: ed25519.Sign(b.Key, capabilityMessage("server", hi.Nonce, BodyMax, true))})
		}()

		_, _, _, err := a.Call(client, "beta", (&inbox{}).deliver, time.Now)
		require.ErrorContains(t, err, "unauthenticated tell_receipts")
		require.NoError(t, <-done)
	})
}

func TestTellReceiptsNotTakenFromGossip(t *testing.T) {
	a := newOwner(t, "alpha")
	b := enroll(t, a, "beta")
	c := enroll(t, a, "gamma")

	require.NoError(t, a.metPeer(b.Cert, []Peer{{Name: c.Name, Cert: &c.Cert, Direct: true, TellReceipts: true}},
		Peer{BodyMax: BodyMax}, time.Now()))
	require.False(t, peerNamed(t, a, "gamma").TellReceipts, "a told node's word does not authenticate it")

	// A session without the capability takes back what an earlier one granted.
	require.NoError(t, a.metPeer(b.Cert, nil, Peer{BodyMax: BodyMax, TellReceipts: true}, time.Now()))
	require.True(t, peerNamed(t, a, "beta").TellReceipts)
	require.NoError(t, a.metPeer(b.Cert, nil, Peer{BodyMax: BodyMax}, time.Now()))
	require.False(t, peerNamed(t, a, "beta").TellReceipts)
}
