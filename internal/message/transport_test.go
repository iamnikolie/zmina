package message

import (
	"net"
	"testing"
	"time"

	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignedPipeReportSurvivesLostACKRestartAndLogicalRetry(t *testing.T) {
	now := time.Now
	ad, bd := t.TempDir(), t.TempDir()
	_, err := mesh.Init(ad, "alpha")
	require.NoError(t, err)
	require.NoError(t, mesh.MakeOwner(ad, now()))
	req, err := mesh.Init(bd, "beta")
	require.NoError(t, err)
	grant, err := mesh.SignRequest(ad, req, now())
	require.NoError(t, err)
	require.NoError(t, mesh.Accept(bd, grant))
	a, err := mesh.Open(ad)
	require.NoError(t, err)
	b, err := mesh.Open(bd)
	require.NoError(t, err)
	origin, dest := Store{Dir: t.TempDir()}, Store{Dir: t.TempDir()}
	r := report("Full report\nПривет\n/path with spaces/result.txt", 1)
	_, _, err = origin.Add(r)
	require.NoError(t, err)
	body, err := Encode(Wire{Kind: "report", Report: &r})
	require.NoError(t, err)
	env, err := a.NewEnvelope("ops", mesh.Address{Node: "beta", Crew: "ops"}, body, now())
	require.NoError(t, err)
	require.NoError(t, a.Queue(env))
	// No connection: pending envelope survives a process restart.
	a, err = mesh.Open(ad)
	require.NoError(t, err)
	out, err := a.Outbox()
	require.NoError(t, err)
	require.Len(t, out, 1)
	notifications := 0
	receive := func(crew, from, id, body string, at time.Time) error {
		w, handled, err := Decode(body)
		require.NoError(t, err)
		require.True(t, handled)
		require.Equal(t, "alpha/ops", from)
		require.Equal(t, "ops", crew)
		m, fresh, err := dest.Add(*w.Report)
		if err != nil {
			return err
		}
		if fresh {
			notifications++
		}
		if err := Advance(m.Revisions[r.Revision], "received", now()); err != nil {
			return err
		}
		if err := dest.Save(m); err != nil {
			return err
		}
		rec := Receipt{ID: r.ID, Revision: r.Revision, Sequence: r.Sequence, Origin: r.From, Recipient: r.To, Stage: "received", At: now()}
		text, err := Encode(Wire{Kind: "receipt", Receipt: &rec})
		if err != nil {
			return err
		}
		e, err := b.NewEnvelope("ops", mesh.Address{Node: "alpha", Crew: "ops"}, text, now())
		if err != nil {
			return err
		}
		return b.Queue(e)
	}
	apply := func(crew, from, id, body string, at time.Time) error {
		w, _, err := Decode(body)
		if err != nil {
			return err
		}
		return origin.ApplyReceipt(from, "alpha/"+crew, *w.Receipt, now())
	}
	round := func(lose bool) error {
		client, server := net.Pipe()
		require.NoError(t, client.SetDeadline(now().Add(5*time.Second)))
		require.NoError(t, server.SetDeadline(now().Add(5*time.Second)))
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() { _ = server.Close() }()
			_ = b.Serve(server, func(c, f, i, s string, at time.Time) error {
				err := receive(c, f, i, s, at)
				if lose {
					_ = client.Close()
				} // durable receipt/body, but transport ACK is lost
				return err
			}, now)
		}()
		_, _, _, err := a.Call(client, "beta", apply, now)
		_ = client.Close()
		<-done
		return err
	}
	require.Error(t, round(true))
	a, err = mesh.Open(ad)
	require.NoError(t, err)
	b, err = mesh.Open(bd)
	require.NoError(t, err)
	require.NoError(t, round(false))
	assert.Equal(t, 1, notifications)
	m, err := origin.Load(r.ID)
	require.NoError(t, err)
	assert.Equal(t, "received", m.Revisions[r.Revision].Stage)
	// A new transport ID retries the same logical report without another notification.
	retry, err := a.NewEnvelope("ops", mesh.Address{Node: "beta", Crew: "ops"}, body, now())
	require.NoError(t, err)
	require.NoError(t, a.Queue(retry))
	require.NoError(t, round(false))
	assert.Equal(t, 1, notifications)
	m, err = dest.Load(r.ID)
	require.NoError(t, err)
	assert.Equal(t, r.Body, m.Revisions[r.Revision].Report.Body)
	assert.Equal(t, 2, m.Revisions[r.Revision].Count)
	peers, err := a.Peers()
	require.NoError(t, err)
	require.Len(t, peers, 1)
	assert.True(t, peers[0].ReportsV1)
	out, err = a.Outbox()
	require.NoError(t, err)
	assert.Empty(t, out)
}
