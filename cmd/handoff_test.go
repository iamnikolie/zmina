package cmd

import (
	"encoding/json"
	"github.com/iamnikolie/zmina/internal/handoff"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestHandoffIncludesCurrentNoteAndReassignmentQueuesPacket(t *testing.T) {
	crew := testCrew(t, "  designer:\n    handoff:\n      sections:\n        - name: decisions\n          guidance: Explain the choice\n  support: {}\n")
	_, _, err := zm(t, crew, "task", "new", "designer", "First line\nSecond line", "--note", "Latest decision")
	require.NoError(t, err)
	raw, _, err := zm(t, crew, "task", "handoff", "1", "--json")
	require.NoError(t, err)
	var p handoff.Packet
	require.NoError(t, json.Unmarshal([]byte(raw), &p))
	assert.Equal(t, "Latest decision", p.Task.Note)
	assert.Equal(t, "decisions", p.Shape.Sections[0].Name)
	_, _, err = zm(t, crew, "up", "designer", "--task", "1")
	require.NoError(t, err)
	_, _, err = zm(t, crew, "task", "set", "1", "--role", "support", "--note", "Answer still needed")
	require.NoError(t, err)
	out, _, err := zm(t, crew, "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "[zmina T-1] Read handoff packet")
	assert.NotContains(t, out, "Second line")
	assert.Equal(t, 1, strings.Count(out, "Read handoff packet"), "old assignment capsule removed")
}

func TestNewTaskQueuesPacketForRunningRole(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	_, _, err := zm(t, file, "up", "dev")
	require.NoError(t, err)
	raw, _, err := zm(t, file, "task", "new", "dev", "Review the change", "--note", "Current scope", "--json")
	require.NoError(t, err)
	var got task.Task
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	require.NotNil(t, got.Delivery, "an already running role needs a handoff to accept")
	assert.Equal(t, "dev", got.Delivery.Role)
	assert.Zero(t, got.Delivery.SentAt, "queued does not mean submitted")
	out, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(out, got.Delivery.Line))
	raw, _, err = zm(t, file, "task", "handoff", "1", "--json")
	require.NoError(t, err)
	var p handoff.Packet
	require.NoError(t, json.Unmarshal([]byte(raw), &p))
	assert.Equal(t, "Current scope", p.Task.Note)
	assert.Equal(t, "Review the change", p.Task.Asked)
}
