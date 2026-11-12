package shift

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/inbox"
)

// A doorbell is what a role is sent in place of what is big: one short line, `[zmina] 3 lines wait for you (dev, ops on
// T-1): zmina inbox show in-<id>`, while the text itself — the lines joined (join.go) or the one long line — is stored once in
// the crew's inbox, where the role reads it with that command. Each line keeps its own ack tail inside the text, so each
// is acked by its own id; the bell carries none.
//
// The bell is recorded as the post (Posted) or the typed line (Sent) it is, with a part for each line, so what happens to
// a post — shown, never shown, ended, acked, given back, failed, retried — happens to its lines one by one, as it does
// to a joined post. That a record is a bell is not a field: it is its text (bellID), and its lines' cards say which body
// they are in (CardBell). What a bell adds is facts about the lines, and they live on the cards: the bell was shown (a
// conversation or a bridge shows the bell, never the line), and the body was fetched, which is the only way a line that
// went in a bell is seen (zmina inbox show appends to the role's fetches, without the state lock; the next pass reads it).
// A bell that was shown and not fetched is never rung again by itself: after postedWait its lines' cards need attention
// and the dispatcher hears of it once, and the role's brief (zmina pending --brief) lists it with its command.
//
// Only a role whose current start was told what a bell is (BellRule, in its boot line) is rung one: the start leaves a mark
// beside the role's cards, which a role started by an older zmina does not have. A crew with doorbell: off rings none.

const (
	// bellInline is the most bytes a post to a role carries whole: what is bigger goes as a bell.
	bellInline = 1200
	// bellTyped is the most characters of one line typed into a role's input line whole: what is longer, or more than one
	// line, goes as a bell.
	bellTyped = 200
	// bellNames is how many names a bell's list of senders or of tasks shows before it counts the rest.
	bellNames = 3
	// bellNameMax is the most characters of a name in a bell.
	bellNameMax = 20
	// bellFetchMax caps how much of a role's fetches is read; a fetch is one short line.
	bellFetchMax = 1 << 20
)

// BellRule is the sentence a role is told at boot, and the one that lets zmina ring it a bell.
const BellRule = "[zmina] A line that reads `[zmina] N lines wait for you (…): zmina inbox show <id>` is a doorbell: " +
	"the lines themselves wait in zmina's inbox, so run that command, act on each numbered line it prints, and ack " +
	"each by its own id (`zmina ack S-n`)."

// bellUnfetched is what the card of a line says when its bell was shown and its body not fetched within postedWait.
const bellUnfetched = "bell shown, body not fetched"

// bellWhat is how a delivery as a bell is told in the log: how many lines it is for, and the bell.
func bellWhat(n int, text string) string {
	if n == 1 {
		return "bell for 1 line: " + text
	}

	return "bell for " + strconv.Itoa(n) + " lines: " + text
}

// bellForm is the one reading of a bell's text: how many lines wait, and the body's id. It is the only way a record is
// known to be a bell.
var bellForm = regexp.MustCompile(`^\[zmina\] [0-9]+ lines? waits? for you(?: \(.*\))?: zmina inbox show (in-[0-9a-f]{12})$`)

// bellID is the body id a bell's text names, "" when the text is not a bell.
func bellID(text string) string {
	if m := bellForm.FindStringSubmatch(text); m != nil {
		return m[1]
	}

	return ""
}

// IsBell reports a record's text that is a bell's.
func IsBell(text string) bool { return bellID(text) != "" }

// bellRecord is the body id of a role's record in flight when it is a bell: a text that reads as one, over parts, which a
// plain line has none. The dispatcher is never rung one.
func (s *Shift) bellRecord(name, line string, parts []Part) string {
	if name == s.Crew.Dispatcher || len(parts) == 0 {
		return ""
	}

	return bellID(line)
}

// bellRule is what a role's boot line says of a bell: the dispatcher is never rung one, and a crew that turned them off
// tells nobody.
func (s *Shift) bellRule(r *crew.Role) string {
	if r.Name != "" && r.Name == s.Crew.Dispatcher || !s.Crew.DoorbellOn() {
		return ""
	}

	return BellRule
}

// bellMark is what a start that told its role what a bell is leaves: the time of the start, which is the one in the role's
// state while that start lasts.
type bellMark struct {
	Started time.Time `json:"started"`
}

