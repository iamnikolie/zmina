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
	dir := filepath.Dir(crewFile)
	proj := filepath.Join(dir, "ns", "projects", "-work")
	require.NoError(t, os.MkdirAll(proj, 0o700))

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	yesterday := today.Add(-12 * time.Hour)

	name := strings.TrimPrefix(strings.SplitN(string(mustRead(t, crewFile)), "\n", 2)[0], "name: ")
	body := `{"type":"agent-name","agentName":"` + name + `-dev"}` + "\n" + `{"type":"assistant","timestamp":"` +
		yesterday.UTC().Format(time.RFC3339) + `","cwd":"/work","message":{"id":"m1","model":"claude-opus-5-5",` +
		`"usage":{"input_tokens":4,"cache_creation_input_tokens":0,"cache_read_input_tokens":200000,` +
		`"output_tokens":2000}}}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(proj, "s1.jsonl"), []byte(body), 0o600))

	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	state := func() *shift.State {
		st, err := shift.Read(dir)
		require.NoError(t, err)

		return st
	}

	a.morningTick(today.Add(7 * time.Hour))
	assert.Empty(t, state().Notes, "not before the crew's morning")

	a.morningTick(today.Add(9 * time.Hour))
	require.Len(t, state().Notes, 1)
	assert.True(t, strings.HasPrefix(state().Notes[0], "morning: yesterday $0.080 API-equivalent, 1 turns"), state().Notes[0])
	assert.Equal(t, yesterday.Format(time.DateOnly), state().MorningDay)

	a.morningTick(today.Add(10 * time.Hour))
	assert.Len(t, state().Notes, 1, "once a day")
}
