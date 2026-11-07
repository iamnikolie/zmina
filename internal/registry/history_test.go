package registry

import (
	"fmt"
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

// A role's SendUserFile (as Claude Code records it: the call on the role's line, the delivered paths on its result's)
// is the role's message in Chat: its caption, then the paths as a block of their own, which Chat shows as files.
func TestChatMessagesShowsSendUserFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "example")
	require.NoError(t, os.MkdirAll(dir, 0700))
	lines := []string{
		`{"type":"assistant","uuid":"call","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"SendUserFile","input":{"files":["/tmp/s/report.pdf"],"caption":"The report, 20 pages.","status":"normal","display":"attach"}}]}}`,
		`{"type":"user","uuid":"res","timestamp":"2026-11-07T21:02:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"1 file delivered to user."}]},"toolUseResult":{"caption":"The report, 20 pages.","attachments":[{"path":"/tmp/s/report.pdf","size":376712,"pathValidated":true}]}}`,
		`{"type":"api-request-blob","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"SendUserFile","input":{"files":["/tmp/s/report.pdf"]}}]}}`,
		`{"type":"assistant","uuid":"call2","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"SendUserFile","input":{"files":["/nope.png"]}}]}}`,
		`{"type":"user","uuid":"res2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_2","is_error":true,"content":"no such file"}]}}`,
		`{"type":"assistant","uuid":"call3","message":{"content":[{"type":"tool_use","id":"toolu_3","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","uuid":"res3","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_3","content":"a.pdf"}]},"toolUseResult":{"stdout":"a.pdf"}}`,
		`{"type":"assistant","uuid":"a","message":{"content":[{"type":"text","text":"Sent."}]}}`,
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(strings.Join(lines, "\n")), 0600))
	reg := Registry{Namespace: root}
	messages, err := reg.ChatMessages("session")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Equal(t, "res", messages[0].ID)
	require.Equal(t, "assistant", messages[0].Role)
	require.Equal(t, "The report, 20 pages.\n\n```\n/tmp/s/report.pdf\n```", messages[0].Text)
	require.False(t, messages[0].At.IsZero())
	require.Equal(t, "Sent.", messages[1].Text)
	sent, err := reg.SentFiles("session")
	require.NoError(t, err)
	require.Equal(t, []string{"/tmp/s/report.pdf"}, sent, "the delivered file, not the one whose send failed")
}

// The end Claude Code may still replace is provisional: a slash command as typed, a command's line before its output.
func TestChatMessagesMarksAProvisionalEnd(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "example")
	require.NoError(t, os.MkdirAll(dir, 0700))
	write := func(lines ...string) []string {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(strings.Join(lines, "\n")), 0600))
		messages, err := (&Registry{Namespace: root}).ChatMessages("session")
		require.NoError(t, err)
		var got []string
		for _, m := range messages {
			got = append(got, fmt.Sprint(m.ID, " ", m.Provisional))
		}
		return got
	}
	reply := `{"type":"assistant","uuid":"a","message":{"content":[{"type":"text","text":"Done."}]}}`
	typed := `{"type":"user","uuid":"typed","message":{"content":"/compact"}}`
	caveat := `{"type":"user","isMeta":true,"uuid":"c","message":{"content":"<local-command-caveat>The command below was run directly in Claude Code, not sent to you as a request.</local-command-caveat>"}}`
	cmd := `{"type":"user","uuid":"cmd","message":{"content":"<command-name>/compact</command-name>\n<command-message>compact</command-message>\n<command-args></command-args>"}}`
	out := `{"type":"user","uuid":"out","message":{"content":"<local-command-stdout>Compacted</local-command-stdout>"}}`
	require.Equal(t, []string{"a false", "typed true"}, write(reply, typed))
	require.Equal(t, []string{"a false", "cmd true"}, write(reply, typed, caveat, cmd))
	require.Equal(t, []string{"a false", "cmd false"}, write(reply, typed, caveat, cmd, out))
	require.Equal(t, []string{"typed false", "a false"}, write(typed, reply), "a typed line something followed has stayed")
	require.Equal(t, []string{"a false", "q false"}, write(reply, `{"type":"user","uuid":"q","message":{"content":"the path is /tmp/x"}}`))
	require.Equal(t, []string{"a false", "multi true"}, write(reply, `{"type":"user","uuid":"multi","message":{"content":"/review the diff\nand the tests"}}`), "a typed command with arguments over lines")
}