// markBell leaves, or removes, the mark of the start that has just begun: written when its boot line carried BellRule. A
// mark that cannot be written is the same as none, and the role is sent every line whole.
func (s *Shift) markBell(name string, r *crew.Role, started time.Time) {
	if !hookRole(name) {
		return
	}

	l := s.Crew.Layout()
	_ = os.Remove(l.DeliveryBell(name))

	if !strings.Contains(s.bootText(r, false), BellRule) {
		return
	}

	raw, err := json.Marshal(bellMark{Started: started})
	if err != nil || fsx.MkdirAll(l.Delivery(), 0o700) != nil {
		return
	}

	_ = fsx.WriteFile(l.DeliveryBell(name), append(raw, '\n'), 0o600)
}

// bellKnown reports whether the start a role runs now told it what a bell is: the mark is of that start, by the time the
// state keeps for it. A role started by a zmina that wrote no mark, or restarted by one that does not know of it, is not.
func (s *Shift) bellKnown(name string) bool {
	var m bellMark

	return hookRole(name) && readRecord(s.Crew.Layout().DeliveryBell(name), &m) && !m.Started.IsZero() &&
		m.Started.Equal(s.State.Role(name).Started)
}

// bellsOn reports whether a role may be rung a bell: not the dispatcher, the crew has not turned them off, the start that
// runs told the role what one is, and no boot line waits in its queue. A clear starts a conversation that has not read the
// sentence, and queues the boot, which carries it, at the tail: what waits ahead of the boot goes as it always went, and
// the bells begin once the boot has left the queue.
func (s *Shift) bellsOn(name string) bool {
	if s.Crew == nil || name == s.Crew.Dispatcher || !s.Crew.DoorbellOn() || !s.bellKnown(name) {
		return false
	}

	rs := s.State.Role(name)

	return rs.QueuedBoot == "" || !slices.Contains(rs.Inbox, rs.QueuedBoot)
}

// bell is a delivery that goes as a doorbell: the line that goes out and the id of the text it is about.
type bell struct {
	text string
	id   string
}

// bellFor is the doorbell the lines of run (the longest run joinRun found: the first line, and those that may be read
// with it) go as, nil when they go as they are: the role is not rung bells, or they are not big enough. Posted to its own
// channel they are big past bellInline bytes; typed they are more than one line, or one line past bellTyped characters.
// The text of the bell — the numbered lines, or the one line — is stored in the crew's inbox before the bell is returned,
// under an id made of the role and the text, so that a delivery that is tried again stores it over itself. A text that
// cannot be stored is a bell that cannot be rung: the lines go as they are.
func (s *Shift) bellFor(name string, run []string, typed bool, now time.Time) *bell {
	if len(run) == 0 || !s.bellsOn(name) {
		return nil
	}

	body := run[0]
	if len(run) > 1 {
		body = numbered(run)
	}

	if typed {
		if len(run) == 1 && utf8.RuneCountInString(body) <= bellTyped {
			return nil
		}
	} else if len(body) <= bellInline {
		return nil
	}

	// Stored each time, whether an entry of the id is there or not: the same text replaces itself (one entry for the lines,
	// not one for each try), which keeps it for as long as the lines are being rung; and a text that was given up
	// (bellSuperseded) is the text again.
	id := inbox.IDOf("bell " + name + "\x00" + body)
	if err := s.inboxStore().Put(inbox.Entry{ID: id, From: "queue " + name, At: now, Role: name, Body: body}); err != nil {
		s.log(name, "bell-failed", "", "its text could not be stored, the lines go as they are: "+err.Error())

		return nil
	}

	who, what := s.bellWhoWhat(name, run)

	return &bell{text: bellText(len(run), who, what, id), id: id}
}

// BellSupersededText is what the text of a bell says once its lines were given back to the queue.
const BellSupersededText = "These lines were queued again and will come with a new bell; nothing here is current."

