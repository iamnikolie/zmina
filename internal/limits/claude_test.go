package limits

import (
	"encoding/json"
	"fmt"
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

func reading(five, fiveResets, seven, sevenResets int) []byte {
	return []byte(fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":%d,"resets_at":%d},"seven_day":{"used_percentage":%d,"resets_at":%d}}}`,
		five, fiveResets, seven, sevenResets))
}

// A session that redraws with figures it saw before the window the file holds must not take the file back to them.
func TestTapKeepsTheStoredReadingWhenTheIncomingOneIsOlder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")

	_, err := Tap(dir, reading(20, 1893500000, 41, 1894000000), noon)
	require.NoError(t, err)

	before, err := os.ReadFile(path)
	require.NoError(t, err)

	// Both windows reset earlier than the stored ones: the previous windows, however full they were.
	wrote, err := Tap(dir, reading(99, 1893480000, 90, 1893900000), noon.Add(Heartbeat+time.Minute))
	require.NoError(t, err)
	assert.False(t, wrote, "an older reading writes nothing, the heartbeat included")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))

	c, err := ReadClaude(dir)
	require.NoError(t, err)
	assert.Equal(t, 20.0, c.FiveHour.Used)
	assert.Equal(t, 41.0, c.SevenDay.Used)
	assert.True(t, c.At.Equal(noon), "the file still says when its figures were seen")

	// The same reset with less spent is the same window seen earlier by another session: usage only grows within a period.
	for _, later := range []time.Duration{3 * time.Second, time.Minute, 2 * Heartbeat} {
		wrote, err = Tap(dir, reading(15, 1893500000, 30, 1894000000), noon.Add(later))
		require.NoError(t, err)
		assert.False(t, wrote, "%v after", later)
	}

	c, err = ReadClaude(dir)
	require.NoError(t, err)
	assert.Equal(t, 20.0, c.FiveHour.Used)

	// Only when the stored reading is older than ten minutes does a lower figure replace it (TestTapReplacesAStoredWindowNobodyDrewForTenMinutes).
	wrote, err = Tap(dir, reading(15, 1893500000, 30, 1894000000), noon.Add(2*Heartbeat+time.Second))
	require.NoError(t, err)
	assert.True(t, wrote)

	c, err = ReadClaude(dir)
	require.NoError(t, err)
	assert.Equal(t, 15.0, c.FiveHour.Used)
}

// Each window is judged on its own reset: a stale one is kept while the other moves on.
func TestTapJudgesEachWindowOnItsOwn(t *testing.T) {
	dir := t.TempDir()

	_, err := Tap(dir, reading(20, 1893500000, 41, 1894000000), noon)
	require.NoError(t, err)

	// Five-hour: the next window (later reset, less spent). Seven-day: an older window.
	wrote, err := Tap(dir, reading(3, 1893518000, 77, 1893400000), noon.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, wrote)

	c, err := ReadClaude(dir)
	require.NoError(t, err)
	assert.Equal(t, 3.0, c.FiveHour.Used, "a later reset is a new window, whatever it has spent")
	assert.Equal(t, time.Unix(1893518000, 0).UTC(), c.FiveHour.Resets.UTC())
	assert.Equal(t, 41.0, c.SevenDay.Used, "an earlier reset is an old window")
	assert.Equal(t, time.Unix(1894000000, 0).UTC(), c.SevenDay.Resets.UTC())
	assert.True(t, c.At.Equal(noon.Add(time.Minute)))

	// More spent at the same reset is later in the window.
	wrote, err = Tap(dir, reading(3, 1893518000, 45, 1894000000), noon.Add(2*time.Minute))
	require.NoError(t, err)
	assert.True(t, wrote)

	c, err = ReadClaude(dir)
	require.NoError(t, err)
	assert.Equal(t, 45.0, c.SevenDay.Used)
}

