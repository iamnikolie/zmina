package recovery

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadOfNothingIsAnEmptyStateAtVersionOne(t *testing.T) {
	s, err := Load(t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, State{Version: 1}, s)

	s, err = Load(filepath.Join(t.TempDir(), "not-made-yet"))
	require.NoError(t, err, "a state directory that does not exist is no state, not an error")
	assert.Equal(t, 1, s.Version)
}

func TestSaveThenLoadKeepsFollowupsAndReadResults(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	want := State{
		Version: 1,
		Followups: map[string]Followup{
			"F-1": {ID: "F-1", Text: "tell alex the build is green", Task: "T-1", Source: "owner message", At: at},
			"F-2": {ID: "F-2", Text: "check the docs", At: at, Resolved: at.Add(time.Hour)},
		},
		ReadResults: map[string]bool{"T-1/abc": true},
	}

	require.NoError(t, Save(dir, want))

	got, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// A state value that never had its version set (a map built in a command) is written as version 1, the only one Load
// takes.
func TestSaveWritesVersionOneWhateverTheValueSays(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Save(dir, State{Version: 7}))

	raw, err := os.ReadFile(filepath.Join(dir, "dispatcher.json"))
	require.NoError(t, err)

	var on struct{ Version int }
	require.NoError(t, json.Unmarshal(raw, &on))
	assert.Equal(t, 1, on.Version)

	_, err = Load(dir)
	assert.NoError(t, err)
}

func TestSaveWritesPrivateIndentedFileThatEndsInANewlineAndOmitsEmptyFields(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "deeper")
	require.NoError(t, Save(dir, State{Followups: map[string]Followup{"F-1": {ID: "F-1", Text: "x"}}}))

	path := filepath.Join(dir, "dispatcher.json")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	dirInfo, err := os.Stat(dir)
	require.NoError(t, err, "Save makes the directory it writes into")
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(raw)
	assert.Equal(t, byte('\n'), text[len(text)-1])
	assert.Contains(t, text, "\n  \"version\": 1", "indented")
	assert.NotContains(t, text, "resolved", "a follow-up not kept yet has no resolved time")
	assert.NotContains(t, text, "read_results", "an empty map is left out")
	assert.NotContains(t, text, "\"task\"")
	assert.NotContains(t, text, "\"source\"")
}

func TestSaveReplacesTheFileWholeAndLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Save(dir, State{Followups: map[string]Followup{"F-1": {ID: "F-1", Text: "first"}, "F-2": {ID: "F-2", Text: "second"}}}))
	require.NoError(t, Save(dir, State{Followups: map[string]Followup{"F-3": {ID: "F-3", Text: "third"}}}))

	got, err := Load(dir)
	require.NoError(t, err)
	assert.Len(t, got.Followups, 1, "the second save is the state, not a merge")
	assert.Contains(t, got.Followups, "F-3")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only dispatcher.json is left")
	assert.Equal(t, "dispatcher.json", entries[0].Name())
}

func TestLoadRefusesWhatItCannotTrust(t *testing.T) {
	cases := map[string]struct {
		content string
		want    string
	}{
		"broken json":         {"broken", "invalid character"},
		"truncated":           {`{"version":1,"followups":{"F-1":`, "unexpected end"},
		"no version":          {`{}`, "unsupported dispatcher state version 0"},
		"a newer version":     {`{"version":2}`, "unsupported dispatcher state version 2"},
		"an older version":    {`{"version":0,"followups":{}}`, "unsupported dispatcher state version 0"},
		"wrong type of field": {`{"version":1,"followups":[1]}`, "cannot unmarshal"},
		"empty file":          {``, "unexpected end"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "dispatcher.json")
			require.NoError(t, os.WriteFile(path, []byte(c.content), 0o600))

			_, err := Load(dir)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)

			raw, rerr := os.ReadFile(path)
			require.NoError(t, rerr)
			assert.Equal(t, c.content, string(raw), "Load never rewrites a file it refused")
		})
	}
}

func TestLoadReportsAnUnreadableStateFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "dispatcher.json"), 0o700), "a directory where the file belongs")

	_, err := Load(dir)
	require.Error(t, err)
	assert.False(t, os.IsNotExist(err), "a read error is no missing file")
}

