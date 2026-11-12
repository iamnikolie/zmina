package shift

import (
	"fmt"
	"strings"
	"time"
)

// Two roles that write to each other about one task can cross: A writes B a line about T, and meanwhile B has written A a
// newer one that A had not read when it wrote. When A's line reaches B it reads as an answer to B's latest line, and it is
// not. So a line that is about to be delivered to B looks at B's own lines to A on the same task, in A's cards, and carries
// a mark in front of its words when it was written before B's last line (`newer`) or before A had read it (`unread`):
//
//	[written 14:02, before your last line on T-12 (14:05)] …
//	[written 14:02, before dev had read your line of 13:50 on T-12] …
//
// The task of a line is only what was said by structure, never read from its words: zmina send --task, a task packet, a
// scheduled run, a report key. Never marked: a line with no task, or with no line of B's to A on it; a line from zmina, the
// owner or no one; a line that goes alone and is never read with others (joinable: a slash command, the rename, the boot,
// the owner's own message, a task packet, a session-bound line).
//
// The mark is made when the line is delivered and goes into the text that goes out, nowhere else: the queued line, its ids,
// its card's id and the record of what was posted or typed stay the line as it was written, so acks, give-back, retry and
// the cards keep working on it. The card of the line records that it crossed (Crossed). The cards of A are read, never
// written, and a file that is missing or does not read is no mark and no error: a mark is the one thing a card changes in a
// delivery, and a delivery never waits for one. Typed, the mark is the start of what the role's input line holds, which
// holds knows.

// What a mark says of the line it is on.
const (
	// CrossNewer is that the role wrote to the writer on the task after the line was written.
	CrossNewer = "newer"
	// CrossUnread is that the role's last line to the writer on the task was written before the line, and the writer had
	// not read it when it wrote.
	CrossUnread = "unread"

	// crossOpen is how every mark opens.
	crossOpen = "[written "
	// crossClose is how every mark ends: the bracket, and the space between it and the words.
	crossClose = "] "
)

// CardCrossed is the mark a line was delivered with: With is the id of the card of the role's line it crossed, Kind says
// how (newer or unread), At when the mark was made.
type CardCrossed struct {
	With string    `json:"with"`
	Kind string    `json:"kind"`
	At   time.Time `json:"at,omitzero"`
}

// crossMark is the mark one line carries when it is delivered.
type crossMark struct {
	text string
	with string
	kind string
}

// prefix is the mark as it stands before the line, "" for a line with none.
func (m *crossMark) prefix() string {
	if m == nil {
		return ""
	}

	return m.text
}

// withMark is a line as it goes out: behind its mark when it has one.
func (m *crossMark) withMark(line string) string { return m.prefix() + line }

// crossCount is how many of the marks are there.
func crossCount(marks []*crossMark) int {
	n := 0

	for _, m := range marks {
		if m != nil {
			n++
		}
	}

	return n
}

// crossMarks is the mark of each line of a run, in step with it (nil where a line has none).
func (s *Shift) crossMarks(name string, run []string) []*crossMark {
	marks := make([]*crossMark, len(run))

	for i, line := range run {
		marks[i] = s.crossingFor(name, line)
	}

	return marks
}

// crossingFor is the mark a queued line of a role carries when it is delivered, nil for none. What it knows of the line
// (who wrote it, the task, when) is its card's.
func (s *Shift) crossingFor(name, line string) *crossMark {
	if s.crossingOff(name) {
		return nil
	}

	rc := s.cardsOf(name)
	if rc == nil {
		return nil
	}

	c := rc.pick(CardID(name, line), true)
	if c == nil {
		return nil
	}

	return s.crossing(name, line, c.From, c.Task, c.Written)
}

// crossingOff reports that no mark can be made for a role at all: the crew turned them off, there is nothing to read
// cards with, or the role is the dispatcher, whose queue is delivered by its own post (postDispatcher: its signals, its
// notes and its queued lines in one message), a path no line is marked on; a mark that came and went with the path a line
// happened to take would say nothing.
func (s *Shift) crossingOff(name string) bool {
	return s.Crew == nil || s.Now == nil || !s.Crew.CrossingOn() || !hookRole(name) || name == s.Crew.Dispatcher
}

