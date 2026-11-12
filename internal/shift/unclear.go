package shift

import (
	"errors"
	"strings"
)

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

// typedUnclear is a typeLine error after which the line may have reached the pane: its text opens with UnclearTyping and
// carries the cause. typeLine returns it for every failure but the two after which nothing of the line is left or was
// taken (errUnsent, and a start seen in the input line, cleared and read gone).
type typedUnclear struct{ cause error }

func (e *typedUnclear) Error() string { return UnclearTyping + " (" + e.cause.Error() + ")" }
func (e *typedUnclear) Unwrap() error { return e.cause }

// unclearTyping marks err as one after which the line may have reached the pane.
func unclearTyping(err error) error { return &typedUnclear{cause: err} }

// typedMayHaveReached reports a typeLine error after which the line is never typed again by itself.
func typedMayHaveReached(err error) bool {
	var u *typedUnclear

	return errors.As(err, &u)
}
