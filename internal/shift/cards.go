package shift

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/inbox"
)

// A delivery card is what zmina saw of one line it queued for a role, or typed or posted to the dispatcher: who wrote
// it and when, when a role's input or inbox took it (accepted) and how, when the role was seen to read it (seen), and
// how it ended. The cards of a role are one small file, delivery/<role>.json in the crew's directory, written through
// fsx under the state lock the delivery already holds, once per pass and only when something changed.
//
// A card is a record of what was seen and nothing else: no delivery decision reads one. A card file that cannot be
// read or written is told once as an event (cards-failed) and the delivery goes on; one that is corrupt is moved aside
// as <file>.broken-<time>, as state.json is, and started anew. Looks (zmina queue ls) read the file and never write it.

const (
	// cardsVersion is the number the card file carries. A file with a higher one is a newer zmina's: it is left alone.
	cardsVersion = 1
	// cardHeadMax is the most bytes of a line a card keeps (its head, to say which line it is).
	cardHeadMax = 200
	// cardKeep is how long a finished card stays in the file: past it the next write drops the card.
	cardKeep = 7 * 24 * time.Hour
	// cardsMax is the most cards a file keeps; the oldest finished ones go first.
	cardsMax = 500
	// cardFileMax is the largest card file that is read; a larger one is not one zmina wrote.
	cardFileMax = 8 << 20
)

// How a line was accepted, seen and closed. A typed line shows no seen: its end is landed, the role's status moved
// after it. A line that went in a doorbell is seen only by its fetch (bell.go): the transcript and the Codex bridge show
// the bell, never the line.
const (
	AcceptedTyped      = "typed"
	AcceptedSocket     = "socket"
	AcceptedCodexInbox = "codex-inbox"

	SeenTranscript = "transcript"
	SeenTaken      = "taken"
	SeenFetch      = "fetch"

	ClosedLanded    = "landed"
	ClosedAcked     = "acked"
	ClosedDropped   = "dropped"
	ClosedFailed    = "failed"
	ClosedGivenBack = "given-back"
)

// Card is the facts of one line. A fact that was not seen is not there.
type Card struct {
	// ID is the line's content id: the one a long line is stored under in the crew's inbox (zmina inbox show), made of
	// the role and the line's text. Two lines of the same words for one role share it, and are told apart by the order
	// they were written in.
	ID string `json:"id"`
	// From is who wrote the line where zmina knows it (a role, "owner", "zmina"); Task the task it is about where that is
	// structure (a task packet, a scheduled run), never read from the words.
	From string `json:"from,omitempty"`
	Task string `json:"task,omitempty"`
	// Head is the start of the line, bounded.
	Head string `json:"head,omitempty"`
	// Written is when the line was queued: zero for a line first met after it was queued (a card made when its first
	// fact was seen).
	Written   time.Time      `json:"written,omitzero"`
	Accepted  *CardAccepted  `json:"accepted,omitempty"`
	Seen      *CardSeen      `json:"seen,omitempty"`
	Attention *CardAttention `json:"attention,omitempty"`
	// Back is that the line went back to the head of the queue after it was accepted (its conversation ended before it
	// showed, a retry): its card stays, the new delivery has its own accepted and seen.
	Back *CardBack `json:"back,omitempty"`
	// Acks are the ack ids (S-<n>) of a tracked line whose typing broke where its outcome is unclear: it leaves the queue
	// with no record, and its ack is what closes its card (cardsAckedBy), or says it again (cardsSaidAgain).
	Acks []string `json:"acks,omitempty"`
	// Bell is set when the line went in a doorbell (bell.go): which body, and when the bell was shown. Its Acks are then
	// the line's own, which close the card as they close any line's.
	Bell   *CardBell   `json:"bell,omitempty"`
	Closed *CardClosed `json:"closed,omitempty"`
}

// CardBell is the doorbell a line went in: Body is the id of the text stored in the crew's inbox (zmina inbox show), Shown
// when the bell was shown to its role (its conversation shows it, the Codex bridge took it, or the typed bell landed);
// zero while it was not. That the bell was shown says nothing of the line: the line is seen when its body is fetched.
type CardBell struct {
	Body  string    `json:"body"`
	Shown time.Time `json:"shown,omitzero"`
}

