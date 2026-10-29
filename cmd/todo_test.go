package cmd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

func TestTodoFlow(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n")

	out, _, err := zm(t, crewFile, "todo", "ls")
	require.NoError(t, err)
	assert.Equal(t, "nothing waits on you\n", out)

	out, _, err = zm(t, crewFile, "todo", "add", "--from", "dispatcher", "--rec", "keep the old name",
		"--reason", "links point at it", "rename", "the", "flag?")
	require.NoError(t, err)
	assert.Equal(t, "A-1\n", out)

	out, _, err = zm(t, crewFile, "todo", "add", "--blocks", "dev", "sign in and publish the test board")
	require.NoError(t, err)
	assert.Equal(t, "A-2\n", out)

	_, _, err = zm(t, crewFile, "task", "new", "dev", "ship", "it")
	require.NoError(t, err)
	_, _, err = zm(t, crewFile, "task", "set", "T-1", "waiting", "--note", "which region?")
	require.NoError(t, err)

	out, _, err = zm(t, crewFile, "todo", "ls", "--json")
	require.NoError(t, err)

	var got struct {
		Items []todoItem `json:"items"`
	}

	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Items, 3)
	assert.Equal(t, []string{"A-2", "A-1", "T-1"}, []string{got.Items[0].ID, got.Items[1].ID, got.Items[2].ID},
		"blocking first, oldest first inside a group")
	assert.Equal(t, "check", got.Items[0].Kind)
	assert.Equal(t, "decision", got.Items[1].Kind)
	assert.Equal(t, "waiting", got.Items[2].Kind)
	assert.Equal(t, "which region?", got.Items[2].Note)
	assert.Empty(t, got.Items[2].Blocks, "a waiting task whose role is down holds nothing up")

	_, _, err = zm(t, crewFile, "todo", "yes", "A-2")
	require.ErrorContains(t, err, "no recommendation", "a check has nothing to say yes to")

	out, _, err = zm(t, crewFile, "todo", "yes", "1")
	require.NoError(t, err)
	assert.Equal(t, "A-1 · yes · queued for the dispatcher\n", out)

	_, _, err = zm(t, crewFile, "todo", "yes", "A-1")
	require.ErrorContains(t, err, "already yes")

	_, _, err = zm(t, crewFile, "todo", "answer", "T-1", "eu-west")
	require.NoError(t, err)

	_, _, err = zm(t, crewFile, "todo", "done", "T-1")
	require.ErrorContains(t, err, "answer it or park it")

	out, _, err = zm(t, crewFile, "todo", "park", "A-2")
	require.NoError(t, err)
	assert.Contains(t, out, "A-2 · park")

	out, _, err = zm(t, crewFile, "later", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "sign in and publish the test board · from A-2")

	out, _, err = zm(t, crewFile, "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "hand: A-1 · yes: keep the old name — rename the flag?")
	assert.Contains(t, out, "hand: T-1 · eu-west")
	assert.Contains(t, out, "hand: A-2 · later (L-1)")

	out, _, err = zm(t, crewFile, "todo", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "T-1", "a waiting task stays until its state changes")
	assert.NotContains(t, out, "A-")
	assert.Contains(t, out, "later: 1 parked")
}

func TestWaitingTaskBlocksOnlyAnIdleRole(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n")

	_, _, err := zm(t, crewFile, "task", "new", "dev", "ship", "it")
	require.NoError(t, err)
	_, _, err = zm(t, crewFile, "task", "set", "T-1", "waiting")
	require.NoError(t, err)

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	for _, tc := range []struct {
		view   shift.View
		blocks string
	}{
		{shift.View{Role: "dev", State: shift.PresenceUp, Status: "idle"}, "dev"},
		{shift.View{Role: "dev", State: shift.PresenceUp, Status: "busy"}, ""},
		{shift.View{Role: "dev", State: shift.PresenceDown}, ""},
	} {
		items, _ := todo(c, time.Now(), []shift.View{tc.view})
		require.Len(t, items, 1)
		assert.Equal(t, tc.blocks, items[0].Blocks, "%s %s", tc.view.State, tc.view.Status)
	}
}
