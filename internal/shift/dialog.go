package shift

import (
	"regexp"
	"strconv"
	"strings"
)

// Dialog is a question Claude Code shows instead of the input line (AskUserQuestion, a permission prompt): its words and
// its numbered choices, read from the screen. Cursor is the choice the ❯ stands on.
type Dialog struct {
	Question string         `json:"question"`
	Options  []DialogOption `json:"options"`
	Cursor   int            `json:"cursor"`
}

// DialogOption is one numbered choice.
type DialogOption struct {
	N     int    `json:"n"`
	Label string `json:"label"`
}

var optionLine = regexp.MustCompile(`^\s*(❯\s*)?([1-9])\.\s+(.+?)\s*$`)

// freeText are the choices that ask for words, not a pick: the terminal answers them.
var freeText = []string{"Type something.", "Chat about this"}

// ParseDialog reads the last numbered list on a screen and the words above it; nil when there is none of two or more
// choices numbered from 1.
func ParseDialog(screen string) *Dialog {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")

	first, last := -1, -1
	var opts []DialogOption
	cursor := 0

	for i := len(lines) - 1; i >= 0; i-- {
		m := optionLine.FindStringSubmatch(lines[i])
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

	if len(opts) < 2 || opts[0].N != 1 {
		return nil
	}

	d := &Dialog{Cursor: cursor}
	for _, o := range opts {
		free := false
		for _, f := range freeText {
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

	return d
}

// ChooseKeys are the keys that move the dialog's cursor to choice n and take it.
func (d *Dialog) ChooseKeys(n int) []string {
	from := d.Cursor
	if from == 0 {
		from = 1
	}

	var keys []string

	for ; from < n; from++ {
		keys = append(keys, "Down")
	}

	for ; from > n; from-- {
		keys = append(keys, "Up")
	}

	return append(keys, "Enter")
}
