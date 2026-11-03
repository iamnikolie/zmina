package shift

import (
	"crypto/rand"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/peer"
	"github.com/iamnikolie/zmina/internal/registry"
)

// postedWait is how long a line posted to a role's inbox socket may go unseen in its conversation, the role sitting
// idle, before it is kept as failed: the session held or dropped it.
const postedWait = 10 * time.Minute

// postedNever is the failure of a posted line the conversation never showed.
const postedNever = "posted to its inbox socket, never seen in its conversation (held or dropped); inspect before retry"

// channel is a role's agent's own channel and the place its deliveries are looked up in: a Claude session's inbox
// socket (the namespace), a Codex bridge's inbox (its directory). Nil: lines are typed.
func (s *Shift) channel(r *crew.Role) (Peer, string) {
	if r.IsCodex() {
		return s.CodexInbox, codex.InboxDir(filepath.Join(s.Crew.StateDir(), "codex"), r.Session)
	}

	return s.Peer, s.Crew.Namespace
}

// peerFor is channel by role name.
func (s *Shift) peerFor(name string) (Peer, string) {
	r, err := s.Role(name)
	if err != nil {
		return nil, ""
	}

	return s.channel(r)
}

// inbound reports whether a role's lines go to its own channel: a Claude role starts with zmina's settings, and
// either is told so at boot.
func (s *Shift) inbound(r *crew.Role) bool {
	p, _ := s.channel(r)

	return p != nil && s.Crew.DeliveryFor(r) == crew.DeliveryNative
}

// native is the session a role's lines are posted to: a role started on its own channel, its boot over (booted), whose
// session takes posts (a Claude inbox socket zmina can write, a bridge with an inbox). Nil: its lines are typed.
func (s *Shift) native(name string, now time.Time) *registry.Session {
	p, _ := s.peerFor(name)
	if p == nil {
		return nil
	}

	rs := s.State.Role(name)
	if !rs.Inbound || rs.Tag == "" || !s.booted(name, now) {
		return nil
	}

	sess := s.session(name)
	if sess == nil || !p.Usable(sess) {
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

// post posts a line to the role's own channel, behind this start's tag, and follows it until its conversation shows
// it. A post whose write broke after it started is followed the same way (the session may have it), never posted
// again; an error wrapping peer.ErrUnavailable means nothing reached the session.
func (s *Shift) post(name string, sess *registry.Session, line string, now time.Time) error {
	p, ns := s.peerFor(name)
	rs := s.State.Role(name)

	id, err := p.Post(ns, sess, tagMark(rs.Tag)+" "+line)

	var broke *peer.Committed
	if err != nil && (!errors.As(err, &broke) || id == "") {
		return err
	}

	rs.Posted = append(rs.Posted, Posted{Line: line, MsgID: id, Session: sess.SessionID, At: now})

	if broke != nil {
		s.log(name, "post-broken", "", broke.Error()+"; followed until it shows or "+span(postedWait)+" pass: "+line)
	} else {
		s.log(name, "posted", "", line)
	}

	return nil
}

// tagMark is what opens every line posted to a role: zmina and the tag of the role's start.
func tagMark(tag string) string { return "[zmina:" + tag + "]" }

// tag is the tag of a role's start, made the first time its boot line asks for it (start clears it, so each start
// has its own).
func (s *Shift) tag(name string) string {
	rs := s.State.Role(name)
	if rs.Tag == "" {
		rs.Tag = newTag()
	}

	return rs.Tag
}

// newTag is twelve random characters of base32.
func newTag() string { return strings.ToLower(rand.Text()[:12]) }

// followPosted settles the role's posted lines: one its conversation shows is done; one that went to a conversation
// that is over (the role restarted) goes back to the head of its queue; one an idle session never showed within
// postedWait is kept as failed (zmina queue). It reports whether a posted line is still unseen.
func (s *Shift) followPosted(name string, now time.Time) bool {
	rs := s.State.Role(name)
	ch, ns := s.peerFor(name)

	if len(rs.Posted) == 0 || ch == nil {
		return false
	}

	sess := s.session(name)

	var keep, back []Posted

	for _, p := range rs.Posted {
		seen, err := ch.Delivered(ns, p.Session, p.MsgID)

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

// TurnOver reports whether a role's session is the one a Stop hook ran for and its turn is over (status idle), so a
// line posted now starts a turn of its own.
func (s *Shift) TurnOver(name, sessionID string) bool {
	sess := s.session(name)

	return sess != nil && sess.SessionID == sessionID && sess.Status == "idle"
}

// TurnEnd posts what waits for a role whose turn just ended (its Stop hook), the way a beat would post it: only to
// its own channel, only past its boot and between turns, each line once the one before it showed, the dispatcher's
// signals before its queue. Nothing is typed: a slash command, or a role whose lines are typed, waits for the beat.
// It returns the lines for the log.
func (s *Shift) TurnEnd(name, sessionID string) []string {
	now := s.Now()

	sess := s.native(name, now)
	if sess == nil || sess.SessionID != sessionID {
		return nil
	}

	if name == s.Crew.Dispatcher {
		if line := s.deliver(now); line != "" {
			return []string{"told the dispatcher: " + line}
		}
	}

	// Pruned first, as deliverInbox prunes: an obsolete task packet must not leave a slash command at the head
	// for deliverInbox to type.
	s.pruneTaskInbox(name)

	rs := s.State.Role(name)
	if rs.Sent != nil || len(rs.Inbox) == 0 || !postable(rs.Inbox[0]) {
		return nil
	}

	if line := s.deliverInbox(name, now); line != "" {
		return []string{line}
	}

	return nil
}
