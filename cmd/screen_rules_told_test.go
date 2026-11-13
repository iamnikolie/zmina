package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/screenrules"
)

const badScreenRules = "rules:\n  - id: acme.bad\n    engine: claude\n    sets: usage_limit\n    means: m\n    match: {regexp: '('}\n"

// asNewProcess forgets what this process told: the next command is what a new process of the ticker is.
func asNewProcess() { screenRulesTold.Clear() }

// refusedEvents is how many events the log has for a refused screen-rules.yaml.
func refusedEvents(t *testing.T, crewFile string) int {
	t.Helper()

	evs, err := eventLog(mustCrew(t, crewFile)).Read(events.Filter{})
	require.NoError(t, err)

	n := 0

	for _, e := range evs {
		if e.Event == screenRulesEvent {
			n++
		}
	}

	return n
}

// A refused file is one event a day for each reason, and what says so is a small marker beside the crew's other files, not
// the event log: a ticker that is a new process every beat would otherwise read the whole log for it every time.
func TestRefusedScreenRulesAreEventedOncePerReasonADayByAMarker(t *testing.T) {
	crewFile := testCrew(t, "")
	dir := filepath.Dir(crewFile)
	rules := filepath.Join(dir, "screen-rules.yaml")
	marker := mustCrew(t, crewFile).Layout().ScreenRulesTold()

	require.NoError(t, os.WriteFile(rules, []byte(badScreenRules), 0o600))

	asNewProcess()

	_, _, err := zm(t, crewFile, "ls")
	require.NoError(t, err)

	raw, err := os.ReadFile(marker)
	require.NoError(t, err, "the marker is kept")
	assert.NotContains(t, string(raw), "acme.bad", "what it holds is a hash of the reason, not the reason")
	assert.Equal(t, 1, refusedEvents(t, crewFile))

	// A new process within the day: nothing more.
	asNewProcess()

	_, _, err = zm(t, crewFile, "ls")
	require.NoError(t, err)
	assert.Equal(t, 1, refusedEvents(t, crewFile))

	// Another reason is told at once.
	require.NoError(t, os.WriteFile(rules, []byte(strings.Replace(badScreenRules, "acme.bad", "acme.other", 1)), 0o600))
	asNewProcess()

	_, _, err = zm(t, crewFile, "ls")
	require.NoError(t, err)
	assert.Equal(t, 2, refusedEvents(t, crewFile))

	// The first reason again, a day and an hour after it was told.
	var told map[string]time.Time

	require.NoError(t, json.Unmarshal(mustRead(t, marker), &told))
	require.Len(t, told, 2)

	for k := range told {
		told[k] = told[k].Add(-25 * time.Hour)
	}

	old, err := json.Marshal(told)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(marker, old, 0o600))

	asNewProcess()

	_, _, err = zm(t, crewFile, "ls")
	require.NoError(t, err)
	assert.Equal(t, 3, refusedEvents(t, crewFile), "told again after a day")

	// A marker that does not read is no marker: told, and the marker is made again.
	require.NoError(t, os.WriteFile(marker, []byte("not json"), 0o600))
	asNewProcess()

	_, _, err = zm(t, crewFile, "ls")
	require.NoError(t, err)
	assert.Equal(t, 4, refusedEvents(t, crewFile))
	assert.True(t, json.Valid(mustRead(t, marker)))
}

// The commands a role's hooks run stay silent on the error stream whatever is wrong with the crew's files: what they print
// reaches a session, and nobody reads it. The log and doctor still say it.
func TestHookRunsSayNothingOfARefusedScreenRulesFile(t *testing.T) {
	crewFile := testCrew(t, "")
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(crewFile), "screen-rules.yaml"), []byte(badScreenRules), 0o600))

	asNewProcess()

	_, errOut, err := zm(t, crewFile, "pending", "--brief", "--role", "dispatcher")
	require.NoError(t, err)
	assert.NotContains(t, errOut, "screen-rules", "the hook that runs pending --brief")
	assert.Equal(t, 1, refusedEvents(t, crewFile), "but the event is in the log")

	out, _, _ := zm(t, crewFile, "doctor")
	assert.Regexp(t, `warn +screens +\S+screen-rules\.yaml is ignored`, out)

	// The same command typed by hand says it, as every other.
	asNewProcess()

	_, errOut, err = zm(t, crewFile, "pending", "--role", "dispatcher")
	require.NoError(t, err)
	assert.Contains(t, errOut, "screen-rules.yaml is ignored")
}