func TestSaveFailsWhereItCannotWrite(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))

	assert.Error(t, Save(filepath.Join(blocker, "sub"), State{}), "a directory path under a regular file")
}

func pack(texts ...string) Pack {
	p := Pack{
		Version: 1,
		At:      time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Entries: map[string][]Entry{},
		Counts:  map[string]int{},
	}

	for i, text := range texts {
		p.Entries["tasks"] = append(p.Entries["tasks"], Entry{ID: fmt.Sprintf("T-%d", i+1), Text: text, Ref: "ref", Data: map[string]any{"n": float64(i)}})
		p.Counts["tasks"]++
	}

	return p
}

var packName = regexp.MustCompile(`^pack-[0-9a-f]{64}\.json$`)

func TestPublishNamesTheFileByItsContent(t *testing.T) {
	dir := t.TempDir()
	p := pack("the first thing", "the second thing")

	path, err := Publish(dir, "pack", p)
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(path))
	assert.Equal(t, filepath.Join(dir, "context", "dispatcher"), filepath.Dir(path))
	assert.Regexp(t, packName, filepath.Base(path))

	raw, err := json.Marshal(p)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("pack-%x.json", sha256.Sum256(raw)), filepath.Base(path),
		"the name is the sha256 of the compact JSON, so a reader can tell two packs apart by name")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	dirInfo, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, byte('\n'), body[len(body)-1])

	var back Pack
	require.NoError(t, json.Unmarshal(body, &back))
	assert.Equal(t, 1, back.Version)
	assert.True(t, back.At.Equal(p.At))
	assert.Equal(t, p.Counts, back.Counts)
	require.Len(t, back.Entries["tasks"], 2)
	assert.Equal(t, "the second thing", back.Entries["tasks"][1].Text)
	assert.Equal(t, "ref", back.Entries["tasks"][1].Ref)
}

func TestPublishOfTheSameContentIsTheSameFile(t *testing.T) {
	dir := t.TempDir()

	one, err := Publish(dir, "pack", pack("same"))
	require.NoError(t, err)
	two, err := Publish(dir, "pack", pack("same"))
	require.NoError(t, err)
	assert.Equal(t, one, two)

	other, err := Publish(dir, "pack", pack("not the same"))
	require.NoError(t, err)
	assert.NotEqual(t, one, other)

	files, err := filepath.Glob(filepath.Join(dir, "context", "dispatcher", "*.json"))
	require.NoError(t, err)
	assert.Len(t, files, 2, "an unchanged pack is not written a second time under a new name")
}

// The kind is part of the name only: the same pack published as the full index and as the short pack are two files.
func TestPublishKindIsPartOfTheName(t *testing.T) {
	dir := t.TempDir()
	p := pack("x")

	index, err := Publish(dir, "index", p)
	require.NoError(t, err)
	short, err := Publish(dir, "pack", p)
	require.NoError(t, err)

	assert.Regexp(t, `^index-[0-9a-f]{64}\.json$`, filepath.Base(index))
	assert.Regexp(t, packName, filepath.Base(short))
	assert.NotEqual(t, index, short)
	assert.Equal(t, filepath.Base(index)[len("index-"):], filepath.Base(short)[len("pack-"):], "same content, same hash")
}

func TestPublishLeavesTheStateFileAlone(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Save(dir, State{Followups: map[string]Followup{"F-1": {ID: "F-1", Text: "keep me"}}}))

	_, err := Publish(dir, "pack", pack("x"))
	require.NoError(t, err)

	got, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, "keep me", got.Followups["F-1"].Text)
}

func TestPublishFailsWhereItCannotWrite(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))

	_, err := Publish(blocker, "pack", pack("x"))
	assert.Error(t, err)
}

func TestPublishRefusesWhatJSONCannotWrite(t *testing.T) {
	p := pack("x")
	p.Entries["tasks"][0].Data = func() {}

	_, err := Publish(t.TempDir(), "pack", p)
	assert.Error(t, err, "an entry whose data does not encode is an error, not an empty file")
}
