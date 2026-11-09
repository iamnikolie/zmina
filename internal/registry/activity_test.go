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
		// The boot line (zmina's: its rules carry BootMark), then the owner from Chat.
		`{"type":"user","timestamp":"` + at(0) + `","origin":{"kind":"human"},"message":{"role":"user","content":"read AGENTS.md\n\n` + BootMark + ` ask him"}}`,
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
		// zmina's tagged line mid-turn: a peer's by its origin, whatever its text starts with.
		`{"type":"attachment","timestamp":"` + at(9) + `","attachment":{"type":"queued_command","commandMode":"prompt","isMeta":true,"origin":{"kind":"peer","from":"zmina"},"prompt":"[zmina:abcdefghijkl] T-1 ship it"}}`,
		// A shell stopped with TaskStop reports no end of its own: the stop is its end.
		`{"type":"user","timestamp":"` + at(9) + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t5","content":"running"}]},"toolUseResult":{"backgroundTaskId":"b2"}}`,
		`{"type":"user","timestamp":"` + at(9) + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t6","content":"stopped"}]},"toolUseResult":{"message":"Successfully stopped task: b2 (sleep 600)","task_id":"b2","task_type":"local_bash"}}`,
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

// Background work launched long before the transcript's end still holds; only the ends count before it, never an
// old line as the owner's or as the last turn's start.
func TestActivityReadsLaunchesInTheWholeTranscript(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	at := func(m int) string { return time.Date(2026, 11, 9, 10, m, 0, 0, time.UTC).Format(time.RFC3339) }
	filler := `{"type":"assistant","timestamp":"` + at(5) + `","message":{"role":"assistant","content":"` +
		strings.Repeat("x", 1<<20) + `"}}`

	var b strings.Builder
	for _, l := range []string{
		`{"type":"user","timestamp":"` + at(0) + `","message":{"role":"user","content":"watch the deploy"}}`,
		`{"type":"user","timestamp":"` + at(1) + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"watching"}]},"toolUseResult":{"taskId":"m1","persistent":true}}`,
		`{"type":"user","timestamp":"` + at(1) + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"running"}]},"toolUseResult":{"backgroundTaskId":"b1"}}`,
		`{"type":"user","timestamp":"` + at(2) + `","origin":{"kind":"task-notification"},"message":{"role":"user","content":"<task-notification>\n<task-id>b1</task-id>\n<status>completed</status>\n</task-notification>"}}`,
	} {
		b.WriteString(l + "\n")
	}

	for range 5 {
		b.WriteString(filler + "\n")
	}

	b.WriteString(`{"type":"user","timestamp":"` + at(9) + `","message":{"role":"user","content":"[zmina] restart: read the file"}}` + "\n")
	require.Greater(t, b.Len(), activitySize)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(b.String()), 0o600))

	a, err := (&Registry{Namespace: ns}).Activity("s1")
	require.NoError(t, err)
	assert.Equal(t, []string{"m1"}, a.Background, "the Monitor started 5 MB back still watches; the shell ended")
	assert.True(t, a.Owner.IsZero(), "the owner's line is older than the part read whole")
	assert.Equal(t, at(9), a.Prompt.Format(time.RFC3339))
}

// zmina's boot line is typed at the prompt as the start's first line, but it is not the owner speaking.
func TestActivityBootIsNotTheOwner(t *testing.T) {
	ns := t.TempDir()
	dir := filepath.Join(ns, "projects", "-work")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	boot := `{"type":"user","timestamp":"2026-11-09T10:00:00Z","origin":{"kind":"human"},"message":{"role":"user",` +
		`"content":"read AGENTS.md\n\n` + BootMark + ` ask him"}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(boot+"\n"), 0o600))

	a, err := (&Registry{Namespace: ns}).Activity("s1")
	require.NoError(t, err)
	assert.True(t, a.Owner.IsZero())
	assert.Equal(t, "2026-11-09T10:00:00Z", a.Prompt.Format(time.RFC3339), "the boot begins a turn")
}
