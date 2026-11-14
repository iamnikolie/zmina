package cmd

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/iamnikolie/zmina/internal/plugin"
	"github.com/iamnikolie/zmina/internal/shift"
)

// turnAnswerMax bounds the owner's words of an answer given through a plugin to the question a role's turn ended on: the words are
// taken whole or not at all, never cut.
const turnAnswerMax = plugin.TextMax

// turnAnswerLine is the line a role is given when the owner answers its last question through a plugin. It opens as zmina's own lines do,
// so it is never taken for a line the owner typed, and says whose words follow, through which plugin, and where the answer is
// recorded: a role is told that the owner's decisions are in the ledger, and a line from zmina is no approval by itself.
func turnAnswerLine(via, words string) string {
	return "[zmina] The owner's answer to your last question, given through plugin " + via +
		" and recorded in the ledger (zmina log): «" + shift.QuoteN(words, turnAnswerMax) + "»"
}

// answerTurn gives the role the owner's words as the answer to the question its last turn ended on: `zmina send --by <plugin> --dialog
// <id> -- <role> <text>`. The plugin has been checked as todo answer checks it (pluginVoice). The answer is taken only while the role
// stands on that very question — the id the event carried names its record, and the role is idle in the stand the record is of
// (standingTurn, as dialogStill reads it) — so it can land nowhere else, and a second one finds none. The line is queued for the
// role and goes at once when it can take it, as send --when-idle does, so that a role that cannot be typed into just now still gets
// it; it is a line of zmina's (turnAnswerLine) and never typed as the owner's own, so it does not move the owner's last line to the
// role, and it does not mark the owner as at the machine. What is taken is recorded: in the event log, and as the end of the card (the
// plugins are told it was answered, by whom, and what). The question's record goes, so that the card ends at the next look.
func (a *app) answerTurn(s *shift.Shift, via, dialog, role, text string) error {
	words := strings.TrimSpace(text)

	switch {
	case words == "":
		return errors.New("--by: the answer is empty: not taken")
	case utf8.RuneCountInString(words) > turnAnswerMax:
		return fmt.Errorf("--by: the answer is longer than %d characters: not taken; answer in a terminal", turnAnswerMax)
	}

	// The id is the event's: role:fingerprint:mark.
	rest, ok := strings.CutPrefix(dialog, role+":")
	fingerprint, mark, _ := strings.Cut(rest, ":")

	if !ok || fingerprint == "" || mark == "" {
		return errors.New("--dialog: give the id of the event as it was told (role:fingerprint:mark)")
	}

	v, _, found := s.Look(role)
	if !found {
		return fmt.Errorf("%s: not a role of this crew", role)
	}

	f, standing, known := standingTurn(s.Crew, v)

	switch {
	case !known:
		return fmt.Errorf("%s: the record of the question its last turn ended on does not read: not taken; answer in a terminal", role)
	case !standing:
		return fmt.Errorf("%s is waiting on no question its last turn ended on (it took another turn since, or asked none, or has its question on the todo): not taken; look again", role)
	case f.ID != fingerprint:
		return fmt.Errorf("%s: the question its last turn ended on is not the one this card was told of: not taken; look again", role)
	}

	if err := a.queueSend(s, role, turnAnswerLine(via, words), shift.Facts{From: answeredBy(by(), via)}); err != nil {
		return err
	}

	// Taken: what the owner said is in the ledger, as the answer to a todo item is, and the question is no longer waited on.
	s.Log(role, "answered", f.Task, fmt.Sprintf("its turn's question: «%s» (answered through plugin %s)", shift.QuoteN(words, 200), via))
	noteDecision(s, plugin.EventDialogID(role, fingerprint, mark), answeredBy(by(), via), 0, shift.QuoteN(words, plugin.AnswerMax))
	_ = shift.DropTurn(s.Crew.Layout(), role)

	return nil
}
