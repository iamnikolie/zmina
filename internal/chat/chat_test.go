package chat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexMessagesExposeConversationNotSystemOrReasoning(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "01", "01")
	require.NoError(t, os.MkdirAll(dir, 0700))
	body := `{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"system instructions"}]}}
{"type":"response_item","payload":{"type":"reasoning","text":"private reasoning"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"[zmina chat abcdefab-1234-1234-1234-123456789abc] hello"}]}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"**answer**"}]}}
{"type":"response_item"`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rollout-example-session.jsonl"), []byte(body), 0600))
	messages, err := CodexMessages(home, "session")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Equal(t, "web:abcdefab-1234-1234-1234-123456789abc", messages[0].ID)
	require.Equal(t, "hello", messages[0].Text)
	require.Equal(t, "**answer**", messages[1].Text)
	none, err := CodexMessages(home, "../session")
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestCodexUsesProvenanceToHideHarnessContextNotOwnerXML(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "01", "01")
	require.NoError(t, os.MkdirAll(dir, 0700))
	body := `{"type":"response_item","payload":{"type":"message","role":"user","internal_chat_message_metadata_passthrough":{"content_item_kinds":["environments.environment_context"]},"content":[{"type":"input_text","text":"<environment_context><current_date>2026-01-01</current_date></environment_context>"}]}}
{"type":"response_item","payload":{"type":"message","role":"user","internal_chat_message_metadata_passthrough":{"content_item_kinds":["agents_md.instructions"]},"content":[{"type":"input_text","text":"private injected agent instructions"}]}}
{"type":"response_item","payload":{"type":"message","role":"user","internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]},"content":[{"type":"input_text","text":"[zmina chat abcdefab-1234-1234-1234-123456789abc] <environment_context>literal owner text</environment_context>"}]}}
{"type":"response_item","payload":{"type":"message","role":"user","internal_chat_message_metadata_passthrough":{"content_item_kinds":["environments.environment_context","user.text"]},"content":[{"type":"input_text","text":"injected context"},{"type":"input_text","text":"keep this message"}]}}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rollout-example-session.jsonl"), []byte(body), 0600))
	got, err := CodexMessages(home, "session")
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "<environment_context>literal owner text</environment_context>", got[0].Text)
	require.Equal(t, "web:abcdefab-1234-1234-1234-123456789abc", got[0].ID)
	require.Equal(t, "keep this message", got[1].Text)
}
