package limits

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tokenCount is one rollout line as Codex writes a token_count event with its rate limits.
func tokenCount(at time.Time, used float64, resets int64, plan string) string {
	return fmt.Sprintf(`{"timestamp":%q,"ordinal":7,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":10}},`+
		`"rate_limits":{"limit_id":"codex","limit_name":null,"primary":{"used_percent":%v,"window_minutes":10080,"resets_at":%d},`+
		`"secondary":null,"credits":{"has_credits":false},"plan_type":%q}}}`, at.UTC().Format("2006-01-02T15:04:05.000Z"), used, resets, plan)
}

const plainLine = `{"timestamp":"2030-01-01T10:00:00.000Z","type":"response_item","payload":{"type":"message","text":"hello"}}`

// writeRollout writes a rollout under <home>/sessions/<day>/ with the lines given and the modification time mod.
func writeRollout(t *testing.T, home, day, name string, mod time.Time, lines ...string) string {
	t.Helper()

	dir := filepath.Join(home, "sessions", day)
	require.NoError(t, os.MkdirAll(dir, 0o700))

	path := filepath.Join(dir, "rollout-"+name+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	require.NoError(t, os.Chtimes(path, mod, mod))

	return path
}

func TestCodexReadsTheLastLimitsOfTheNewestRollout(t *testing.T) {
	home := t.TempDir()
	reset := noon.Add(3 * 24 * time.Hour).Unix()

	writeRollout(t, home, "2030/01/01", "2030-01-01T09-00-00-aaa", noon.Add(-time.Hour),
		plainLine,
		tokenCount(noon.Add(-3*time.Hour), 48, reset, "pro"),
		plainLine,
		tokenCount(noon.Add(-2*time.Hour), 50, reset, "pro"),
		plainLine)

	c := ReadCodex([]Home{{Path: home, Label: "dev"}})
	require.NotNil(t, c)

	assert.Equal(t, 50.0, c.Week.Used, "the last limits object, not the first")
	assert.Equal(t, reset, c.Week.Resets.Unix())
	assert.Equal(t, "pro", c.Plan)
	assert.True(t, c.At.Equal(noon.Add(-2*time.Hour)), "the line's own timestamp, not the file's")
	assert.Equal(t, "rollout dev", c.Source)
}

func TestCodexSearchesByModificationTimeAcrossDays(t *testing.T) {
	home := t.TempDir()
	reset := noon.Add(24 * time.Hour).Unix()

	// A session started days ago and still written to today is the newest, though its folder is the oldest.
	writeRollout(t, home, "2030/03/01", "2030-03-01T08-00-00-old", noon.Add(-10*time.Minute),
		tokenCount(noon.Add(-10*time.Minute), 62, reset, "pro"))
	writeRollout(t, home, "2030/03/04", "2030-03-04T08-00-00-mid", noon.Add(-30*time.Hour),
		tokenCount(noon.Add(-30*time.Hour), 55, reset, "pro"))
	writeRollout(t, home, "2030/03/03", "2030-03-03T08-00-00-mid", noon.Add(-50*time.Hour),
		tokenCount(noon.Add(-50*time.Hour), 40, reset, "pro"))

	c := ReadCodex([]Home{{Path: home, Label: "dev"}})
	require.NotNil(t, c)
	assert.Equal(t, 62.0, c.Week.Used)
}

func TestCodexTakesTheHomeRecordedLast(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	reset := noon.Add(24 * time.Hour).Unix()

	writeRollout(t, a, "2030/01/01", "2030-01-01T01-00-00-a", noon.Add(-5*time.Hour),
		tokenCount(noon.Add(-5*time.Hour), 70, reset, "pro"))
	writeRollout(t, b, "2030/01/01", "2030-01-01T02-00-00-b", noon.Add(-90*time.Minute),
		tokenCount(noon.Add(-90*time.Minute), 52, reset, "pro"))

	c := ReadCodex([]Home{{Path: a, Label: "codex-dev"}, {Path: b, Label: "codex-reviewer"}})
	require.NotNil(t, c)
	assert.Equal(t, 52.0, c.Week.Used)
	assert.Equal(t, "rollout codex-reviewer", c.Source)

	c = ReadCodex([]Home{{Path: filepath.Join(a, "missing"), Label: "gone"}, {Path: a, Label: "codex-dev"}})
	require.NotNil(t, c, "a home that is not there is passed over")
	assert.Equal(t, 70.0, c.Week.Used)
}

func TestCodexPassesOverALineWithoutLimitsAndAHalfWrittenOne(t *testing.T) {
	home := t.TempDir()
	reset := noon.Add(24 * time.Hour).Unix()

	good := tokenCount(noon.Add(-time.Hour), 33, reset, "pro")
	half := good[:len(good)/2] // the writer is in the middle of the next line: no newline, no closing braces

	path := writeRollout(t, home, "2030/01/01", "2030-01-01T09-00-00-x", noon, good, plainLine, `{"payload":{"rate_limits":null}}`)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(half)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Chtimes(path, noon, noon))

	c := ReadCodex([]Home{{Path: home, Label: "dev"}})
	require.NotNil(t, c)
	assert.Equal(t, 33.0, c.Week.Used)
}

