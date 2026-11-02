package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHistoryReadsVisibleTranscriptWithoutThinkingOrSidechains(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "example")
	require.NoError(t, os.MkdirAll(dir, 0700))
	raw := `{"type":"user","uuid":"one","message":{"content":"Earlier question"}}
{"type":"assistant","uuid":"two","message":{"content":[{"type":"thinking","thinking":"private reasoning"},{"type":"text","text":"Earlier answer"}]}}
{"type":"assistant","uuid":"two","message":{"content":[{"type":"text","text":"Earlier answer"}]}}
{"type":"assistant","isSidechain":true,"message":{"content":"sidechain text"}}
{"type":"user","isMeta":true,"message":{"content":"internal metadata"}}
{"type":"user","message":{"content":"Latest question"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Latest answer"}]}}
{"type":"assistant","message":`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(raw), 0600))
	reg := Registry{Namespace: root}
	history, err := reg.History("session", 10000)
	require.NoError(t, err)
	require.Equal(t, "You:\nEarlier question\n\nAssistant:\nEarlier answer\n\nYou:\nLatest question\n\nAssistant:\nLatest answer", history)
	require.Equal(t, 1, strings.Count(history, "Earlier answer"))
	_, err = reg.History("../session", 10000)
	require.NoError(t, err)
}

func TestChatMessagesClassifiesTaskNotification(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "example")
	require.NoError(t, os.MkdirAll(dir, 0700))
	raw := `{"type":"user","uuid":"event","message":{"content":"<task-notification><status>completed</status><summary>Worker ended</summary><event>Exit 1</event></task-notification>"}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(raw), 0600))
	reg := Registry{Namespace: root}
	messages, err := reg.ChatMessages("session")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "event", messages[0].Role)
	require.Equal(t, "Worker ended", messages[0].Event.Summary)
	history, err := reg.History("session", 100)
	require.NoError(t, err)
	require.NotContains(t, history, "<task-notification>")
	require.Contains(t, history, "Background task:")
}