// bellSuperseded gives up the text of a bell whose lines went back to the queue (its conversation ended before it showed,
// queue retry): what it holds is no longer what is rung, and a role that reads it finds that said, and no fetch is
// noted for it. The text is stored again, whole, if the same lines are rung again (bellFor). A text that is not there, or
// is not this role's, is left.
func (s *Shift) bellSuperseded(name, id string) {
	if s.Crew == nil || !inbox.ValidID(id) {
		return
	}

	store := s.inboxStore()

	e, err := store.Get(id)
	if err != nil || e.Role != name {
		return
	}

	e.Body, e.Role = BellSupersededText, ""
	_ = store.Put(e)
}

// inboxStore is the crew's inbox, where the whole text of what is too long to send goes.
func (s *Shift) inboxStore() *inbox.Store {
	if s.Inbox != nil {
		return s.Inbox
	}

	return &inbox.Store{Dir: s.Crew.Layout().Inbox(), Now: s.Now}
}

// bellWhoWhat is who wrote the lines and what they are about, as their cards say, each once, in the order of the lines.
func (s *Shift) bellWhoWhat(name string, run []string) (who, what []string) {
	rc := s.cardsOf(name)
	if rc == nil {
		return nil, nil
	}

	for _, line := range run {
		c := rc.pick(CardID(name, line), true)
		if c == nil {
			continue
		}

		if from := oneLine(c.From); from != "" && !slices.Contains(who, from) {
			who = append(who, from)
		}

		if task := oneLine(c.Task); task != "" && !slices.Contains(what, task) {
			what = append(what, task)
		}
	}

	return who, what
}

// bellText is the line a role is rung: how many lines wait, who wrote them and what they are about where that is known,
// and the command that prints them.
func bellText(n int, who, what []string, id string) string {
	var b strings.Builder

	b.WriteString("[zmina] ")

	if n == 1 {
		b.WriteString("1 line waits for you")
	} else {
		b.WriteString(strconv.Itoa(n) + " lines wait for you")
	}

	names, tasks := bellList(who), bellList(what)

	switch {
	case names != "" && tasks != "":
		b.WriteString(" (" + names + " on " + tasks + ")")
	case names != "":
		b.WriteString(" (" + names + ")")
	case tasks != "":
		b.WriteString(" (on " + tasks + ")")
	}

	b.WriteString(": zmina inbox show " + id)

	return b.String()
}

// bellList is the first bellNames of the names, each cut at bellNameMax characters, and how many more there are.
func bellList(names []string) string {
	if len(names) == 0 {
		return ""
	}

	shown := names[:min(len(names), bellNames)]
	out := make([]string, len(shown))

	for i, n := range shown {
		out[i] = clipRunes(n, bellNameMax)
	}

	list := strings.Join(out, ", ")
	if more := len(names) - len(shown); more > 0 {
		list += " +" + strconv.Itoa(more)
	}

	return list
}

// typeBell types a bell into a role's input line, for the lines of run (bellFor), and records it as the typed line it is, with
// a part for each line. The lines it is for leave the queue with it, each with its ids. A typing that failed is judged as
// a line's is (deliverInboxOnce): one that broke where nothing proves that none of it reached the pane may have reached the
// role, so all the lines it is for are kept for a look (a tracked one by its ack alone), and none is typed again by
// itself; any other failure is the first line's alone, counted, and the lines after it wait. The caller has checked the
// input line and the last line typed.
func (s *Shift) typeBell(name string, run []string, b *bell, now time.Time) string {
	rs := s.State.Role(name)
	what := bellWhat(len(run), b.text)

	// The line's time is the moment its typing starts, not the beat's (typedTaken).
	typedAt := s.Now()

	if err := s.typeLine(name, b.text); err != nil {
		if typedMayHaveReached(err) {
			parts, _ := s.takeParts(name, run)
			rs.Inbox = rs.Inbox[len(run):]
			rs.clearTries()
			s.cardsBellUnclear(name, parts, b.id)

			// A tracked line counts as delivered and is said again by its ack alone, as one typed alone is.
			var (
				kept    []Part
				keptIDs []string
			)

			for _, p := range parts {
				if s.tracked(p.IDs) {
					s.delivered(p.IDs, now)
					s.log(name, "type-unclear", "", "may have reached the pane; its ack says it again: "+p.Line)
					s.cardUnclearTracked(name, p.Line, err.Error(), p.IDs, now)

					continue
				}

				kept, keptIDs = append(kept, p), append(keptIDs, p.IDs...)
			}

			if len(kept) == 0 {
				return ""
			}

			s.failUnclear(name, Failed{Line: b.text, Error: err.Error(), At: now, IDs: keptIDs, Parts: kept})

			return name + ": a queued bell may have reached its pane, kept in zmina queue as needing attention: " + oneLine(what)
		}

		head := run[0]

		rs.Attempts++
		rs.TriedAt = now
		s.deliveryFailure(name, head, err.Error())
		s.log(name, "type-failed", "", fmt.Sprintf("try %d: %s: %s", rs.Attempts, what, err))

		if rs.Attempts < typeTries {
			return ""
		}

		s.addFailed(name, Failed{Line: head, Error: err.Error(), At: now, IDs: s.takeIDs(name, head)})
		rs.Inbox = rs.Inbox[1:]
		rs.clearTries()

		return name + ": a queued line failed " + strconv.Itoa(typeTries) + " times, kept in zmina queue: " + oneLine(head)
	}

	parts, ids := s.takeParts(name, run)
	rs.Inbox = rs.Inbox[len(run):]
	rs.clearTries()
	s.log(name, "typed", "", what)
	s.cardsBelled(name, parts, b.id, AcceptedTyped, s.cardSession(name), typedAt)
	s.delivered(ids, now)

	for _, line := range run {
		s.submittedTask(name, line)
	}

	rs.Sent, rs.TypedAt = &Sent{Line: b.text, At: typedAt, IDs: ids, Parts: parts}, now

	return "typed into " + name + ": " + oneLine(what)
}