// crossing is the mark of a line written by from, on task, at written, that is to be delivered to name: nil when it
// does not cross a line of name's to from on that task.
func (s *Shift) crossing(name, line, from, task string, written time.Time) *crossMark {
	if s.crossingOff(name) || task == "" || written.IsZero() || !s.crossSender(name, from) || !s.joinable(name, line) {
		return nil
	}

	// The role's lines to the writer on the task: the cards of the writer's file written by the role, newest first.
	var last *Card

	cards := s.cardsPeek(from)
	for i := range cards {
		c := &cards[i]
		if c.From != name || c.Task != task || c.Written.IsZero() {
			continue
		}

		if last == nil || !c.Written.Before(last.Written) {
			last = c
		}
	}

	if last == nil {
		return nil
	}

	switch read := last.readAt(); {
	case last.Written.After(written):
		return &crossMark{
			text: fmt.Sprintf("%s%s, before your last line on %s (%s)%s", crossOpen, s.crossTime(written), markWord(task),
				s.crossTime(last.Written), crossClose),
			with: last.ID, kind: CrossNewer,
		}
	case read.IsZero() || read.After(written):
		return &crossMark{
			text: fmt.Sprintf("%s%s, before %s had read your line of %s on %s%s", crossOpen, s.crossTime(written), markWord(from),
				s.crossTime(last.Written), markWord(task), crossClose),
			with: last.ID, kind: CrossUnread,
		}
	}

	return nil
}

// crossSender reports whether from is a role's name that a line can cross a line of the role's with: not zmina's own
// tools, not the owner, not the role itself.
func (s *Shift) crossSender(name, from string) bool {
	switch from {
	case "", "zmina", "owner", name:
		return false
	}

	return hookRole(from)
}

// markWord is a name or a task as a mark says it: one word that cannot close the mark.
func markWord(word string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(word), "_"), "]", ")")
}

// crossTime is a time as a mark says it, on the crew's clock: the hour and minute for today, with the day before them
// for any other day (clock).
func (s *Shift) crossTime(t time.Time) string {
	loc := s.Crew.Location()
	if t.In(loc).Format(time.DateOnly) == s.Now().In(loc).Format(time.DateOnly) {
		return t.In(loc).Format("15:04")
	}

	return s.clock(t)
}

// readAt is when the role was seen to read the line of the card, zero when it was not: its seen (the conversation, the
// Codex bridge, the fetch of its doorbell), else for a typed line, which shows no seen, when it landed; and for any line
// when its reader acked it. A typed bell that landed was shown, and its lines are not read until fetched.
func (c *Card) readAt() time.Time {
	switch {
	case c.Seen != nil:
		return c.Seen.At
	case c.Closed != nil && c.Closed.How == ClosedAcked:
		return c.Closed.At
	case c.Closed != nil && c.Closed.How == ClosedLanded && c.Bell == nil:
		return c.Closed.At
	}

	return time.Time{}
}

// cardsPeek is another role's cards for a look: what this pass has of them in memory, else the file as it is, read once and
// kept for the pass. It moves nothing aside, writes nothing and tells nothing; a file that is missing or does not read has
// none.
func (s *Shift) cardsPeek(name string) []Card {
	if s.Crew == nil || !hookRole(name) {
		return nil
	}

	if s.cards == nil {
		s.cards = &cardBook{roles: map[string]*roleCards{}}
	}

	if rc := s.cards.roles[name]; rc != nil {
		if rc.off {
			return nil
		}

		return rc.cards
	}

	if cards, ok := s.cards.peeked[name]; ok {
		return cards
	}

	cards, _, err := readCardFile(s.Crew.Layout().DeliveryCards(name))
	if err != nil {
		cards = nil
	}

	if s.cards.peeked == nil {
		s.cards.peeked = map[string][]Card{}
	}

	s.cards.peeked[name] = cards

	return cards
}

// cardCrossed notes on the card of a line about to be accepted that it was delivered with a mark. It is called before the
// acceptance, which is what the card is found by.
func (s *Shift) cardCrossed(name, line string, m *crossMark, at time.Time) {
	if m == nil {
		return
	}

	if rc := s.cardsOf(name); rc != nil {
		rc.known(name, line, true).Crossed = &CardCrossed{With: m.with, Kind: m.kind, At: at}
		rc.dirty = true
	}
}

// cardsCrossed is cardCrossed for each line of a run.
func (s *Shift) cardsCrossed(name string, run []string, marks []*crossMark, at time.Time) {
	for i, line := range run {
		if i < len(marks) {
			s.cardCrossed(name, line, marks[i], at)
		}
	}
}
