package cmd

// The peek footer and the browser chat's dialog readers as they were before the rules table: copied from the code that
// kept every marker in a Go literal, renamed with a legacy prefix, and kept here only to prove that the table reads every
// screen the same way (screen_equivalence_test.go). Delete this file together with that test once the table has been in
// use for a release.

import (
	"regexp"
	"strconv"
	"strings"
)

// compactScreen is a captured screen without what carries no words: trailing spaces, blank lines, rules, and the
// footer under the input line (path, model, mode). The input line itself stays, with a draft when there is one.
// The last n lines are kept. The footer is cut only when it is recognised as such (footerAt); anything else under
// the input box — a dialog, boxed or not, an option list, any text — keeps the whole screen: a few lines too many
// cost a few tokens, a lost dialog costs the reader the question it has to answer.
func legacyCompactScreen(text string, n int) string {
	lines := strings.Split(text, "\n")
	if end := legacyFooterAt(lines); end >= 0 {
		lines = lines[:end]
	}

	var out []string

	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if strings.TrimSpace(l) == "" || isRule(l) {
			continue
		}

		out = append(out, l)
	}

	if len(out) > n {
		out = out[len(out)-n:]
	}

	return strings.Join(out, "\n")
}

// footerAt is the index of the rule under Claude Code's input box when that box is the last block on the screen and
// only its own footer follows; -1 otherwise, and then nothing is cut. Read from the bottom up: blank lines, then
// footer lines (isFooterLine; the first under the rule may be the status line), then the closing rule, then the
// input line with its draft, then the opening rule. Any line that does not fit, or an input line that reads as a
// dialog's option, and it is not the input box.
func legacyFooterAt(lines []string) int {
	i := len(lines) - 1
	for i >= 0 && strings.TrimSpace(lines[i]) == "" {
		i--
	}

	footer := i
	for i >= 0 && !isRule(lines[i]) {
		i--
	}

	if i < 0 {
		return -1
	}

	closing := i
	for k := closing + 1; k <= footer; k++ {
		if !legacyIsFooterLine(lines[k], k == closing+1) {
			return -1
		}
	}

	// The box: from the opening rule down to the closing one, the ❯ line first, then a draft's further lines.
	i = closing - 1
	for i >= 0 && !isRule(lines[i]) {
		i--
	}

	if i < 0 {
		return -1
	}

	body := lines[i+1 : closing]
	for len(body) > 0 && strings.TrimSpace(body[0]) == "" {
		body = body[1:]
	}

	if len(body) == 0 || !strings.HasPrefix(body[0], "❯") || legacyOptionRe.MatchString(body[0]) {
		return -1
	}

	for _, l := range body[1:] {
		if legacyOptionRe.MatchString(l) || strings.ContainsAny(l, "❯│┃") {
			return -1
		}
	}

	return closing
}

// legacyOptionRe is a line that reads as a dialog's option: a number and a dot or bracket, maybe behind a cursor or a box.
var legacyOptionRe = regexp.MustCompile(`^\s*[│┃]?\s*(❯\s*)?\d+[.)]\s`)

// legacyFooterHints are the words of Claude Code's own lines under the input box: the mode line and its hints.
var legacyFooterHints = []string{
	"⏵⏵", "⏸", "? for shortcuts", "shift+tab to cycle", "esc to interrupt", "until auto-compact",
	"Auto-update failed", "Press Ctrl-C again", "ctrl+t to", "ctrl+o to",
}

// isFooterLine is a line Claude Code draws under its input box: indented, no box or cursor in it, and either one of
// its hint lines or — right under the rule — a status line (a path or a [model] in it).
func legacyIsFooterLine(l string, first bool) bool {
	if strings.TrimSpace(l) == "" {
		return true
	}

	if !strings.HasPrefix(l, "  ") || strings.ContainsAny(l, "❯│┃╭╰") || legacyOptionRe.MatchString(l) {
		return false
	}

	for _, h := range legacyFooterHints {
		if strings.Contains(l, h) {
			return true
		}
	}

	t := strings.TrimSpace(l)

	return first && (strings.HasPrefix(t, "~/") || strings.HasPrefix(t, "/") || legacyModelRe.MatchString(t))
}

// legacyModelRe is a status line's model in brackets: [Opus 5.5], [Sonnet], [gpt-6].
var legacyModelRe = regexp.MustCompile(`\[[A-Za-z][\w .-]*\]`)

// dialogOption is a numbered row of the active question dialog: its number, label and, for ticks, whether ticked.
type legacyDialogOption struct {
	N       int
	Label   string
	Ticked  bool
	Current bool
}

var legacyDialogRow = regexp.MustCompile(`^\s*(❯\s*)?(\d+)\.\s+(?:\[([ ✔])\]\s+)?(.*?)\s*$`)

// activeDialog reads the question dialog at the bottom of the screen: the last place the question starts, its whole
// text from there, and the numbered rows under it in order. ok is false when that question is not the active one.
func legacyActiveDialog(screen, question string) ([]legacyDialogOption, bool) {
	flat := func(t string) string { return strings.Join(strings.Fields(t), " ") }
	q := flat(question)
	head := []rune(q)
	if len(head) > 20 {
		head = head[:20]
	}
	// The question's text is read without the dialog's frame; its rows are read as drawn: a row inside a box (another
	// dialog's) is not one of this dialog's.
	raw, lines := strings.Split(screen, "\n"), legacyDialogLines(screen)
	at := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(flat(lines[i]), string(head)) {
			at = i
			break
		}
	}
	if q == "" || at < 0 || !strings.Contains(flat(strings.Join(lines[at:], " ")), q) {
		return nil, false
	}
	var out []legacyDialogOption
	current := 0
	for _, l := range raw[at:] {
		m := legacyDialogRow.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		if n == 1 && len(out) > 0 {
			return nil, false // numbering starts again: another dialog below this text, the active one is not it
		}
		if n != len(out)+1 {
			continue // a number in the question's own text, not a row
		}
		out = append(out, legacyDialogOption{N: n, Label: flat(m[4]), Ticked: m[3] == "✔", Current: m[1] != ""})
		if m[1] != "" {
			current++
		}
	}
	// The active dialog has its cursor on exactly one of its rows; text that only looks like one has none.
	return out, len(out) > 0 && current == 1
}

// legacyDialogBar is the rule Claude Code draws down the left of a question's text (and a box's sides): a frame, not words.
var legacyDialogBar = regexp.MustCompile(`^\s*[│┃▏▎]\s?|\s*[│┃]\s*$`)

// dialogLines are the screen's lines without the dialog's frame, so a question wrapped inside it reads as its words;
// one line for each of the screen's, in order.
func legacyDialogLines(screen string) []string {
	lines := strings.Split(screen, "\n")
	for i, l := range lines {
		lines[i] = legacyDialogBar.ReplaceAllString(l, "")
	}
	return lines
}