// cardsBelled notes that a bell was accepted: the lines it is for are accepted as any line is, and each card says which
// body it is in, and carries the line's ack ids, which close it as they close any line's (cardsAckedBy).
func (s *Shift) cardsBelled(name string, parts []Part, id, how, session string, at time.Time) {
	rc := s.cardsOf(name)
	if rc == nil {
		return
	}

	for _, p := range parts {
		c := rc.known(name, p.Line, true)
		c.Accepted, c.Attention = &CardAccepted{At: at, How: how, Session: session}, nil
		c.Bell, c.Acks = &CardBell{Body: id}, slices.Clone(p.IDs)
		rc.dirty = true
	}
}

// cardsBellUnclear notes, on the cards of the lines of a bell that was typed where it may have reached the pane, which body
// they are in: they are kept for a look and not typed again, and a retry that gives them back (cardBack) gives up the text,
// while a fetch of it, before that, is the lines read. Nothing is said of their being accepted.
func (s *Shift) cardsBellUnclear(name string, parts []Part, id string) {
	rc := s.cardsOf(name)
	if rc == nil {
		return
	}

	for _, p := range parts {
		rc.known(name, p.Line, false).Bell = &CardBell{Body: id}
		rc.dirty = true
	}
}

// cardsBellShown notes that the bell of a body was shown: on each card in it that was not told so yet, done or not.
func (s *Shift) cardsBellShown(name, id string, at time.Time) {
	rc := s.cardsOf(name)
	if rc == nil {
		return
	}

	for i := range rc.cards {
		if c := &rc.cards[i]; c.Bell != nil && c.Bell.Body == id && c.Bell.Shown.IsZero() {
			c.Bell.Shown = at
			rc.dirty = true
		}
	}
}

// rung reports a card whose bell was shown: its delivery is over, and what waits for it is the fetch. No later delivery
// of the same words is noted on it.
func (c *Card) rung() bool { return c.Bell != nil && !c.Bell.Shown.IsZero() }

// bellOpen reports a card whose bell went out and whose body was not fetched, and that nothing else ended: in play, or typed
// and landed (the end of a typed line), and with the line still waiting for its ack where it has one.
func (s *Shift) bellOpen(c *Card) bool {
	if c.Bell == nil || c.Seen != nil || c.Closed != nil && c.Closed.How != ClosedLanded {
		return false
	}

	return len(c.Acks) == 0 || s.tracked(c.Acks)
}

// fetchLine is a line of a role's fetches: the body that was read, when, and by whom it was run.
type fetchLine struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
	By string    `json:"by,omitempty"`
}

