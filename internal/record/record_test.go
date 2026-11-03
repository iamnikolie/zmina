package record

import (
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"os"
	"strings"
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

func TestFindRefusesAmbiguityInAnySource(t *testing.T) {
	dir := t.TempDir()
	put := func(id, rev string) {
		require.NoError(t, os.MkdirAll(dir+"/checkpoints/"+id, 0o700))
		require.NoError(t, os.WriteFile(dir+"/checkpoints/"+id+"/"+rev+".json", []byte("{}"), 0o600))
	}
	a := "12345678" + strings.Repeat("a", 56)
	b := "12345678" + strings.Repeat("b", 56)
	c := "12345678" + strings.Repeat("c", 56)
	put("T-1", a)
	put("T-1", b)
	put("T-2", c)

	_, _, err := Find(dir, "checkpoint", []string{"T-2", "T-1"}, "12345678")
	require.ErrorContains(t, err, "names 3 checkpoints")
	_, _, err = Find(dir, "checkpoint", []string{"T-1", "T-2"}, "12345678")
	require.ErrorContains(t, err, "names 3 checkpoints", "the order of the sources changes nothing")

	id, rev, err := Find(dir, "checkpoint", []string{"T-2", "T-1"}, "12345678c")
	require.NoError(t, err)
	assert.Equal(t, "T-2", id)
	assert.Equal(t, c, rev)
	id, rev, err = Find(dir, "checkpoint", []string{"T-2", "T-1"}, b)
	require.NoError(t, err, "a source with no match is no error")
	assert.Equal(t, "T-1", id)
	assert.Equal(t, b, rev)

	all := Revisions(dir, "checkpoint", []string{"T-2", "T-1", "T-2"})
	assert.ElementsMatch(t, []string{a, b, c}, all)
	assert.Equal(t, "12345678c", Short([]string{a, b, c}, c)[:9])
	assert.Len(t, Short(all, c), ShortLen)
}