// CardAccepted is when and how the role's input or inbox took the line: How is typed, socket or codex-inbox.
type CardAccepted struct {
	At      time.Time `json:"at"`
	How     string    `json:"how"`
	Session string    `json:"session,omitempty"`
}

// CardSeen is when zmina saw the role read the line: By is transcript (its conversation shows it) or taken (the
// Codex bridge took it).
type CardSeen struct {
	At time.Time `json:"at"`
	By string    `json:"by"`
}

// CardAttention is when zmina found the line's outcome unclear (it may have reached the role), and why.
type CardAttention struct {
	At  time.Time `json:"at"`
	Why string    `json:"why"`
}

// CardBack is the line given back to the queue: when (the last time), why, and how many times.
type CardBack struct {
	At  time.Time `json:"at"`
	Why string    `json:"why,omitempty"`
	N   int       `json:"n"`
}

// CardClosed is how the line ended: landed, acked, dropped, failed or given-back (a post to the dispatcher made of
// signals and notes that went back on their own queues); Why says more where there is more.
type CardClosed struct {
	At  time.Time `json:"at"`
	How string    `json:"how"`
	Why string    `json:"why,omitempty"`
}

// cardFile is delivery/<role>.json.
type cardFile struct {
	Version int    `json:"version"`
	Cards   []Card `json:"cards"`
}

// CardID is the id of a line for a role.
func CardID(role, line string) string { return inbox.IDOf("queue " + role + "\x00" + line) }

// Facts are what the caller of a queueing knows of the line: who wrote it, and the task it is about when that is
// structure. Both are optional.
type Facts struct {
	From string
	Task string
}

// done reports whether delivery of the card's line has ended: it was seen or it was closed.
func (c *Card) done() bool { return c.Closed != nil || c.Seen != nil }

// lastFact is the latest moment any fact of the card carries: when it was written, accepted, needed a look, went back,
// was seen or closed.
func (c *Card) lastFact() time.Time {
	latest := c.Written

	for _, at := range []*time.Time{accAt(c), attAt(c), backAt(c)} {
		if at != nil && at.After(latest) {
			latest = *at
		}
	}

	if end := c.last(); end.After(latest) {
		latest = end
	}

	return latest
}

func accAt(c *Card) *time.Time {
	if c.Accepted == nil {
		return nil
	}

	return &c.Accepted.At
}

func attAt(c *Card) *time.Time {
	if c.Attention == nil {
		return nil
	}

	return &c.Attention.At
}

func backAt(c *Card) *time.Time {
	if c.Back == nil {
		return nil
	}

	return &c.Back.At
}

// last is the moment the card stopped changing: when it was closed, else seen; zero for a card still in play.
func (c *Card) last() time.Time {
	switch {
	case c.Closed != nil:
		return c.Closed.At
	case c.Seen != nil:
		return c.Seen.At
	}

	return time.Time{}
}

// cardHead is the start of a line as a card keeps it: one line, at most cardHeadMax bytes, cut at a character.
func cardHead(line string) string {
	line = oneLine(line)
	if len(line) <= cardHeadMax {
		return line
	}

	cut := cardHeadMax - len("…")
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}

	return line[:cut] + "…"
}

// pruneCards drops the cards finished more than cardKeep ago, then the oldest finished ones, and last the oldest of
// the rest, until no more than cardsMax are left.
func pruneCards(cards []Card, now time.Time) []Card {
	cards = slices.DeleteFunc(cards, func(c Card) bool {
		at := c.last()

		return !at.IsZero() && now.Sub(at) > cardKeep
	})

	for over := len(cards) - cardsMax; over > 0; over = len(cards) - cardsMax {
		drop := slices.IndexFunc(cards, func(c Card) bool { return c.done() })
		if drop < 0 {
			drop = 0
		}

		cards = slices.Delete(cards, drop, drop+1)
	}

	return cards
}

// readCardFile reads a card file. A file that is not there has no cards and no error; one that does not read, or does
// not parse, is the error (corrupt tells which).
func readCardFile(path string) (cards []Card, corrupt bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, err
	}

	if len(raw) > cardFileMax {
		return nil, true, fmt.Errorf("%d bytes, more than a card file holds", len(raw))
	}

	var f cardFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, true, err
	}

	if f.Version > cardsVersion {
		return nil, false, fmt.Errorf("version %d is from a newer zmina", f.Version)
	}

	return f.Cards, false, nil
}