// A window with no reset moment tells nothing of its age: it is taken as before.
func TestTapTakesAWindowWithoutAResetAsItIs(t *testing.T) {
	dir := t.TempDir()

	_, err := Tap(dir, reading(20, 1893500000, 41, 1894000000), noon)
	require.NoError(t, err)

	wrote, err := Tap(dir, []byte(`{"rate_limits":{"five_hour":{"used_percentage":5},"seven_day":{"used_percentage":41,"resets_at":1894000000}}}`), noon.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, wrote)

	c, err := ReadClaude(dir)
	require.NoError(t, err)
	assert.Equal(t, 5.0, c.FiveHour.Used)
	assert.Nil(t, c.FiveHour.Resets)
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

// resetsOf is a status line with only the 5-hour window, its reset at the given moment.
func fiveAt(used int, resets time.Time) []byte {
	return []byte(fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":%d,"resets_at":%d}}}`, used, resets.Unix()))
}

// A status line that sends milliseconds for seconds lands in the year 50000: it is read as the moment it means.
func TestParseStatuslineReadsAMillisecondEpochAsTheMomentItMeans(t *testing.T) {
	soon := noon.Add(2 * time.Hour)

	c := ParseStatusline([]byte(fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":5,"resets_at":%d}}}`, soon.UnixMilli())), noon)
	require.NotNil(t, c)
	require.NotNil(t, c.FiveHour.Resets)
	assert.True(t, c.FiveHour.Resets.Equal(soon), "%v", c.FiveHour.Resets)

	// Anything else that is too big to be seconds or milliseconds is no moment at all.
	for _, big := range []string{"1893500000000000", "99999999999999999999", `"1893500000000000"`} {
		c = ParseStatusline([]byte(`{"rate_limits":{"five_hour":{"used_percentage":5,"resets_at":`+big+`}}}`), noon)
		require.NotNil(t, c, big)
		assert.Nil(t, c.FiveHour.Resets, big)
	}
}

// A reset further off than the window is long (and a little slack) cannot be this window's: it is no reset.
func TestParseStatuslineDropsAResetBeyondTheWindowsLength(t *testing.T) {
	line := func(five, seven time.Time) []byte {
		return []byte(fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":5,"resets_at":%d},"seven_day":{"used_percentage":5,"resets_at":%d}}}`,
			five.Unix(), seven.Unix()))
	}

	ok := ParseStatusline(line(noon.Add(5*time.Hour+10*time.Minute), noon.Add(7*24*time.Hour+30*time.Minute)), noon)
	require.NotNil(t, ok)
	assert.NotNil(t, ok.FiveHour.Resets, "within the slack")
	assert.NotNil(t, ok.SevenDay.Resets, "within the slack")

	far := ParseStatusline(line(noon.Add(5*time.Hour+16*time.Minute), noon.Add(7*24*time.Hour+61*time.Minute)), noon)
	require.NotNil(t, far)
	assert.Nil(t, far.FiveHour.Resets)
	assert.Nil(t, far.SevenDay.Resets)
	assert.Equal(t, 5.0, far.FiveHour.Used, "the figure is kept: it is only the reset that cannot be")
}

// A file an older tap wrote with a bad reset is judged again when read: it never makes a hold that cannot end.
func TestCollectDropsAResetThatWasWrittenBeforeTheBoundExisted(t *testing.T) {
	dir := t.TempDir()
	bad := noon.Add(400 * 24 * time.Hour)

	raw := fmt.Sprintf(`{"five_hour":{"used":97,"resets":%q},"seven_day":{"used":41,"resets":%q},"at":%q,"source":"statusline"}`,
		bad.Format(time.RFC3339), noon.Add(48*time.Hour).Format(time.RFC3339), noon.Format(time.RFC3339))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude.json"), []byte(raw), 0o600))

	r, err := Collect(dir, nil, DefaultPolicy(), noon.Add(time.Minute))
	require.NoError(t, err)
	require.NotNil(t, r.Claude)
	assert.Nil(t, r.Claude.FiveHour.Resets, "a year off for a 5-hour window")
	assert.Equal(t, 97.0, r.Claude.FiveHour.Used)
	assert.NotNil(t, r.Claude.SevenDay.Resets, "two days off for a week is fine")
}

// Usage inside one period only grows: a lower figure at the same reset is an older one (a session redrawing the figure of its last reply). It
// replaces the stored reading only when that is itself older than ten minutes.
func TestTapKeepsTheHigherFigureAtTheSameResetUntilTheStoredReadingIsOld(t *testing.T) {
	dir := t.TempDir()
	reset := noon.Add(3 * time.Hour)

	_, err := Tap(dir, fiveAt(90, reset), noon)
	require.NoError(t, err)

	for _, later := range []time.Duration{5 * time.Second, time.Minute, 10 * time.Minute} {
		wrote, err := Tap(dir, fiveAt(20, reset), noon.Add(later))
		require.NoError(t, err)
		assert.False(t, wrote, "%v after: the higher figure stands", later)
	}

	c, err := ReadClaude(dir)
	require.NoError(t, err)
	assert.Equal(t, 90.0, c.FiveHour.Used)

	// A higher figure at the same reset is the window filling: it stands at once.
	wrote, err := Tap(dir, fiveAt(95, reset), noon.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, wrote)

	// Eleven minutes after the stored reading nobody has drawn: the lower figure is what there is to go by.
	wrote, err = Tap(dir, fiveAt(20, reset), noon.Add(time.Minute+11*time.Minute))
	require.NoError(t, err)
	assert.True(t, wrote)

	c, err = ReadClaude(dir)
	require.NoError(t, err)
	assert.Equal(t, 20.0, c.FiveHour.Used)
}

// A stored window nobody has redrawn for ten minutes yields to whatever is drawn, an earlier reset included: another account.
func TestTapReplacesAStoredWindowNobodyDrewForTenMinutes(t *testing.T) {
	dir := t.TempDir()

	_, err := Tap(dir, fiveAt(95, noon.Add(4*time.Hour)), noon)
	require.NoError(t, err)

	// Within ten minutes an earlier reset is an old input redrawn: the stored window stays.
	wrote, err := Tap(dir, fiveAt(3, noon.Add(2*time.Hour)), noon.Add(9*time.Minute))
	require.NoError(t, err)
	assert.False(t, wrote)

	// After ten minutes of silence it is another account (or a reset the stored reading got wrong): it wins.
	wrote, err = Tap(dir, fiveAt(3, noon.Add(2*time.Hour)), noon.Add(11*time.Minute))
	require.NoError(t, err)
	assert.True(t, wrote)

	c, err := ReadClaude(dir)
	require.NoError(t, err)
	assert.Equal(t, 3.0, c.FiveHour.Used)
	assert.True(t, c.FiveHour.Resets.Equal(noon.Add(2*time.Hour)))
}

// A reset that cannot be this window's never gets into the file, so it cannot poison it.
func TestTapNeverWritesAResetBeyondTheBound(t *testing.T) {
	dir := t.TempDir()

	wrote, err := Tap(dir, []byte(fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":97,"resets_at":%d}}}`,
		noon.Add(500*24*time.Hour).UnixMilli())), noon)
	require.NoError(t, err)
	assert.True(t, wrote)

	c, err := ReadClaude(dir)
	require.NoError(t, err)
	assert.Nil(t, c.FiveHour.Resets)

	// And the next honest reading is not held back by it.
	_, err = Tap(dir, fiveAt(40, noon.Add(3*time.Hour)), noon.Add(time.Minute))
	require.NoError(t, err)

	c, err = ReadClaude(dir)
	require.NoError(t, err)
	assert.Equal(t, 40.0, c.FiveHour.Used)
	assert.True(t, c.FiveHour.Resets.Equal(noon.Add(3*time.Hour)))
}
