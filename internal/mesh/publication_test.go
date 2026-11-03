package mesh

import (
	"crypto/ed25519"
	"github.com/stretchr/testify/require"
	"net"
	"strings"
	"testing"
	"time"
)

func TestPublicationRequiresV2EvenForAnAuthenticatedV1Receiver(t *testing.T) {
	a := newOwner(t, "alpha")
	b := enroll(t, a, "beta")
	require.NoError(t, a.metCapabilities(b.Cert, nil, BodyMax, true, time.Now()))
	// This is a persisted publication from before reports_v2 negotiation was introduced.
	tell(t, a, "ops", "beta/ops", `[zmina-report-v1]{"version":1,"kind":"report","report":{"version":1,"id":"39d35e58fb848e45e792cf179f96780f922700d01d73efd0bb5f9b1f44b17a0f","key":"review","revision":"ff6f5e1b7cfb8972622169ead9f7822129849929b4095ac5ff0cfa2b8960b192","content_hash":"284084c3b2ec44ba31cebb144124eaa15a11c837a5e5711fa1a8246a824771c5","sequence":1,"from":"alpha/ops","to":"beta/ops","body":"Ready","at":"2026-11-02T19:40:00Z"}}`)
	queued, _ := a.Outbox()
	// Current wire v2 must be protected as well as the old queued v1-prefixed publication.
	native := strings.ReplaceAll(queued[0].Body, `"version":1`, `"version":2`)
	native = strings.Replace(native, TypedReportPrefix, TypedPublicationPrefix, 1)
	tell(t, a, "ops", "beta/ops", native)
	tell(t, a, "ops", "beta/ops", "plain compatible note")
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
	_, _, _, err := a.Call(client, "beta", (&inbox{}).deliver, time.Now)
	require.NoError(t, err)
	got := <-done
	peers, err := a.Peers()
	require.NoError(t, err)
	require.Len(t, peers, 1)
	require.True(t, peers[0].ReportsV1)
	out, err := a.Outbox()
	require.NoError(t, err)
	t.Logf("peer reports_v1=%v; delivered envelopes=%d; pending envelopes=%d", peers[0].ReportsV1, len(got), len(out))
	for _, e := range got {
		require.False(t, strings.HasPrefix(e.Body, "[zmina-report-v1]"), "an authenticated v1 peer must never receive a publication it would refuse and permanently ACK")
	}
	require.Len(t, got, 1)
	require.Equal(t, "plain compatible note", got[0].Body)
	require.Len(t, out, 2)
	problem, err := a.PendingError(out[0].ID)
	require.NoError(t, err)
	require.Contains(t, problem, "reports_v2")
	a, err = Open(a.Dir)
	require.NoError(t, err)
	problem, err = a.PendingError(out[0].ID)
	require.NoError(t, err)
	require.Contains(t, problem, "reports_v2", "pending reason survives restart")
	// An upgraded peer can drain the same persisted envelope; no new report is required.
	nextClient, nextServer := net.Pipe()
	require.NoError(t, nextClient.SetDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, nextServer.SetDeadline(time.Now().Add(5*time.Second)))
	serveDone := make(chan error, 1)
	go func() {
		defer func() { _ = nextServer.Close() }()
		serveDone <- b.Serve(nextServer, (&inbox{}).deliver, time.Now)
	}()
	_, _, _, err = a.Call(nextClient, "beta", (&inbox{}).deliver, time.Now)
	require.NoError(t, err)
	require.NoError(t, nextClient.Close())
	require.NoError(t, <-serveDone)
	pending, err := a.Outbox()
	require.NoError(t, err)
	require.Empty(t, pending)
	problem, err = a.PendingError(out[0].ID)
	require.NoError(t, err)
	require.Empty(t, problem)

}