// ReadCards is a role's cards as the file has them, for a look: it writes nothing and moves nothing, and a file that
// is missing or does not read has none.
func ReadCards(l crew.Layout, role string) []Card {
	if !hookRole(role) {
		return nil
	}

	cards, _, _ := readCardFile(l.DeliveryCards(role))

	return cards
}

// CardsOf are all the cards a role has for a line, oldest first, in play or finished: no choice among them is made here.
// A look that shows a card under a row resolves it by the row's kind (QueueView).
func CardsOf(cards []Card, role, line string) []Card {
	id := CardID(role, line)

	var out []Card

	for _, c := range cards {
		if c.ID == id {
			out = append(out, c)
		}
	}

	return out
}

// roleCards is one role's cards as loaded in this pass.
type roleCards struct {
	cards []Card
	dirty bool
	// off is that the file could not be read or written: nothing more is tried this pass.
	off bool
}

// cardBook is the cards a shift has loaded: lazily, a role at a time.
type cardBook struct {
	roles map[string]*roleCards
}

// cardWarned remembers the card failures already told, by file and kind, so that a courier that goes on passing and
// failing the same way tells it once.
var cardWarned sync.Map

// cardWarn tells a card failure once as an event and never fails the delivery.
func (s *Shift) cardWarn(role, path, what string, err error) {
	if _, told := cardWarned.LoadOrStore(path+"\x00"+what, true); told {
		return
	}

	s.log(role, "cards-failed", "", what+": "+path+": "+err.Error())
}

// cardsOf is a role's cards, loaded the first time a pass asks for them. Nil when there is nothing to keep them in
// (no crew) or the file cannot be used: the caller then does nothing.
func (s *Shift) cardsOf(name string) *roleCards {
	if s.Crew == nil || s.Now == nil || !hookRole(name) {
		return nil
	}

	if s.cards == nil {
		s.cards = &cardBook{roles: map[string]*roleCards{}}
	}

	rc := s.cards.roles[name]
	if rc == nil {
		rc = s.loadCards(name)
		s.cards.roles[name] = rc

		if !rc.off {
			s.settleAcks(rc)
		}
	}

	if rc.off {
		return nil
	}

	return rc
}

// loadCards reads a role's card file. A corrupt one is moved aside (<file>.broken-<time>) and the cards start anew.
func (s *Shift) loadCards(name string) *roleCards {
	path := s.Crew.Layout().DeliveryCards(name)

	cards, corrupt, err := readCardFile(path)

	switch {
	case err == nil:
		return &roleCards{cards: cards}
	case !corrupt:
		s.cardWarn(name, path, "unreadable", err)

		return &roleCards{off: true}
	}

	aside := path + ".broken-" + s.Now().Format("20060102-150405")
	if rerr := os.Rename(path, aside); rerr != nil {
		s.cardWarn(name, path, "corrupt and not moved aside", errors.Join(err, rerr))

		return &roleCards{off: true}
	}

	s.cardWarn(name, path, "corrupt, moved aside to "+aside+" and started anew", err)

	return &roleCards{}
}

// FlushCards writes the cards of the roles whose cards changed, one write a role. Call it under the state lock, where
// the delivery made the changes. A write that fails is told once and leaves the delivery as it was.
func (s *Shift) FlushCards() {
	if s.cards == nil || s.Crew == nil {
		return
	}

	now := s.Now()

	for _, name := range slices.Sorted(maps.Keys(s.cards.roles)) {
		rc := s.cards.roles[name]
		if rc.off || !rc.dirty {
			continue
		}

		rc.dirty = false
		rc.cards = pruneCards(rc.cards, now)
		path := s.Crew.Layout().DeliveryCards(name)

		if err := writeCardFile(s.Crew.Layout(), path, rc.cards); err != nil {
			rc.off = true

			s.cardWarn(name, path, "not written", err)
		}
	}
}

// writeCardFile puts a role's cards on disk whole or not at all.
func writeCardFile(l crew.Layout, path string, cards []Card) error {
	if cards == nil {
		cards = []Card{}
	}

	raw, err := json.MarshalIndent(cardFile{Version: cardsVersion, Cards: cards}, "", " ")
	if err != nil {
		return err
	}

	if err := fsx.MkdirAll(l.Delivery(), 0o700); err != nil {
		return err
	}

	return fsx.WriteFile(path, append(raw, '\n'), 0o600)
}

