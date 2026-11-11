package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// contextLines are the `context` lines doctor prints for the crew file at path, as "level text".
func contextLines(t *testing.T, path string) []string {
	t.Helper()

	c, err := crew.Load(path)
	require.NoError(t, err)

	var lines []string

	checkContext(c, func(name, level, format string, args ...any) {
		assert.Equal(t, "context", name)
		lines = append(lines, level+" "+fmt.Sprintf(format, args...))
	})

	return lines
}

// A crew at the recommendation gets one ok line with the effective values; the dispatcher, never restarted for its
// context, does not count for the restart keys.
func TestDoctorContextAtTheRecommendation(t *testing.T) {
	path := testCrew(t, `  dev:
  ops:
    context_max: 200k
    restart_at: 100k
context_max: 250k
restart_at: 180k
restart_done: 60k
`)

	assert.Equal(t, []string{"ok Claude roles at or below the recommended values: context_max 200k to 250k, " +
		"restart_at 100k to 180k, restart_done 60k"}, contextLines(t, path))
}

// Above it at crew level: a line for each key, with the value, the recommendation and the key to set. A key left
// unset says so: context_max has no default, restart_at and restart_done have.
func TestDoctorContextAboveAtCrewLevel(t *testing.T) {
	path := testCrew(t, `  dev:
context_max: 400k
`)

	assert.Equal(t, []string{
		"warn context_max is 400k, above the recommended 250k: set context_max: 250k at the crew file's top level",
		"warn restart_at is 250k (the default), above the recommended 180k: set restart_at: 180k at the crew file's top level",
		"warn restart_done is 150k (the default), above the recommended 60k: set restart_done: 60k at the crew file's top level",
	}, contextLines(t, path))

	path = testCrew(t, `  dev:
restart_at: 300k
restart_done: 90k
`)

	assert.Equal(t, []string{
		"warn context_max is not set, so there is no limit; recommended 250k: set context_max: 250k at the crew file's top level",
		"warn restart_at is 300k, above the recommended 180k: set restart_at: 180k at the crew file's top level",
		"warn restart_done is 90k, above the recommended 60k: set restart_done: 60k at the crew file's top level",
	}, contextLines(t, path))
}

// A role's own value past the recommendation is listed by name in one line, with no line of its own and no ok line;
// a role's own value within it is left out; roles that set none are not named.
func TestDoctorContextRoleWithOwnHigherValue(t *testing.T) {
	path := testCrew(t, `  dev:
    context_max: 400k
    restart_at: off
  ops:
    restart_done: 100k
  qa:
    restart_at: 100k
  plain:
context_max: 250k
restart_at: 180k
restart_done: 60k
`)

	assert.Equal(t, []string{"warn own values above the recommendation: dev (context_max 400k, restart_at off), " +
		"ops (restart_done 100k); if deliberate, leave them, else zmina seat set <role> context_max=250k " +
		"restart_at=180k restart_done=60k (the keys named)"}, contextLines(t, path))
}

// A crew-level value past it is said once, and a role that overrides it is not said to inherit it: here every role
// that restarts sets its own restart_done, so only context_max is the crew's to fix.
func TestDoctorContextCrewLevelOnlyForRolesThatInherit(t *testing.T) {
	path := testCrew(t, `  dev:
    restart_at: 100k
    restart_done: 50k
context_max: 400k
restart_at: 300k
restart_done: 120k
`)

	assert.Equal(t, []string{
		"warn context_max is 400k, above the recommended 250k: set context_max: 250k at the crew file's top level",
	}, contextLines(t, path))
}

// Keys off are past every recommendation: crew-wide and role by role.
func TestDoctorContextKeysOff(t *testing.T) {
	path := testCrew(t, `  dev:
restart_at: off
restart_done: off
`)

	assert.Equal(t, []string{
		"warn context_max is not set, so there is no limit; recommended 250k: set context_max: 250k at the crew file's top level",
		"warn restart_at is off; recommended 180k: set restart_at: 180k at the crew file's top level",
		"warn restart_done is off; recommended 60k: set restart_done: 60k at the crew file's top level",
	}, contextLines(t, path))

	path = testCrew(t, `  dev:
    context_max: off
    restart_done: off
context_max: 250k
restart_at: 180k
restart_done: 60k
`)

	assert.Equal(t, []string{"warn own values above the recommendation: dev (context_max off, restart_done off); " +
		"if deliberate, leave them, else zmina seat set <role> context_max=250k restart_at=180k restart_done=60k " +
		"(the keys named)"}, contextLines(t, path))
}

// The dispatcher is measured by context_max (it compacts it) but not by the restart keys (it is never restarted for
// its context); a Codex role is not measured at all, whatever it sets.
func TestDoctorContextSkipsWhatDoesNotApply(t *testing.T) {
	path := testCrew(t, `  dev:
    runtime: codex
    context_max: 1m
    restart_at: off
context_max: 250k
restart_at: 180k
restart_done: 60k
`)

	assert.Equal(t, []string{"ok Claude roles at or below the recommended values: context_max 250k"},
		contextLines(t, path))

	path = filepath.Join(t.TempDir(), "crew.yaml")
	require.NoError(t, os.WriteFile(path, []byte("dir: "+t.TempDir()+
		"\nroles:\n  dispatcher:\n    runtime: codex\n  dev:\n    runtime: codex\n    context_max: 1m\n"), 0o600))

	assert.Equal(t, []string{"ok no Claude role to measure against the recommended context values"}, contextLines(t, path))
}

// doctor prints the area between boot and the ticker, in its line style, for the crew file given.
func TestDoctorPrintsContextLines(t *testing.T) {
	crewFile := testCrew(t, `  dev:
context_max: 400k
`)
	fakeAuth(t, filepath.Dir(crewFile), `{"loggedIn": true}`)

	out, _, _ := zm(t, crewFile, "doctor")
	assert.Contains(t, out, "warn  context   context_max is 400k, above the recommended 250k: set context_max: 250k at "+
		"the crew file's top level\n")
	assert.Contains(t, out, "warn  context   restart_at is 250k (the default), above the recommended 180k: set "+
		"restart_at: 180k at the crew file's top level\n")
	assert.NotContains(t, out, "ok    context")

	crewFile = testCrew(t, `  dev:
context_max: 250k
restart_at: 180k
restart_done: 60k
`)
	fakeAuth(t, filepath.Dir(crewFile), `{"loggedIn": true}`)

	out, _, _ = zm(t, crewFile, "doctor")
	assert.Contains(t, out, "ok    context   Claude roles at or below the recommended values: context_max 250k, "+
		"restart_at 180k, restart_done 60k\n")
	assert.NotContains(t, out, "warn  context")

	out, _, _ = zm(t, crewFile, "doctor", "--json")
	assert.Contains(t, out, `"name": "context"`)
}
