package shift

import (
	"errors"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/peer"
	"github.com/iamnikolie/zmina/internal/registry"
)

// postedWait is how long a line posted to a role's inbox socket may go unseen in its conversation, the role sitting
// idle, before it is kept as failed: the session held or dropped it.
const postedWait = 10 * time.Minute

// postedNever is the failure of a posted line the conversation never showed.
const postedNever = "posted to its inbox socket, never seen in its conversation (held or dropped); inspect before retry"

// inbound reports whether a role starts with zmina's settings, its lines posted to its inbox socket.
func (s *Shift) inbound(r *crew.Role) bool {
	return s.Peer != nil && s.Crew.DeliveryFor(r) == crew.DeliverySocket
}

// native is the session a role's lines are posted to: a Claude role started with zmina's settings, past its boot
// grace, whose session has an inbox socket zmina can write. Nil: its lines are typed.
func (s *Shift) native(name string, now time.Time) *registry.Session {
	if s.Peer == nil || s.isCodex(name) {
		return nil
	}

	rs := s.State.Role(name)
	if !rs.Inbound || now.Sub(rs.Started) < s.Crew.BootGraceD() {
		return nil
	}

	sess := s.session(name)
	if sess == nil || !s.Peer.Usable(sess) {
		return nil
	}

	return sess
}

// quiet reports whether a posted line would start a turn rather than join one: the session is idle, or its last turn
// is over while a background shell or agent keeps its status off idle. Never while it shows a dialog.
func (s *Shift) quiet(name string, sess *registry.Session) bool {
	switch sess.Status {
	case "idle":
		return true
	case "waiting":
		return false
	}

	turn, err := s.registryFor(name).Last(sess.SessionID)

	return err == nil && turn.Ended
}

// post posts a line to the role's inbox socket and follows it until its conversation shows it. An error wrapping
// peer.ErrUnavailable means nothing reached the session.
func (s *Shift) post(name string, sess *registry.Session, line string, now time.Time) error {
	id, err := s.Peer.Post(s.Crew.Namespace, sess, line)
	if err != nil {
		return err
	}

	rs := s.State.Role(name)
	rs.Posted = append(rs.Posted, Posted{Line: line, MsgID: id, Session: sess.SessionID, At: now})
	s.log(name, "posted", "", line)

	return nil
}

// followPosted settles the role's posted lines: one its conversation shows is done; one that went to a conversation
// that is over (the role restarted) goes back to the head of its queue; one an idle session never showed within
// postedWait is kept as failed (zmina queue). It reports whether a posted line is still unseen.
func (s *Shift) followPosted(name string, now time.Time) bool {
	rs := s.State.Role(name)
	if len(rs.Posted) == 0 || s.Peer == nil {
		return false
	}

	sess := s.session(name)

	var keep, back []Posted

	for _, p := range rs.Posted {
		seen, err := s.Peer.Delivered(s.Crew.Namespace, p.Session, p.MsgID)

		switch {
		case err == nil && seen:
			s.log(name, "seen", "", p.Line)
		case sess != nil && sess.SessionID != p.Session:
			back = append(back, p)
			s.log(name, "requeued", "", "its conversation ended before it showed: "+p.Line)
		case sess != nil && now.Sub(p.At) >= postedWait && s.quiet(name, sess):
			rs.Failed = append(rs.Failed, Failed{Line: p.Line, Error: postedNever, At: now})
			s.log(name, "type-failed", "", postedNever+": "+p.Line)
			s.deliveryFailure(name, p.Line, postedNever)
		default:
			keep = append(keep, p)
		}
	}

	rs.Posted = keep

	if len(back) > 0 {
		lines := make([]string, 0, len(back)+len(rs.Inbox))
		for _, p := range back {
			lines = append(lines, p.Line)
		}

		rs.Inbox = append(lines, rs.Inbox...)
	}

	return len(keep) > 0
}

// postable reports whether a queued line can go to the inbox socket: a slash command is typed, since a posted one
// arrives as plain text.
func postable(line string) bool { return !strings.HasPrefix(strings.TrimSpace(line), "/") }

// unavailable reports a post that never started.
func unavailable(err error) bool { return errors.Is(err, peer.ErrUnavailable) }
