package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/cost"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
)

func TestMorningLine(t *testing.T) {
	day := time.Date(2026, 10, 26, 0, 0, 0, 0, time.Local)
	cell := func(d time.Time, role string, turns, ctx int, usd float64) cost.Cell {
		return cost.Cell{Role: role, Day: d.Format(time.DateOnly), Turns: turns, Read: turns * ctx, Cost: usd,
			Active: turns / 10}
	}
	before := day.AddDate(0, 0, -1)
	cells := []cost.Cell{
		cell(day, "dev", 100, 300_000, 12), cell(before, "dev", 100, 200_000, 6),
		cell(day, "ops", 40, 50_000, 2), cell(before, "ops", 40, 45_000, 1.9),
		cell(day.AddDate(0, 0, 1), "dev", 500, 900_000, 99), // today: not yesterday's
	}
	cells[0].Compacts = 2
	stretches := []usageStretch{{Role: "dev", Stretches: []int64{cost.StretchOf(day.Add(time.Hour)),
		cost.StretchOf(day.Add(2 * time.Hour)), cost.StretchOf(before)}}}
	fills := []usageFill{{Role: "dev", Fill: registry.Fill{Tool: "Bash: cat/sed/head/tail", Chars: 300}},
		{Role: "dev", Fill: registry.Fill{Tool: "Read", Chars: 100}}}

	assert.Equal(t, "morning: yesterday $14.00 API-equivalent, 140 turns, 0.2 h at work (7.0 sessions at once); "+
		"context per turn: dev 300k, ops 50k; filled by cat/sed/head/tail 75%, Read 25%; "+
		"jumped: dev $0.060 -> $0.120 a turn, dev context 200k -> 300k; compacted 2 "+
		"(zmina usage --since 24h, --by ctx, --by tool)", morningLine(cells, fills, stretches, day))

	assert.Empty(t, morningLine(cells[:2], nil, nil, day.AddDate(0, 0, -5)), "nobody worked that day")

	few := []cost.Cell{cell(day, "dev", 10, 300_000, 3), cell(before, "dev", 10, 100_000, 1)}
	assert.NotContains(t, morningLine(few, nil, nil, day), "jumped", "too few turns to call it a jump")
}

func TestMorningTick(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	crewFile := testCrew(t, "  dev:\n")
	f, err := os.OpenFile(crewFile, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString("timezone: America/New_York\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	dir := filepath.Dir(crewFile)
	proj := filepath.Join(dir, "ns", "projects", "-work")
	require.NoError(t, os.MkdirAll(proj, 0o700))

	// The crew's clock, not the host's: 1 November 2026 has 25 hours there (the clocks go back at 02:00).
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)

	reply := func(at time.Time, id string) string {
		return `{"type":"assistant","timestamp":"` + at.UTC().Format(time.RFC3339) + `","cwd":"/work","message":{"id":"` +
			id + `","model":"claude-opus-5-5","usage":{"input_tokens":4,"cache_creation_input_tokens":0,` +
			`"cache_read_input_tokens":200000,"output_tokens":2000}}}` + "\n"
	}

	name := strings.TrimPrefix(strings.SplitN(string(mustRead(t, crewFile)), "\n", 2)[0], "name: ")
	body := `{"type":"agent-name","agentName":"` + name + `-dev"}` + "\n" +
		reply(time.Date(2026, 10, 31, 23, 30, 0, 0, ny), "m1") + // yesterday there, already 1 November in UTC
		reply(time.Date(2026, 11, 1, 0, 30, 0, 0, ny), "m2") // today there
	require.NoError(t, os.WriteFile(filepath.Join(proj, "s1.jsonl"), []byte(body), 0o600))

	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	state := func() *shift.State {
		st, err := shift.Read(dir)
		require.NoError(t, err)

		return st
	}

	a.morningTick(time.Date(2026, 11, 1, 7, 30, 0, 0, ny))
	assert.Empty(t, state().Notes, "07:30 there: 8.5 hours past midnight, but not yet 08:00")

	a.morningTick(time.Date(2026, 11, 1, 8, 0, 0, 0, ny))
	require.Len(t, state().Notes, 1)
	assert.True(t, strings.HasPrefix(state().Notes[0], "morning: yesterday $0.080 API-equivalent, 1 turns"),
		"yesterday is the crew's day: %s", state().Notes[0])
	assert.Equal(t, "2026-10-31", state().MorningDay)

	a.morningTick(time.Date(2026, 11, 1, 10, 0, 0, 0, ny))
	assert.Len(t, state().Notes, 1, "once a day")
}

func TestMorningTickBrokenCrew(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n")
	require.NoError(t, os.WriteFile(crewFile, []byte("roles: [oops\n"), 0o600))

	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	assert.NotPanics(t, func() { a.morningTick(time.Now()) }, "a crew file left broken mid-edit stops no beat")
}
