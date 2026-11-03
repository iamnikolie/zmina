package cmd

import (
	"fmt"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/message"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reportHome(t *testing.T) (string, string) {
	t.Helper()
	uiHome(t)
	from, to := crewPathOf(t, "default"), crewPathOf(t, "ops")
	_, _, err := zm(t, from, "node", "init", "--name", "local", "--owner")
	require.NoError(t, err)
	return from, to
}
func msgStore(t *testing.T, file string) message.Store {
	t.Helper()
	c, err := crew.Load(file)
	require.NoError(t, err)
	return message.Store{Dir: filepath.Join(c.StateDir(), "messages")}
}
func TestTypedLocalReportReceiptsBodyAndLogicalDedup(t *testing.T) {
	from, to := reportHome(t)
	body := "First line\n\nОтвет для клиента.\n" + strings.Repeat("Full text. ", 100)
	file := filepath.Join(t.TempDir(), "report.txt")
	require.NoError(t, os.WriteFile(file, []byte(body), 0600))
	id := message.Identity("local/default", "local/ops", "T-1/review")
	for i := 0; i < 2; i++ {
		_, _, err := zm(t, from, "tell", "local/ops", "--report-key", "T-1/review", "--file", file)
		require.NoError(t, err)
	}
	m, err := msgStore(t, from).Load(id)
	require.NoError(t, err)
	assert.Equal(t, "queued_dispatcher", m.Revisions[m.Latest].Stage)
	assert.Equal(t, 2, m.Revisions[m.Latest].Count)
	dest, err := msgStore(t, to).Load(id)
	require.NoError(t, err)
	assert.Equal(t, body, dest.Revisions[dest.Latest].Report.Body)
	assert.Equal(t, 1, dest.Revisions[dest.Latest].Count)
	_, _, err = zm(t, to, "message", "relayed", id)
	require.ErrorContains(t, err, "mark the report read")
	_, _, err = zm(t, to, "message", "read", id)
	require.NoError(t, err)
	m, err = msgStore(t, from).Load(id)
	require.NoError(t, err)
	assert.Equal(t, "read", m.Revisions[m.Latest].Stage)
	assert.NotEmpty(t, m.Revisions[m.Latest].RemoteObserved)
	_, _, err = zm(t, to, "message", "relayed", id)
	require.NoError(t, err)
	m, err = msgStore(t, from).Load(id)
	require.NoError(t, err)
	assert.Equal(t, "relayed", m.Revisions[m.Latest].Stage)
	_, _, err = zm(t, from, "message", "retry", id)
	require.NoError(t, err)
	dest, err = msgStore(t, to).Load(id)
	require.NoError(t, err)
	assert.Equal(t, 2, dest.Revisions[dest.Latest].Count)
	c, err := crew.Load(to)
	require.NoError(t, err)
	all, err := taskStore(c).All()
	require.NoError(t, err)
	assert.Empty(t, all, "a report never creates a task")
	st, err := shift.Read(c.StateDir())
	require.NoError(t, err)
	assert.Empty(t, st.Signals, "explicit consumption clears only the report notice")
	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	out, err := me.Outbox()
	require.NoError(t, err)
	assert.Empty(t, out, "same-node envelopes use the same verified replay path and are acknowledged")
}
func TestTypedReportQueuePressureAndCommittedStages(t *testing.T) {
	from, to := reportHome(t)
	a := &app{crew: to, out: io.Discard, err: io.Discard}
	require.NoError(t, a.withState(func(s *shift.Shift) error {
		for i := 0; i < 20; i++ {
			_, err := s.Note("owner", fmt.Sprintf("request %d", i), false)
			if err != nil {
				return err
			}
		}
		return nil
	}))
	_, _, err := zm(t, from, "tell", "local/ops", "Report ready", "--report-key", "status")
	require.NoError(t, err)
	id := message.Identity("local/default", "local/ops", "status")
	m, err := msgStore(t, from).Load(id)
	require.NoError(t, err)
	assert.Equal(t, "received", m.Revisions[m.Latest].Stage, "queue full must not overstate dispatcher acceptance")
	require.NoError(t, a.withState(func(s *shift.Shift) error { s.State.Signals = s.State.Signals[1:]; return a.reconcileMessages(s) }))
	m, err = msgStore(t, to).Load(id)
	require.NoError(t, err)
	assert.Equal(t, "received", m.Revisions[m.Latest].Stage, "queue stage not published until state commit is observed")
	require.NoError(t, a.withState(a.reconcileMessages))
	require.NoError(t, a.pumpLocalReports())
	m, err = msgStore(t, from).Load(id)
	require.NoError(t, err)
	assert.Equal(t, "queued_dispatcher", m.Revisions[m.Latest].Stage)
	c, err := crew.Load(to)
	require.NoError(t, err)
	st, err := shift.Read(c.StateDir())
	require.NoError(t, err)
	assert.Len(t, st.Signals, 20)
}
func TestReportCapabilityCapsAndMalformedBodies(t *testing.T) {
	require.ErrorContains(t, validateReportPeer(mesh.Peer{Name: "old"}, 1), "reports_v1")
	p := mesh.Peer{Name: "new", ReportsV1: true, BodyMax: 100}
	require.NoError(t, validateReportPeer(p, 100))
	require.Error(t, validateReportPeer(p, 101))
	from, _ := reportHome(t)
	_, _, err := zm(t, from, "tell", "local/ops", strings.Repeat("x", 16000), "--report-key", "large")
	require.ErrorContains(t, err, "negotiated limit")
	_, _, err = zm(t, from, "tell", "local", "body", "--report-key", "key")
	require.ErrorContains(t, err, "explicit node/crew")
}

