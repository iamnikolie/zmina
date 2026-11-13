package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	assert.Regexp(t, `ok +screens +overrides in effect: 3 rules from \S+screen-rules\.yaml \(1 added, 1 replaced, 1 disabled\)`, out)
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

	// A command on this crew runs, with nothing said on stderr of the rules, and the embedded rows are in force, the good
	// half of the bad file included in nothing.
	_, errOut, err := zm(t, crewFile, "ls")
	require.NoError(t, err)
	assert.NotContains(t, errOut, "screen")

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
