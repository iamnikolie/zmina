package shift

import "strings"

// A failed line is one of two kinds. Nothing reached its role: tmux refused it and what it typed was seen cleared from
// the input line, or it never left the input line; retrying it is safe. Or it is unclear: it may have reached the role
// (it was typed and its end is not known, or posted and never shown), so retrying it may say it twice, and it needs a
// look first (zmina queue ls says "attention required"; zmina queue retry skips it unless --unclear).
//
// The kind is not a field of the state: it is the text of the failure, which the record keeps, and these are the
// texts. Never reword one: a record written under the old words is no longer known as unclear.
const (
	// UnclearTyping opens the error of a typed line whose typing broke where nothing proves that none of it reached the
	// pane (typeLine).
	UnclearTyping = "typing broke and may have reached the pane; inspect before a retry"
	// UnclearEnded is the failure of a typed line whose session ended before it landed (settleEnded).
	UnclearEnded = "typed into a session that ended before it landed; it may have reached the conversation; inspect before a retry"
	// UnclearUnconfirmed is the failure of a typed line that left the input line with no change of status after it
	// (landed).
	UnclearUnconfirmed = "typed, acceptance unconfirmed; inspect before retry"
)

// unclearTexts are the failures that need a look before a retry. A posted line the conversation never showed is one.
var unclearTexts = []string{UnclearTyping, UnclearEnded, UnclearUnconfirmed, postedNever}

// Unclear reports a failed line that may have reached its role: retrying it may say it twice.
func (f Failed) Unclear() bool {
	for _, text := range unclearTexts {
		if strings.HasPrefix(f.Error, text) {
			return true
		}
	}

	return false
}
