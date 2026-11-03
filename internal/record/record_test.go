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

func TestResolveAndShortName(t *testing.T) {
	dir := t.TempDir()
	d := Document{Version: 1, Kind: "result", Task: "T-1", Role: "dev", Shape: crew.HandoffShape{Sections: []crew.Section{{Name: "summary"}}}, Values: map[string]string{"summary": "one"}}
	ref, err := Save(dir, d)
	require.NoError(t, err)

	got, err := Resolve(dir, "result", "T-1", ref.Revision[:8])
	require.NoError(t, err)
	assert.Equal(t, ref.Revision, got)
	got, err = Resolve(dir, "result", "T-1", ref.Revision)
	require.NoError(t, err)
	assert.Equal(t, ref.Revision, got)

	_, err = Resolve(dir, "result", "T-1", ref.Revision[:7])
	require.ErrorContains(t, err, "8 or more")
	_, err = Resolve(dir, "result", "T-1", "../../x")
	require.ErrorContains(t, err, "invalid revision")
	_, err = Resolve(dir, "result", "T-2", ref.Revision[:12])
	require.ErrorContains(t, err, "no result revision")

	// Two files that share a start: the short form grows until it names one, and a short prefix is ambiguous.
	twin := ref.Revision[:20] + "0000000000000000000000000000000000000000000f"
	if twin == ref.Revision {
		twin = twin[:63] + "e"
	}
	require.NoError(t, os.WriteFile(dir+"/results/T-1/"+twin+".json", []byte("{}"), 0o600))
	_, err = Resolve(dir, "result", "T-1", ref.Revision[:12])
	require.ErrorContains(t, err, "names 2 results")

	revs := []string{ref.Revision, twin}
	short := Short(revs, ref.Revision)
	assert.Greater(t, len(short), 20)
	assert.True(t, len(short) < 64 || twin == ref.Revision)
	assert.Equal(t, ref.Revision[:ShortLen], Short([]string{ref.Revision}, ref.Revision))
}
