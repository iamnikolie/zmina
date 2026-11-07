package chat

import (
	"regexp"
	"strings"
)

// Claude Code records a command run at its prompt as user lines of its own: the command in <command-name>,
// <command-message> and <command-args> elements, then (a local command) what it printed in <local-command-stdout> or
// <local-command-stderr>, dimmed with terminal escapes. None of them is the owner's message.
var (
	commandPart = regexp.MustCompile(`<command-(name|message|args)>([^<]*)</command-(name|message|args)>`)
	localOutput = regexp.MustCompile(`^<local-command-(stdout|stderr)>((?s:.*))</local-command-(stdout|stderr)>$`)
	// ansi is a terminal escape: CSI (colours, cursor), OSC (titles, links) to BEL or ST, or a two-byte one.
	ansi = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)?|[@-Z\\-_])`)
	// hookLine is Claude Code's report of a hook it ran around the command («PreCompact [command hook 1] completed
	// successfully»): the machine's plumbing, not the command's answer.
	hookLine = regexp.MustCompile(`^\w+ \[[^\]]*\bhook\b[^\]]*\]`)
)

// ParseCommand reads a command line Claude Code recorded, the elements in any order and nothing else: its name
// ("/compact") and its arguments.
func ParseCommand(text string) (name, args string, ok bool) {
	parts := commandPart.FindAllStringSubmatch(text, -1)
	if len(parts) == 0 || strings.TrimSpace(commandPart.ReplaceAllString(text, "")) != "" {
		return "", "", false
	}

	for _, p := range parts {
		if p[1] != p[3] {
			return "", "", false
		}

		switch p[1] {
		case "name":
			name = strings.TrimSpace(p[2])
		case "args":
			args = strings.TrimSpace(p[2])
		}
	}

	if !strings.HasPrefix(name, "/") || len(name) < 2 {
		return "", "", false
	}

	return name, args, true
}

// ParseLocalOutput reads what a local command printed, its escapes and hook lines taken out; failed is true for
// what it wrote to stderr.
func ParseLocalOutput(text string) (out string, failed, ok bool) {
	m := localOutput.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil || m[1] != m[3] {
		return "", false, false
	}

	var keep []string

	for _, line := range strings.Split(StripANSI(m[2]), "\n") {
		if !hookLine.MatchString(strings.TrimSpace(line)) {
			keep = append(keep, line)
		}
	}

	return strings.TrimSpace(strings.Join(keep, "\n")), m[1] == "stderr", true
}

// StripANSI takes terminal escapes out of a text: a browser shows them as boxes and brackets.
func StripANSI(text string) string {
	if !strings.ContainsRune(text, 0x1b) {
		return text
	}

	return strings.ReplaceAll(ansi.ReplaceAllString(text, ""), "\x1b", "")
}

// CommandEvent is the one quiet line a command shows as in the conversation: what it did, never its tags.
func CommandEvent(name, args, output string, failed bool) *Event {
	first, _, _ := strings.Cut(output, "\n")
	first = strings.TrimSpace(strings.TrimLeft(first, "# "))

	summary := strings.TrimSpace(name + " " + args)

	switch {
	case name == "/compact" && !failed:
		summary = "Context compacted"
	case name == "/clear" && !failed:
		summary = "Context cleared"
	case name == "" && first != "":
		summary = first
	case first != "":
		summary += " · " + first
	}

	e := &Event{Kind: "command", Summary: clip(summary, 160)}
	if failed {
		e.Status = "failed"
	}

	if strings.Contains(output, "\n") {
		e.Detail = clip(output, 16000)
	}

	return e
}

func clip(text string, limit int) string {
	if r := []rune(text); len(r) > limit {
		return string(r[:limit]) + "…"
	}

	return text
}

// Clean takes terminal escapes out of every text of a message that reaches the page.
func Clean(m Message) Message {
	m.Text = StripANSI(m.Text)

	if m.Event != nil {
		e := *m.Event
		e.Summary, e.Result, e.Detail = StripANSI(e.Summary), StripANSI(e.Result), StripANSI(e.Detail)
		m.Event = &e
	}

	return m
}