// asideInfix is what a card file moved aside carries after its name: delivery/<role>.json.broken-<time>.
const asideInfix = ".json.broken-"

// PruneCards drops, in every card file of the crew, the cards past their keep and over the cap, and writes the files that
// changed; it removes the files moved aside as corrupt (<role>.json.broken-<time>) once they are older than the keep,
// and the file of a role known no longer to be in the crew (known says) once every card in it is past its keep — a card
// still in play counts from its last fact. A role's fetches (delivery/<role>.fetched.jsonl) are cut to the lines within
// the keep, and the mark of a start of a role gone from the crew is removed (bell.go). It returns how many cards, fetch
// lines, marks and aside files went. With dry it only counts. A
// nil known takes every role as in the crew. Call it under the state lock (the retention pass takes it), since the
// delivery writes the same files.
func PruneCards(l crew.Layout, now time.Time, known func(role string) bool, dry bool) (int, error) {
	entries, err := os.ReadDir(l.Delivery())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}

	if err != nil {
		return 0, err
	}

	var (
		total int
		errs  []error
	)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		// A role's fetches (<role>.fetched.jsonl) are not a card file: they are cut to the lines still within the keep, and
		// the file goes with its last line.
		if role, ok := strings.CutSuffix(e.Name(), ".fetched.jsonl"); ok && hookRole(role) {
			n, err := pruneFetches(filepath.Join(l.Delivery(), e.Name()), now, cardKeep, dry)
			if err != nil {
				errs = append(errs, err)
			}

			total += n

			continue
		}

		// The mark of a role's start (<role>.bell) goes with a role no longer in the crew.
		if role, ok := strings.CutSuffix(e.Name(), ".bell"); ok && hookRole(role) {
			if known != nil && !known(role) {
				total++

				if !dry {
					if err := os.Remove(filepath.Join(l.Delivery(), e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
						errs = append(errs, err)
					}
				}
			}

			continue
		}

		if i := strings.Index(e.Name(), asideInfix); i > 0 {
			if asideOld(e, now) {
				total++

				if !dry {
					if err := os.Remove(filepath.Join(l.Delivery(), e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
						errs = append(errs, err)
					}
				}
			}

			continue
		}

		role, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !hookRole(role) {
			continue
		}

		path := l.DeliveryCards(role)

		cards, _, err := readCardFile(path)
		if err != nil {
			continue
		}

		gone := known != nil && !known(role)

		kept := slices.Clone(cards)
		if gone {
			kept = slices.DeleteFunc(kept, func(c Card) bool { return now.Sub(c.lastFact()) > cardKeep })
		}

		kept = pruneCards(kept, now)

		switch {
		case gone && len(kept) == 0 && len(cards) > 0:
			total += len(cards)

			if !dry {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, err)
				}
			}
		case len(kept) != len(cards):
			total += len(cards) - len(kept)

			if !dry {
				if err := writeCardFile(l, path, kept); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}

	return total, errors.Join(errs...)
}

// asideOld reports a card file moved aside as corrupt more than the keep ago: by the time in its name, else by the file's.
func asideOld(e os.DirEntry, now time.Time) bool {
	_, stamp, _ := strings.Cut(e.Name(), asideInfix)

	if at, err := time.ParseInLocation("20060102-150405", stamp, time.Local); err == nil {
		return now.Sub(at) > cardKeep
	}

	info, err := e.Info()

	return err == nil && now.Sub(info.ModTime()) > cardKeep
}

// pick is the card an observation is about, by the line's id, oldest first (a queue is read from its head): for a new
// acceptance the first card still in play that was not accepted yet, else the first in play; for anything later the
// first in play that was accepted, else the first in play. Nil when none is in play.
func (rc *roleCards) pick(id string, accepting bool) *Card {
	var first *Card

	for i := range rc.cards {
		c := &rc.cards[i]
		if c.ID != id || c.done() || c.rung() {
			continue
		}

		if first == nil {
			first = c
		}

		if (c.Accepted == nil) == accepting {
			return c
		}
	}

	return first
}

// add puts a card at the end.
func (rc *roleCards) add(c Card) *Card {
	rc.cards = append(rc.cards, c)
	rc.dirty = true

	return &rc.cards[len(rc.cards)-1]
}

