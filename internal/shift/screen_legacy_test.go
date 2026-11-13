package shift

// The screen readers as they were before the rules table: copied from the code that read every marker from a Go literal,
// renamed with a legacy prefix and kept here only to prove that the table reads every screen the same way
// (screen_equivalence_test.go). Delete this file together with that test once the table has been in use for a release.

import (
	"regexp"
	"strconv"
	"strings"
)

// legacyLimitRe finds Claude Code telling that a usage limit stops the role.
var legacyLimitRe = regexp.MustCompile(`(?i)(usage limit reached|usage credit limit|hit your (\w+ )?limit|` +
	`limit (reached|will reset)|out of extra usage|/rate-limit-options)`)

// legacyLimitLine is what watchScreen did with the rows it captured: the last row the limit regexp matches, clipped.
func legacyLimitLine(text string) string {
	limit := ""

	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); legacyLimitRe.MatchString(l) && !strings.HasPrefix(l, "❯") {
			limit = clip(l)
		}
	}

	return limit
}

var (
	// legacyToolRe matches a ⏺ line that is a tool call rather than words: Bash(…), Update(…), mcp__x__y (MCP)(…),
	// "Read 3 files (ctrl+o to expand)", "Searched for 2 patterns", a subagent's "main ◯ …" line, "Stop Task".
	legacyToolRe = regexp.MustCompile(`^([\w.:-]+\(|.*\(MCP\)|.*ctrl\+o to expand|(Read|Reading|Searched|Searching|Listed|` +
		`Wrote|Writing|Fetched|Ran|Running|Called|Found|Updated|Updating|Making|Editing|Edited) .*(\d|…)|.*◯|` +
		`.*↑ [\d.]+k? tokens|(Stop Task|Task Output)\b|Agent ")`)
	legacySpaceRe = regexp.MustCompile(`\s+`)
)

// legacyNowLine is the last thing the role said in words on its screen: the last "⏺ …" line that is not a tool call,
// with the lines it wraps onto, on one line and clipped. Empty when the screen has none.
func legacyNowLine(screen string) string {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		rest, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), "⏺")
		rest = strings.TrimSpace(rest)

		if !ok || rest == "" || legacyToolRe.MatchString(rest) {
			continue
		}

		// A long line wraps onto lines indented under it.
		for _, l := range lines[i+1:] {
			if !strings.HasPrefix(l, "  ") || strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "⎿") {
				break
			}

			rest += " " + strings.TrimSpace(l)
		}

		return clip(legacySpaceRe.ReplaceAllString(rest, " "))
	}

	return ""
}

// legacyTrustOpt matches the answers of Claude Code's folder-trust question.
var legacyTrustOpt = regexp.MustCompile(`(?i)^(❯\s*)?(\d\.\s*)?(yes, i trust this folder|yes, proceed|no, exit)`)

// legacyTrustKeys is what trust did with the screen it captured, up to the keys it pressed (nil: it pressed none).
func legacyTrustKeys(text string) []string {
	if !strings.Contains(text, "trust") {
		return nil
	}

	var opts []string

	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); legacyTrustOpt.MatchString(l) {
			opts = append(opts, l)
		}
	}

	sel, yes := -1, -1

	for i, o := range opts {
		if strings.HasPrefix(o, "❯") {
			sel = i
		}

		if strings.Contains(strings.ToLower(o), "yes") {
			yes = i
		}
	}

	if sel < 0 || yes < 0 {
		return nil
	}

	var keys []string

	for i := sel; i < yes; i++ {
		keys = append(keys, "Down")
	}

	for i := sel; i > yes; i-- {
		keys = append(keys, "Up")
	}

	return append(keys, "Enter")
}

// legacyExitDialog and legacyExitKey are what answerExit did with the screen it captured.
var legacyExitDialog = regexp.MustCompile(`(\d)\. Exit and stop tasks`)

func legacyExitKey(screen string) (string, bool) {
	m := legacyExitDialog.FindStringSubmatch(screen)
	if m == nil {
		return "", false
	}

	return m[1], true
}

var legacyOptionLine = regexp.MustCompile(`^\s*(❯\s*)?([1-9])\.\s+(.+?)\s*$`)

// legacyFreeText are the choices that ask for words, not a pick: the terminal answers them.
var legacyFreeText = []string{"Type something.", "Chat about this"}

// legacyParseDialog reads the last numbered list on a screen and the words above it; nil when there is none of two or more
// choices numbered from 1.
func legacyParseDialog(screen string) *Dialog {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")

	first, last := -1, -1
	var opts []DialogOption
	cursor := 0

	for i := len(lines) - 1; i >= 0; i-- {
		m := legacyOptionLine.FindStringSubmatch(lines[i])
		if m == nil {
			if first >= 0 && strings.TrimSpace(lines[i]) != "" && !strings.HasPrefix(lines[i], "     ") {
				break // the words above the list
			}

			continue
		}

		n, _ := strconv.Atoi(m[2])
		if len(opts) > 0 && n != opts[0].N-1 {
			break
		}

		opts = append([]DialogOption{{N: n, Label: m[3]}}, opts...)
		if m[1] != "" {
			cursor = n
		}

		first = i
		if last < 0 {
			last = i
		}

		if n == 1 {
			break
		}
	}

	// A live dialog, not a numbered list in the conversation: the ❯ stands on a choice, and the screen ends with the
	// dialog's own footer (nothing but choice descriptions and blank lines between).
	if len(opts) < 2 || opts[0].N != 1 || cursor == 0 {
		return nil
	}

	// The footer is found by where it stands, never by words a description may also hold: the screen's last
	// non-blank line, not indented as a description is. Between the last choice and it, only descriptions and blanks.
	end := len(lines) - 1
	for end > last && strings.TrimSpace(lines[end]) == "" {
		end--
	}

	if end <= last || isDetail(lines[end]) {
		return nil
	}

	if footerWords := strings.Contains(lines[end], "Enter to select") || strings.Contains(lines[end], "Esc to cancel"); !footerWords {
		return nil
	}

	for _, l := range lines[last+1 : end] {
		if strings.TrimSpace(l) != "" && !isDetail(l) {
			return nil
		}
	}

	// Each choice's description: the indented lines under it, up to the next choice or the footer, whatever they say.
	at := -1
	for _, l := range lines[first:end] {
		if m := legacyOptionLine.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[2])
			at = n - 1
			continue
		}
		if t := strings.TrimSpace(l); t != "" && isDetail(l) && at >= 0 && at < len(opts) {
			opts[at].Detail = strings.TrimSpace(opts[at].Detail + " " + t)
		}
	}

	d := &Dialog{Cursor: cursor}
	for _, o := range opts {
		free := false
		for _, f := range legacyFreeText {
			free = free || strings.HasPrefix(o.Label, f)
		}

		if !free {
			d.Options = append(d.Options, o)
		}
	}

	// The question: the lines above the list, up to a blank line or the dialog's title (☐), without its frame.
	var q []string
	for i := first - 1; i >= 0 && i >= first-12; i-- {
		l := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(lines[i]), "│|"))
		if l == "" {
			if len(q) > 0 {
				break
			}

			continue
		}

		if strings.HasPrefix(l, "☐") {
			q = append([]string{strings.TrimSpace(strings.TrimPrefix(l, "☐"))}, q...)

			break
		}

		q = append([]string{l}, q...)
	}

	d.Question = strings.Join(q, " ")

	if len(d.Options) == 0 {
		return nil
	}

	d.ID = dialogID(d.Question, opts)

	return d
}
