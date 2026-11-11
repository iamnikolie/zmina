package cmd

import (
	"regexp"
	"strings"
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
