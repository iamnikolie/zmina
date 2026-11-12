package cmd

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/shift"
)

// queueWords is how many characters of a queued line's words queue ls shows, unless --full.
const queueWords = 160

var (
	// queueHeadID is an id at the head of a queued line: "S-12 mrs: …" (a signal), "T-12 third commit …".
	queueHeadID = regexp.MustCompile(`^((?:LS|[TALSR])-[0-9]+)[: ] ?`)
	// queueTailID is the ack ids zmina puts at the very end of a tracked line: "… (zmina S-14)", "… (zmina S-14 S-15)",
	// "… (zmina S-14, still pending since Mon 9 Nov 12:00)".
	queueTailID = regexp.MustCompile(`(?: |^)\(zmina ((?:S-[0-9]+ ?)+)(?:, still pending since [^()]*)?\)$`)
)

// queueLine is a queued line as queue ls shows it: its words first, clipped, and the ids it carries after them
// (`fix the login · T-12 · S-610`), so a line reads as what it says and the id is still there to act on. A line with no
// id is only clipped. JSON output does not pass through here.
func queueLine(text string, full bool) string {
	words, ids := queueSplit(text)

	if !full {
		words = clip(oneLine(words), queueWords)
	}

	if len(ids) == 0 {
		return words
	}

	return words + " · " + strings.Join(ids, " · ")
}

// queueSplit takes the ids off the head and the tail of a line: the words, and the ids in the order they stood. A line
// that is nothing but an id is left whole.
func queueSplit(text string) (string, []string) {
	var head, tail []string

	words := text

	if m := queueHeadID.FindStringSubmatch(words); m != nil {
		head = append(head, m[1])
		words = words[len(m[0]):]
	}

	if loc := queueTailID.FindStringSubmatchIndex(words); loc != nil {
		tail = strings.Fields(words[loc[2]:loc[3]])
		words = words[:loc[0]]
	}

	if strings.TrimSpace(words) == "" {
		return text, nil
	}

	return words, append(head, tail...)
}

// queueFactsIndent is what the line of facts under a queued line starts with.
const queueFactsIndent = "      "

// queueFacts is what zmina saw of a line, from its card, in one line: who wrote it and when, when it was accepted and how,
// when it was seen, when it was given back or needed a look. Empty when there is no card or nothing to say.
func queueFacts(c *shift.Card) string {
	if c == nil {
		return ""
	}

	at := func(t time.Time) string { return t.Local().Format("01-02 15:04") }

	var parts []string

	switch {
	case c.From != "" && !c.Written.IsZero():
		parts = append(parts, "from "+c.From+" "+at(c.Written))
	case c.From != "":
		parts = append(parts, "from "+c.From)
	case !c.Written.IsZero():
		parts = append(parts, "written "+at(c.Written))
	}

	if c.Task != "" {
		parts = append(parts, "on "+c.Task)
	}

	if c.Back != nil {
		back := "given back " + at(c.Back.At)
		if c.Back.N > 1 {
			back += fmt.Sprintf(" (%d times)", c.Back.N)
		}

		parts = append(parts, back)
	}

	if a := c.Accepted; a != nil {
		parts = append(parts, "accepted "+at(a.At)+" "+a.How)
	}

	// A line that went in a doorbell says so, and whether its body was fetched: the bell shown is not the line seen.
	switch b, s := c.Bell, c.Seen; {
	case b != nil && s != nil && s.By == shift.SeenFetch:
		parts = append(parts, "bell, fetched "+at(s.At))
	case b != nil && !b.Shown.IsZero():
		parts = append(parts, "bell shown "+at(b.Shown)+", not fetched")
	case b != nil:
		parts = append(parts, "bell, not shown yet")
	case s != nil:
		parts = append(parts, "seen "+at(s.At)+" "+s.By)
	}

	if a := c.Attention; a != nil {
		parts = append(parts, "attention "+at(a.At))
	}

	// A card shown for a row that is not in play (a failed line's, closed when it failed) says how it ended.
	if k := c.Closed; k != nil {
		parts = append(parts, "closed "+k.How+" "+at(k.At))
	}

	return strings.Join(parts, " · ")
}
