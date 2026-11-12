package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
)

// rotationLinesAt is a rotation's record that began at a time: prepared, then switching and started a second later, then what
// else is given (lines of the record, written as they are with the id and the time after the start).
func rotationLinesAt(id, role, kind, why string, at time.Time, rest ...string) []string {
	f := func(d time.Duration) string { return at.Add(d).UTC().Format(time.RFC3339Nano) }
	lines := []string{
		fmt.Sprintf(`{"id":%q,"at":%q,"phase":"prepared","role":%q,"kind":%q,"why":%q,"from":{"runtime":"claude","session":"sid-old"},"artifacts":[{"task":"T-1"}]}`,
			id, f(0), role, kind, why),
		fmt.Sprintf(`{"id":%q,"at":%q,"phase":"switching","claim":%q}`, id, f(time.Second), f(time.Second)),
		fmt.Sprintf(`{"id":%q,"at":%q,"phase":"started"}`, id, f(2*time.Second)),
	}

	for i, r := range rest {
		lines = append(lines, fmt.Sprintf(`{"id":%q,"at":%q,%s}`, id, f(time.Duration(3+i)*time.Second), r))
	}

	return lines
}

// A role with a rotation in flight says so where its status is, and one whose rotation ended in ls --wide and ls <role>; a role
// never rotated says nothing; --json has the field only where there is something (an older reader meets no new key).
func TestLsShowsTheRotations(t *testing.T) {
	file := testCrew(t, "  dev: {}\n  lead: {}\n  sage: {}\n")

	_, _, err := zm(t, file, "up", "dev", "lead")
	require.NoError(t, err)
	screenHas(t, file, "dev", "claude ")
	screenHas(t, file, "lead", "claude ")

	now := time.Now()
	rotationRecord(t, file, "dev", rotationLinesAt("open0001", "dev", "restart_at", "context 262k over restart_at 250k", now.Add(-2*time.Minute-10*time.Second))...)
	rotationRecord(t, file, "lead", append(rotationLinesAt("done0002", "lead", "manual", "by hand (zmina restart)", now.Add(-3*time.Hour),
		`"what":"boot","by":"lead"`), fmt.Sprintf(`{"id":"done0002","at":%q,"phase":"closed","outcome":"rebuilt","confirmed":["boot"]}`,
		now.Add(-3*time.Hour+time.Minute).UTC().Format(time.RFC3339Nano)))...)

	out, _, err := zm(t, file, "ls")
	require.NoError(t, err)
	assert.Regexp(t, `dev\s+up rotating open0001 \(started 2m\)`, out)
	assert.NotRegexp(t, `lead\s+up [^\n]*rotating`, out, "a rotation that ended is not in the table")
	assert.NotContains(t, out, "done0002")

	out, _, err = zm(t, file, "ls", "--wide")
	require.NoError(t, err)
	assert.Contains(t, out, "dev last rotation: rotating open0001 (started 2m) (restart_at)")
	assert.Regexp(t, `lead last rotation: rebuilt \d\d:\d\d \(manual\)`, out)
	assert.NotContains(t, out, "sage last rotation")

	out, _, err = zm(t, file, "ls", "lead")
	require.NoError(t, err)
	assert.Regexp(t, `lead last rotation: rebuilt \d\d:\d\d \(manual\)`, out)
	assert.NotContains(t, out, "dev last rotation")

	out, _, err = zm(t, file, "ls", "--json")
	require.NoError(t, err)

	var views []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &views))

	byRole := map[string]map[string]any{}
	for _, v := range views {
		byRole[v["role"].(string)] = v
	}

	rot, ok := byRole["dev"]["rotation"].(map[string]any)
	require.True(t, ok, "%v", byRole["dev"])
	assert.Equal(t, "open0001", rot["id"])
	assert.Equal(t, true, rot["open"])
	assert.Equal(t, "started", rot["phase"])

	rot = byRole["lead"]["rotation"].(map[string]any)
	assert.Equal(t, "rebuilt", rot["outcome"])
	assert.NotContains(t, rot, "open")

	assert.NotContains(t, byRole["sage"], "rotation", "none: no key")
}

