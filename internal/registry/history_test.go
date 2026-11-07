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

// A command run at Claude Code's prompt is one quiet line, never the owner's bubble: not the line typed, not the
// recorded <command-*> elements, not the <local-command-stdout> with its escapes and hook lines (as Claude Code writes them).
func TestChatMessagesFoldsLocalCommands(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "example")
	require.NoError(t, os.MkdirAll(dir, 0700))
	caveat := `{"type":"user","isMeta":true,"uuid":"%s","message":{"content":"<local-command-caveat>The command below was run directly in Claude Code, not sent to you as a request, and its output goes straight to the user. It's recorded here as context for later messages.</local-command-caveat>"}}`
	lines := []string{
		`{"type":"user","uuid":"q","origin":{"kind":"human"},"message":{"content":"Before the compaction"}}`,
		`{"type":"user","uuid":"typed","message":{"content":"/compact"}}`,
		`{"type":"system","subtype":"compact_boundary","content":"Conversation compacted"}`,
		`{"type":"user","uuid":"summary","isCompactSummary":true,"message":{"content":"This session is being continued from a previous conversation."}}`,
		strings.Replace(caveat, "%s", "c1", 1),
		`{"type":"user","uuid":"cmd","message":{"content":"<command-name>/compact</command-name>\n            <command-message>compact</command-message>\n            <command-args></command-args>"}}`,
		`{"type":"user","uuid":"out","message":{"content":"<local-command-stdout>\u001b[2mCompacted (ctrl+o to see full summary)\u001b[22m\n\u001b[2mPreCompact [command hook 1] completed successfully\u001b[22m\n\u001b[2mPreCompact [command hook 2] completed successfully\u001b[22m</local-command-stdout>"}}`,
		strings.Replace(caveat, "%s", "c2", 1),
		`{"type":"user","uuid":"model","message":{"content":"<command-name>/model</command-name>\n            <command-message>model</command-message>\n            <command-args></command-args>"}}`,
		`{"type":"user","uuid":"model-out","message":{"content":"<local-command-stdout>Set model to ` + "`" + `Opus` + "`" + `</local-command-stdout>"}}`,
		strings.Replace(caveat, "%s", "c3", 1),
		`{"type":"user","uuid":"clear","message":{"content":"<command-name>/clear</command-name>\n<command-message>clear</command-message>\n<command-args></command-args>"}}`,
		`{"type":"user","uuid":"bad","message":{"content":"<local-command-stderr>\u001b[31mUnknown effort level\u001b[39m</local-command-stderr>"}}`,
		`{"type":"user","uuid":"skill","message":{"content":"<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>the last commit</command-args>"}}`,
		`{"type":"assistant","uuid":"a","message":{"content":[{"type":"text","text":"\u001b[1mDone\u001b[22m: reviewed."}]}}`,
		`{"type":"user","uuid":"talk","origin":{"kind":"human"},"message":{"content":"<command-name> is an XML tag, see /compact"}}`,
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(strings.Join(lines, "\n")), 0600))
	reg := Registry{Namespace: root}
	messages, err := reg.ChatMessages("session")
	require.NoError(t, err)

	var got []string
	for _, m := range messages {
		if m.Event != nil {
			got = append(got, m.ID+" "+m.Role+" "+m.Event.Kind+" "+m.Event.Status+"|"+m.Event.Summary)
		} else {
			got = append(got, m.ID+" "+m.Role+"|"+m.Text)
		}
	}
	require.Equal(t, []string{
		"q user|Before the compaction",
		"cmd event command |Context compacted",
		"model event command |/model · Set model to `Opus`",
		"clear event command failed|/clear · Unknown effort level",
		"skill user|/review the last commit",
		"a assistant|\u001b[1mDone\u001b[22m: reviewed.",
		"talk user|<command-name> is an XML tag, see /compact",
	}, got)

	history, err := reg.History("session", 100)
	require.NoError(t, err)
	require.NotContains(t, history, "<command-args>")
	require.NotContains(t, history, "<local-command")
	require.NotContains(t, history, "hook")
	require.Contains(t, history, "Session:\nContext compacted")
}
