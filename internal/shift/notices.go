package shift

import (
	"fmt"
	"github.com/iamnikolie/zmina/internal/tmux"
	"slices"
	"strings"
	"time"
)

// NoticeMeta augments old string queues without changing their wire format.
type NoticeMeta struct {
	QueuedAt time.Time `json:"queued_at,omitempty"`
	Task     string    `json:"task,omitempty"`
	Handoff  string    `json:"handoff,omitempty"`
	Line     string    `json:"line,omitempty"`
	Count    int       `json:"count"`
	Role     string    `json:"role,omitempty"`
	Session  string    `json:"session,omitempty"`
	Since    int64     `json:"since,omitempty"`
	Kind     string    `json:"kind,omitempty"`
}

// ReportState keeps the latest explicitly keyed report. It is not an owner request.
type ReportState struct {
	Source string    `json:"source"`
	Key    string    `json:"key"`
	Text   string    `json:"text"`
	Count  int       `json:"count"`
	At     time.Time `json:"at"`
}

func (s *Shift) noticeMeta(text string) NoticeMeta {
	n := NoticeMeta{Count: 1}
	name, body, ok := strings.Cut(text, ": ")
	if !ok {
		return n
	}
	if strings.HasPrefix(body, "idle for ") {
		n.Kind = "idle"
	} else if strings.HasPrefix(body, "busy for ") {
		n.Kind = "busy"
	} else if strings.HasPrefix(body, "no input line on its screen for ") {
		n.Kind, n.Role = "blocked", name
	} else {
		return n
	}
	if sess := s.session(name); sess != nil {
		n.Role, n.Session, n.Since = name, sess.SessionID, sess.StatusUpdatedAt
	}
	return n
}

func (s *Shift) noticeCurrent(n NoticeMeta) bool {
	if n.Kind == "blocked" {
		if _, err := s.Role(n.Role); err != nil {
			return false
		}
		sess := s.session(n.Role)
		if n.Session != "" && (sess == nil || sess.SessionID != n.Session) {
			return false
		}
		if s.isCodex(n.Role) {
			return sess != nil && sess.Status == "waiting"
		}
		p, err := s.probe(n.Role)
		if err != nil {
			return true
		}
		if p.State != tmux.Up {
			return false
		}
		_, ok, err := s.Tmux.Prompt(n.Role)
		return err != nil || !ok
	}
	if n.Kind == "task-input" {
		p, err := s.probe(n.Role)
		if err != nil {
			return true
		}
		if p.State != tmux.Up {
			return false
		}
		draft, ok, err := s.Tmux.Prompt(n.Role)
		return err != nil || !ok || draft != ""
	}
	if n.Kind == "acceptance" {
		if s.Tasks == nil {
			return false
		}
		t, err := s.Tasks.Get(n.Task)
		return err == nil && s.acceptanceDue(t) && t.Delivery.ID == n.Handoff &&
			t.Delivery.Session == n.Session && t.Delivery.SentAt.UnixMilli() == n.Since &&
			(n.QueuedAt.IsZero() || n.QueuedAt.Equal(t.Delivery.QueuedAt))
	}
	if n.Kind == "delivery" {
		rs := s.State.Role(n.Role)
		if rs.Attempts > 0 && slices.Contains(rs.Inbox, n.Line) {
			return true
		}
		for _, f := range rs.Failed {
			if f.Line == n.Line {
				return true
			}
		}
		return false
	}

	if n.Role == "" {
		return true
	}
	sess := s.session(n.Role)
	if sess == nil || sess.SessionID != n.Session || sess.StatusUpdatedAt != n.Since {
		return false
	}
	if n.Kind == "idle" {
		return sess.Status == "idle" && len(s.held()[n.Role]) == 0 && len(s.State.Role(n.Role).Inbox) == 0
	}
	return n.Kind != "busy" || sess.Status == "busy"
}

func repeated(text string, n int) string {
	if n > 1 {
		return fmt.Sprintf("%s (repeated %d times)", text, n)
	}
	return text
}

func (s *Shift) currentNotes() []string {
	var pending, out []string
	for _, text := range s.State.Notes {
		n, typed := s.State.NoticeMeta[text]
		if typed && !s.noticeCurrent(n) {
			s.withdrawAcceptanceNotice(n)
			s.log(n.Role, "notice-resolved", "", text)
			delete(s.State.NoticeMeta, text)
			continue
		}
		pending = append(pending, text)
		out = append(out, repeated(text, n.Count))
	}
	s.State.Notes = pending
	return out
}

// Report coalesces a worker's same keyed conclusion across deliveries; new content is news.
func (s *Shift) Report(source, key, text string) (bool, error) {
	if key == "" || len(key) > 160 || strings.ContainsAny(key, "\r\n") {
		return false, fmt.Errorf("report: a key of 1-160 characters is required")
	}
	if strings.TrimSpace(text) == "" {
		return false, fmt.Errorf("report: empty text")
	}
	if s.State.Reports == nil {
		s.State.Reports = map[string]ReportState{}
	}
	id := source + "/" + key
	old := s.State.Reports[id]
	if old.Text == text {
		old.Count++
		old.At = s.Now()
		s.State.Reports[id] = old
		for _, sig := range s.State.Signals {
			if strings.HasPrefix(sig, source+": report "+key+": ") {
				if s.State.SignalCounts == nil {
					s.State.SignalCounts = map[string]int{}
				}
				s.State.SignalCounts[sig] = old.Count
			}
		}
		return false, nil
	}
	if len(s.State.Reports) >= 1024 && old.Key == "" {
		return false, fmt.Errorf("report ledger full (1024); remove old keyed reports explicitly")
	}
	if old.Key != "" {
		prefix := source + ": report " + key + ": "
		s.State.Signals = slices.DeleteFunc(s.State.Signals, func(v string) bool {
			if strings.HasPrefix(v, prefix) {
				delete(s.State.SignalCounts, v)
				return true
			}
			return false
		})
	}
	queued, err := s.Note(source, "report "+key+": "+text, false)
	if err != nil {
		return false, err
	}
	s.State.Reports[id] = ReportState{Source: source, Key: key, Text: text, Count: 1, At: s.Now()}
	return queued, nil
}
