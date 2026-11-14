package cmd

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/iamnikolie/zmina/internal/plugin"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/tmux"
)

const (
	// turnAnswerMax bounds the owner's words of an answer given through a plugin to the question a role's turn ended on: the words are
	// taken whole or not at all, never cut.
	turnAnswerMax = plugin.TextMax
	// turnHeadMax is how much of the question a line of an answer quotes, to name what it answers.
	turnHeadMax = 120
)

// zminaOpen is a bracket that would open a line of zmina's (`[zmina`, with the spaces a sender may put in it).
var zminaOpen = regexp.MustCompile(`(?i)\[\s*zmina`)

// quoted is a text of the owner's or a role's made fit to stand between « and » in a line a role reads: control characters gone, white
// space folded to single spaces, a session tag masked and the text cut to n characters (shift.QuoteN), the quotation marks that would end
// the quotation turned into single ones (so no word of it can leave it and read as zmina's own), and a bracket that would open a line of
// zmina's broken.
func quoted(text string, n int) string {
	text = strings.NewReplacer("«", "‹", "»", "›").Replace(shift.QuoteN(text, n))

	return zminaOpen.ReplaceAllString(text, "(zmina")
}

// turnAnswerLine is the line a role is given when the owner answers the question its turn ended on through a plugin. It opens as zmina's
// own lines do, so it is never taken for a line the owner typed, and it names the question it answers (when that turn ended, and how its
// question began), so that even read late it is not taken for the answer to another; it says whose words follow, through which plugin,
// and where they are recorded: a role is told that the owner's decisions are in the ledger, and a line from zmina is no approval by itself.
// at is when the turn ended, as the crew tells time.
func turnAnswerLine(via, at, question, words string) string {
	return "[zmina] The owner's answer to the question your turn ended on at " + at + " («" + quoted(question, turnHeadMax) +
		"»), given through plugin " + via + " and recorded in the ledger (zmina log): «" + quoted(words, turnAnswerMax) + "»"
}

// answerTurn gives the role the owner's words as the answer to the question its last turn ended on: `zmina send --by <plugin> --dialog
// <id> -- <role> <text>`. The plugin has been checked as todo answer checks it (pluginVoice). The answer is taken only while the role
// stands on that very question — the id the event carried names its record, and the role is idle in the stand the record is of
// (standingTurn, as dialogStill reads it) — and once: the event's mark, armed for the plugin that was told it, is spent first.
//
// That is checked when the answer is accepted, and the line is given at the same moment or not at all (shift.SendNow): never queued, since
// a line that waits may be read long after the question it answers is over. A role that cannot take it just now (lines wait for it, its
// delivery is held, a draft, a dialog) is a refusal, the mark is given back, and nothing is recorded: the card stays open and the owner
// answers in the role's terminal. The line itself names the question it answers (turnAnswerLine), as a line of zmina's, never typed as the
// owner's own, so it does not move the owner's last line to the role and he is not marked as at the machine.
//
// What was given is recorded: in the event log, and as the end of the card (the plugins are told it was answered, by whom, and what). The
// question's record is cleared, so that the card ends at the next look; if it cannot be, the answer was given all the same, the problem is
// logged and the spent mark refuses a second one.
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

	// One answer, from the plugin that was told the question, in the stand it was told in.
	armed := plugin.Armed{Path: s.Crew.Layout().PluginArmed(via), Lock: s.Crew.Layout().PluginArmedLock(via)}
	now := s.Now()
	dialogID := plugin.DialogID(role, fingerprint)

	if err := armed.Consume(mark, dialogID, f.Since, now); err != nil {
		return err
	}

	line := turnAnswerLine(via, f.Since.In(s.Crew.Location()).Format("15:04"), f.Question, words)

	if err := s.SendNow(role, line, answeredBy(by(), via)); err != nil {
		// Nothing was given: the card is as it was. Not when tmux did not answer in time, which may have typed it.
		if !tmux.NoAnswer(err) {
			_ = armed.Rearm(mark, now)
		}

		if notNow := (*shift.NotNow)(nil); errors.As(err, &notNow) {
			return shift.Partial(fmt.Errorf("%s; answer in its terminal", notNow))
		}

		return err
	}

	// Given: what the owner said is in the ledger, as the answer to a todo item is, and no other card of the question answers it again.
	s.Log(role, "answered", f.Task, fmt.Sprintf("its turn's question: «%s» (answered through plugin %s)", quoted(words, 200), via))
	noteDecision(s, plugin.EventDialogID(role, fingerprint, mark), answeredBy(by(), via), 0, quoted(words, plugin.AnswerMax))

	for _, pl := range s.Crew.Plugins {
		other := plugin.Armed{Path: s.Crew.Layout().PluginArmed(pl.Name), Lock: s.Crew.Layout().PluginArmedLock(pl.Name)}
		_ = other.Spend(dialogID, f.Since, now)
	}

	l := s.Crew.Layout()
	if err := shift.ClearTurn(l, role); err != nil {
		if err := shift.DropTurn(l, role); err != nil {
			pluginProblem(s.Crew, by(), via, fmt.Sprintf("the owner's answer to %s's turn question was given, but its record could not be cleared (%v): "+
				"the spent mark refuses a second answer", role, err))
		}
	}

	return nil
}