func TestCodexLooksPastAnEmptyNewestRollout(t *testing.T) {
	home := t.TempDir()
	reset := noon.Add(24 * time.Hour).Unix()

	writeRollout(t, home, "2030/01/01", "2030-01-01T08-00-00-old", noon.Add(-2*time.Hour),
		tokenCount(noon.Add(-2*time.Hour), 21, reset, "pro"))
	writeRollout(t, home, "2030/01/01", "2030-01-01T11-59-00-new", noon, plainLine, plainLine) // no turn yet

	c := ReadCodex([]Home{{Path: home, Label: "dev"}})
	require.NotNil(t, c)
	assert.Equal(t, 21.0, c.Week.Used)

	empty := t.TempDir()
	writeRollout(t, empty, "2030/01/01", "2030-01-01T11-59-00-new", noon, plainLine)
	assert.Nil(t, ReadCodex([]Home{{Path: empty, Label: "dev"}}), "no limits anywhere: unknown")
	assert.Nil(t, ReadCodex([]Home{{Path: t.TempDir(), Label: "dev"}}), "no rollouts at all")
	assert.Nil(t, ReadCodex(nil))
}

func TestCodexTakesOnlyTheWeeklyWindow(t *testing.T) {
	home := t.TempDir()
	stamp := `"timestamp":"2030-01-01T10:00:00.000Z"`

	// The week may be the secondary window; a 5-hour primary beside it is not the week.
	both := `{` + stamp + `,"payload":{"rate_limits":{"plan_type":"team",` +
		`"primary":{"used_percent":9.0,"window_minutes":300,"resets_at":1893500000},` +
		`"secondary":{"used_percent":44.5,"window_minutes":10080,"resets_at":1894000000}}}}`
	writeRollout(t, home, "2030/01/01", "a", noon, both)

	c := ReadCodex([]Home{{Path: home, Label: "dev"}})
	require.NotNil(t, c)
	assert.Equal(t, 44.5, c.Week.Used)
	assert.Equal(t, "team", c.Plan)

	short := t.TempDir()
	writeRollout(t, short, "2030/01/01", "a", noon, `{`+stamp+`,"payload":{"rate_limits":{"primary":{"used_percent":9.0,"window_minutes":300}}}}`)
	assert.Nil(t, ReadCodex([]Home{{Path: short, Label: "dev"}}), "no weekly window: no figure for the week")

	noStamp := t.TempDir()
	writeRollout(t, noStamp, "2030/01/01", "a", noon, `{"payload":{"rate_limits":{"primary":{"used_percent":9.0,"window_minutes":10080}}}}`)
	assert.Nil(t, ReadCodex([]Home{{Path: noStamp, Label: "dev"}}), "no timestamp: nobody knows when it was seen")

	topLevel := t.TempDir()
	writeRollout(t, topLevel, "2030/01/01", "a", noon, `{`+stamp+`,"rate_limits":{"primary":{"used_percent":7,"window_minutes":10080}}}`)
	c = ReadCodex([]Home{{Path: topLevel, Label: "dev"}})
	require.NotNil(t, c)
	assert.Equal(t, 7.0, c.Week.Used)
	assert.Nil(t, c.Week.Resets, "a window without its reset has none")
}

func TestCodexFindsLimitsFarBackFromTheEnd(t *testing.T) {
	home := t.TempDir()
	reset := noon.Add(24 * time.Hour).Unix()

	// A tool output longer than several read chunks sits after the last limits line, and one line spans chunks.
	big := `{"timestamp":"2030-01-01T10:00:00.000Z","type":"response_item","payload":{"output":"` + strings.Repeat("x", 3*scanChunk) + `"}}`
	writeRollout(t, home, "2030/01/01", "a", noon,
		tokenCount(noon.Add(-time.Hour), 36, reset, "pro"), big, big, plainLine)

	c := ReadCodex([]Home{{Path: home, Label: "dev"}})
	require.NotNil(t, c)
	assert.Equal(t, 36.0, c.Week.Used)
}

