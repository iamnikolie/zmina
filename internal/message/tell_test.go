package message

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sentLine(t *testing.T, s TellStore, to string, at time.Time) TellOut {
	t.Helper()

	o := TellOut{ID: "alpha-20261105T113347-0a1b2c3d", From: "alpha/ops", To: to, Head: "deploy steps", Sent: at}
	require.NoError(t, s.SaveOut(o))

	return o
}

func TestTellReceiptsApplyInOrderAndOnlyRise(t *testing.T) {
	s := TellStore{Dir: t.TempDir()}
	at := time.Date(2026, 11, 5, 11, 33, 47, 0, time.UTC)
	o := sentLine(t, s, "beta/acme", at)

	r := TellReceipt{ID: o.ID, Origin: "alpha/ops", Recipient: "beta/acme", Stage: "queued_dispatcher", At: at.Add(time.Second)}
	require.NoError(t, s.ApplyTellReceipt("beta/acme", "alpha/ops", r, at.Add(2*time.Second)))

	// read before submitted (receipts may cross): the stage rises to read and stays there.
	r.Stage, r.At = "read", at.Add(time.Minute)
	require.NoError(t, s.ApplyTellReceipt("beta/acme", "alpha/ops", r, at.Add(61*time.Second)))
	r.Stage, r.At = "submitted", at.Add(30*time.Second)
	require.NoError(t, s.ApplyTellReceipt("beta/acme", "alpha/ops", r, at.Add(62*time.Second)))

	// A second copy keeps the first time.
	r.Stage, r.At = "queued_dispatcher", at.Add(time.Hour)
	require.NoError(t, s.ApplyTellReceipt("beta/acme", "alpha/ops", r, at.Add(time.Hour)))

	got, err := s.LoadOut(o.ID)
	require.NoError(t, err)
	assert.Equal(t, "read", got.Stage)
	assert.Equal(t, "beta/acme", got.Recipient)
	assert.Equal(t, map[string]time.Time{
		"queued_dispatcher": at.Add(time.Second), "submitted": at.Add(30 * time.Second), "read": at.Add(time.Minute),
	}, got.Stages)
	assert.Equal(t, at.Add(2*time.Second), got.Seen["queued_dispatcher"])
}

func TestTellReceiptFromTheWrongNodeRefused(t *testing.T) {
	s := TellStore{Dir: t.TempDir()}
	at := time.Now().UTC()
	o := sentLine(t, s, "beta", at) // node only: its default crew answers

	r := TellReceipt{ID: o.ID, Origin: "alpha/ops", Recipient: "gamma/acme", Stage: "read", At: at}
	require.ErrorContains(t, s.ApplyTellReceipt("gamma/acme", "alpha/ops", r, at), "sent to beta")
	require.ErrorIs(t, s.ApplyTellReceipt("gamma/acme", "alpha/ops", r, at), ErrBadTellReceipt)

	// Signed by one crew, naming another as the recipient.
	r.Recipient = "beta/default"
	require.ErrorContains(t, s.ApplyTellReceipt("gamma/acme", "alpha/ops", r, at), "names")

	// Delivered to another crew of the sender's node.
	require.ErrorContains(t, s.ApplyTellReceipt("beta/default", "alpha/box-ops", r, at), "did not send")

	// The first crew to answer pins the recipient.
	r.Stage = "queued_dispatcher"
	require.NoError(t, s.ApplyTellReceipt("beta/default", "alpha/ops", r, at))
	r.Recipient, r.Stage = "beta/other", "read"
	require.ErrorContains(t, s.ApplyTellReceipt("beta/other", "alpha/ops", r, at), "took")

	r.Recipient, r.Stage = "beta/default", "relayed"
	require.ErrorContains(t, s.ApplyTellReceipt("beta/default", "alpha/ops", r, at), "unknown receipt stage")

	got, err := s.LoadOut(o.ID)
	require.NoError(t, err)
	assert.Equal(t, "queued_dispatcher", got.Stage)

	r.ID, r.Stage = "alpha-unknown", "read"
	require.ErrorIs(t, s.ApplyTellReceipt("beta/default", "alpha/ops", r, at), ErrUnknownTell)
}

func TestTellReceiptWire(t *testing.T) {
	r := TellReceipt{ID: "alpha-1", Origin: "alpha/ops", Recipient: "beta/acme", Stage: "submitted", At: time.Now().UTC()}
	body, err := Encode(Wire{TellReceipt: &r})
	require.NoError(t, err)
	assert.True(t, mesh.IsTellReceipt(body))
	assert.Zero(t, mesh.ReportWireVersion(body), "not a report: the report gates do not apply")

	w, typed, err := Decode(body)
	require.NoError(t, err)
	assert.True(t, typed)
	assert.Equal(t, "tell_receipt", w.Kind)
	assert.Equal(t, r, *w.TellReceipt)

	// A tell receipt is never smuggled in a report body, nor a report in a tell receipt body.
	_, typed, err = Decode(Prefix + `{"version":1,"kind":"tell_receipt","tell_receipt":{"id":"x"}}`)
	assert.True(t, typed)
	require.Error(t, err)
	_, _, err = Decode(mesh.TellReceiptPrefix + `{"version":1,"kind":"report","report":{"id":"x"}}`)
	require.Error(t, err)
	_, _, err = Decode(mesh.TellReceiptPrefix + `{"version":2,"kind":"tell_receipt","tell_receipt":{"id":"x"}}`)
	require.Error(t, err)
}

func TestTellStoreReadsAndPrunes(t *testing.T) {
	s := TellStore{Dir: t.TempDir()}
	now := time.Now().UTC()

	old := TellOut{ID: "alpha-old", From: "alpha/ops", To: "beta/acme", Sent: now.Add(-31 * 24 * time.Hour)}
	require.NoError(t, s.SaveOut(old))
	fresh := sentLine(t, s, "beta/acme", now)

	in := TellIn{ID: "beta-1", Origin: "beta/acme", Recipient: "alpha/ops", At: now}
	require.NoError(t, s.SaveIn(in))
	got, err := s.LoadIn(in.ID)
	require.NoError(t, err)
	assert.NotNil(t, got.Intents)

	// A half-written file is skipped, not an error.
	require.NoError(t, os.WriteFile(filepath.Join(s.Dir, "out", "broken.json"), []byte("{"), 0o600))

	require.NoError(t, s.PruneOut(now, TellKeep))
	all, err := s.AllOut()
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, fresh.ID, all[0].ID)

	_, err = s.LoadOut("../escape")
	require.Error(t, err)
	_, err = s.LoadOut(strings.Repeat("x", 300))
	require.Error(t, err)

	require.NoError(t, s.RemoveIn(in.ID))
	require.NoError(t, s.RemoveIn(in.ID))
	_, err = s.LoadIn(in.ID)
	require.ErrorIs(t, err, os.ErrNotExist)
}
