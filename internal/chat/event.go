package chat

import (
	"encoding/xml"
	"strings"
)

// Event describes runtime-generated notifications, separate from owner messages.
type Event struct {
	Kind    string `json:"kind,omitempty"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Result  string `json:"result,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// TaskNotification recognizes the runtime's envelope only, never arbitrary XML inside a message.
func TaskNotification(m Message) Message {
	text := strings.TrimSpace(m.Text)
	if m.Role != "user" || strings.HasPrefix(m.ID, "web:") || !strings.HasPrefix(text, "<task-notification>") {
		return m
	}
	envelope, _, ok := strings.Cut(text, "</task-notification>")
	if !ok {
		return m
	}
	var task struct {
		ID      string `xml:"task-id"`
		Output  string `xml:"output-file"`
		Status  string `xml:"status"`
		Summary string `xml:"summary"`
		Detail  string `xml:"event"`
	}
	if xml.Unmarshal([]byte(envelope+"</task-notification>"), &task) != nil || strings.TrimSpace(task.Summary) == "" {
		return m
	}
	detail := strings.TrimSpace(task.Detail)
	result, _, _ := strings.Cut(detail, "\n")
	if task.ID != "" {
		detail += "\n\nTask: " + task.ID
	}
	if task.Output != "" {
		detail += "\nOutput: " + task.Output
	}
	m.Role = "event"
	clip := func(text string, limit int) string {
		r := []rune(text)
		if len(r) > limit {
			return string(r[:limit]) + "…"
		}
		return text
	}
	m.Event = &Event{Status: strings.TrimSpace(task.Status), Summary: clip(strings.TrimSpace(task.Summary), 1000), Result: clip(result, 240), Detail: clip(strings.TrimSpace(detail), 16000)}
	return m
}

// PresentInput never attributes unmarked runtime/terminal input to the browser
// owner. The source transcript cannot reliably identify its human/crew author.
// Preserve it as an expandable session message, without guessing by sender name.
func PresentInput(m Message) Message {
	if m.Role != "user" || strings.HasPrefix(m.ID, "web:") {
		return m
	}
	m = TaskNotification(m)
	if m.Event != nil {
		return m
	}
	summary, _, _ := strings.Cut(strings.TrimSpace(m.Text), "\n")
	runes := []rune(summary)
	if len(runes) > 160 {
		summary = string(runes[:160]) + "…"
	}
	detail := m.Text
	if text := []rune(detail); len(text) > 16000 {
		detail = string(text[:16000]) + "\n[Full message truncated for display]"
	}
	m.Role = "event"
	m.Event = &Event{Kind: "session", Status: "received", Summary: summary, Detail: detail}
	return m
}