// Every command line a released zmina wrote into a role's start is a hook run, and is told so.
func TestEveryHookLineIsAHookRun(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "hook-lines.txt"))
	require.NoError(t, err)

	seen := 0

	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		_, words, _ := strings.Cut(line, " ")
		args := strings.Fields(words)

		c, rest, err := newRoot(&app{}).Find(args)
		require.NoError(t, err, line)
		require.NoError(t, c.ParseFlags(rest), line)
		assert.True(t, isHookRun(c), line)

		seen++
	}

	assert.Positive(t, seen)

	// And a command the owner types is not.
	for _, args := range [][]string{{"ls"}, {"pending"}, {"doctor"}, {"explain", "dispatcher"}, {"tick"}} {
		c, rest, err := newRoot(&app{}).Find(args)
		require.NoError(t, err)
		require.NoError(t, c.ParseFlags(rest))
		assert.False(t, isHookRun(c), "%v", args)
	}
}

// What a refused file says, on the error stream and in doctor, is printable text: a wording the file chose, a key it misspelled,
// reaches a terminal, and a control, bidirectional or zero-width character there is not text.
func TestRefusedScreenRulesReasonIsPrintable(t *testing.T) {
	crewFile := testCrew(t, "")
	body := "rules:\n  - id: acme.x\n    engine: claude\n    sets: usage_limit\n    means: m\n    match: {contains: a}\n" +
		"    mach\u200b\u202e\u2066x: 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(crewFile), "screen-rules.yaml"), []byte(body), 0o600))

	asNewProcess()

	_, errOut, err := zm(t, crewFile, "ls")
	require.NoError(t, err)
	require.Contains(t, errOut, "screen-rules.yaml is ignored")

	out, _, _ := zm(t, crewFile, "doctor")
	require.Contains(t, out, "screen-rules.yaml is ignored")

	for label, text := range map[string]string{"stderr": errOut, "doctor": out} {
		for _, bad := range []string{"\u200b", "\u202e", "\u2066"} {
			assert.NotContains(t, text, bad, label)
		}
	}

	// The reason is cut, and on one line.
	assert.LessOrEqual(t, strings.Count(errOut, "\n"), 1)
}

// A path or a reason with a control character in it — DEL, a C1 control, an escape sequence — is cleaned on both lines the same.
func TestRefusalLinesAreCleanedOfControls(t *testing.T) {
	o := screenrules.Overrides{
		Path:    "/crew\x1b[31m\x7f/screen-rules.yaml",
		Present: true,
		Err:     errors.New("open /crew\x1b]0;title\a\x7f\u009b/screen-rules.yaml: bad \u202e" + strings.Repeat("w", 400)),
	}

	reason := refusalReason(o.Err)
	assert.LessOrEqual(t, len([]rune(reason)), 300)

	var line string

	for _, text := range []string{reason, printableLine(o.Path)} {
		line += text + "\n"
	}

	for _, bad := range []string{"\x1b", "\x7f", "\u009b", "\a", "\u202e"} {
		assert.NotContains(t, line, bad)
	}

	var out bytes.Buffer

	(&app{err: &out}).tellRefusedScreenRules(mustCrew(t, testCrew(t, "")), o)

	for _, bad := range []string{"\x1b", "\x7f", "\u009b", "\a", "\u202e"} {
		assert.NotContains(t, out.String(), bad)
	}

	assert.Contains(t, out.String(), "screen-rules.yaml is ignored")
}