// known is the card of a line an observation is about, made when the line was never carded (it was queued before cards
// were kept, or its card was pruned): it has no written time.
func (rc *roleCards) known(name, line string, accepting bool) *Card {
	id := CardID(name, line)
	if c := rc.pick(id, accepting); c != nil {
		return c
	}

	return rc.add(Card{ID: id, Head: cardHead(line)})
}

// cardNew writes the card of a line just queued.
func (s *Shift) cardNew(name, line string, f Facts) {
	if rc := s.cardsOf(name); rc != nil {
		rc.add(Card{ID: CardID(name, line), From: f.From, Task: f.Task, Head: cardHead(line), Written: s.Now()})
	}
}

// cardAccept notes that the role's input or inbox took the line.
func (s *Shift) cardAccept(name, line, how, session string, at time.Time) {
	if rc := s.cardsOf(name); rc != nil {
		c := rc.known(name, line, true)
		c.Accepted, c.Attention = &CardAccepted{At: at, How: how, Session: session}, nil
		rc.dirty = true
	}
}

// cardSeen notes that the role was seen to read the line. A line seen is done: its card takes no more deliveries.
func (s *Shift) cardSeen(name, line, by string, at time.Time) {
	if rc := s.cardsOf(name); rc != nil {
		rc.known(name, line, false).Seen = &CardSeen{At: at, By: by}
		rc.dirty = true
	}
}

// cardAttention notes that the line's outcome is unclear.
func (s *Shift) cardAttention(name, line, why string, at time.Time) {
	if rc := s.cardsOf(name); rc != nil {
		rc.known(name, line, false).Attention = &CardAttention{At: at, Why: why}
		rc.dirty = true
	}
}

// cardClose notes how the line ended, on the oldest card of the line still in play. A line with no card in play is not
// given one and closes none: the card it had was finished or pruned, and one of an earlier delivery of the same words is
// not this line's.
func (s *Shift) cardClose(name, line, how, why string, at time.Time) {
	rc := s.cardsOf(name)
	if rc == nil {
		return
	}

	c := rc.pick(CardID(name, line), false)
	if c == nil {
		return
	}

	c.Closed = &CardClosed{At: at, How: how, Why: why}
	rc.dirty = true
}

// cardUnclearTracked notes a tracked line whose typing broke where its outcome is unclear (it counts as delivered, no
// failed record stands beside it, its ack says it again): attention, and the ack ids that will close the card.
func (s *Shift) cardUnclearTracked(name, line, why string, ids []string, at time.Time) {
	if rc := s.cardsOf(name); rc != nil {
		c := rc.known(name, line, false)
		c.Attention, c.Acks = &CardAttention{At: at, Why: why}, slices.Clone(ids)
		rc.dirty = true
	}
}

// cardsAckedBy closes, how and why, the cards in play that carry the ack id: the reader of an unclear tracked line acked
// it, or its ack is said again.
func (s *Shift) cardsAckedBy(name, ack, how, why string, at time.Time) {
	rc := s.cardsOf(name)
	if rc == nil {
		return
	}

	for i := range rc.cards {
		if c := &rc.cards[i]; !c.done() && slices.Contains(c.Acks, ack) {
			c.Closed = &CardClosed{At: at, How: how, Why: why}
			rc.dirty = true
		}
	}
}

// settleAcks closes the cards in play whose every ack is gone from the state (acked, dropped, evicted past acksMax) by a
// way that did not name the card: the ack no longer says anything again. It runs when a role's cards are loaded.
func (s *Shift) settleAcks(rc *roleCards) {
	if s.State == nil {
		return
	}

	for i := range rc.cards {
		c := &rc.cards[i]
		if c.done() || len(c.Acks) == 0 || slices.ContainsFunc(c.Acks, func(id string) bool { return s.State.Acks[id] != nil }) {
			continue
		}

		c.Closed = &CardClosed{At: s.Now(), How: ClosedDropped, Why: "its ack is gone"}
		rc.dirty = true
	}
}

