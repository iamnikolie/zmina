package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/screenrules"
)

const goodScreenRules = `rules:
  - id: acme.limit.wording
    engine: claude
    sets: usage_limit
    means: a new wording of the usage-limit notice
    match:
      contains: "plan exhausted"
  - id: claude.peek.hint-update
    engine: claude
    sets: footer_hint
    means: the update notice, as a newer release words it
    match:
      contains: "Update failed"
  - id: claude.peek.hint-todos
    disabled: true
`

// A crew with no screen-rules.yaml says nothing of it; with one, doctor says in one line that its overrides are in effect
// and how many rows it lists; and the rows are in force for the commands of that crew.
func TestDoctorSaysScreenRuleOverridesAreInForce(t *testing.T) {
	crewFile := testCrew(t, "")
	dir := filepath.Dir(crewFile)

	out, _, _ := zm(t, crewFile, "doctor")
	assert.NotContains(t, out, "screens", "no file, no line")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "screen-rules.yaml"), []byte(goodScreenRules), 0o600))

	out, _, _ = zm(t, crewFile, "doctor")
	assert.Regexp(t, `ok +screens +overrides in effect: 3 rules from \S+screen-rules\.yaml: added acme\.limit\.wording; `+
		`replaced claude\.peek\.hint-update; disabled claude\.peek\.hint-todos$`, strings.TrimSpace(lineOf(out, "screens")))
	assert.Equal(t, 1, countLines(out, "screens"), "one line")

	// The shift built for the crew carries its rows.
	s, err := (&app{out: io.Discard, err: io.Discard, crew: crewFile}).newShift()
	require.NoError(t, err)

	_, ok := s.ScreenRules().Test(screenrules.Claude, screenrules.UsageLimit, "plan exhausted")
	assert.True(t, ok, "the crew's rows are in force for the shift built for its crew")

	_, ok = s.ScreenRules().Test(screenrules.Claude, screenrules.FooterHint, "ctrl+t to hide todos")
	assert.False(t, ok, "and a row it disables is gone")
}

// A file that does not read is reported by doctor and ignored: the embedded rules stay in force, and no command stops.
func TestBadScreenRulesAreReportedAndIgnored(t *testing.T) {
	crewFile := testCrew(t, "")
	dir := filepath.Dir(crewFile)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "screen-rules.yaml"),
		[]byte("rules:\n  - id: claude.peek.hint-todos\n    disabled: true\n  - id: acme.bad\n    engine: claude\n    sets: usage_limit\n    means: m\n    match: {regexp: '('}\n"), 0o600))

	out, _, _ := zm(t, crewFile, "doctor")
	assert.Regexp(t, `warn +screens +\S+screen-rules\.yaml is ignored and the built-in rules stay in force: rule "acme\.bad": bad regexp`, out)
	assert.Equal(t, 1, countLines(out, "screens"))
	assert.NotRegexp(t, `fail +screens`, out, "a warning, never a fail")

	// A command on this crew runs, saying on stderr that the file is ignored, and the embedded rows are in force, the good
	// half of the bad file included in nothing.
	_, errOut, err := zm(t, crewFile, "ls")
	require.NoError(t, err)
	assert.Contains(t, errOut, "screen-rules.yaml is ignored")

	s, err := (&app{out: io.Discard, err: io.Discard, crew: crewFile}).newShift()
	require.NoError(t, err)
	assert.Same(t, screenrules.Default(), s.ScreenRules())

	_, ok := s.ScreenRules().Test(screenrules.Claude, screenrules.FooterHint, "ctrl+t to hide todos")
	assert.True(t, ok)
}

func TestEmptyScreenRulesFileIsSaidToo(t *testing.T) {
	crewFile := testCrew(t, "")
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(crewFile), "screen-rules.yaml"), []byte("rules: []\n"), 0o600))

	out, _, _ := zm(t, crewFile, "doctor")
	assert.Regexp(t, `ok +screens +overrides in effect: 0 rules`, out)
}

// countLines is how many lines of a doctor report are for the named check (the second column).
func countLines(out, check string) int {
	n := 0

	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 1 && f[1] == check {
			n++
		}
	}

	return n
}

// lineOf is the line of a doctor report for the named check.
func lineOf(out, check string) string {
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 1 && f[1] == check {
			return l
		}
	}

	return ""
}

// The ids a file added, replaced and disabled are listed, so that a reader sees what it changes; a long list is cut.
func TestDoctorListsTheRowsAnOverrideChangesUpToAFew(t *testing.T) {
	crewFile := testCrew(t, "")

	var body strings.Builder

	body.WriteString("rules:\n")

	for i := range 20 {
		fmt.Fprintf(&body, "  - id: acme.limit.%02d\n    engine: claude\n    sets: usage_limit\n    means: m\n    match: {contains: \"w%02d\"}\n", i, i)
	}

	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(crewFile), "screen-rules.yaml"), []byte(body.String()), 0o600))

	out, _, _ := zm(t, crewFile, "doctor")
	line := lineOf(out, "screens")

	assert.Contains(t, line, "overrides in effect: 20 rules from")
	assert.Contains(t, line, "added acme.limit.00, acme.limit.01")
	assert.Contains(t, line, "acme.limit.11")
	assert.NotContains(t, line, "acme.limit.12", "twelve ids, then the count of the rest")
	assert.Contains(t, line, "and 8 more")
}

// A screen-rules.yaml that is refused is not silent outside doctor: the command says so on its error stream, once in the
// process, as a crew file's warnings are said, and the event log has one event for the reason a day.
func TestRefusedScreenRulesAreToldOnTheErrorStreamAndInTheLog(t *testing.T) {
	crewFile := testCrew(t, "")
	dir := filepath.Dir(crewFile)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "screen-rules.yaml"),
		[]byte("rules:\n  - id: acme.bad\n    engine: claude\n    sets: usage_limit\n    means: m\n    match: {regexp: '('}\n"), 0o600))

	run := func() string {
		var out, errb bytes.Buffer

		a := &app{out: &out, err: &errb, crew: crewFile}
		root := newRoot(a)
		root.SetArgs([]string{"--crew", crewFile, "ls"})
		require.NoError(t, root.Execute())

		return errb.String()
	}

	first := run()
	assert.Contains(t, first, "zmina: warning:")
	assert.Contains(t, first, "screen-rules.yaml is ignored")
	assert.Contains(t, first, `rule "acme.bad": bad regexp`)
	assert.Contains(t, first, "zmina doctor")

	assert.NotContains(t, run(), "screen-rules.yaml", "once in a process")

	evs, err := eventLog(mustCrew(t, crewFile)).Read(events.Filter{})
	require.NoError(t, err)

	var told []events.Event

	for _, e := range evs {
		if e.Event == "screen-rules-refused" {
			told = append(told, e)
		}
	}

	require.Len(t, told, 1, "one event for the reason, whatever the number of commands")
	assert.Contains(t, told[0].Detail, `rule "acme.bad": bad regexp`)

	// A good file says nothing and logs nothing.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "screen-rules.yaml"), []byte("rules: []\n"), 0o600))
	assert.NotContains(t, run(), "screen-rules")
}

func mustCrew(t *testing.T, file string) *crew.Crew {
	t.Helper()

	c, err := crew.Load(file)
	require.NoError(t, err)

	return c
}
