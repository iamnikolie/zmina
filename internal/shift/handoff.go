package shift

import (
	"fmt"
	"github.com/iamnikolie/zmina/internal/handoff"
	"github.com/iamnikolie/zmina/internal/task"
	"path/filepath"
	"slices"
	"strings"
)

// TaskPacket renders current task state and the assigned role's reporting shape.
func (s *Shift) TaskPacket(name string, t task.Task) (handoff.Packet, error) {
	r, err := s.Role(name)
	if err != nil {
		return handoff.Packet{}, err
	}
	shape := s.Crew.HandoffFor(r)
	t.Delivery = nil // Receipt metadata must not recursively change the packet identity.
	return handoff.Packet{Version: 1, Role: name, Task: t, Shape: shape, Instructions: fmt.Sprintf("Read repository instructions. Run zmina --crew %s task show %s and confirm this task is still assigned to %s; use the latest note before working.\n", shellWord(s.Crew.Path), t.ID, name) + shape.Request("checkpoint or result") + criteriaRule(t) + fmt.Sprintf(ReportRule, shellWord(s.Crew.Path), t.ID)}, nil
}

// criteriaRule is the part of a packet's instructions that says what done means, when the task has criteria; a task
// without them gets nothing, so its packet is the one it always had.
func criteriaRule(t task.Task) string {
	if len(t.Criteria) == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteString("Done means every criterion below holds; in your result say for each whether it holds and how you checked.\n")

	for i, c := range t.Criteria {
		fmt.Fprintf(&b, "%d. %s\n", i+1, c)
	}

	return b.String()
}

// ReportRule is how a worker reports a finished task: the result goes to the ledger, which tells the dispatcher
// the gist and the command that shows it. A message of the worker's own on top is the same news twice, each with
// its own ack, and a whole report sent as a message sits in the dispatcher's context on every turn after it.
const ReportRule = "When done, save it with zmina --crew %s task result %s --file <json>: that tells the dispatcher, " +
	"with the gist and the command that shows it. Send the dispatcher no message of your own about the result: " +
	"it would hear it twice.\n"

// HandTask queues only a packet locator, never the task's body.
func (s *Shift) HandTask(name string, t task.Task) (string, error) {
	h, err := s.prepareHand(name, t, false)
	if err != nil {
		return "", err
	}

	if h.queue {
		if err := s.QueueFrom(name, h.line, Facts{Task: t.ID}); err != nil {
			return "", err
		}
	}

	return h.path, nil
}

// packetHand is a task's hand-out as far as it can be made before its line is queued: the packet's file, the line that
// locates it, whether that line is yet to be queued (not when it waits already, or the role has it), and the task's
// delivery as it was before.
type packetHand struct {
	path, line string
	queue      bool
	before     *task.Delivery
}

// prepareHand does everything of HandTask but queue the line: the packet is written, the task's delivery is renewed
// when it has to be, and a line it replaces is taken out of every queue. fresh says the role is about to start a new
// session (a restart hands out before it starts it): the delivery is then renewed whatever the role's last start, which is
// of the session that is going, says. The line is the caller's to queue; HandTask puts it at the end of the queue, a
// restart at the head.
func (s *Shift) prepareHand(name string, t task.Task, fresh bool) (packetHand, error) {
	p, err := s.TaskPacket(name, t)
	if err != nil {
		return packetHand{}, err
	}
	path, err := handoff.Write(s.Crew.StateDir(), p)
	if err != nil {
		return packetHand{}, err
	}
	id := strings.TrimSuffix(filepath.Base(path), ".json")
	line := fmt.Sprintf("[zmina %s] Read handoff packet %q; before work run: zmina --crew %s task accept %s --handoff %s --role %s", t.ID, path, shellWord(s.Crew.Path), t.ID, id, shellWord(name))
	current, err := s.Tasks.Get(t.ID)
	if err != nil {
		return packetHand{}, err
	}

	h := packetHand{path: path, line: line}
	if current.Delivery != nil {
		before := *current.Delivery
		h.before = &before
	}

	// The role started since the delivery was handed to a session of its own: the one it was queued or sent for is gone.
	// A delivery that was sent is judged by when it was sent (a submission after the start belongs to this session); one
	// that never was, by when it was queued. A restart queues its hand-out before it starts the role, so only the
	// former stays right for the whole life of the fresh session.
	renew := fresh
	if d := current.Delivery; d != nil {
		since := d.QueuedAt
		if !d.SentAt.IsZero() {
			since = d.SentAt
		}

		renew = renew || s.State.Role(name).Started.After(since)
	}

	if current.Delivery != nil && current.Delivery.ID == id && !current.Delivery.AcceptedAt.IsZero() {
		live := s.session(name)
		if !renew && live != nil && live.SessionID == current.Delivery.Session {
			return h, nil
		}
		renew = true
	}
	if renew || current.Delivery == nil || current.Delivery.ID != id || current.Delivery.Role != name {
		r, _ := s.Role(name)
		runtime, home := s.runtimeScope(r)
		current.Delivery = &task.Delivery{ID: id, Role: name, Runtime: runtime, Home: home, Line: line, TaskUpdated: current.Updated, QueuedAt: s.Now().UTC()}
		if err := s.Tasks.Put(current); err != nil {
			return packetHand{}, err
		}
	}

	// A restart takes the lines this replaces out once the role has started (replacePackets): a start that fails goes
	// back to the old conversation, and the lines that waited for it are still what they were.
	if !fresh {
		s.replacePackets(t.ID, line)
	}

	// A line already on its way to the role (posted and not yet seen, typed and not yet landed) is not queued again,
	// unless the role started since: a command that delivers at once (DeliverTo) takes it out of the queue long
	// before the beat would have, and the packet must not reach the same session twice. Only a session that lives
	// has a line on its way: what a dead one was typed is dropped when it is settled, so it is queued for the next.
	h.queue = !slices.Contains(s.State.Role(name).Inbox, line) && (renew || !s.onItsWay(name, line))

	return h, nil
}

// replacePackets takes the lines of older packets of a task, whoever's queue they wait in, out of the queues: the packet
// whose line is given is the newer one.
func (s *Shift) replacePackets(id, line string) {
	prefix := "[zmina " + id + "] Read handoff packet "

	for holder, rs := range s.State.Roles {
		head := ""
		if len(rs.Inbox) > 0 {
			head = rs.Inbox[0]
		}

		s.trimInbox(holder, func(old string) bool { return strings.HasPrefix(old, prefix) && old != line }, ClosedDropped,
			"a newer packet for the task replaces it")

		// The failed tries counted were the old first line's: the line that is first now starts with none.
		if head != "" && (len(rs.Inbox) == 0 || rs.Inbox[0] != head) {
			rs.clearTries()
		}
	}
}

// onItsWay reports whether a line was posted or typed to the role's live conversation and has not yet been seen or
// landed there. A role with no live session has nothing on its way, and neither does a post made to a conversation
// that is no longer the role's (cleared since).
func (s *Shift) onItsWay(name, line string) bool {
	sess := s.session(name)
	if sess == nil {
		return false
	}

	rs := s.State.Role(name)

	return rs.Sent != nil && rs.Sent.Line == line ||
		slices.ContainsFunc(rs.Posted, func(p Posted) bool { return p.Line == line && p.Session == sess.SessionID })
}

func shellWord(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
