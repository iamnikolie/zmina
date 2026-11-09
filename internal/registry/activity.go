package registry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// activitySize is how much of a transcript's end Activity reads whole: the last turn's start and the owner's last
// line are looked for there. Before it only the lines that launch or end background work are read, so work a
// session started long ago and still runs holds a restart too.
const activitySize = 4 << 20

// BootMark is in every boot line zmina gives a role (the rules after its boot): typed at the prompt as the start's
// first line, but zmina's words, not the owner's.
const BootMark = "[zmina] Questions for the owner:"

// Activity is what a session's conversation says about who spoke last and what still runs.
type Activity struct {
	// Prompt is when the last turn began: the newest line that starts one (a prompt, a peer's message, a background
	// task's report), zero when none is in the transcript's last activitySize.
	Prompt time.Time
	// Owner is when the newest line typed at the prompt was written: the owner's (Chat, a terminal, Remote Control) —
	// zmina's own lines (its boot too), peers' messages and background reports are not his. Zero when none is in the
	// transcript's last activitySize.
	Owner time.Time
	// Background are the ids of background work the session started (a background shell, a Monitor, a background
	// agent) that has not reported its end, anywhere in its transcript.
	Background []string
}

var (
	taskIDRe     = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)
	taskStatusRe = regexp.MustCompile(`<status>([^<]+)</status>`)
)

// Activity reads the session's transcript for the last turn's start, the owner's last line and the background work
// still running.
func (r *Registry) Activity(sessionID string) (Activity, error) {
	path := r.transcript(sessionID)
	if path == "" {
		return Activity{}, nil
	}

	return readActivity(path)
}

// backgroundMarks are in every line that launches background work (its tool result) or reports its end.
var backgroundMarks = [][]byte{[]byte(`"backgroundTaskId"`), []byte(`"taskId"`), []byte(`"isAsync"`),
	[]byte("task-notification"), []byte(`"task_id"`), []byte(`"shell_id"`)}

// background reports a line that may launch or end background work: the only lines read before the transcript's
// last activitySize, where reading every line whole would cost a beat too much.
func background(b []byte) bool {
	return slices.ContainsFunc(backgroundMarks, func(m []byte) bool { return bytes.Contains(b, m) })
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

	var (
		a       Activity
		started []string
		ended   = map[string]bool{}
		tail    = max(st.Size()-activitySize, 0)
		pos     int64
		rd      = bufio.NewReaderSize(f, 64<<10)
	)

	for {
		b, err := rd.ReadBytes('\n')
		head := pos < tail
		pos += int64(len(b))

		if len(b) > 0 && (!head || background(b)) {
			started = a.read(b, head, started, ended)
		}

		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return Activity{}, err
		}
	}

	for _, id := range started {
		if !ended[id] {
			a.Background = append(a.Background, id)
		}
	}

	return a, nil
}

// read takes one transcript line: a launch of background work it adds to started, an end it marks in ended, and —
// unless the line is before the transcript's last activitySize (head) — a turn's start and the owner's line.
func (a *Activity) read(b []byte, head bool, started []string, ended map[string]bool) []string {
	// Only user lines matter — prompts, tool results (a launch's id) and reports — and the attachments a line that
	// came in mid-turn is recorded as.
	if !bytes.Contains(b, []byte(`"type":"user"`)) && !bytes.Contains(b, []byte(`"type":"attachment"`)) {
		return started
	}

	var l struct {
		line
		Attachment struct {
			Type   string `json:"type"`
			Prompt string `json:"prompt"`
			Origin *struct {
				Kind string `json:"kind"`
			} `json:"origin"`
		} `json:"attachment"`
		Result struct {
			Shell   string `json:"backgroundTaskId"`
			Monitor string `json:"taskId"`
			Agent   string `json:"agentId"`
			Async   bool   `json:"isAsync"`
			// A TaskStop (KillShell before it) names the work it stopped; no report of its end follows.
			Stopped string `json:"task_id"`
			Killed  string `json:"shell_id"`
			Message string `json:"message"`
		} `json:"toolUseResult"`
	}
	if len(bytes.TrimSpace(b)) == 0 || json.Unmarshal(b, &l) != nil || l.Sidechain {
		return started
	}

	// A line queued while a turn ran (the owner's, a peer's, a background report) goes into that turn as an
	// attachment: it begins no turn, but it is said.
	if l.Type == "attachment" {
		if l.Attachment.Type == "queued_command" {
			// Who sent it is in its origin, as on a user line: a peer's text (zmina's tagged lines too) may start
			// with anything.
			trigger, _ := textTrigger(strings.TrimSpace(l.Attachment.Prompt))
			if o := l.Attachment.Origin; o != nil && o.Kind == "peer" {
				trigger = TriggerPeer
			} else if o != nil && o.Kind == "task-notification" {
				trigger = TriggerAgent
			}

			a.saw(l.Timestamp, l.Attachment.Prompt, trigger, ended, false, head)
		}

		return started
	}

	if l.Type != "user" {
		return started
	}

	if l.toolResult() {
		switch {
		case l.Result.Shell != "":
			started = append(started, l.Result.Shell)
		case l.Result.Monitor != "":
			started = append(started, l.Result.Monitor)
		case l.Result.Agent != "" && l.Result.Async:
			started = append(started, l.Result.Agent)
		case strings.HasPrefix(l.Result.Message, "Successfully stopped") && l.Result.Stopped != "":
			ended[l.Result.Stopped] = true
		case strings.HasPrefix(l.Result.Message, "Successfully killed") && l.Result.Killed != "":
			ended[l.Result.Killed] = true
		}

		return started
	}

	if l.Meta || l.Compact {
		return started
	}

	if trigger, _ := l.trigger(); trigger != "" {
		a.saw(l.Timestamp, l.text(), trigger, ended, true, head)
	}

	return started
}

// saw notes a line said to the session at a time: a background report ends its work, the owner's line is his, and a
// line that begins a turn (prompt) is the turn's start. Before the transcript's last activitySize (head) only the ends
// count.
func (a *Activity) saw(at time.Time, text, trigger string, ended map[string]bool, prompt, head bool) {
	// A report with a status is the end of that work; a Monitor's event has none and the watch goes on.
	if trigger == TriggerAgent {
		for _, block := range strings.Split(text, "</task-notification>") {
			if id := taskIDRe.FindStringSubmatch(block); id != nil && taskStatusRe.MatchString(block) {
				ended[id[1]] = true
			}
		}
	}

	if head {
		return
	}

	if prompt {
		a.Prompt = at
	}

	if trigger == TriggerOwner && !commandLine(text) && !strings.Contains(text, BootMark) {
		a.Owner = at
	}
}

// commandLine is a slash command's record (<command-name>…) or its output: typed at the prompt, but it starts no
// exchange with the owner (zmina types /compact itself).
func commandLine(text string) bool {
	return bytes.HasPrefix(bytes.TrimSpace([]byte(text)), []byte("<command-")) ||
		bytes.HasPrefix(bytes.TrimSpace([]byte(text)), []byte("<local-command-"))
}
