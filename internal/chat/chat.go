// Package chat defines the owner-facing conversation protocol, independent of terminal rendering.
package chat

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type Message struct {
	Attachments []Attachment `json:"attachments,omitempty"`
	Event       *Event       `json:"event,omitempty"`
	ID          string       `json:"id"`
	Role        string       `json:"role"`
	Text        string       `json:"text"`
	At          time.Time    `json:"at,omitempty"`
	State       string       `json:"state,omitempty"`
}
type Snapshot struct {
	PresentationVersion  int         `json:"presentation_version,omitempty"`
	AttachmentsSupported bool        `json:"attachments_supported,omitempty"`
	Uploaded             *Attachment `json:"uploaded,omitempty"`
	File                 *Upload     `json:"file,omitempty"`
	RetryWindowSeconds   int         `json:"retry_window_seconds,omitempty"`
	Cursor               string      `json:"cursor,omitempty"`
	Reset                bool        `json:"reset,omitempty"`
	More                 bool        `json:"more,omitempty"`
	HistoryGap           bool        `json:"history_gap,omitempty"`
	Role                 string      `json:"role"`
	Runtime              string      `json:"runtime"`
	Profile              string      `json:"profile,omitempty"`
	Session              string      `json:"session,omitempty"`
	State                string      `json:"state"`
	Status               string      `json:"status,omitempty"`
	NeedsInput           bool        `json:"needs_input,omitempty"`
	Active               bool        `json:"active"`
	Context              int         `json:"context,omitempty"`
	Messages             []Message   `json:"messages"`
	Pending              []Message   `json:"pending"`
}

// Receipt persists browser request identity in the same transaction as the role's queue entry.
type Receipt struct {
	Attachments []Attachment `json:"attachments,omitempty"`
	Session     string       `json:"session,omitempty"`
	ID          string       `json:"id"`
	Hash        string       `json:"hash"`
	Text        string       `json:"text"`
	Line        string       `json:"line"`
	At          time.Time    `json:"at"`
	// Dismissed is when the owner set aside a message whose delivery stayed unconfirmed: it is no longer shown as
	// pending. The receipt stays, so a retry with the same ID is still recognised.
	Dismissed time.Time `json:"dismissed,omitzero"`
}

var idPattern = regexp.MustCompile(`^[a-f0-9-]{16,64}$`)

func ValidID(id string) bool  { return idPattern.MatchString(id) }
func Hash(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }
func Prefix(id string) string { return "[zmina chat " + id + "] " }
func Normalize(m Message) Message {
	if m.Role != "user" || !strings.HasPrefix(m.Text, "[zmina chat ") {
		return m
	}
	head, rest, ok := strings.Cut(strings.TrimPrefix(m.Text, "[zmina chat "), "] ")
	if ok && ValidID(head) {
		m.ID = "web:" + head
		m.Text = rest
	}
	return m
}

// Limit retains a bounded recent window for browser snapshots without splitting UTF-8 text.
func Limit(messages []Message) []Message {
	total := 0
	start := len(messages)
	for start > 0 && len(messages)-start < 200 {
		n := len(messages[start-1].Text)
		if total+n > 1<<20 && start < len(messages) {
			break
		}
		total += n
		start--
	}
	out := append([]Message{}, messages[start:]...)
	for i := range out {
		if len(out[i].Text) > 1<<20 {
			r := []rune(out[i].Text)
			for len(string(r)) > 1<<20 {
				r = r[len(r)/4:]
			}
			out[i].Text = "[Earlier text omitted]\n" + string(r)
		}
	}
	return out
}
