package registry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// tailSize is how much of a transcript's end Last reads: the last turn is there, the conversation is not.
const tailSize = 1 << 20

// Turn is what the end of a session's transcript says about its last turn.
type Turn struct {
	// Context is how many tokens the conversation fills: the input and output of its last turn. It is 0 when that
	// is not known: no transcript yet, no turn in its last stretch, or a compaction after the last turn.
	Context int
	// Error is the API error the last turn ended in (a usage limit, a lost connection), empty when it did not;
	// ErrorID tells one such error from the next.
	Error   string
	ErrorID string
}

// line is the part of a transcript line zmina reads.
type line struct {
	Type      string    `json:"type"`
	Subtype   string    `json:"subtype"`
	UUID      string    `json:"uuid"`
	Sidechain bool      `json:"isSidechain"`
	APIError  bool      `json:"isApiErrorMessage"`
	Timestamp time.Time `json:"timestamp"`
	Name      string    `json:"agentName"`
	Message   struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			Input         int `json:"input_tokens"`
			CacheCreation int `json:"cache_creation_input_tokens"`
			CacheRead     int `json:"cache_read_input_tokens"`
			Output        int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

func (l *line) tokens() int {
	if u := l.Message.Usage; u != nil {
		return u.Input + u.CacheCreation + u.CacheRead + u.Output
	}

	return 0
}

func (l *line) text() string {
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}

	if json.Unmarshal(l.Message.Content, &parts) != nil {
		var s string
		_ = json.Unmarshal(l.Message.Content, &s)

		return s
	}

	var out []string

	for _, p := range parts {
		if p.Type == "text" {
			out = append(out, p.Text)
		}
	}

	return strings.Join(out, " ")
}

// transcript is the file of a session, empty when there is none (yet).
func (r *Registry) transcript(sessionID string) string {
	if sessionID == "" || filepath.Base(sessionID) != sessionID {
		return ""
	}

	files, _ := filepath.Glob(filepath.Join(r.Namespace, "projects", "*", sessionID+".jsonl"))
	if len(files) == 0 {
		return ""
	}

	return files[0]
}

// Context is how many tokens the session's conversation fills, 0 when not known (see Turn).
func (r *Registry) Context(sessionID string) (int, error) {
	t, err := r.Last(sessionID)

	return t.Context, err
}

// Last reads the end of the session's transcript (<namespace>/projects/*/<session id>.jsonl).
func (r *Registry) Last(sessionID string) (Turn, error) {
	path := r.transcript(sessionID)
	if path == "" {
		return Turn{}, nil
	}

	return lastTurn(path)
}

func lastTurn(path string) (Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return Turn{}, err
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return Turn{}, err
	}

	off := max(st.Size()-tailSize, 0)

	tail := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(tail, off); err != nil && !errors.Is(err, io.EOF) {
		return Turn{}, err
	}

	lines := bytes.Split(tail, []byte("\n"))
	if off > 0 {
		// The first line was cut by the offset.
		lines = lines[1:]
	}

	var t Turn

	seenTurn := false

	for i := len(lines) - 1; i >= 0; i-- {
		var l line
		if len(bytes.TrimSpace(lines[i])) == 0 || json.Unmarshal(lines[i], &l) != nil || l.Sidechain {
			continue
		}

		if l.Type == "system" && l.Subtype == "compact_boundary" {
			t.Context = 0

			return t, nil
		}

		if l.Type != "assistant" {
			continue
		}

		// The newest assistant line decides whether the last turn ended in an error.
		if !seenTurn && l.APIError {
			t.Error, t.ErrorID = strings.TrimSpace(l.text()), l.UUID
		}

		seenTurn = true

		// An API error or "No response requested." has all-zero usage: not a turn of the conversation.
		if n := l.tokens(); n > 0 {
			t.Context = n

			return t, nil
		}
	}

	return t, nil
}

// Usage is what one session spent: its turns and their tokens, subagents included.
type Usage struct {
	Session    string    `json:"session"`
	ID         string    `json:"session_id"`
	Turns      int       `json:"turns"`
	Input      int       `json:"input"`
	CacheWrite int       `json:"cache_write"`
	CacheRead  int       `json:"cache_read"`
	Output     int       `json:"output"`
	Last       time.Time `json:"last"`
}

// headSize is how much of a transcript's start is read for its session name.
const headSize = 64 << 10

// UsageSince sums the turns since a time of the sessions with the given names (claude --name), from their
// transcripts and their subagents' — Claude Code records every API reply's usage there. A reply split over
// several lines is counted once.
func (r *Registry) UsageSince(names []string, since time.Time) ([]Usage, error) {
	files, err := filepath.Glob(filepath.Join(r.Namespace, "projects", "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}

	var out []Usage

	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil || st.ModTime().Before(since) {
			continue
		}

		name := sessionName(f)
		if name == "" || !slices.Contains(names, name) {
			continue
		}

		u := Usage{Session: name, ID: strings.TrimSuffix(filepath.Base(f), ".jsonl")}
		seen := map[string]bool{}

		subs, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(f, ".jsonl"), "subagents", "*.jsonl"))
		for _, p := range append([]string{f}, subs...) {
			if err := addUsage(p, since, seen, &u); err != nil {
				return nil, err
			}
		}

		if u.Turns > 0 {
			out = append(out, u)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })

	return out, nil
}

// sessionName is the name a transcript's session runs under, from its first agent-name line.
func sessionName(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	head := make([]byte, headSize)
	n, _ := io.ReadFull(f, head)

	for _, raw := range bytes.Split(head[:n], []byte("\n")) {
		if !bytes.Contains(raw, []byte(`"agent-name"`)) {
			continue
		}

		var l line
		if json.Unmarshal(raw, &l) == nil && l.Type == "agent-name" {
			return l.Name
		}
	}

	return ""
}

func addUsage(path string, since time.Time, seen map[string]bool, u *Usage) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	rd := bufio.NewReaderSize(f, 1<<20)

	for {
		raw, err := rd.ReadBytes('\n')
		if len(raw) > 0 && bytes.Contains(raw, []byte(`"usage"`)) && bytes.Contains(raw, []byte(`"assistant"`)) {
			var l line
			if json.Unmarshal(raw, &l) == nil && l.Type == "assistant" && l.tokens() > 0 &&
				!l.Timestamp.Before(since) && (l.Message.ID == "" || !seen[l.Message.ID]) {
				seen[l.Message.ID] = true
				m := l.Message.Usage
				u.Turns++
				u.Input += m.Input
				u.CacheWrite += m.CacheCreation
				u.CacheRead += m.CacheRead
				u.Output += m.Output

				if l.Timestamp.After(u.Last) {
					u.Last = l.Timestamp
				}
			}
		}

		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return err
		}
	}
}
