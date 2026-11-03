package registry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
	// Ended is true when the conversation's newest line closes a turn (an end_turn reply, or the turn's closing
	// system line): the session waits for its next prompt, even while its status says otherwise because a
	// background shell or agent runs.
	Ended bool
	// At is when the newest line that closed a turn was written; zero when the last turn is not over or the time
	// is not known. A start's boot turn is over once a turn ended at or after the start.
	At time.Time `json:",omitzero"`
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
	Cwd       string    `json:"cwd"`
	Meta      bool      `json:"isMeta"`
	Compact   bool      `json:"isCompactSummary"`
	Origin    *struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"origin"`
	Message struct {
		ID         string          `json:"id"`
		Model      string          `json:"model"`
		StopReason string          `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
		Usage      *struct {
			Input         int `json:"input_tokens"`
			CacheCreation int `json:"cache_creation_input_tokens"`
			CacheRead     int `json:"cache_read_input_tokens"`
			Output        int `json:"output_tokens"`
			Split         *struct {
				Write5m int `json:"ephemeral_5m_input_tokens"`
				Write1h int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
			Server *struct {
				WebSearch int `json:"web_search_requests"`
			} `json:"server_tool_use"`
			Speed string `json:"speed"`
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

	seenTurn, decided := false, false

	for i := len(lines) - 1; i >= 0; i-- {
		var l line
		if len(bytes.TrimSpace(lines[i])) == 0 || json.Unmarshal(lines[i], &l) != nil || l.Sidechain {
			continue
		}

		// The newest line of the conversation says whether its turn is over: a prompt or a tool result waits on
		// a reply, a tool call on its result; an end_turn reply or the turn's closing system lines end it.
		if !decided {
			switch {
			case l.Type == "system" && (l.Subtype == "turn_duration" || l.Subtype == "stop_hook_summary"):
				t.Ended, decided = true, true
			case l.Type == "assistant":
				t.Ended, decided = l.Message.StopReason == "end_turn" || l.Message.StopReason == "stop_sequence", true
			case l.Type == "user":
				decided = true
			}

			if t.Ended {
				t.At = l.Timestamp
			}
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

// headSize is how much of a transcript's start is read for its session name.
const headSize = 64 << 10

// Transcript is one session's transcript file.
type Transcript struct {
	Path string
	// ID is the session id, Name the name it runs under (claude --name), empty for an unnamed session.
	ID, Name string
}

// Transcripts lists the namespace's session transcripts written to since a time, with the names they run under.
// A subagent's transcript is not one of them: it is read with its parent's (Messages).
func (r *Registry) Transcripts(since time.Time) ([]Transcript, error) {
	files, err := filepath.Glob(filepath.Join(r.Namespace, "projects", "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}

	var out []Transcript

	for _, f := range files {
		if st, err := os.Stat(f); err != nil || (st.ModTime().Before(since) && !subagentsSince(f, since)) {
			continue
		}

		out = append(out, Transcript{Path: f, ID: strings.TrimSuffix(filepath.Base(f), ".jsonl"), Name: sessionName(f)})
	}

	return out, nil
}

// subagentsSince reports a subagent transcript of the session written to since a time: a parent can be quiet
// while its agents work.
func subagentsSince(path string, since time.Time) bool {
	subs, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "*.jsonl"))
	for _, s := range subs {
		if st, err := os.Stat(s); err == nil && !st.ModTime().Before(since) {
			return true
		}
	}

	return false
}

// ProjectDir is the directory Claude Code keeps the transcripts of sessions started in dir under: every character
// but a letter or digit becomes a dash.
func (r *Registry) ProjectDir(dir string) string {
	b := []byte(dir)
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			b[i] = '-'
		}
	}

	return filepath.Join(r.Namespace, "projects", string(b))
}

// Message is one API reply of a session: its model, its tokens and where and why it ran.
type Message struct {
	// ID is the API message id: a reply split over several transcript lines is one Message.
	ID    string    `json:"id,omitempty"`
	At    time.Time `json:"at"`
	Model string    `json:"model"`
	// Speed is the reply's speed when it is not standard (fast).
	Speed          string `json:"speed,omitempty"`
	PricingPrompt  int    `json:"pricing_prompt,omitempty"`
	UnpricedReason string `json:"unpriced_reason,omitempty"`
	Input          int    `json:"input"`
	// Write5m and Write1h are the cache writes by their lifetime; Read the cache reads.
	Write     int `json:"write,omitempty"` // Codex cache writes whose lifetime is not in the event.
	Write5m   int `json:"write_5m"`
	Write1h   int `json:"write_1h"`
	Read      int `json:"read"`
	Output    int `json:"output"`
	WebSearch int `json:"web_search,omitempty"`
	// Cwd is the session's working directory when it ran.
	Cwd string `json:"cwd,omitempty"`
	// Sub is set for a subagent's reply: it is counted with the session that started the agent.
	Sub bool `json:"sub,omitempty"`
	// Trigger is what started the turn the reply is part of (see Trigger*); From names the session a peer
	// message came from.
	Trigger string `json:"trigger"`
	From    string `json:"from,omitempty"`
}

// Prompt is the tokens the reply read: its input, cache writes and cache reads.
func (m Message) Prompt() int { return m.Input + m.Write + m.Write5m + m.Write1h + m.Read }

// Tokens is every token of the reply.
func (m Message) Tokens() int { return m.Prompt() + m.Output }

// What starts a turn, read from the user line that opens it.
const (
	// TriggerOwner is a line typed at the prompt: the owner, or zmina send.
	TriggerOwner = "owner"
	// TriggerHandoff is a task typed in by zmina ([zmina T-5] ...), TriggerSchedule a scheduled run of one.
	TriggerHandoff  = "handoff"
	TriggerSchedule = "schedule"
	// TriggerNotice is a line of zmina's own: the ticker's health notes, idle and release notices, the owner's
	// answers the dispatcher hears of.
	TriggerNotice = "notice"
	// TriggerRemote is a line from another crew (zmina tell); TriggerPeer a message from another Claude session.
	TriggerRemote = "remote"
	TriggerPeer   = "peer"
	// TriggerAgent is a background agent or task that finished; TriggerCompact the summary a compaction leaves.
	TriggerAgent   = "agent"
	TriggerCompact = "compact"
)

// fromNameRe is the sender's name in a peer message's text.
var fromNameRe = regexp.MustCompile(`^<cross-session-message[^>]*\sfrom-name="([^"]*)"`)

// trigger is what a user line that opens a turn says started it; "" for a line that opens none: a tool's
// result, a skill's text, a reminder. from is the sending session of a peer message.
func (l *line) trigger() (trigger, from string) {
	if l.Compact {
		return TriggerCompact, ""
	}

	text := ""
	if !l.toolResult() {
		text = strings.TrimSpace(l.text())
	}

	if l.Origin != nil {
		switch l.Origin.Kind {
		case "peer":
			if from = l.Origin.Name; from == "" {
				if m := fromNameRe.FindStringSubmatch(text); m != nil {
					from = m[1]
				}
			}

			return TriggerPeer, from
		case "task-notification":
			return TriggerAgent, ""
		}
	}

	if l.Meta || l.toolResult() {
		return "", ""
	}

	return textTrigger(text)
}

// TextTrigger classifies a prompt without storing its body in accounting records.
func TextTrigger(text string) (string, string) { return textTrigger(text) }

func textTrigger(text string) (string, string) {
	switch {
	case strings.HasPrefix(text, "<cross-session-message"):
		from := ""
		if m := fromNameRe.FindStringSubmatch(text); m != nil {
			from = m[1]
		}

		return TriggerPeer, from
	case strings.HasPrefix(text, "<task-notification"):
		return TriggerAgent, ""
	case strings.HasPrefix(text, "[zmina T-"):
		if head, _, _ := strings.Cut(text, "]"); strings.HasSuffix(head, ", scheduled") {
			return TriggerSchedule, ""
		}

		return TriggerHandoff, ""
	case strings.HasPrefix(text, "[zmina] remote "):
		return TriggerRemote, ""
	case strings.HasPrefix(text, "[zmina]"):
		return TriggerNotice, ""
	}

	return TriggerOwner, ""
}

func (l *line) toolResult() bool {
	var parts []struct {
		Type string `json:"type"`
	}

	if json.Unmarshal(l.Message.Content, &parts) != nil {
		return false
	}

	return slices.ContainsFunc(parts, func(p struct {
		Type string `json:"type"`
	}) bool {
		return p.Type == "tool_result"
	})
}

// cue is a turn's start in a transcript: when and what.
type cue struct {
	at            time.Time
	trigger, from string
}

// cueAt is the last turn start at or before a time; the owner before the first.
func cueAt(cues []cue, at time.Time) cue {
	i := sort.Search(len(cues), func(i int) bool { return cues[i].at.After(at) })
	if i == 0 {
		return cue{trigger: TriggerOwner}
	}

	return cues[i-1]
}

// Messages reads the replies since a time of a session and of its subagents, each reply once. A subagent's reply
// carries the trigger of the parent's turn it ran in.
func Messages(t Transcript, since time.Time) ([]Message, error) {
	seen := map[string]bool{}

	out, cues, err := readMessages(t.Path, since, seen, nil)
	if err != nil {
		return nil, err
	}

	subs, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(t.Path, ".jsonl"), "subagents", "*.jsonl"))
	for _, p := range subs {
		ms, _, err := readMessages(p, since, seen, cues)
		if err != nil {
			return nil, err
		}

		out = append(out, ms...)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })

	return out, nil
}

