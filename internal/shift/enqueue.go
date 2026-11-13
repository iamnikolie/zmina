package shift

import "slices"

// Every line that goes into a role's queue goes through here, so that each has its card (cards.go). TestInboxIsOnlyAppendedByEnqueue
// walks the sources and fails for an append anywhere else. The lines that leave a queue by being typed, posted, dropped
// or acked note their card where they leave (the callers of cardAccept, cardClose); the helpers here are the entries, and
// the one exit that is a loop (trimInbox).

// enqueue queues a line at the end of a role's queue and writes its card.
func (s *Shift) enqueue(name, line string, f Facts) {
	rs := s.State.Role(name)
	rs.Inbox = append(rs.Inbox, line)
	s.cardNew(name, line, f)
}

// moveToHead takes the line at place i of a role's queue to its head: the one that goes out of its order (the owner's own
// message through a hold, deliverInboxOnce). It is the same line with the same card. The failed tries counted (Attempts,
// TriedAt) are those of the line that was first, and are not touched here: the caller, which knows whether the line goes,
// keeps them for the line they are of.
func (s *Shift) moveToHead(name string, i int) {
	rs := s.State.Role(name)
	if i <= 0 || i >= len(rs.Inbox) {
		return
	}

	line := rs.Inbox[i]
	rs.Inbox = slices.Concat([]string{line}, rs.Inbox[:i], rs.Inbox[i+1:])
}

// moveBack puts the head of a role's queue back at place i, where moveToHead took it from: the order the queue was in.
func (s *Shift) moveBack(name string, i int) {
	rs := s.State.Role(name)
	if i <= 0 || len(rs.Inbox) < 2 {
		return
	}

	line := rs.Inbox[0]
	rest := slices.Clone(rs.Inbox[1:])
	i = min(i, len(rest))
	rs.Inbox = slices.Concat(rest[:i], []string{line}, rest[i:])
}

// enqueueHead queues lines at the head of a role's queue, in the order given, each with its card: lines that are to go
// before everything that waits (the recovery pointer, a rename).
func (s *Shift) enqueueHead(name string, f Facts, lines ...string) {
	rs := s.State.Role(name)
	rs.Inbox = slices.Concat(lines, rs.Inbox)

	for _, line := range lines {
		s.cardNew(name, line, f)
	}
}

// headLine is a line for enqueueHeadLines, with what its caller knows of it.
type headLine struct {
	line string
	f    Facts
}

// enqueueHeadLines is enqueueHead for lines that each carry facts of their own (the restart's line from zmina, a task's
// packet about its task): they go at the head in the order given, each with its card and its "queued" line in the log,
// as a line queued by QueueFrom has them. A line that goes before everything that waits is another's first now, so
// the failed tries counted are forgotten. No bound: the caller has made its room (inboxMax is the bound of what a
// sender may add; the lines of zmina's own that a role cannot start without are never refused).
func (s *Shift) enqueueHeadLines(name string, lines ...headLine) {
	if len(lines) == 0 {
		return
	}

	rs := s.State.Role(name)
	rs.Inbox = slices.Concat(headLinesText(lines), rs.Inbox)
	rs.clearTries()

	for _, l := range lines {
		s.cardNew(name, l.line, l.f)
		s.log(name, "queued", "", l.line)
	}
}

func headLinesText(lines []headLine) []string {
	text := make([]string, len(lines))
	for i, l := range lines {
		text[i] = l.line
	}

	return text
}

// takeOutLines takes each of the lines out of a role's queue once (a line queued twice loses one copy per mention); each
// one's card is closed how, with why.
func (s *Shift) takeOutLines(name string, lines []string, how, why string) {
	left := slices.Clone(lines)
	rs := s.State.Role(name)

	head := ""
	if len(rs.Inbox) > 0 {
		head = rs.Inbox[0]
	}

	s.trimInbox(name, func(line string) bool {
		i := slices.Index(left, line)
		if i < 0 {
			return false
		}

		left = slices.Delete(left, i, i+1)

		return true
	}, how, why)

	// The failed tries counted were the old first line's: the line that is first now starts with none.
	if head != "" && (len(rs.Inbox) == 0 || rs.Inbox[0] != head) {
		rs.clearTries()
	}
}

// giveBackHead puts lines that went out and did not reach the role (giveBack) back at the head of its queue. Each keeps
// the card it had and says it was given back, why; a line that never had a card gets one.
func (s *Shift) giveBackHead(name, why string, lines []string) {
	if len(lines) == 0 {
		return
	}

	rs := s.State.Role(name)
	rs.Inbox = slices.Concat(lines, rs.Inbox)

	now := s.cardNow()
	for _, line := range lines {
		s.cardBack(name, line, why, now)
	}
}

// trimInbox takes the lines drop names out of a role's queue; each one's card is closed how, with why.
func (s *Shift) trimInbox(name string, drop func(line string) bool, how, why string) {
	rs := s.State.Role(name)
	now := s.cardNow()

	rs.Inbox = slices.DeleteFunc(rs.Inbox, func(line string) bool {
		if !drop(line) {
			return false
		}

		s.cardClose(name, line, how, why, now)

		return true
	})
}

// trimFailed takes the failed lines drop names out of a role's failed list; each one's card is closed how, with why.
func (s *Shift) trimFailed(name string, drop func(Failed) bool, how, why string) {
	rs := s.State.Role(name)

	rs.Failed = slices.DeleteFunc(rs.Failed, func(f Failed) bool {
		if !drop(f) {
			return false
		}

		s.cardsClosed(name, f.Line, f.Parts, how, why)

		return true
	})
}

// DismissUnclear takes a failed line that may have reached its role off the failed list, as the owner's look at it in the
// cockpit does; its card is closed as dropped.
func (s *Shift) DismissUnclear(name, line string) {
	s.trimFailed(name, func(f Failed) bool { return f.Line == line && f.Unclear() }, ClosedDropped, "the owner dismissed it")
}

// addFailed puts a line on a role's failed list, and notes it on its card: an unclear one needs attention, any other
// is closed as failed.
func (s *Shift) addFailed(name string, f Failed) {
	rs := s.State.Role(name)
	rs.Failed = append(rs.Failed, f)
	s.cardFailed(name, f, f.At)
}