func TestPublicationCapabilitiesRequireTheirOwnSignature(t *testing.T) {
	a := newOwner(t, "alpha")
	b := enroll(t, a, "beta")
	for _, side := range []string{"client", "server"} {
		t.Run(side, func(t *testing.T) {
			client, server := net.Pipe()
			defer func() { _ = client.Close() }()
			require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))
			require.NoError(t, server.SetDeadline(time.Now().Add(5*time.Second)))
			done := make(chan error, 1)
			if side == "client" {
				go func() { defer func() { _ = server.Close() }(); done <- a.Serve(server, (&inbox{}).deliver, time.Now) }()
				c := newConn(client)
				challenge, err := nonce()
				require.NoError(t, err)
				require.NoError(t, c.send(wire{Op: "hello", V: protocolVersion, Cert: &b.Cert, Nonce: challenge, BodyMax: BodyMax, ReportsV1: true, ReportsV2: true}))
				hi, err := c.expect("hello")
				require.NoError(t, err)
				// Ordinary and v1 signatures cannot grant publication support.
				require.NoError(t, c.send(wire{Op: "auth", Sig: ed25519.Sign(b.Key, helloMessage("client", hi.Nonce)), CapsSig: ed25519.Sign(b.Key, capabilityMessage("client", hi.Nonce, BodyMax, true))}))
				_, err = c.expect("ok")
				require.ErrorContains(t, err, "unauthenticated reports_v2")
				require.Error(t, <-done)
			} else {
				go func() {
					defer func() { _ = server.Close() }()
					c := newConn(server)
					hi, err := c.expect("hello")
					if err != nil {
						done <- err
						return
					}
					challenge, _ := nonce()
					done <- c.send(wire{Op: "hello", V: protocolVersion, Cert: &b.Cert, Nonce: challenge, BodyMax: BodyMax, ReportsV1: true, ReportsV2: true, Sig: ed25519.Sign(b.Key, helloMessage("server", hi.Nonce)), CapsSig: ed25519.Sign(b.Key, capabilityMessage("server", hi.Nonce, BodyMax, true))})
				}()
				_, _, _, err := a.Call(client, "beta", (&inbox{}).deliver, time.Now)
				require.ErrorContains(t, err, "unauthenticated reports_v2")
				require.NoError(t, <-done)
			}
		})
	}
}

func TestPublicationReceiptPullStaysQueuedForV1Peer(t *testing.T) {
	a := newOwner(t, "alpha")
	b := enroll(t, a, "beta")
	publication := tell(t, a, "ops", "beta/ops", TypedPublicationPrefix+`{"version":2,"kind":"receipt","receipt":{"report_version":2}}`)
	plain := tell(t, a, "ops", "beta/ops", "plain")
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, server.SetDeadline(time.Now().Add(5*time.Second)))
	done := make(chan error, 1)
	go func() { defer func() { _ = server.Close() }(); done <- a.Serve(server, (&inbox{}).deliver, time.Now) }()
	c := newConn(client)
	challenge, err := nonce()
	require.NoError(t, err)
	require.NoError(t, c.send(wire{Op: "hello", V: protocolVersion, Cert: &b.Cert, Nonce: challenge, BodyMax: BodyMax, ReportsV1: true}))
	hi, err := c.expect("hello")
	require.NoError(t, err)
	require.NoError(t, c.send(wire{Op: "auth", Sig: ed25519.Sign(b.Key, helloMessage("client", hi.Nonce)), CapsSig: ed25519.Sign(b.Key, capabilityMessage("client", hi.Nonce, BodyMax, true))}))
	_, err = c.expect("ok")
	require.NoError(t, err)
	require.NoError(t, c.send(wire{Op: "pull"}))
	got, err := c.expect("envs")
	require.NoError(t, err)
	require.Len(t, got.Envs, 1)
	require.Equal(t, plain.ID, got.Envs[0].ID)
	require.Contains(t, got.Err, "reports_v2")
	// A peer cannot ACK an envelope the drain guard did not hand it.
	require.NoError(t, c.send(wire{Op: "ack", IDs: []string{plain.ID, publication.ID}}))
	_, err = c.expect("ok")
	require.NoError(t, err)
	require.NoError(t, c.send(wire{Op: "bye"}))
	require.NoError(t, <-done)
	out, err := a.Outbox()
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, publication.ID, out[0].ID)
}

func TestPublicationRelayCannotUseCachedV2OrGossipAsAuthority(t *testing.T) {
	a := newOwner(t, "alpha")
	b := enroll(t, a, "beta")
	c := enroll(t, a, "gamma")
	require.NoError(t, a.metCapabilities(b.Cert, []Peer{{Name: c.Name, Cert: &c.Cert, Direct: true, ReportsV1: true, ReportsV2: true}}, BodyMax, true, time.Now(), true))
	ps, err := a.Peers()
	require.NoError(t, err)
	for _, p := range ps {
		if p.Name == c.Name {
			require.False(t, p.ReportsV2, "gossip does not authenticate a format")
		}
	}
	e := tell(t, a, "ops", "gamma/ops", TypedPublicationPrefix+`{"version":2}`)
	hand, problems := a.handFor(Peer{Name: "beta", ReportsV1: true, BodyMax: BodyMax}, ps)
	require.Empty(t, hand)
	require.Contains(t, strings.Join(problems, ";"), "reports_v2")
	hand, problems = a.handFor(Peer{Name: "beta", ReportsV1: true, ReportsV2: true, BodyMax: len(e.Body)}, ps)
	require.Len(t, hand, 1)
	require.Empty(t, problems)
}
