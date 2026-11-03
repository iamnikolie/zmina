package mesh

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"net"
	"strings"
	"testing"
	"time"
)

func TestTypedReportNotSentToDowngradedPeer(t *testing.T) {
	a := newOwner(t, "alpha")
	b := enroll(t, a, "beta")
	require.NoError(t, a.metCapabilities(b.Cert, nil, BodyMax, true, time.Now()))
	hash := func(v any) string { raw, _ := json.Marshal(v); return fmt.Sprintf("%x", sha256.Sum256(raw)) }
	rev := hash(struct {
		Body, Task string
		Status     bool
		Kind       string
		Record     any
	}{Body: "Ready"})
	r := map[string]any{"version": 1, "id": hash([]string{"alpha/ops", "beta/ops", "review"}), "key": "review", "revision": rev, "sequence": 1, "from": "alpha/ops", "to": "beta/ops", "body": "Ready", "at": time.Now().UTC()}
	raw, err := json.Marshal(map[string]any{"version": 1, "kind": "report", "report": r})
	require.NoError(t, err)
	tell(t, a, "ops", "beta/ops", "[zmina-report-v1]"+string(raw))
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
		_ = c.send(wire{Op: "hello", V: protocolVersion, Cert: &b.Cert, Nonce: challenge, Sig: ed25519.Sign(b.Key, helloMessage("server", hi.Nonce)), BodyMax: OldBodyMax})
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
	_, _, _, err = a.Call(client, "beta", (&inbox{}).deliver, time.Now)
	require.NoError(t, err)
	got := <-done
	peers, err := a.Peers()
	require.NoError(t, err)
	require.Len(t, peers, 1)
	require.False(t, peers[0].ReportsV1)
	out, err := a.Outbox()
	require.NoError(t, err)
	t.Logf("peer reports_v1=%v; delivered envelopes=%d; pending envelopes=%d", peers[0].ReportsV1, len(got), len(out))
	for _, e := range got {
		require.False(t, strings.HasPrefix(e.Body, "[zmina-report-v1]"), "typed reports must remain pending when the current handshake no longer supports them")
	}
	require.Len(t, got, 1)
	require.Equal(t, "plain compatible note", got[0].Body)
	require.Len(t, out, 1)
	problem, err := a.PendingError(out[0].ID)
	require.NoError(t, err)
	require.Contains(t, problem, "reports_v1")
	a, err = Open(a.Dir)
	require.NoError(t, err)
	problem, err = a.PendingError(out[0].ID)
	require.NoError(t, err)
	require.Contains(t, problem, "reports_v1", "pending reason survives restart")
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

func TestTypedReportPullUsesCurrentHandshakeRatherThanCachedSupport(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reports bool
		cap     int
		problem string
	}{
		{"legacy", false, OldBodyMax, "reports_v1"}, {"smaller-cap", true, 40, "negotiated cap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newOwner(t, "alpha")
			b := enroll(t, a, "beta")
			require.NoError(t, a.metCapabilities(b.Cert, nil, BodyMax, true, time.Now()))
			typed := tell(t, a, "ops", "beta/ops", TypedReportPrefix+`{"version":1,"kind":"receipt"}`)
			plain := tell(t, a, "ops", "beta/ops", "plain note")
			client, server := net.Pipe()
			defer func() { _ = client.Close() }()
			require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))
			require.NoError(t, server.SetDeadline(time.Now().Add(5*time.Second)))
			done := make(chan error, 1)
			go func() { defer func() { _ = server.Close() }(); done <- a.Serve(server, (&inbox{}).deliver, time.Now) }()
			c := newConn(client)
			challenge, err := nonce()
			require.NoError(t, err)
			require.NoError(t, c.send(wire{Op: "hello", V: protocolVersion, Cert: &b.Cert, Nonce: challenge, BodyMax: tc.cap, ReportsV1: tc.reports}))
			hi, err := c.expect("hello")
			require.NoError(t, err)
			require.NoError(t, c.send(wire{Op: "auth", Sig: ed25519.Sign(b.Key, helloMessage("client", hi.Nonce)), CapsSig: ed25519.Sign(b.Key, capabilityMessage("client", hi.Nonce, tc.cap, true))}))
			_, err = c.expect("ok")
			require.NoError(t, err)
			require.NoError(t, c.send(wire{Op: "pull"}))
			got, err := c.expect("envs")
			require.NoError(t, err)
			require.Len(t, got.Envs, 1)
			require.Equal(t, plain.ID, got.Envs[0].ID)
			require.Contains(t, got.Err, tc.problem)
			require.NoError(t, c.send(wire{Op: "ack", IDs: []string{plain.ID}}))
			_, err = c.expect("ok")
			require.NoError(t, err)
			require.NoError(t, c.send(wire{Op: "bye"}))
			require.NoError(t, <-done)
			out, err := a.Outbox()
			require.NoError(t, err)
			require.Len(t, out, 1)
			require.Equal(t, typed.ID, out[0].ID)
		})
	}
}

func TestTypedReportCapAndRelayUseConnectionSnapshot(t *testing.T) {
	a := newOwner(t, "alpha")
	env := tell(t, a, "ops", "gamma/ops", TypedReportPrefix+strings.Repeat("x", 150))
	// Cached recipient/relay capabilities cannot override the connection's smaller cap or missing support.
	book := []Peer{{Name: "gamma", Through: []string{"beta"}, ReportsV1: true, BodyMax: BodyMax}, {Name: "beta", ReportsV1: true, BodyMax: BodyMax}}
	for _, peer := range []Peer{{Name: "beta", BodyMax: BodyMax}, {Name: "beta", ReportsV1: true, BodyMax: 100}} {
		out, problems := a.handFor(peer, book)
		require.Empty(t, out)
		require.Len(t, problems, 1)
		problem, err := a.PendingError(env.ID)
		require.NoError(t, err)
		require.NotEmpty(t, problem)
	}
	out, problems := a.handFor(Peer{Name: "beta", ReportsV1: true, BodyMax: len(env.Body)}, book)
	require.Empty(t, problems)
	require.Len(t, out, 1, "exact negotiated byte cap is accepted")
	problem, err := a.PendingError(env.ID)
	require.NoError(t, err)
	require.Empty(t, problem)
}