// RecordFetch notes that the text of a doorbell was read (zmina inbox show), as one more line of the role's fetches. It
// takes no lock: the file is only appended to, a line at a time, by a write that is one call, so it cannot be torn or
// lost to another writer; the delivery reads it under the state lock when it next looks at the role. It is for the
// caller to ignore the error: a fetch that could not be noted is a bell that is told as not fetched, and the read itself
// is never to fail for it.
func RecordFetch(l crew.Layout, role, id, by string, at time.Time) error {
	if !hookRole(role) {
		return fmt.Errorf("no fetch file for role %q", role)
	}

	raw, err := json.Marshal(fetchLine{ID: id, At: at.UTC(), By: by})
	if err != nil {
		return err
	}

	if err := fsx.MkdirAll(l.Delivery(), 0o700); err != nil {
		return err
	}

	f, err := os.OpenFile(l.DeliveryFetched(role), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}

	_, werr := f.Write(append(raw, '\n'))

	return errors.Join(werr, f.Close())
}

// by reports a fetch that is the role's own: run by the role itself, or by a caller that does not say who it is (an agent's
// tool with no ZMINA_ROLE, as a shared Codex role's). Run by another role of the crew, the dispatcher included, or by the
// owner's shell (hand), it is no one reading the lines for the role.
func (f fetchLine) by(role string) bool { return f.By == "" || f.By == role }

// sees reports whether a fetch is the role reading the lines of a card: the card went in the bell the fetch is for, was
// accepted before the fetch (a fetch of the same words from an earlier bell is not this one's), and the role ran it.
func (f fetchLine) sees(role string, c *Card) bool {
	if c.Bell == nil || c.Bell.Body != f.ID || !f.by(role) {
		return false
	}

	switch {
	case c.Accepted != nil:
		return !f.At.Before(c.Accepted.At)
	case c.Attention != nil:
		// A bell typed where it may have reached the pane is kept for a look with no acceptance: its lines are read from the
		// time it needed that look.
		return !f.At.Before(c.Attention.At)
	}

	return false
}

// readFetches reads a role's fetches: every line that reads, none for a file that is not there or does not read. Only the
// last bellFetchMax bytes are read, the newest lines, and a line cut at the start of them is left out.
func readFetches(path string) []fetchLine {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}

	defer func() { _ = f.Close() }()

	cut := false

	if st, err := f.Stat(); err == nil && st.Size() > bellFetchMax {
		if _, err := f.Seek(st.Size()-bellFetchMax, io.SeekStart); err != nil {
			return nil
		}

		cut = true
	}

	var out []fetchLine

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 64<<10)

	for first := true; sc.Scan(); first = false {
		// The first line of a tail starts somewhere in the middle of a line.
		if cut && first {
			continue
		}

		var l fetchLine
		if json.Unmarshal(sc.Bytes(), &l) == nil && inbox.ValidID(l.ID) && !l.At.IsZero() {
			out = append(out, l)
		}
	}

	return out
}

// syncBells marks the lines whose body the role fetched as seen, by fetch, on their cards: each fetch seen in the role's
// fetches that is not on the cards yet (a fetch is applied once, to the cards of the bell it was read for that were
// accepted before it, and only when the role ran it; reading the file again changes nothing). A bell that was kept as
// failed because its conversation never showed it, and was read all the same, has its failed record closed: its lines were
// read, and queue retry would say them twice. It says each in the log. Call it under the state lock, where the cards are
// written.
func (s *Shift) syncBells(name string) {
	if s.Crew == nil || s.Now == nil || !hookRole(name) {
		return
	}

	path := s.Crew.Layout().DeliveryFetched(name)
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		return
	}

	fetches := readFetches(path)
	if len(fetches) == 0 {
		return
	}

	rc := s.cardsOf(name)
	if rc == nil {
		return
	}

	for _, f := range fetches {
		n := 0

		for i := range rc.cards {
			c := &rc.cards[i]
			if !f.sees(name, c) || c.Seen != nil || c.Closed != nil && c.Closed.How != ClosedLanded && c.Closed.How != ClosedAcked {
				continue
			}

			c.Seen = &CardSeen{At: f.At, By: SeenFetch}

			if c.Bell.Shown.IsZero() {
				c.Bell.Shown = f.At
			}

			if c.Attention != nil && c.Attention.Why == bellUnfetched {
				c.Attention = nil
			}

			rc.dirty = true
			n++
		}

		if n == 0 {
			continue
		}

		said := fmt.Sprintf("%s: %d lines seen", f.ID, n)
		if f.By != "" {
			said = f.ID + " by " + f.By + ": " + strconv.Itoa(n) + " lines seen"
		}

		// The cards are seen first, so that closing the failed record leaves them so.
		failed := 0

		s.trimFailed(name, func(fl Failed) bool {
			if s.bellRecord(name, fl.Line, fl.Parts) != f.ID {
				return false
			}

			failed++

			return true
		}, ClosedDropped, "its body was fetched")

		if failed > 0 {
			said += ", its failed record closed"
		}

		s.log(name, "fetched", "", said)
	}
}

