package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivity(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	at := func(m int) string { return time.Date(2026, 11, 9, 10, m, 0, 0, time.UTC).Format(time.RFC3339) }
	lines := []string{
		// The boot line, then the owner from Chat.
		`{"type":"user","timestamp":"` + at(0) + `","message":{"role":"user","content":"read AGENTS.md"}}`,
		`{"type":"user","timestamp":"` + at(1) + `","message":{"role":"user","content":"[zmina chat 0123456789abcdef] go on"}}`,
		// Three background launches: a shell, a Monitor, an agent; a plain tool result and a foreground agent.
		`{"type":"user","timestamp":"` + at(2) + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"running"}]},"toolUseResult":{"stdout":"","backgroundTaskId":"b1"}}`,
		`{"type":"user","timestamp":"` + at(2) + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"watching"}]},"toolUseResult":{"taskId":"m1","persistent":false}}`,
		`{"type":"user","timestamp":"` + at(2) + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t3","content":"launched"}]},"toolUseResult":{"agentId":"a1","isAsync":true,"status":"async_launched"}}`,
		`{"type":"user","timestamp":"` + at(2) + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t4","content":"done"}]},"toolUseResult":{"agentId":"a2","status":"completed"}}`,
		// The shell ends between turns; the Monitor reports an event (no status: it watches on).
		`{"type":"user","timestamp":"` + at(3) + `","origin":{"kind":"task-notification"},"message":{"role":"user","content":"<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n</task-notification>"}}`,
		`{"type":"user","timestamp":"` + at(4) + `","message":{"role":"user","content":"<task-notification>\n<task-id>m1</task-id>\n<summary>Monitor event</summary>\n</task-notification>"}}`,
		// zmina's lines and a peer's message are not the owner's.
		`{"type":"user","timestamp":"` + at(5) + `","message":{"role":"user","content":"[zmina] restart: read the file"}}`,
		`{"type":"user","timestamp":"` + at(6) + `","origin":{"kind":"peer","name":"zm-dispatcher"},"message":{"role":"user","content":"[zmina:abcdefghijkl] [zmina T-1] Read handoff packet"}}`,
		// A slash command zmina typed is not the owner either.
		`{"type":"user","timestamp":"` + at(7) + `","message":{"role":"user","content":"<command-name>/compact</command-name>"}}`,
		// Mid-turn, the agent's end comes as an attachment; a sidechain line is a subagent's own.
		`{"type":"attachment","timestamp":"` + at(8) + `","attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"<task-notification>\n<task-id>a1</task-id>\n<status>completed</status>\n</task-notification>"}}`,
		`{"type":"user","isSidechain":true,"timestamp":"` + at(9) + `","message":{"role":"user","content":"hello from a subagent"}}`,
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600))

	r := &Registry{Namespace: ns}
	a, err := r.Activity("s1")
	require.NoError(t, err)
	assert.Equal(t, []string{"m1"}, a.Background, "the Monitor still watches")
	assert.Equal(t, at(1), a.Owner.Format(time.RFC3339), "the owner's Chat line")
	assert.Equal(t, at(7), a.Prompt.Format(time.RFC3339), "the last line that began a turn")

	// The owner's line typed mid-turn is his too.
	owner := `{"type":"attachment","timestamp":"` + at(10) + `","attachment":{"type":"queued_command","commandMode":"prompt","prompt":"and the footer"}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(strings.Join(append(lines, owner), "\n")+"\n"), 0o600))
	a, err = r.Activity("s1")
	require.NoError(t, err)
	assert.Equal(t, at(10), a.Owner.Format(time.RFC3339))

	a, err = r.Activity("missing")
	require.NoError(t, err)
	assert.Equal(t, Activity{}, a)
}
