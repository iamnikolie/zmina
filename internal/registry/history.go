package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/iamnikolie/zmina/internal/chat"
)

// History reads visible conversation text, not thinking blocks, from a session's recent transcript.
// Full-screen applications keep their history internally instead of in tmux's alternate-screen buffer.
func (r *Registry) History(sessionID string, lines int) (string, error) {
	messages, err := r.ChatMessages(sessionID)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, m := range messages {
		label := "You"
		if m.Role == "assistant" {
			label = "Assistant"
		}
		text := m.Text
		if m.Event != nil {
			label = "Background task"
			text = m.Event.Summary + "\n" + m.Event.Detail
		}
		out.WriteString(label + ":\n" + text + "\n\n")
	}
	text := strings.TrimSpace(out.String())
	split := strings.Split(text, "\n")
	if lines > 0 && len(split) > lines {
		text = strings.Join(split[len(split)-lines:], "\n")
	}
	return text, nil
}

// ChatMessages exposes only visible user and assistant text with stable message identities.
func (r *Registry) ChatMessages(sessionID string) ([]chat.Message, error) {
	path := r.transcript(sessionID)
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	const historyTail = 8 << 20
	offset := max(stat.Size()-historyTail, 0)
	raw := make([]byte, stat.Size()-offset)
	if _, err = file.ReadAt(raw, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	records := bytes.Split(raw, []byte("\n"))
	if offset > 0 {
		records = records[1:]
	}
	out := []chat.Message{}
	seen := map[string]bool{}
	for _, record := range records {
		var l line
		if json.Unmarshal(record, &l) != nil || l.Sidechain || l.Compact || (l.Type != "user" && l.Type != "assistant") {
			continue
		}
		if l.UUID != "" && seen[l.UUID] {
			continue
		}
		// A line zmina posted to the session's inbox socket is a meta line from a peer, wrapped in Claude Code's
		// words; of those only an owner's message from Chat is the conversation's.
		if l.Meta {
			if m, ok := peerChat(l); ok {
				seen[l.UUID] = true
				out = append(out, m)
			}
			continue
		}
		seen[l.UUID] = true
		text := l.text()
		if strings.TrimSpace(text) == "" {
			continue
		}
		id := l.UUID
		if id == "" {
			id = chat.Hash(string(record))
		}
		out = append(out, chat.TaskNotification(chat.Normalize(chat.Message{ID: id, Role: l.Type, Text: text, At: l.Timestamp})))
	}
	return out, nil
}

// Claude Code's words around a message another session posted to this one's inbox socket.
const (
	peerWrapHead = "Another Claude session sent a message:\n"
	peerWrapTail = "\n\nThis came from another Claude session"
)

// peerChat is an owner's message from Chat that zmina posted to the session's inbox socket, without Claude Code's
// wrapping and zmina's tag; ok is false for any other meta line.
func peerChat(l line) (chat.Message, bool) {
	if l.Type != "user" || l.Origin == nil || l.Origin.Kind != "peer" {
		return chat.Message{}, false
	}
	text, ok := strings.CutPrefix(l.text(), peerWrapHead)
	if !ok {
		return chat.Message{}, false
	}
	if i := strings.LastIndex(text, peerWrapTail); i >= 0 {
		text = text[:i]
	}
	m := chat.Normalize(chat.Message{ID: l.UUID, Role: "user", Text: text, At: l.Timestamp})
	if !strings.HasPrefix(m.ID, "web:") {
		return chat.Message{}, false
	}
	m.Peer = true
	return m, true
}