// bellWatch is the beat's look at the bells: the fetches are applied to the cards, a bell shown more than postedWait ago
// whose body is still not fetched puts attention on each of its lines' cards and gives the dispatcher one note (the note
// is made once: the attention is what says it was), and a bell that has gone unfetched for as long as a card is kept is
// let go. It rings nothing again. It returns the notes for the dispatcher. Call it under the state lock.
//
// The attention on the cards is what says the dispatcher was told, so a role's note is made only once its cards are on disk
// with the attention: a role whose cards cannot be written (or were lost, a file that was corrupt and started anew) is
// told of nothing this pass, rather than told again each beat. What that leaves is a gap, said here: with its cards lost a
// bell unfetched makes no note; the lines' acks (said again as ever) and the queue's own records still stand.
func (s *Shift) bellWatch(now time.Time) []string {
	var notes []string

	for _, name := range s.Names() {
		if name == s.Crew.Dispatcher || !hookRole(name) {
			continue
		}

		// A role that was never rung a bell has nothing here: no card file is read for it.
		if !s.bellKnown(name) {
			if _, err := os.Stat(s.Crew.Layout().DeliveryFetched(name)); err != nil {
				continue
			}
		}

		s.syncBells(name)

		rc := s.cardsOf(name)
		if rc == nil {
			continue
		}

		if told := s.bellOverdue(name, rc, now); len(told) > 0 && s.flushRole(name) {
			for _, n := range told {
				notes = append(notes, n.text)
				s.log(name, "bell-unfetched", "", n.event)
			}
		}
	}

	return notes
}

// countLines is how many lines, in words.
func countLines(n int) string {
	if n == 1 {
		return "1 line"
	}

	return strconv.Itoa(n) + " lines"
}

// bellNote is what a bell left unfetched has to say: the dispatcher's note and the event.
type bellNote struct{ text, event string }

// bellOverdue is bellWatch for the cards of one role.
func (s *Shift) bellOverdue(name string, rc *roleCards, now time.Time) []bellNote {
	var (
		notes  []bellNote
		order  []string
		groups = map[string][]*Card{}
	)

	for i := range rc.cards {
		if c := &rc.cards[i]; s.bellOpen(c) && c.rung() {
			if _, ok := groups[c.Bell.Body]; !ok {
				order = append(order, c.Bell.Body)
			}

			groups[c.Bell.Body] = append(groups[c.Bell.Body], c)
		}
	}

	for _, id := range order {
		cards := groups[id]
		shown := slices.MinFunc(cards, func(a, b *Card) int { return a.Bell.Shown.Compare(b.Bell.Shown) }).Bell.Shown
		waited := now.Sub(shown)

		switch {
		case waited > cardKeep:
			for _, c := range cards {
				c.Closed = &CardClosed{At: now, How: ClosedDropped, Why: "bell shown, body never fetched in " + span(cardKeep)}
			}

			rc.dirty = true
		case waited >= postedWait:
			// The attention says the dispatcher was told: a bell is told of once, however long it stays unfetched.
			told := slices.ContainsFunc(cards, func(c *Card) bool { return c.Attention != nil })

			for _, c := range cards {
				if c.Attention == nil {
					c.Attention = &CardAttention{At: now, Why: bellUnfetched}
					rc.dirty = true
				}
			}

			if !told {
				// The dispatcher is told which role, how many lines and for how long, and where to look; the command that prints
				// the lines is the role's to run, and a run by anyone else is no fetch.
				notes = append(notes, bellNote{
					text: fmt.Sprintf("%s: a bell for %s was shown %s ago and the role has not fetched them; "+
						"it is not rung again: zmina queue ls", name, countLines(len(cards)), span(waited)),
					event: fmt.Sprintf("%d lines, shown %s ago: %s", len(cards), span(waited), id),
				})
			}
		}
	}

	return notes
}

