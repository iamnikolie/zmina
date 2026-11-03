package record

import (
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
	"time"
)

func TestRoleRecordIdempotencyRevisionAndCorruption(t *testing.T) {
	dir := t.TempDir()
	d := Document{Version: 1, Kind: "result", Task: "T-1", Role: "support", At: time.Now(), Shape: crew.HandoffShape{Sections: []crew.Section{{Name: "draft", Guidance: "Proposed answer"}}}, Values: map[string]string{"draft": "Answer\nwith formatting"}}
	ref, err := Save(dir, d)
	require.NoError(t, err)
	d.At = d.At.Add(time.Hour)
	again, err := Save(dir, d)
	require.NoError(t, err)
	assert.Equal(t, ref, again)
	got, err := Load(dir, "result", "T-1", ref.Revision)
	require.NoError(t, err)
	assert.Equal(t, d.Values, got.Values)
	d.Values["draft"] = "Revised answer"
	next, err := Save(dir, d)
	require.NoError(t, err)
	assert.NotEqual(t, ref.Revision, next.Revision)
	d.Values["commit"] = "not in this role's shape"
	_, err = Save(dir, d)
	require.ErrorContains(t, err, "unknown section")
	delete(d.Values, "commit")
	delete(d.Values, "draft")
	_, err = Save(dir, d)
	require.ErrorContains(t, err, "missing section")
	p, err := path(dir, "result", "T-1", next.Revision)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, []byte("broken"), 0600))
	_, err = Load(dir, "result", "T-1", next.Revision)
	require.Error(t, err)
	_, err = Load(dir, "result", "../x", ref.Revision)
	require.Error(t, err)
}