func TestTypedReportReconcilesCrashBeforeOutboundIntentAndDestinationInbox(t *testing.T) {
	from, to := reportHome(t)
	r := message.Report{Version: 1, Key: "crash", From: "local/default", To: "local/ops", Body: "durable body", Sequence: 1, At: time.Now().UTC()}
	r.ID = message.Identity(r.From, r.To, r.Key)
	r.Revision = message.Content(r)
	_, _, err := msgStore(t, from).Add(r)
	require.NoError(t, err)
	origin := &app{crew: from, out: io.Discard, err: io.Discard}
	require.NoError(t, origin.withState(origin.reconcileMessages))
	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	out, err := me.Outbox()
	require.NoError(t, err)
	require.Len(t, out, 1)
	// A verified receive persisted its report, then crashed before its full inbox body.
	_, _, err = msgStore(t, to).Add(r)
	require.NoError(t, err)
	dest := &app{crew: to, out: io.Discard, err: io.Discard}
	require.NoError(t, dest.withState(dest.reconcileMessages))
	require.NoError(t, dest.withState(func(s *shift.Shift) error {
		entry, err := s.Inbox.Get(reportInbox(r))
		require.NoError(t, err)
		assert.Equal(t, r.Body, entry.Body)
		assert.Len(t, s.State.Signals, 1)
		return nil
	}))
	require.NoError(t, origin.pumpLocalReports())
	m, err := msgStore(t, from).Load(r.ID)
	require.NoError(t, err)
	assert.Equal(t, "queued_dispatcher", m.Revisions[r.Revision].Stage)
}

func TestReportStatusReturningToHistoricalContentIsANewPublication(t *testing.T) {
	from, to := reportHome(t)
	id := message.Identity("local/default", "local/ops", "availability")
	var first string
	for i, body := range []string{"idle", "working", "idle"} {
		_, _, err := zm(t, from, "tell", "local/ops", "--report-key", "availability", "--status", body)
		require.NoError(t, err)
		m, err := msgStore(t, from).Load(id)
		require.NoError(t, err)
		if i == 0 {
			first = m.Latest
		}
	}
	for _, file := range []string{from, to} {
		m, err := msgStore(t, file).Load(id)
		require.NoError(t, err)
		assert.Equal(t, "idle", m.Revisions[m.Latest].Report.Body)
		assert.Equal(t, uint64(3), m.Revisions[m.Latest].Report.Sequence)
		assert.Len(t, m.Revisions, 3, "returning content must preserve immutable publication history")
	}
	// A repeated current publication coalesces; a delayed retry of the first cannot win.
	_, _, err := zm(t, from, "tell", "local/ops", "--report-key", "availability", "--status", "idle")
	require.NoError(t, err)
	_, _, err = zm(t, from, "message", "retry", id, "--revision", first)
	require.NoError(t, err)
	for _, file := range []string{from, to} {
		m, err := msgStore(t, file).Load(id)
		require.NoError(t, err)
		assert.Equal(t, uint64(3), m.Revisions[m.Latest].Report.Sequence)
		assert.False(t, m.Revisions[m.Latest].Superseded)
		assert.True(t, m.Revisions[first].Superseded)
		assert.Len(t, m.Revisions, 3)
	}
}

