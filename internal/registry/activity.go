package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// activitySize is how much of a transcript's end Activity reads: background work a session started further back
// than this is not seen.
const activitySize = 4 << 20

// Activity is what the end of a session's conversation says about who spoke last and what still runs.
type Activity struct {
	// Prompt is when the last turn began: the newest line that starts one (a prompt, a peer's message, a background
	// task's report), zero when none is in the part read.
	Prompt time.Time
	// Owner is when the newest line typed at the prompt was written: the owner's (Chat, a terminal, Remote Control) —
	// zmina's own lines, peers' messages and background reports are not his. Zero when none is in the part read.
	Owner time.Time
	// Background are the ids of background work the session started (a background shell, a Monitor, a background
	// agent) that has not reported its end.
	Background []string
}

var (
	taskIDRe     = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)
	taskStatusRe = regexp.MustCompile(`<status>([^<]+)</status>`)
)

// Activity reads the end of the session's transcript for the last turn's start, the owner's last line and the
// background work still running.
func (r *Registry) Activity(sessionID string) (Activity, error) {
	path := r.transcript(sessionID)
	if path == "" {
		return Activity{}, nil
	}

	return readActivity(path)
}

func readActivity(path string) (Activity, error) {
	f, err := os.Open(path)
	if err != nil {
		return Activity{}, err
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return Activity{}, err
	}

	off := max(st.Size()-activitySize, 0)

	tail := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(tail, off); err != nil && !errors.Is(err, io.EOF) {
		return Activity{}, err
	}

	raw := bytes.Split(tail, []byte("\n"))
	if off > 0 {
		raw = raw[1:]
	}

	var (
		a       Activity
		started []string
		ended   = map[string]bool{}
	)

	for _, b := range raw {
		// Only user lines matter — prompts, tool results (a launch's id) and reports — and the attachments a line that
		// came in mid-turn is recorded as.
		if !bytes.Contains(b, []byte(`"type":"user"`)) && !bytes.Contains(b, []byte(`"type":"attachment"`)) {
			continue
		}

		var l struct {
			line
			Attachment struct {
				Type   string `json:"type"`
				Prompt string `json:"prompt"`
			} `json:"attachment"`
			Result struct {
				Shell   string `json:"backgroundTaskId"`
				Monitor string `json:"taskId"`
				Agent   string `json:"agentId"`
				Async   bool   `json:"isAsync"`
			} `json:"toolUseResult"`
		}
		if len(bytes.TrimSpace(b)) == 0 || json.Unmarshal(b, &l) != nil || l.Sidechain {
			continue
		}

		// A line queued while a turn ran (the owner's, a peer's, a background report) goes into that turn as an
		// attachment: it begins no turn, but it is said.
		if l.Type == "attachment" {
			if l.Attachment.Type == "queued_command" {
				trigger, _ := textTrigger(strings.TrimSpace(l.Attachment.Prompt))
				a.saw(l.Timestamp, l.Attachment.Prompt, trigger, ended, false)
			}

			continue
		}

		if l.Type != "user" {
			continue
		}

		if l.toolResult() {
			switch {
			case l.Result.Shell != "":
				started = append(started, l.Result.Shell)
			case l.Result.Monitor != "":
				started = append(started, l.Result.Monitor)
			case l.Result.Agent != "" && l.Result.Async:
				started = append(started, l.Result.Agent)
			}

			continue
		}

		if l.Meta || l.Compact {
			continue
		}

		trigger, _ := l.trigger()
		if trigger == "" {
			continue
		}

		a.saw(l.Timestamp, l.text(), trigger, ended, true)
	}

	for _, id := range started {
		if !ended[id] {
			a.Background = append(a.Background, id)
		}
	}

	return a, nil
}

// saw notes a line said to the session at a time: a background report ends its work, the owner's line is his, and a
// line that begins a turn (prompt) is the turn's start.
func (a *Activity) saw(at time.Time, text, trigger string, ended map[string]bool, prompt bool) {
	// A report with a status is the end of that work; a Monitor's event has none and the watch goes on.
	if trigger == TriggerAgent {
		for _, block := range strings.Split(text, "</task-notification>") {
			if id := taskIDRe.FindStringSubmatch(block); id != nil && taskStatusRe.MatchString(block) {
				ended[id[1]] = true
			}
		}
	}

	if prompt {
		a.Prompt = at
	}

	if trigger == TriggerOwner && !commandLine(text) {
		a.Owner = at
	}
}

// commandLine is a slash command's record (<command-name>…) or its output: typed at the prompt, but it starts no
// exchange with the owner (zmina types /compact itself).
func commandLine(text string) bool {
	return bytes.HasPrefix(bytes.TrimSpace([]byte(text)), []byte("<command-")) ||
		bytes.HasPrefix(bytes.TrimSpace([]byte(text)), []byte("<local-command-"))
}
