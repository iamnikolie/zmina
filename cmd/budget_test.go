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
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

func TestCrossed(t *testing.T) {
	cells := []cost.Cell{
		{Role: "dev", Task: "T-1", Day: "2026-10-26", Cost: 12},
		{Role: "dev", Task: "T-1", Day: "2026-10-25", Cost: 30},
		{Role: "ops", Task: "shared T-1+T-2", Day: "2026-10-26", Cost: 4},
	}

	got := crossed(cells, map[string]float64{"dev": 10, "ops": 5},
		[]task.Task{{ID: "T-1", Role: "dev", Budget: 40}, {ID: "T-2", Role: "ops", Budget: 1}}, "2026-10-26")

	assert.Equal(t, []over{
		{key: "role:dev:2026-10-26", note: "budget: dev spent $12.00 today (API-equivalent), over its $10.00 a day"},
		{key: "task:T-1:40", note: "budget: T-1 (dev) cost $42.00 so far (API-equivalent), over its $40.00"},
	}, got, "a day's spend counts for a role, a task's own turns for it — not the ones it shares")
}

func TestBudgetTick(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	crewFile := testCrew(t, "  dev:\n    budget: 0.05\n")
	dir := filepath.Dir(crewFile)
	proj := filepath.Join(dir, "ns", "projects", "-work")
	require.NoError(t, os.MkdirAll(proj, 0o700))

	_, _, err := zm(t, crewFile, "task", "new", "dev", "--budget", "0.01", "fix", "the", "login")
	require.NoError(t, err)

	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))

	name := strings.TrimPrefix(strings.SplitN(string(mustRead(t, crewFile)), "\n", 2)[0], "name: ")
	body := `{"type":"agent-name","agentName":"` + name + `-dev"}` + "\n" + `{"type":"assistant","timestamp":"` +
		time.Now().UTC().Format(time.RFC3339) + `","cwd":"/work","message":{"id":"m1","model":"claude-opus-5-5",` +
		`"usage":{"input_tokens":4,"cache_creation_input_tokens":0,"cache_read_input_tokens":200000,` +
		`"output_tokens":2000}}}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(proj, "s1.jsonl"), []byte(body), 0o600))

	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	now := time.Now()
	notes := func() []string {
		st, err := shift.Read(dir)
		require.NoError(t, err)

		return st.Notes
	}

	a.budgetTick(now)
	assert.Equal(t, []string{
		"budget: dev spent $0.080 today (API-equivalent), over its $0.050 a day",
		"budget: T-1 (dev) cost $0.080 so far (API-equivalent), over its $0.010",
	}, notes())

	shiftSaved := func(fn func(s *shift.Shift)) {
		require.NoError(t, a.withState(func(s *shift.Shift) error { fn(s); return nil }))
	}
	shiftSaved(func(s *shift.Shift) { s.State.Notes = nil })

	a.budgetTick(now.Add(budgetEvery))
	assert.Empty(t, notes(), "each crossed budget is told once")

	out, _, err := zm(t, crewFile, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "· over its $0.010 budget")

	_, _, err = zm(t, crewFile, "task", "set", "1", "--budget", "5")
	require.NoError(t, err)

	out, _, err = zm(t, crewFile, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "· within its $5.00 budget")
	assert.Contains(t, out, "task  budget $5")

	_, _, err = zm(t, crewFile, "task", "set", "1", "--budget", "lots")
	assert.ErrorContains(t, err, "a number of dollars")
}

func TestNoBudgetReadsNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	crewFile := testCrew(t, "  dev:\n")
	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	a.budgetTick(time.Now())

	st, err := shift.Read(filepath.Dir(crewFile))
	require.NoError(t, err)
	assert.True(t, st.BudgetAt.IsZero(), "no budget: the check never ran")
}
