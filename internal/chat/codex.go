package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CodexMessages reads visible message items from the runtime's own rollout, without resuming a thread.
func CodexMessages(home, id string) ([]Message, error) {
	if id == "" || strings.ContainsAny(id, "/\\*?[]") {
		return nil, nil
	}
	paths, err := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "*"+id+".jsonl"))
	if err != nil || len(paths) == 0 {
		return nil, err
	}
	f, err := os.Open(paths[len(paths)-1])
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	offset := max(st.Size()-(8<<20), 0)
	raw := make([]byte, st.Size()-offset)
	if _, err = f.ReadAt(raw, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	lines := bytes.Split(raw, []byte("\n"))
	if offset > 0 {
		lines = lines[1:]
	}
	out := []Message{}
	for _, line := range lines {
		var record struct {
			Type    string    `json:"type"`
			At      time.Time `json:"timestamp"`
			Payload struct {
				Type     string `json:"type"`
				Role     string `json:"role"`
				Metadata struct {
					Kinds []string `json:"content_item_kinds"`
				} `json:"internal_chat_message_metadata_passthrough"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &record) != nil || record.Type != "response_item" || record.Payload.Type != "message" || (record.Payload.Role != "user" && record.Payload.Role != "assistant") {
			continue
		}
		parts := []string{}
		for i, part := range record.Payload.Content {
			// Codex stores harness context as user-role items. Its explicit
			// content provenance, not XML-looking text, identifies hidden input.
			if record.Payload.Role == "user" && len(record.Payload.Metadata.Kinds) == len(record.Payload.Content) {
				kind := record.Payload.Metadata.Kinds[i]
				if kind == "environments.environment_context" || kind == "agents_md.instructions" {
					continue
				}
			}
			if part.Type == "input_text" || part.Type == "output_text" {
				parts = append(parts, part.Text)
			}
		}
		text := strings.Join(parts, "\n")
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, Normalize(Message{ID: Hash(string(line)), Role: record.Payload.Role, Text: text, At: record.At}))
	}
	return out, nil
}
