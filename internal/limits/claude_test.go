package limits

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fresh = `{"session_id":"s-1","model":{"display_name":"M"},"rate_limits":{
  "five_hour":{"used_percentage":12.5,"resets_at":1893500000},
  "seven_day":{"used_percentage":41,"resets_at":1894000000}}}`

func TestParseStatuslineReadsBothWindows(t *testing.T) {
	c := ParseStatusline([]byte(fresh), noon)
	require.NotNil(t, c)

	assert.Equal(t, 12.5, c.FiveHour.Used)
	assert.Equal(t, time.Unix(1893500000, 0).UTC(), c.FiveHour.Resets.UTC())
	assert.Equal(t, 41.0, c.SevenDay.Used)
	assert.Equal(t, time.Unix(1894000000, 0).UTC(), c.SevenDay.Resets.UTC())
	assert.Equal(t, noon, c.At)
	assert.Equal(t, "statusline", c.Source)
}

func TestParseStatuslineWithMissingFields(t *testing.T) {
	cases := map[string]string{
		"no limits at all (an API key, or no reply yet)": `{"model":{"display_name":"M"}}`,
		"empty limits":                   `{"rate_limits":{}}`,
		"windows without a percentage":   `{"rate_limits":{"five_hour":{"resets_at":1893500000}}}`,
		"a negative percentage":          `{"rate_limits":{"five_hour":{"used_percentage":-1}}}`,
		"a percentage that is no number": `{"rate_limits":{"five_hour":{"used_percentage":"lots"}}}`,
		"null windows":                   `{"rate_limits":{"five_hour":null,"seven_day":null}}`,
		"not JSON":                       `statusline: broken`,
		"nothing":                        ``,
	}

	for name, in := range cases {
		assert.Nil(t, ParseStatusline([]byte(in), noon), name)
	}

	one := ParseStatusline([]byte(`{"rate_limits":{"seven_day":{"used_percentage":41}}}`), noon)
	require.NotNil(t, one)
	assert.Nil(t, one.FiveHour, "a window the status line did not send is absent")
	assert.Nil(t, one.SevenDay.Resets, "a missing reset is no reset, not the epoch")

	for name, in := range map[string]string{
		"a reset that is no moment": `{"rate_limits":{"seven_day":{"used_percentage":5,"resets_at":"soon"}}}`,
		"a zero reset":              `{"rate_limits":{"seven_day":{"used_percentage":5,"resets_at":0}}}`,
		"a null reset":              `{"rate_limits":{"seven_day":{"used_percentage":5,"resets_at":null}}}`,
	} {
		c := ParseStatusline([]byte(in), noon)
		require.NotNil(t, c, name)
		assert.Nil(t, c.SevenDay.Resets, name)
	}
}

func TestParseStatuslineReadsTheOtherResetSpellings(t *testing.T) {
	want := time.Unix(1894000000, 0)

	for _, in := range []string{
		`{"rate_limits":{"seven_day":{"used_percentage":5,"resets_at":"2030-01-07T07:06:40Z"}}}`,
		`{"rate_limits":{"seven_day":{"used_percentage":5,"resets_at":"2030-01-07T08:06:40+01:00"}}}`,
		`{"rate_limits":{"seven_day":{"used_percentage":5,"resets_at":"1894000000"}}}`,
		`{"rate_limits":{"seven_day":{"used_percentage":5,"resets_at":1894000000.0}}}`,
	} {
		c := ParseStatusline([]byte(in), noon)
		require.NotNil(t, c, in)
		require.NotNil(t, c.SevenDay.Resets, in)
		assert.True(t, c.SevenDay.Resets.Equal(want), "%s: %v", in, c.SevenDay.Resets)
	}
}

func TestTapWritesTheFileAndReadsItBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "limits")

	wrote, err := Tap(dir, []byte(fresh), noon)
	require.NoError(t, err)
	assert.True(t, wrote)

	var onDisk map[string]any
	raw, err := os.ReadFile(filepath.Join(dir, "claude.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &onDisk))
	assert.Contains(t, onDisk, "five_hour")
	assert.Contains(t, onDisk, "seven_day")
	assert.Equal(t, "statusline", onDisk["source"])

	c, err := ReadClaude(dir)
	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Equal(t, 41.0, c.SevenDay.Used)
	assert.True(t, c.At.Equal(noon))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary file left behind")
}

func TestTapWritesOnlyWhenAFigureChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")

	_, err := Tap(dir, []byte(fresh), noon)
	require.NoError(t, err)

	// Same figures a minute later: the file is not touched.
	require.NoError(t, os.Chtimes(path, noon.Add(-time.Hour), noon.Add(-time.Hour)))

	wrote, err := Tap(dir, []byte(fresh), noon.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, wrote, "nothing changed")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(noon.Add(-time.Hour)), "the file was not rewritten")

	// A figure moved.
	moved := `{"rate_limits":{"five_hour":{"used_percentage":13,"resets_at":1893500000},"seven_day":{"used_percentage":41,"resets_at":1894000000}}}`
	wrote, err = Tap(dir, []byte(moved), noon.Add(2*time.Minute))
	require.NoError(t, err)
	assert.True(t, wrote)

	c, err := ReadClaude(dir)
	require.NoError(t, err)
	assert.Equal(t, 13.0, c.FiveHour.Used)
	assert.True(t, c.At.Equal(noon.Add(2*time.Minute)))

	// The window rolled over: same percentage, new reset.
	rolled := `{"rate_limits":{"five_hour":{"used_percentage":13,"resets_at":1893600000},"seven_day":{"used_percentage":41,"resets_at":1894000000}}}`
	wrote, err = Tap(dir, []byte(rolled), noon.Add(3*time.Minute))
	require.NoError(t, err)
	assert.True(t, wrote, "a new reset is a changed value")

	// A window that went away.
	gone := `{"rate_limits":{"seven_day":{"used_percentage":41,"resets_at":1894000000}}}`
	wrote, err = Tap(dir, []byte(gone), noon.Add(4*time.Minute))
	require.NoError(t, err)
	assert.True(t, wrote)

	c, err = ReadClaude(dir)
	require.NoError(t, err)
	assert.Nil(t, c.FiveHour)
}

func TestTapRefreshesAnUnchangedReadingOncePerHeartbeat(t *testing.T) {
	dir := t.TempDir()

	_, err := Tap(dir, []byte(fresh), noon)
	require.NoError(t, err)

	wrote, err := Tap(dir, []byte(fresh), noon.Add(Heartbeat-time.Second))
	require.NoError(t, err)
	assert.False(t, wrote)

	wrote, err = Tap(dir, []byte(fresh), noon.Add(Heartbeat))
	require.NoError(t, err)
	assert.True(t, wrote, "the status line is still drawn: the file says so")

	c, err := ReadClaude(dir)
	require.NoError(t, err)
	assert.True(t, c.At.Equal(noon.Add(Heartbeat)))
}

func TestTapIgnoresAStatuslineWithoutFigures(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "limits")

	wrote, err := Tap(dir, []byte(`{"model":{}}`), noon)
	require.NoError(t, err)
	assert.False(t, wrote)
	assert.NoDirExists(t, dir, "nothing to record, nothing created")

	_, err = Tap(dir, []byte(fresh), noon)
	require.NoError(t, err)

	wrote, err = Tap(dir, []byte(`{"model":{}}`), noon.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, wrote)

	c, err := ReadClaude(dir)
	require.NoError(t, err)
	assert.Equal(t, 41.0, c.SevenDay.Used, "an earlier reading is not erased by a draw without figures")
}

func TestTapReplacesAFileThatDoesNotParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"five_hour":`), 0o600))

	_, err := ReadClaude(dir)
	assert.Error(t, err, "a half-written file is reported, not read")

	wrote, err := Tap(dir, []byte(fresh), noon)
	require.NoError(t, err)
	assert.True(t, wrote)

	c, err := ReadClaude(dir)
	require.NoError(t, err)
	assert.NotNil(t, c)
}

func TestTapFailsLoudlyWhereItCannotWrite(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "limits")
	require.NoError(t, os.WriteFile(blocker, []byte("a file where the directory goes"), 0o600))

	wrote, err := Tap(blocker, []byte(fresh), noon)
	assert.Error(t, err)
	assert.False(t, wrote)
}

func TestReadClaudeWithoutAFileIsNoReading(t *testing.T) {
	c, err := ReadClaude(t.TempDir())
	assert.NoError(t, err)
	assert.Nil(t, c)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude.json"), []byte(`{}`), 0o600))
	c, err = ReadClaude(dir)
	assert.Error(t, err, "a file with no figures is no reading")
	assert.Nil(t, c)
}

func TestReadClaudeNeverTrustsAStaleFlagInTheFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude.json"),
		[]byte(`{"seven_day":{"used":41,"stale":true},"at":"2030-03-05T11:59:00Z","source":"statusline"}`), 0o600))

	c, err := ReadClaude(dir)
	require.NoError(t, err)
	assert.False(t, c.SevenDay.Stale)
}