func TestTransportFailureClearsAfterReceiptWithoutReconciliation(t *testing.T) {
	from, _ := reportHome(t)
	_, _, err := zm(t, from, "tell", "local/ops", "--report-key", "status", "ready")
	require.NoError(t, err)
	store := msgStore(t, from)
	m, err := store.Load(message.Identity("local/default", "local/ops", "status"))
	require.NoError(t, err)
	v := m.Revisions[m.Latest]
	// tellReport stores Sync.Problems verbatim, with no transport: prefix.
	id := v.Intents["report"].Envelope.ID
	v.LastError = fmt.Sprintf("report %s pending: peer has no authenticated reports_v1 on this connection; update the peer and sync (plain notes remain compatible)", id)
	require.NoError(t, store.Save(m))
	// Capability restored: ACK removes the pending file and a valid receipt advances stage.
	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	me.Unqueue([]string{id})
	require.NoError(t, store.ApplyReceipt("local/ops", "local/default", message.Receipt{ID: m.ID, Revision: m.Latest, Sequence: v.Report.Sequence, Origin: "local/default", Recipient: "local/ops", Stage: "read", At: time.Now()}, time.Now()))
	out, _, err := zm(t, from, "message", "show", m.ID)
	require.NoError(t, err)
	t.Log(out)
	require.NotContains(t, out, "pending:", "a successful receipt must not retain the recovered capability failure as a current pending error")
}

func TestTellFailureRecoveryReceiptAndShowSeparatesHistoryFromPending(t *testing.T) {
	from, _ := reportHome(t)
	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	require.NoError(t, me.UpdatePeers(func(peers []mesh.Peer) []mesh.Peer {
		// Previously negotiated support is cached, but this connection attempt is offline.
		return append(peers, mesh.Peer{Name: "beta", ReportsV1: true, ReportsV2: true, BodyMax: mesh.BodyMax, Addrs: []mesh.Addr{{Cmd: "exit 7"}}})
	}))
	_, _, err = zm(t, from, "tell", "beta/ops", "--report-key", "status", "ready")
	require.NoError(t, err)
	store := msgStore(t, from)
	m, err := store.Load(message.Identity("local/default", "beta/ops", "status"))
	require.NoError(t, err)
	v := m.Revisions[m.Latest]
	require.Equal(t, "queued_origin", v.Stage)
	require.NotEmpty(t, v.LastSyncError)
	require.False(t, v.LastSyncErrorAt.IsZero())
	require.Empty(t, v.LastError)
	out, _, err := zm(t, from, "message", "show", m.ID)
	require.NoError(t, err)
	require.Contains(t, out, "last failed sync (historical)")
	// Model the successful ACK followed by a matching read receipt before any ticker pass.
	me.Unqueue([]string{v.Intents["report"].Envelope.ID})
	require.NoError(t, store.ApplyReceipt("beta/ops", "local/default", message.Receipt{ReportVersion: 2, ID: m.ID, Revision: m.Latest, Sequence: v.Report.Sequence, Origin: "local/default", Recipient: "beta/ops", Stage: "read", At: time.Now()}, time.Now()))
	out, _, err = zm(t, from, "message", "show", m.ID)
	require.NoError(t, err)
	require.NotContains(t, out, "pending:")
	require.NotContains(t, out, "last failed sync")
	m, err = store.Load(m.ID)
	require.NoError(t, err)
	require.Empty(t, m.Revisions[m.Latest].LastError)
	require.NotEmpty(t, m.Revisions[m.Latest].LastSyncError)
}
