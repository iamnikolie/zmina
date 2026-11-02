package cmd

import (
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexUsageAndBudgets(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	crewFile := testCrew(t, "  dev:\n    runtime: codex\n    budget: 0.01\n")
	_, _, err := zm(t, crewFile, "task", "new", "dev", "--budget", "0.01", "fix", "login")
	require.NoError(t, err)
	reg := codex.Registry{Dir: filepath.Join(filepath.Dir(crewFile), "codex")}
	at := time.Now().Add(time.Second)
	u := codex.Usage{Thread: "thread", Turn: "turn", Role: "dev", Group: "dev", Charge: true,
		Total: codex.Tokens{Input: 100_000, Cached: 20_000, Output: 1000}, Last: codex.Tokens{Input: 100_000, Cached: 20_000, Output: 1000},
		Message: registry.Message{At: at, Model: "gpt-6.1-sol", Cwd: "/work", Trigger: registry.TriggerHandoff}}
	require.NoError(t, reg.SaveUsage(u))
	require.NoError(t, reg.SaveUsage(u))
	// No process registry entry exists: billing remains available after down.
	out, _, err := zm(t, crewFile, "usage", "--json")
	require.NoError(t, err)
	var rep usageReport
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	require.Len(t, rep.Rows, 1)
	assert.Equal(t, "dev", rep.Rows[0].Key)
	assert.Equal(t, 1, rep.Total.Turns)
	assert.InDelta(t, .172, rep.Total.Cost, 1e-9)
	assert.Empty(t, rep.Missing)
	out, _, err = zm(t, crewFile, "usage", "--by", "task")
	require.NoError(t, err)
	assert.Contains(t, out, "T-1")
	out, _, err = zm(t, crewFile, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "cost: $0.172")
	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	a.budgetTick(at.Add(time.Second))
	st, err := shift.Read(filepath.Dir(crewFile))
	require.NoError(t, err)
	require.Len(t, st.Notes, 2)
	assert.Contains(t, st.Notes[0], "budget: dev spent $0.172")
	assert.Contains(t, st.Notes[1], "budget: T-1 (dev) cost $0.172")
}