// readMessages reads one transcript file. parent is nil for a session's own file, whose turn starts it returns;
// a subagent's file takes its triggers from them.
func readMessages(path string, since time.Time, seen map[string]bool, parent []cue) ([]Message, []cue, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()

	var (
		out  []Message
		cues []cue
	)

	current := cue{trigger: TriggerOwner}
	rd := bufio.NewReaderSize(f, 1<<20)

	for {
		raw, err := rd.ReadBytes('\n')
		if len(raw) > 0 {
			if m, c, ok := parseLine(raw); ok && c.trigger != "" && parent == nil {
				current = c
				cues = append(cues, c)
			} else if ok && c.trigger == "" && !m.At.Before(since) && (m.ID == "" || !seen[m.ID]) {
				seen[m.ID] = true
				m.Sub = parent != nil || m.Sub

				at := current
				if parent != nil {
					at = cueAt(parent, m.At)
				}

				m.Trigger, m.From = at.trigger, at.from
				out = append(out, m)
			}
		}

		if errors.Is(err, io.EOF) {
			return out, cues, nil
		}

		if err != nil {
			return nil, nil, err
		}
	}
}

// parseLine reads a transcript line: a reply with usage (trigger ""), or a user line that starts a turn (its
// trigger). ok is false for any other line.
func parseLine(raw []byte) (m Message, c cue, ok bool) {
	user := bytes.Contains(raw, []byte(`"type":"user"`))
	reply := bytes.Contains(raw, []byte(`"usage"`)) && bytes.Contains(raw, []byte(`"assistant"`))
	if !user && !reply {
		return m, c, false
	}

	// A tool's result is most of a transcript's bytes and never starts a turn.
	if user && bytes.Contains(raw, []byte(`"type":"tool_result"`)) && !bytes.Contains(raw, []byte(`"assistant"`)) {
		return m, c, false
	}

	var l line
	if json.Unmarshal(raw, &l) != nil {
		return m, c, false
	}

	if l.Type == "user" {
		if l.Sidechain {
			return m, c, false
		}

		t, from := l.trigger()

		return Message{At: l.Timestamp}, cue{at: l.Timestamp, trigger: t, from: from}, t != ""
	}

	u := l.Message.Usage
	if l.Type != "assistant" || u == nil || l.tokens() == 0 {
		return m, c, false
	}

	m = Message{ID: l.Message.ID, At: l.Timestamp, Model: l.Message.Model, Input: u.Input, Read: u.CacheRead,
		Output: u.Output, Cwd: l.Cwd, Sub: l.Sidechain}

	if u.Speed != "" && u.Speed != "standard" {
		m.Speed = u.Speed
	}

	// Without the split (an older Claude Code) a cache write is the API's default lifetime, five minutes.
	if s := u.Split; s != nil && s.Write5m+s.Write1h == u.CacheCreation {
		m.Write5m, m.Write1h = s.Write5m, s.Write1h
	} else {
		m.Write5m = u.CacheCreation
	}

	if u.Server != nil {
		m.WebSearch = u.Server.WebSearch
	}

	return m, c, true
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
