package cmd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
)

func TestUsageShowsRestarts(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n  backend:\n")
	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	log := eventLog(c)
	now := time.Now()
	for _, e := range []events.Event{
		{TS: now.Add(-time.Hour), Role: "dev", Event: "restarted", Task: "T-1", Detail: "context 262k over restart_at 250k; note T-1 7b2b6944f4c7"},
		{TS: now.Add(-50 * time.Minute), Role: "dev#2", Event: "restarted", Task: "T-2", Detail: "idle 51m at 140k, near restart_idle 1h; note T-2 aa"},
		{TS: now.Add(-40 * time.Minute), Role: "backend", Event: "restarted", Task: "T-3", Detail: "context 300k over restart_at 250k; note T-3 bb"},
		{TS: now.Add(-30 * time.Minute), Role: "backend", Event: "restart-asked", Task: "T-3", Detail: "context 300k over restart_at 250k"},
		{TS: now.Add(-48 * time.Hour), Role: "backend", Event: "restarted", Task: "T-0", Detail: "context 300k over restart_at 250k; note T-0 cc"},
	} {
		require.NoError(t, log.Add(e))
	}

	out, _, err := zm(t, crewFile, "usage")
	require.NoError(t, err)
	assert.Contains(t, out, "Restarted fresh with their note 3 times: dev 2 (context 1, idle 1), backend 1 (context 1, idle 0).",
		"a copy's with its role's; only the window's")

	out, _, err = zm(t, crewFile, "usage", "--role", "backend")
	require.NoError(t, err)
	assert.Contains(t, out, "Restarted fresh with their note once: backend 1")

	out, _, err = zm(t, crewFile, "usage", "--json")
	require.NoError(t, err)
	var report usageReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Len(t, report.Restarts, 3)
	assert.Equal(t, "context 262k over restart_at 250k", report.Restarts[0].Why)
	assert.Equal(t, "T-1", report.Restarts[0].Task)

	out, _, err = zm(t, crewFile, "usage", "--all", "--local", "--cells")
	require.NoError(t, err)
	var pc peerCells
	require.NoError(t, json.Unmarshal([]byte(out), &pc))
	assert.Len(t, pc.Restarts, 3, "a peer answers --all with its restarts too")
}
