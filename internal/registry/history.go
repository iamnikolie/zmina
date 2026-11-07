package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
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
		if m.Event != nil && m.Event.Kind == "command" {
			label, text = "Session", m.Event.Summary
		} else if m.Event != nil {
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
	// local: Claude Code's note that the next line is a command it ran itself; command: the index in out of the
	// command line the next output line belongs to, -1 when none.
	local, command := false, -1
	var ran [2]string // the name and arguments of that command
	sends := map[string]sentFiles{} // SendUserFile calls by tool_use id, until their result
	for _, record := range records {
		var l line
		if json.Unmarshal(record, &l) != nil || l.Sidechain || l.Compact || (l.Type != "user" && l.Type != "assistant") {
			continue
		}
		if l.UUID != "" && seen[l.UUID] {
			continue
		}
		if m, ok := sentFile(&l, sends); ok {
			seen[l.UUID] = true
			out = append(out, m)
			local, command = false, -1
			continue
		}
		// A line zmina posted to the session's inbox socket is a meta line from a peer, wrapped in Claude Code's
		// words; of those only an owner's message from Chat is the conversation's.
		if l.Meta {
			if m, ok := peerChat(l); ok {
				seen[l.UUID] = true
				out = append(out, m)
				local, command = false, -1
			} else if localCaveat(&l) {
				local = true
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
		m := chat.Message{ID: id, Role: l.Type, Text: text, At: l.Timestamp}
		if l.Type == "user" && l.Origin == nil {
			if name, args, ok := chat.ParseCommand(text); ok {
				out = dropTyped(out, name, args)
				if local {
					// A command Claude Code ran itself (/compact, /model): one quiet line, its output joins it.
					m.Role, m.Text, m.Event = "event", "", chat.CommandEvent(name, args, "", false)
					out = append(out, m)
					local, command, ran = false, len(out)-1, [2]string{name, args}
					continue
				}
				// A command that is a prompt (a skill): the owner's words as he typed them.
				m.Text = strings.TrimSpace(name + " " + args)
				out = append(out, chat.Normalize(m))
				local, command = false, -1
				continue
			}
			if output, failed, ok := chat.ParseLocalOutput(text); ok {
				if command >= 0 {
					out[command].Event = chat.CommandEvent(ran[0], ran[1], output, failed)
				} else if output != "" {
					m.Role, m.Text, m.Event = "event", "", chat.CommandEvent("", "", output, failed)
					out = append(out, m)
				}
				local, command = false, -1
				continue
			}
		}
		local, command = false, -1
		out = append(out, chat.TaskNotification(chat.Normalize(m)))
	}
	provisional(out, command)
	return out, nil
}

// provisional marks the end of the conversation that Claude Code may still replace or complete: a command's line
// whose output has not come (command is its index, -1 when none), and a slash command as typed, which goes once
// Claude Code records it as a command (dropTyped).
func provisional(out []chat.Message, command int) {
	if command >= 0 && command == len(out)-1 {
		out[command].Provisional = true
	}
	if n := len(out); n > 0 {
		last := &out[n-1]
		if last.Role == "user" && last.Event == nil && !strings.HasPrefix(last.ID, "web:") && typedCommand.MatchString(strings.TrimSpace(last.Text)) {
			last.Provisional = true
		}
	}
}

// typedCommand is a slash command as typed at Claude Code's prompt: /name and its arguments on one line.
var typedCommand = regexp.MustCompile(`^/[A-Za-z][\w:.-]*(?: [^\n]*)?$`)

// sentFiles is a SendUserFile call: the files the role sent the owner and the words it sent with them.
type sentFiles struct {
	Files   []string `json:"files"`
	Caption string   `json:"caption"`
}

// sentFile reads a role's SendUserFile (Claude Code's way to hand the owner a file) as the role's message in Chat: its
// caption, then the files' paths as a block of their own, which Chat shows as the role's files where its node may
// serve them (D-16) and as paths where it may not. A call is remembered from the role's line (ok false), and becomes
// a message on the line of its result, unless that result is an error.
func sentFile(l *line, sends map[string]sentFiles) (chat.Message, bool) {
	var blocks []struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		IsError   bool            `json:"is_error"`
	}
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return chat.Message{}, false
	}
	for _, b := range blocks {
		switch {
		case l.Type == "assistant" && b.Type == "tool_use" && b.Name == "SendUserFile":
			var call sentFiles
			if json.Unmarshal(b.Input, &call) == nil {
				sends[b.ID] = call
			}
		case l.Type == "user" && b.Type == "tool_result":
			call, ok := sends[b.ToolUseID]
			if !ok || b.IsError {
				continue
			}
			delete(sends, b.ToolUseID)
			// The paths Claude Code delivered, as it recorded them; the call's own when it recorded none.
			var result struct {
				Attachments []struct {
					Path string `json:"path"`
				} `json:"attachments"`
				Caption string `json:"caption"`
			}
			files := call.Files
			if json.Unmarshal(l.ToolResult, &result) == nil && len(result.Attachments) > 0 {
				files = nil
				for _, a := range result.Attachments {
					if a.Path != "" {
						files = append(files, a.Path)
					}
				}
			}
			caption := strings.TrimSpace(call.Caption)
			if caption == "" {
				caption = strings.TrimSpace(result.Caption)
			}
			var paths []string
			for _, f := range files {
				if f = strings.TrimSpace(f); f != "" && !strings.ContainsAny(f, "`\n") {
					paths = append(paths, f)
				}
			}
			if len(paths) == 0 {
				continue
			}
			text := "```\n" + strings.Join(paths, "\n") + "\n```"
			if caption != "" {
				text = caption + "\n\n" + text
			}
			id := l.UUID
			if id == "" {
				id = chat.Hash(b.ToolUseID)
			}
			return chat.Message{ID: id, Role: "assistant", Text: text, At: l.Timestamp}, true
		}
	}
	return chat.Message{}, false
}

// dropTyped takes away the line just before a recorded command when it is that command as typed at the prompt
// ("/compact"): the command is shown once.
func dropTyped(out []chat.Message, name, args string) []chat.Message {
	if len(out) == 0 {
		return out
	}
	last := out[len(out)-1]
	text := strings.TrimSpace(last.Text)
	if last.Role != "user" || last.Event != nil || strings.HasPrefix(last.ID, "web:") || (text != name && text != strings.TrimSpace(name+" "+args)) {
		return out
	}
	return out[:len(out)-1]
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
