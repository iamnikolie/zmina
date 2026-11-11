package shift

import (
	"slices"
	"strings"
	"time"
)

// A line in flight (posted to a role's inbox socket, typed into its prompt) is bookkeeping about one conversation, and
// nothing but that conversation's own events used to end it: a role put down, released or ended left its posted lines
// posted for ever. A session that is gone now settles what it left (settleEnded), when the beat finds the role not up
// and when a new session starts (start). Lines that wait in a role's inbox are not in flight: a role that is down
// simply has them waiting.

// Waiter is a Peer whose posted lines wait as files until the agent's bridge takes them, and stay after the bridge
// is gone (the Codex inbox, keyed by the role's session name): such a line is run by the next bridge, whatever the
// role's records say.
type Waiter interface {
	Waiting(dir, msgID string) bool
}

// Withdrawer is a Waiter that can take such a line back out of the inbox: true only when it removed the file, never for
// a line the bridge already took.
type Withdrawer interface {
	Withdraw(dir, msgID string) bool
}

// withdraw takes a posted line back out of a bridge's inbox, reporting whether it removed it.
func withdraw(ch Peer, dir string, p Posted) bool {
	w, ok := ch.(Withdrawer)

	return ok && w.Withdraw(dir, p.MsgID)
}

// postedBound reports a posted record that is session-bound whole: a line that is (or the boot), or a post made only of
// lines that are. A post that also carries signals or notes is not: its file could not be taken apart.
func (s *Shift) postedBound(name string, p Posted) bool {
	if len(p.Parts) == 0 {
		return p.Boot || s.sessionBound(name, p.Line)
	}

	return !slices.ContainsFunc(p.Parts, func(part Part) bool { return part.Line == "" || !s.sessionBound(name, part.Line) })
}

// stillQueued reports a post that waits in a bridge's inbox untaken. It is not given back to the role's queue (the
// next bridge runs the file, a second one would run it twice) and not aged out; its record stays until the file is
// taken (Delivered) or gone. The line cannot be lost this way: while the file exists a bridge will take it, and when it
// does not, the line is given back as any other.
func stillQueued(ch Peer, dir string, p Posted) bool {
	w, ok := ch.(Waiter)

	return ok && w.Waiting(dir, p.MsgID)
}

// settleEnded settles what a role's conversation left in flight when its session is gone, by kind:
//
//   - A posted line the conversation never showed went nowhere that will read it. Unless it is session-bound it goes
//     back to the head of the role's queue (giveBack, the one way a line goes back; ids and parts as they were), for
//     its next session: once, since it leaves Posted as it enters the queue, and a line the conversation did show
//     (followPosted looks first) is done, not given back. A session-bound line (sessionBound: the role's boot line, a
//     slash command, the release and restart requests) is dropped with an event: it made sense to that conversation
//     only, and a fresh one has its own boot and is not released.
//   - A typed line (Sent) is dropped with an event, never given back: it went into the pane, whether the session took
//     it is not known, and a line that may have been taken is not typed again (landed keeps one as failed for
//     inspection for that reason). start has always cleared it. A tracked line stays in Acks: it is said again
//     (renotify) and listed in the next session's brief (PendingBrief).
//
// A post that waits in a Codex bridge's inbox as a file is neither (stillQueued). It does nothing for a role with
// nothing in flight. The beat calls it for a role that is really gone (see Tick); start for the session it replaces.
func (s *Shift) settleEnded(name string, now time.Time) {
	rs := s.State.Role(name)

	s.followPostedAs(name, now, true)

	if sent := rs.Sent; sent != nil {
		rs.Sent = nil
		s.log(name, "dropped", "", "typed into a session that ended, never seen to land: "+sent.Line)
	}
}

// requeuePosted gives back a posted line whose conversation ended before it showed (followPosted), except what only
// that conversation was meant for: a line, or a part of a post to the dispatcher (an own queued line), that is
// sessionBound is dropped with an event. It returns the lines for the head of the role's queue.
func (s *Shift) requeuePosted(name string, p Posted) []string {
	parts := p.Parts

	if len(parts) == 0 {
		if p.Boot || s.sessionBound(name, p.Line) {
			s.log(name, "dropped", "", "meant for the conversation that ended, never shown in it: "+p.Line)

			return nil
		}
	} else {
		parts = slices.DeleteFunc(slices.Clone(parts), func(part Part) bool {
			bound := part.Line != "" && s.sessionBound(name, part.Line)
			if bound {
				s.log(name, "dropped", "", "meant for the conversation that ended, never shown in it: "+part.Line)
			}

			return bound
		})

		if len(parts) == 0 {
			return nil
		}
	}

	// What goes back is what giveBack returns for the queue, and the signals and notes it puts back on theirs; what
	// its reader acked meanwhile it leaves out (and logs), and then nothing was requeued.
	before := len(s.State.Signals) + len(s.State.Notes)
	lines := s.giveBack(name, p.Line, p.IDs, parts)

	if len(lines) > 0 || len(s.State.Signals)+len(s.State.Notes) > before {
		s.log(name, "requeued", "", "its conversation ended before it showed: "+p.Line)
	}

	return lines
}

// What zmina writes to one conversation only. Matched by the words each line opens with, the way queueRecovery knows
// its own line: the producers are releaseLine, releaseCheckpointRequest, restartLine, earlyLine and recoveryLine
// (TestSessionBoundLinesAreThoseZminaWrites ties them to this list).
var sessionBoundOpenings = []string{
	"[zmina release] ",
	"[zmina recovery] ",
	"[zmina] restart: ",
	"[zmina] restart ahead: ",
}

// sessionBound reports a line that only made sense to the conversation it was posted to: a slash command (it acts on
// that conversation), the role's boot line queued after a clear (a new start brings its own), the request to park
// work before a release, to write a note before a restart, the pointer to a recovery pack. Everything else
// is for the role, not for the session: a line sent to it, an alarm, a task packet, a signal.
func (s *Shift) sessionBound(name, line string) bool {
	// The boot is the line a clear queued, known by equality: the role's boot text is also how a line sent to it may
	// open, and a line is not the boot for opening like it. Once it is posted, the record says so itself (Posted.Boot),
	// since QueuedBoot is cleared as soon as the boot has left the queue.
	if q := s.State.Role(name).QueuedBoot; slashCommand(line) || q != "" && line == q {
		return true
	}

	if strings.HasPrefix(line, "[zmina] idle ") && strings.Contains(line, "you are released and go down") {
		return true
	}

	return slices.ContainsFunc(sessionBoundOpenings, func(open string) bool { return strings.HasPrefix(line, open) })
}
