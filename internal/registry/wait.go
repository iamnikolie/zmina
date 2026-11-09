package registry

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

var (
	// sleepRe is a sleep with its length; a short one (under minSleep) is a settle pause, not a wait.
	sleepRe = regexp.MustCompile(`\bsleep\s+(\d+(?:\.\d+)?)([smh]?)\b`)
	// loopRe is a shell loop; it waits only when it sleeps.
	loopRe = regexp.MustCompile(`\b(until|while|for)\b[^\n]*\bdo\b`)
	// watchRe are commands that wait on a rollout or a CI run.
	watchRe = regexp.MustCompile(`\bkubectl\s+rollout\s+status\b|\bgh\s+run\s+watch\b|\bgh\s+pr\s+checks\b[^\n]*--watch`)
	// heredocRe opens a here-document; its body is text for another program, not shell.
	heredocRe = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)
)

// maxLooks is how many calls may stand between two waits of one polling chain: a look at whether it is done may take
// a few (a status, a log, a queue). More are work again.
const maxLooks = 3

// minSleep is the shortest sleep that is a wait (seconds): shorter ones let a server start or a page settle.
const minSleep = 20

// WaitCall reports whether a Bash command run in the foreground waits: a sleep of minSleep or more, a loop that
// sleeps, a watch on a rollout or a CI run. A here-document's body is not read. A command run in the background or a
// Monitor ends the turn and wakes it once: never a wait here.
func WaitCall(command string) bool {
	command = stripHeredocs(command)

	if watchRe.MatchString(command) {
		return true
	}

	sleeps := false
	for _, m := range sleepRe.FindAllStringSubmatch(command, -1) {
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}

		switch m[2] {
		case "m":
			n *= 60
		case "h":
			n *= 3600
		}

		if n >= minSleep {
			return true
		}

		sleeps = true
	}

	return sleeps && loopRe.MatchString(command)
}

// stripHeredocs drops the bodies of a command's here-documents.
func stripHeredocs(command string) string {
	for {
		loc := heredocRe.FindStringSubmatchIndex(command)
		if loc == nil {
			return command
		}

		word := command[loc[2]:loc[3]]
		start := strings.IndexByte(command[loc[1]:], '\n')
		if start < 0 {
			return command[:loc[0]]
		}

		body := command[loc[1]+start+1:]
		end := len(body)

		for i := 0; i < len(body); {
			j := strings.IndexByte(body[i:], '\n')
			line := body[i:]
			if j >= 0 {
				line = body[i : i+j]
			}

			if strings.TrimLeft(line, "\t") == word {
				end = i + len(line)

				break
			}

			if j < 0 {
				break
			}

			i += j + 1
		}

		command = command[:loc[0]] + " " + body[min(end, len(body)):]
	}
}

// toolCalls reads the tool calls of an assistant line, in order: for each, whether it waits (a foreground Bash call,
// see WaitCall). Any other call — another tool, a background command, a Monitor — is false.
func toolCalls(raw []byte) []bool {
	if !bytes.Contains(raw, []byte(`"tool_use"`)) {
		return nil
	}

	var l struct {
		Message struct {
			Content []struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				Input struct {
					Command    string `json:"command"`
					Background bool   `json:"run_in_background"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &l) != nil {
		return nil
	}

	var out []bool

	for _, b := range l.Message.Content {
		if b.Type == "tool_use" {
			out = append(out, b.Name == "Bash" && !b.Input.Background && WaitCall(b.Input.Command))
		}
	}

	return out
}

// waitChain follows polling in a conversation: foreground waits with at most maxLooks calls between them (the look
// at whether it is done), with no new prompt between. The first wait and its look cost what a background wait costs
// too (woken once, one look); from the second wait of a chain on, the reply after each wait is a wait turn, and the
// reply after the look that came before it. More calls in a row that are not waits are work again: the chain ends.
type waitChain struct {
	waits, since, left int
	pending            bool
	by                 string
}

// prompt is a new turn: the chain ends.
func (w *waitChain) prompt() { *w = waitChain{} }

// reply is a reply seen for the first time: whether it is a wait turn.
func (w *waitChain) reply(id string) bool {
	if !w.pending || id == w.by {
		return false
	}

	w.pending = false
	if w.left == 0 {
		return false
	}

	w.left--

	return true
}

// calls are the tool calls a reply made.
func (w *waitChain) calls(id string, waiting []bool) {
	for _, wait := range waiting {
		w.pending, w.by = true, id

		if !wait {
			if w.since++; w.since > maxLooks {
				w.waits, w.left = 0, 0
			}

			continue
		}

		looked := w.since > 0
		if w.waits > 0 && w.since <= maxLooks {
			w.waits++
		} else {
			w.waits = 1
		}

		w.since = 0

		// A look of its own stood between the waits: its reply was spent too. A look inside the wait's own command
		// (sleep 60; gl pipeline show) costs no reply of its own.
		if w.waits >= 2 {
			w.left = 1
			if looked {
				w.left = 2
			}
		}
	}
}
