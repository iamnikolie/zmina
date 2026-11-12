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

// enqueueHead queues lines at the head of a role's queue, in the order given, each with its card: lines that are to go
// before everything that waits (the recovery pointer, a rename).
func (s *Shift) enqueueHead(name string, f Facts, lines ...string) {
	rs := s.State.Role(name)
	rs.Inbox = slices.Concat(lines, rs.Inbox)

	for _, line := range lines {
		s.cardNew(name, line, f)
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
