package chat

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskNotification(t *testing.T) {
	raw := `<task-notification><task-id>example</task-id><status>completed</status><summary>Monitor &quot;worker&quot; ended</summary><event>DOWN after 15s
Error: active writer</event><output-file>/tmp/example.output</output-file></task-notification>
Internal runtime instruction.`
	got := TaskNotification(Message{ID: "one", Role: "user", Text: raw})
	require.Equal(t, "event", got.Role)
	require.Equal(t, `Monitor "worker" ended`, got.Event.Summary)
	require.Equal(t, "DOWN after 15s", got.Event.Result)
	require.Contains(t, got.Event.Detail, "Error: active writer")
	require.Contains(t, got.Event.Detail, "Task: example")
	require.NotContains(t, got.Event.Detail, "Internal runtime instruction")
	for _, text := range []string{"Explain <task-notification>please", "<task-notification><summary>broken", "<task-notification><summary>x & y</summary></task-notification>"} {
		m := Message{Role: "user", Text: text}
		require.Equal(t, m, TaskNotification(m))
	}
	m := Message{Role: "assistant", Text: raw}
	require.Equal(t, m, TaskNotification(m))
	long := TaskNotification(Message{Role: "user", Text: "<task-notification><summary>Done</summary><event>" + strings.Repeat("x", 70000) + "</event></task-notification>"})
	require.Less(t, len(long.Event.Detail), 65000)
}

func TestSessionInputIsCollapsedWithoutGuessingItsAuthor(t *testing.T) {
	raw := Message{ID: "runtime-one", Role: "user", Text: "dispatcher: inspect this task\nLong technical details"}
	got := PresentInput(raw)
	require.Equal(t, "event", got.Role)
	require.Equal(t, "session", got.Event.Kind)
	require.Equal(t, "dispatcher: inspect this task", got.Event.Summary)
	require.Equal(t, raw.Text, got.Event.Detail)
	owner := raw
	owner.ID = "web:abcdefab-1234-1234-1234-123456789abc"
	require.Equal(t, owner, PresentInput(owner))
	owner.Text = "<task-notification><summary>Literal owner text</summary></task-notification>"
	require.Equal(t, owner, TaskNotification(owner))
	require.Equal(t, owner, PresentInput(owner))
	assistant := raw
	assistant.Role = "assistant"
	require.Equal(t, assistant, PresentInput(assistant))
}