// cardBack notes that the line went back to the queue: its card stays, whether it was in play or closed as failed, and
// the new delivery starts it anew. A line with no card gets one.
func (s *Shift) cardBack(name, line, why string, at time.Time) {
	rc := s.cardsOf(name)
	if rc == nil {
		return
	}

	id := CardID(name, line)

	c := rc.pick(id, false)
	if c == nil {
		for i := len(rc.cards) - 1; i >= 0; i-- {
			if rc.cards[i].ID == id && rc.cards[i].Closed != nil && rc.cards[i].Closed.How == ClosedFailed {
				c = &rc.cards[i]

				break
			}
		}
	}

	if c == nil {
		c = rc.add(Card{ID: id, Head: cardHead(line)})
	}

	n := 1
	if c.Back != nil {
		n = c.Back.N + 1
	}

	c.Back, c.Accepted, c.Seen, c.Attention, c.Closed = &CardBack{At: at, Why: why, N: n}, nil, nil, nil, nil

	// The bell it went in did not reach its role with it: the new delivery has its own.
	if c.Bell != nil {
		c.Bell, c.Acks = nil, nil
	}

	rc.dirty = true
}

// postedLines are the lines a record in flight has cards for: a line stands for itself. A post to the dispatcher made
// of parts has a card for its text when it carries signals or notes (one card for the composed line, never one for each
// signal), and one for each own queued line in it.
func postedLines(line string, parts []Part) []string {
	if len(parts) == 0 {
		return []string{line}
	}

	var own []string

	for _, p := range parts {
		if p.Line != "" && !slices.Contains(own, p.Line) {
			own = append(own, p.Line)
		}
	}

	if composedOf(parts) && !slices.Contains(own, line) {
		return append([]string{line}, own...)
	}

	return own
}

// composedOf reports a post made, wholly or in part, of what zmina composed: signals or notes.
func composedOf(parts []Part) bool {
	return slices.ContainsFunc(parts, func(p Part) bool { return p.Line == "" })
}

// cardSession is the conversation a role runs now, "" where it is not known.
func (s *Shift) cardSession(name string) string {
	if sess := s.session(name); sess != nil {
		return sess.SessionID
	}

	return ""
}

// cardsAccepted notes that a post or a typed line to the dispatcher was accepted: the own queued lines it carried have
// their cards from the queueing; what zmina composed of signals and notes (one line, never a card for each signal)
// gets its card now, written when it goes.
func (s *Shift) cardsAccepted(name, line string, parts []Part, how, session string, at time.Time) {
	for _, l := range postedLines(line, parts) {
		if l == line && composedOf(parts) {
			s.cardNew(name, l, Facts{From: "zmina"})
		}

		s.cardAccept(name, l, how, session, at)
	}
}

// cardNow is the shift's clock for a card, zero where the shift has none (then no card is kept either).
func (s *Shift) cardNow() time.Time {
	if s.Now == nil {
		return time.Time{}
	}

	return s.Now()
}

// cardsClosed is cardClose for every line a record in flight stands for.
func (s *Shift) cardsClosed(name, line string, parts []Part, how, why string) {
	now := s.cardNow()

	for _, l := range postedLines(line, parts) {
		s.cardClose(name, l, how, why, now)
	}
}

// cardFailed notes a line that failed: when it may have reached the role it needs attention (and stays in play, for a
// look and a retry), else it is closed as failed.
func (s *Shift) cardFailed(name string, f Failed, at time.Time) {
	for _, line := range postedLines(f.Line, f.Parts) {
		if f.Unclear() {
			s.cardAttention(name, line, f.Error, at)
		} else {
			s.cardClose(name, line, ClosedFailed, f.Error, at)
		}
	}
}

// senderOf is who a line is from, by who ran the command that queued it: the owner at a shell or in the cockpit, zmina's
// own ticker, courier and turn-end hook, or the role that ran it.
func senderOf(by string) string {
	switch by {
	case "hand", "ui":
		return "owner"
	case "ticker", "courier", "turn-end":
		return "zmina"
	}

	return by
}

// seenBy is how a posted line shows it was read: the Claude conversation shows it, a Codex bridge marks it taken.
func (s *Shift) seenBy(name string) string {
	if r, err := s.Role(name); err == nil && r.IsCodex() {
		return SeenTaken
	}

	return SeenTranscript
}

// acceptedBy is how a post is accepted: by the Claude role's inbox socket, or by the Codex bridge's inbox.
func (s *Shift) acceptedBy(name string) string {
	if r, err := s.Role(name); err == nil && r.IsCodex() {
		return AcceptedCodexInbox
	}

	return AcceptedSocket
}
