package registry

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

var (
	// waitRe are commands that wait in the foreground: a sleep, a shell loop until something is so, a watch on a
	// rollout or a CI run.
	waitRe = regexp.MustCompile(`\bsleep\s+\d|\b(until|while)\b[^\n]*\bdo\b|\bkubectl\s+rollout\s+status\b|` +
		`\bgh\s+run\s+watch\b|\bgh\s+pr\s+checks\b[^\n]*--watch`)
	// checkRe are status checks of CI and deploys: one is a look, the same one again is a poll.
	checkRe = regexp.MustCompile(`\b(gl|glab)\b[^\n]*\b(pipeline|ci|job)s?\b|\bgh\s+(run|pr\s+checks)\b|` +
		`\bhelm\s+status\b|\bargocd\s+app\s+(get|wait)\b`)
)

// WaitCall reads a Bash command run in the foreground: whether it waits (a sleep, a wait loop, a watch), and the
// status check it is, if any (CI, a deploy). A command run in the background or a Monitor ends the turn and wakes
// it once: never a wait here.
func WaitCall(command string) (waits bool, check string) {
	if waitRe.MatchString(command) {
		return true, ""
	}

	if checkRe.MatchString(command) {
		return false, strings.Join(strings.Fields(command), " ")
	}

	return false, ""
}

// waitCalls reads the tool calls of an assistant line: whether one waits in the foreground, and the last status
// check among them.
func waitCalls(raw []byte) (waits bool, check string) {
	if !bytes.Contains(raw, []byte(`"tool_use"`)) || !bytes.Contains(raw, []byte(`"Bash"`)) {
		return false, ""
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
		return false, ""
	}

	for _, b := range l.Message.Content {
		if b.Type != "tool_use" || b.Name != "Bash" || b.Input.Background {
			continue
		}

		w, c := WaitCall(b.Input.Command)
		waits = waits || w

		if c != "" {
			check = c
		}
	}

	return waits, check
}
