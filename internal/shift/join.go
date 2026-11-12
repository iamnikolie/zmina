package shift

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/registry"
)

// A role that takes its lines on its agent's own channel (a Claude inbox socket, a Codex bridge's inbox) gets what waits
// in its queue as one numbered message, not one line a pass: the longest run of lines from the head of the queue that may
// be read together (joinable), at most joinMax of them, the message within postMax. Each line keeps its own text and its
// ack tail, so each id in it is acked by itself. The post is recorded as one Posted whose Parts hold one part per line,
// the shape a post to the dispatcher already has: what happens to a post (shown, never shown, ended, acked, given back,
// failed, retried) happens to its parts one by one, by the machinery that serves the dispatcher's own queued lines. Typed
// delivery stays one line a pass, and one waiting line goes as it always went, whole and unnumbered.

// joinMax is the most queued lines one joined post carries.
const joinMax = 8

var (
	// packetLine is how a task's handoff packet line opens (HandTask): the locator of a packet, which the role accepts by a
	// command naming its id.
	packetLine = regexp.MustCompile(`^\[zmina [^\]\s]+\] Read handoff packet `)
	// locatorForm is the tail of a line stored whole in the crew's inbox and sent as its head and where the rest is
	// (Pointer).
	locatorForm = regexp.MustCompile(`\(\+[0-9]+ chars, full: zmina inbox show [^)\s]+\)`)
)

// joinable reports whether a queued line may go in one message with the lines around it. Never joined, each going alone
// when it is the first in the queue and ending the run before it when it is not: the rename a start queued, the owner's
// own message from Chat (his own turn, and whatever the runtime), the role's boot line, a line that only the conversation
// it was queued for may read (sessionBound: a slash command, which is typed and not read, the boot a clear queued, the
// release and restart requests, the recovery pointer), a task's packet locator (the role accepts it by id, and its
// delivery is marked on its own), and a line stored whole in the inbox and sent as a head with where the rest is.
func (s *Shift) joinable(name, line string) bool {
	rs := s.State.Role(name)

	switch {
	case rs.QueuedRename != "" && line == rs.QueuedRename,
		rs.ownerMessage(line),
		s.isBoot(name, line),
		s.sessionBound(name, line),
		packetLine.MatchString(line),
		locatorForm.MatchString(line):
		return false
	}

	return true
}

// numbered is the text of a joined post: each line after its place in the queue, "1. …", one to a line of the message.
func numbered(lines []string) string {
	var b strings.Builder

	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}

		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(line)
	}

	return b.String()
}

// joinRun is the lines a role's next post joins: the longest prefix of its queue whose lines are all joinable, no more
// than joinMax, the numbered message within postMax, none the same words as one before it (each line has a card and ids
// of its own, told apart by their words). The dispatcher's own queued lines are joined by its own post (postDispatcher),
// never here. A run of one line, or none, is no join.
func (s *Shift) joinRun(name string) []string {
	if name == s.Crew.Dispatcher {
		return nil
	}

	var (
		run  []string
		size int
	)

	for _, line := range s.State.Role(name).Inbox {
		if len(run) == joinMax || !s.joinable(name, line) || slices.Contains(run, line) {
			break
		}

		// The line as the message holds it, and the break before it.
		add := len(strconv.Itoa(len(run)+1)) + len(". ") + len(line)
		if len(run) > 0 {
			add++
		}

		if len(run) > 0 && size+add > postMax {
			break
		}

		run = append(run, line)
		size += add
	}

	return run
}

// postJoined posts lines, the first of a role's queue and as many after it as joinRun found, as one numbered message. The
// record of the post has a part for each line, with the ids it carries; the card of each line is accepted. A post that
// never started is tried again as a line's is (typeTries beats); one whose write broke is followed as posted (post);
// any other failure keeps the whole post as one failed record, whose lines a retry gives back in order, one by one.
func (s *Shift) postJoined(name string, sess *registry.Session, lines []string, now time.Time) string {
	rs := s.State.Role(name)
	text := numbered(lines)
	what := strconv.Itoa(len(lines)) + " lines joined: " + text

	if err := s.postAs(name, sess, text, what, now); err != nil {
		rs.Attempts++
		rs.TriedAt = now
		s.log(name, "post-failed", "", fmt.Sprintf("try %d: %s: %s", rs.Attempts, what, err))

		if unavailable(err) && rs.Attempts < typeTries {
			return ""
		}

		parts, ids := s.takeParts(name, lines)

		s.deliveryFailure(name, text, err.Error())
		s.addFailed(name, Failed{Line: text, Error: err.Error(), At: now, IDs: ids, Parts: parts})
		rs.Inbox = rs.Inbox[len(lines):]
		rs.clearTries()

		return name + ": " + strconv.Itoa(len(lines)) + " queued lines could not be posted, kept in zmina queue: " + oneLine(text)
	}

	parts, ids := s.takeParts(name, lines)
	posted := &rs.Posted[len(rs.Posted)-1]
	posted.IDs, posted.Parts = slices.Clone(ids), parts

	rs.Inbox = rs.Inbox[len(lines):]
	rs.clearTries()

	for _, line := range lines {
		s.submittedTask(name, line)
	}

	s.delivered(ids, now)
	s.cardsAccepted(name, text, parts, s.acceptedBy(name), sess.SessionID, now)

	return "posted to " + name + ": " + oneLine(what)
}

// takeParts takes the ids of each line that leaves the queue, once, and returns a part for each line and all the ids.
func (s *Shift) takeParts(name string, lines []string) ([]Part, []string) {
	parts := make([]Part, 0, len(lines))

	var ids []string

	for _, line := range lines {
		lineIDs := s.takeIDs(name, line)
		parts = append(parts, Part{Line: line, IDs: lineIDs})
		ids = append(ids, lineIDs...)
	}

	return parts, ids
}

// ownerMessage reports a queued line that is the owner's own message from Chat: one a receipt was made for (only Chat's
// send writes receipts), whichever runtime the role has.
func (rs *RoleState) ownerMessage(line string) bool {
	for _, receipt := range rs.ChatReceipts {
		if receipt.Line == line {
			return true
		}
	}

	return false
}