// A down role whose rotation is still open (no session was started) has it in the down line.
func TestLsShowsTheRotationOfADownRole(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	rotationRecord(t, file, "dev", rotationLinesAt("open0003", "dev", "manual", "by hand (zmina restart)", time.Now().Add(-10*time.Minute-10*time.Second))...)

	out, _, err := zm(t, file, "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "dev (rotating open0003 (started 10m))")
}

// The restarts usage counts say how they ended, where the rotation records tell: matched by role, reason and the time of the start;
// a restart with no record is counted as such, and a crew with none is unchanged.
func TestUsageSplitsRestartsByOutcome(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n  backend:\n")
	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	now := time.Now()
	log := eventLog(c)

	for _, e := range []events.Event{
		{TS: now.Add(-3 * time.Hour), Role: "dev", Event: "restarted", Task: "T-1", Detail: "context 262k over restart_at 250k; note T-1 aa"},
		{TS: now.Add(-2 * time.Hour), Role: "dev", Event: "restarted", Task: "T-1", Detail: "context 262k over restart_at 250k; note T-1 bb"},
		{TS: now.Add(-1 * time.Hour), Role: "dev#2", Event: "restarted", Task: "T-2", Detail: "idle 51m at 140k, near restart_idle 1h; note T-2 cc"},
		{TS: now.Add(-30 * time.Minute), Role: "backend", Event: "restarted", Task: "T-3", Detail: "context 300k over restart_at 250k; note T-3 dd"},
	} {
		require.NoError(t, log.Add(e))
	}

	closed := func(id, outcome string, at time.Time) string {
		return fmt.Sprintf(`{"id":%q,"at":%q,"phase":"closed","outcome":%q}`, id, at.UTC().Format(time.RFC3339Nano), outcome)
	}

	// dev: one rebuilt, one lost, each started with its event; dev#2: one still open; backend: no record.
	rotationRecord(t, crewFile, "dev", append(append(
		rotationLinesAt("r1", "dev", "restart_at", "context 262k over restart_at 250k", now.Add(-3*time.Hour)),
		closed("r1", "rebuilt", now.Add(-3*time.Hour+time.Minute))),
		append(rotationLinesAt("r2", "dev", "restart_at", "context 262k over restart_at 250k", now.Add(-2*time.Hour)),
			closed("r2", "lost", now.Add(-2*time.Hour+10*time.Minute)))...)...)
	rotationRecord(t, crewFile, "dev#2", rotationLinesAt("r3", "dev#2", "restart_idle", "idle 51m at 140k, near restart_idle 1h", now.Add(-time.Hour))...)

	out, _, err := zm(t, crewFile, "usage")
	require.NoError(t, err)
	assert.Contains(t, out, "Restarted fresh 4 times: dev 3 (context 2, idle 1; rebuilt 1, lost 1, open 1), backend 1 (context 1, idle 0).", out)

	out, _, err = zm(t, crewFile, "usage", "--json")
	require.NoError(t, err)

	var report usageReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Len(t, report.Restarts, 4)

	got := map[string]string{}
	for _, r := range report.Restarts {
		got[r.Task+r.Why[:4]] = r.Outcome
	}

	assert.Equal(t, "lost", got["T-1cont"], "the later of the two, the one the later record is of")
	assert.Equal(t, "open", got["T-2idle"])
	assert.Empty(t, got["T-3cont"])
}

// doctor says of a rotation open for over an hour, which nothing is following; of a younger one, a closed one and a compact it
// says nothing.
func TestDoctorSaysOfARotationOpenTooLong(t *testing.T) {
	file := testCrew(t, "  dev: {}\n  lead: {}\n  sage: {}\n")

	out, _, _ := zm(t, file, "doctor")
	assert.NotContains(t, out, "rotations")

	now := time.Now()
	rotationRecord(t, file, "dev", rotationLinesAt("old00001", "dev", "restart_at", "context 262k over restart_at 250k", now.Add(-80*time.Minute))...)
	rotationRecord(t, file, "lead", rotationLinesAt("new00002", "lead", "restart_at", "context 262k over restart_at 250k", now.Add(-20*time.Minute))...)
	rotationRecord(t, file, "sage", rotationLinesAt("cmp00003", "sage", "compact", "context 200k over 150k", now.Add(-5*time.Hour))...)

	out, _, _ = zm(t, file, "doctor")
	assert.Contains(t, out, "warn  rotations dev: rotation old00001 (restart_at) has been open for 1h, started: ")
	assert.Contains(t, out, "was not confirmed to have its context, and the role may be at work in it; zmina rotation show dev")

	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, " rotations ") {
			assert.NotContains(t, line, "abort", "nothing destructive is hinted at: the role may be at work")
			assert.NotContains(t, line, "--resume")
		}
	}

	assert.NotContains(t, out, "new00002")
	assert.NotContains(t, out, "cmp00003")

	path := filepath.Join(filepath.Dir(file), "rotations", "dev.jsonl")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	_, _, _ = zm(t, file, "doctor")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "doctor writes nothing")
	assert.False(t, strings.Contains(string(after), "closed"))
}