// rungCards are the cards of a role's lines whose bell was shown and whose body was not fetched, the oldest bell first: the
// lines of a bell that was accepted, shown within the time a card is kept, still waiting for their ack where they have one,
// and not read by a fetch the role ran after the bell was accepted (whether a pass put it on the card yet or not).
func (s *Shift) rungCards(role string, cards []Card, now time.Time) []Card {
	fetches := readFetches(s.Crew.Layout().DeliveryFetched(role))

	var out []Card

	for _, c := range cards {
		if s.bellOpen(&c) && c.rung() && now.Sub(c.Bell.Shown) <= cardKeep &&
			!slices.ContainsFunc(fetches, func(f fetchLine) bool { return f.sees(role, &c) }) {
			out = append(out, c)
		}
	}

	slices.SortStableFunc(out, func(a, b Card) int { return a.Bell.Shown.Compare(b.Bell.Shown) })

	return out
}

// BellWait is a bell its role was shown and has not fetched: the command that prints its lines, how many there are, who
// wrote them and what they are about, and when it was shown.
type BellWait struct {
	ID    string
	Lines int
	Who   []string
	What  []string
	Shown time.Time
}

// BellWaits are the bells a role was shown and whose body it did not fetch, the oldest first, for its brief: from its
// cards and its fetches as the files have them (a look, nothing is written), within the time a card is kept. A line the
// role has acked, or that was said again, is not one that waits.
func (s *Shift) BellWaits(role string, now time.Time) []BellWait {
	if role == "" || role == s.Crew.Dispatcher || !hookRole(role) {
		return nil
	}

	var (
		out []BellWait
		at  = map[string]int{}
	)

	for _, c := range s.rungCards(role, ReadCards(s.Crew.Layout(), role), now) {
		i, ok := at[c.Bell.Body]
		if !ok {
			i = len(out)
			at[c.Bell.Body] = i
			out = append(out, BellWait{ID: c.Bell.Body, Shown: c.Bell.Shown})
		}

		w := &out[i]
		w.Lines++

		if from := oneLine(c.From); from != "" && !slices.Contains(w.Who, from) {
			w.Who = append(w.Who, from)
		}

		if task := oneLine(c.Task); task != "" && !slices.Contains(w.What, task) {
			w.What = append(w.What, task)
		}
	}

	return out
}

// bellBrief is what a role's brief says of the bells it was shown and has not read: each with the command that prints it.
func (s *Shift) bellBrief(role string) string {
	if s.Now == nil {
		return ""
	}

	waits := s.BellWaits(role, s.Now())
	if len(waits) == 0 {
		return ""
	}

	items := make([]string, 0, len(waits))

	for _, w := range waits {
		what := strconv.Itoa(w.Lines) + " lines"
		if w.Lines == 1 {
			what = "1 line"
		}

		if who := bellList(w.Who); who != "" {
			what += " from " + who
		}

		if on := bellList(w.What); on != "" {
			what += " on " + on
		}

		items = append(items, "zmina inbox show "+w.ID+" ("+what+", shown "+s.clock(w.Shown)+")")
	}

	return "[zmina] These doorbells were rung and their lines not read — run each command, act on every numbered line, " +
		"ack each by its own id:\n- " + strings.Join(items, "\n- ")
}

// pruneFetches removes a role's fetches once the file is older than keep, by its modification time, and returns how many
// lines went. A file is never rewritten: the role may append to it at any moment, and a rewrite would lose the line it is
// writing. A file that was written to within the keep stays whole, its older lines with it, until the time comes; they
// are read from its tail and cannot be taken for new ones (a fetch counts only for a bell accepted before it). With dry
// it only counts.
func pruneFetches(path string, now time.Time, keep time.Duration, dry bool) (int, error) {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}

	if err != nil {
		return 0, err
	}

	if now.Sub(st.ModTime()) <= keep {
		return 0, nil
	}

	lines := max(len(readFetches(path)), 1)

	if dry {
		return lines, nil
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}

	return lines, nil
}