func TestCodexNeverReadsPastItsScanLimit(t *testing.T) {
	home := t.TempDir()
	reset := noon.Add(24 * time.Hour).Unix()

	filler := `{"type":"response_item","payload":{"output":"` + strings.Repeat("y", 1<<20-60) + `"}}`
	lines := []string{tokenCount(noon.Add(-time.Hour), 36, reset, "pro")}

	for i := 0; i < scanMax>>20+2; i++ {
		lines = append(lines, filler)
	}

	writeRollout(t, home, "2030/01/01", "a", noon, lines...)
	assert.Nil(t, ReadCodex([]Home{{Path: home, Label: "dev"}}), "a reading that far back is not worth the read")
}

func TestCollectReadsBothSourcesAndJudgesThem(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	reset := noon.Add(24 * time.Hour).Unix()

	writeRollout(t, home, "2030/01/01", "a", noon.Add(-8*time.Hour), tokenCount(noon.Add(-8*time.Hour), 50, reset, "pro"))

	_, err := Tap(dir, []byte(fresh), noon.Add(-2*time.Minute))
	require.NoError(t, err)

	r, err := Collect(dir, []Home{{Path: home, Label: "codex-dev"}}, DefaultPolicy(), noon)
	require.NoError(t, err)

	require.NotNil(t, r.Claude)
	assert.False(t, r.Claude.SevenDay.Stale)
	require.NotNil(t, r.Codex)
	assert.True(t, r.Codex.Week.Stale, "eight hours without a Codex session")
	assert.Equal(t, 50.0, r.Codex.Week.Used, "its last figure stays visible")
}

// The beat does not walk a Codex home's whole tree every minute: what it found is kept, the newest rollout is looked at alone, and the home
// is walked again after five minutes.
func TestReadCodexCachedWalksAHomeOncePerFiveMinutes(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	reset := noon.Add(24 * time.Hour).Unix()

	writeRollout(t, home, "2030/01/01", "a", noon, tokenCount(noon.Add(-time.Hour), 40, reset, "pro"))

	homes := []Home{{Path: home, Label: "dev"}}

	c := ReadCodexCached(dir, homes, noon)
	require.NotNil(t, c)
	assert.Equal(t, 40.0, c.Week.Used)
	assert.Equal(t, "rollout dev", c.Source)
	assert.Equal(t, home, c.Home)
	assert.FileExists(t, filepath.Join(dir, CodexCacheFile))

	// A newer rollout of another session appears: within five minutes the cached walk stands.
	writeRollout(t, home, "2030/01/02", "b", noon.Add(time.Minute), tokenCount(noon, 91, reset, "pro"))

	c = ReadCodexCached(dir, homes, noon.Add(2*time.Minute))
	require.NotNil(t, c)
	assert.Equal(t, 40.0, c.Week.Used, "not walked again yet")

	// ... after five minutes it is found.
	c = ReadCodexCached(dir, homes, noon.Add(6*time.Minute))
	require.NotNil(t, c)
	assert.Equal(t, 91.0, c.Week.Used)
	assert.Equal(t, 91.0, ReadCodex(homes).Week.Used, "the same as an uncached read")
}

// The newest rollout cached is read again as soon as it grows, with no walk: a status that moves is seen at the next beat.
func TestReadCodexCachedReadsTheNewestRolloutAgainWhenItGrows(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	reset := noon.Add(24 * time.Hour).Unix()

	path := writeRollout(t, home, "2030/01/01", "a", noon, tokenCount(noon.Add(-time.Hour), 40, reset, "pro"))
	homes := []Home{{Path: home, Label: "dev"}}

	require.Equal(t, 40.0, ReadCodexCached(dir, homes, noon).Week.Used)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(tokenCount(noon.Add(-time.Minute), 97, reset, "pro") + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Chtimes(path, noon.Add(time.Minute), noon.Add(time.Minute)))

	assert.Equal(t, 97.0, ReadCodexCached(dir, homes, noon.Add(time.Minute)).Week.Used, "no walk needed to see it")
}

func TestReadCodexCachedHandlesNothingToReadAndACacheThatIsBroken(t *testing.T) {
	dir := t.TempDir()

	assert.Nil(t, ReadCodexCached(dir, []Home{{Path: t.TempDir(), Label: "dev"}}, noon), "a home with no rollout")
	assert.Nil(t, ReadCodexCached(dir, nil, noon))

	require.NoError(t, os.WriteFile(filepath.Join(dir, CodexCacheFile), []byte("{broken"), 0o600))

	home := t.TempDir()
	writeRollout(t, home, "2030/01/01", "a", noon, tokenCount(noon, 12, noon.Add(time.Hour).Unix(), "pro"))
	assert.Equal(t, 12.0, ReadCodexCached(dir, []Home{{Path: home, Label: "dev"}}, noon).Week.Used)
}
