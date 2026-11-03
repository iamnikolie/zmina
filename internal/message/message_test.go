package message

import (
	"encoding/json"
	"fmt"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func report(body string, seq uint64) Report {
	r := Report{Version: 1, Key: "update", Body: body, Sequence: seq, From: "alpha/ops", To: "beta/ops", At: time.Now().UTC()}
	r.ID = Identity(r.From, r.To, r.Key)
	r.Revision = Content(r)
	return r
}
func TestReportRevisionsReplayAndReceiptBinding(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	r := report("First\nbody", 1)
	_, fresh, err := s.Add(r)
	require.NoError(t, err)
	assert.True(t, fresh)
	_, fresh, err = s.Add(r)
	require.NoError(t, err)
	assert.False(t, fresh)
	rec := Receipt{ID: r.ID, Revision: r.Revision, Sequence: r.Sequence, Origin: r.From, Recipient: r.To, Stage: "read", At: r.At.Add(-24 * time.Hour)}
	now := r.At.Add(time.Hour)
	require.NoError(t, s.ApplyReceipt(r.To, r.From, rec, now))
	rec.Stage = "received"
	require.NoError(t, s.ApplyReceipt(r.To, r.From, rec, now.Add(time.Hour)))
	m, err := s.Load(r.ID)
	require.NoError(t, err)
	v := m.Revisions[r.Revision]
	assert.Equal(t, "read", v.Stage)
	assert.Equal(t, now, v.Observed["read"])
	assert.Equal(t, rec.At, v.RemoteObserved["read"])
	require.Error(t, s.ApplyReceipt("intruder/ops", r.From, rec, now))
	rec.Sequence++
	require.Error(t, s.ApplyReceipt(r.To, r.From, rec, now))
	assert.Equal(t, 2, v.Count)
}
func TestDelayedStatusCanBeSupersededButRegularReportsStay(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	old := report("Idle", 1)
	old.Status = true
	old.Revision = Content(old)
	next := report("Working", 2)
	next.Status = true
	next.Supersedes = old.Revision
	next.Revision = Content(next)
	_, _, err := s.Add(next)
	require.NoError(t, err)
	m, _, err := s.Add(old)
	require.NoError(t, err)
	assert.Equal(t, next.Revision, m.Latest)
	assert.True(t, m.Revisions[old.Revision].Superseded)
	normal := report("Owner request stays", 3)
	m, _, err = s.Add(normal)
	require.NoError(t, err)
	assert.False(t, m.Revisions[normal.Revision].Superseded)
	corrupt := old
	corrupt.Body = "changed without a new revision"
	_, _, err = s.Add(corrupt)
	require.ErrorContains(t, err, "hash mismatch")
}

func TestPublicationAndLegacyWireContractsStayDistinct(t *testing.T) {
	legacy := report("Ready", 1)
	body, err := Encode(Wire{Kind: "report", Report: &legacy})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(body, Prefix))
	assert.Equal(t, 1, mesh.ReportWireVersion(body))
	pub := legacy
	pub.Version = 2
	pub.ContentHash = Content(pub)
	pub.Revision = Publication(pub)
	body, err = Encode(Wire{Kind: "report", Report: &pub})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(body, mesh.TypedPublicationPrefix))
	assert.Equal(t, 2, mesh.ReportWireVersion(body))
	decoded, handled, err := Decode(body)
	require.NoError(t, err)
	require.True(t, handled)
	require.NoError(t, decoded.Report.Validate())
	rec := Receipt{ReportVersion: 2, ID: pub.ID, Revision: pub.Revision}
	body, err = Encode(Wire{Kind: "receipt", Receipt: &rec})
	require.NoError(t, err)
	assert.Equal(t, 2, mesh.ReportWireVersion(body))
	// Earlier local publication files/envelopes remain readable, but cannot pass a v1-only drain.
	pub.Version = 1
	raw, err := json.Marshal(Wire{Version: 1, Kind: "report", Report: &pub})
	require.NoError(t, err)
	body = Prefix + string(raw)
	decoded, handled, err = Decode(body)
	require.NoError(t, err)
	require.True(t, handled)
	require.NoError(t, decoded.Report.Validate())
	assert.Equal(t, 2, mesh.ReportWireVersion(body))
	rec.ReportVersion = 1
	body, err = Encode(Wire{Kind: "receipt", Receipt: &rec})
	require.NoError(t, err)
	assert.Equal(t, 1, mesh.ReportWireVersion(body))
}

func TestReceivedLegacySourceErrorsMigrateWithoutClearingDestinationPressure(t *testing.T) {
	for _, origin := range []bool{false, true} {
		t.Run(fmt.Sprint(origin), func(t *testing.T) {
			s := Store{Dir: t.TempDir()}
			r := report("Ready", 1)
			m, _, err := s.Add(r)
			require.NoError(t, err)
			v := m.Revisions[m.Latest]
			require.NoError(t, Advance(v, "received", time.Now()))
			v.LastError = "old unscoped sync error"
			if origin {
				v.Intents["report"] = Intent{}
			}
			require.NoError(t, s.Save(m))
			m, err = s.Load(m.ID)
			require.NoError(t, err)
			if origin {
				require.Empty(t, m.Revisions[m.Latest].LastError)
				require.Equal(t, "old unscoped sync error", m.Revisions[m.Latest].LastSyncError)
			} else {
				require.Equal(t, "old unscoped sync error", m.Revisions[m.Latest].LastError)
			}
		})
	}
}
