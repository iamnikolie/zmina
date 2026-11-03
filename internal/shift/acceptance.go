package shift

import (
	"fmt"
	"github.com/iamnikolie/zmina/internal/task"
	"slices"
	"time"
)

func (s *Shift) submittedTask(name, line string) {
	if s.Tasks == nil {
		return
	}
	sess := s.session(name)
	if sess == nil || sess.SessionID == "" {
		return
	}
	for _, t := range s.Tasks.Active() {
		if t.Delivery == nil || t.Role != name || t.Delivery.Line != line {
			continue
		}
		t.Delivery.SentAt = s.Now().UTC()
		t.Delivery.AlertedAt = time.Time{}
		t.Delivery.Session = sess.SessionID
		// A fresh submission supersedes a legacy marker that lacked submission identity.
		delete(s.State.AcceptanceNotified, t.ID+"/"+t.Delivery.ID)
		if err := s.Tasks.Put(t); err != nil && s.Warn != nil {
			s.Warn(err)
		}
	}
}

// AcceptTask binds explicit acceptance to the exact task packet, assigned role and live runtime session.
func (s *Shift) AcceptTask(id, handoff string) error {
	t, err := s.Tasks.Get(id)
	if err != nil {
		return err
	}
	d := t.Delivery
	if !t.State.Active() || d == nil || d.ID != handoff {
		return fmt.Errorf("%s: handoff missing or superseded; refresh with zmina task handoff %s --queue", t.ID, t.ID)
	}
	if s.By != t.Role || d.Role != t.Role {
		return fmt.Errorf("%s: only assigned role %s can accept this handoff", t.ID, t.Role)
	}
	sess := s.session(t.Role)
	r, err := s.Role(t.Role)
	if err != nil {
		return err
	}
	runtime, home := s.runtimeScope(r)
	if sess == nil || sess.SessionID == "" || d.Session == "" || sess.SessionID != d.Session || d.Runtime != runtime || d.Home != home || d.SentAt.IsZero() {
		return fmt.Errorf("%s: handoff is not confirmed for this live session; refresh with zmina task handoff %s --queue", t.ID, t.ID)
	}
	if !t.Updated.Equal(d.TaskUpdated) {
		return fmt.Errorf("%s: task changed; refresh with zmina task handoff %s --queue", t.ID, t.ID)
	}
	if !d.AcceptedAt.IsZero() {
		return nil
	}
	d.AcceptedAt = s.Now().UTC()
	if err := s.Tasks.Put(t); err != nil {
		return err
	}
	s.log(t.Role, "accepted", t.ID, handoff)
	return nil
}

// acceptanceDue requires a current submission, never merely a queued assignment. Busy or unavailable seats
// cannot act on an acceptance reminder; recheck the same conditions before delivering a queued notice.
func (s *Shift) acceptanceDue(t task.Task) bool {
	d := t.Delivery
	if t.State != task.Open || d == nil || d.Role != t.Role || !d.AcceptedAt.IsZero() || d.SentAt.IsZero() ||
		s.Now().Sub(d.SentAt) < 5*time.Minute || !t.Updated.Equal(d.TaskUpdated) || !s.State.Stopped.IsZero() {
		return false
	}
	r, err := s.Role(t.Role)
	if err != nil {
		return false
	}
	rs := s.State.Role(t.Role)
	if rs.Held || !rs.Downing.IsZero() || !rs.Releasing.IsZero() || !s.Asleep(t.Role).IsZero() ||
		rs.Started.After(d.SentAt) || slices.Contains(rs.Inbox, d.Line) {
		return false
	}
	for _, f := range rs.Failed {
		if f.Line == d.Line {
			return false
		}
	}
	sess := s.session(t.Role)
	if sess == nil || sess.Status != "idle" || d.Session == "" || sess.SessionID != d.Session {
		return false
	}
	runtime, home := s.runtimeScope(r)
	if d.Runtime != runtime || d.Home != home {
		return false
	}
	return true
}

// The durable marker includes the actual submission, so resubmitting identical packet content starts a new
// alert without relying on a separate task-file timestamp write and state commit being atomic.
func acceptanceAlertKey(t task.Task) string {
	d := t.Delivery
	return fmt.Sprintf("%s/%s/%q/%s", t.ID, d.ID, d.Session, d.SentAt.UTC().Format(time.RFC3339Nano))
}

func (s *Shift) acceptanceAlertCommitted(t task.Task) bool {
	d := t.Delivery
	for _, key := range []string{acceptanceAlertKey(t), t.ID + "/" + d.ID} {
		if queuedAt, ok := s.State.AcceptanceNotified[key]; ok && queuedAt.Equal(d.QueuedAt) {
			return true
		}
	}
	return false
}

func (s *Shift) acceptanceAlerts() {
	if s.Tasks == nil {
		return
	}
	for _, t := range s.Tasks.Active() {
		if !s.acceptanceDue(t) {
			continue
		}
		d := t.Delivery
		key := acceptanceAlertKey(t)
		if s.acceptanceAlertCommitted(t) {
			continue
		}
		text := fmt.Sprintf("%s: %s has no acceptance after 5 min: submitted to live idle seat, worker has not accepted the task; inspect task show and queue ls", t.Role, t.ID)
		s.addNotes([]string{text})
		meta := s.State.NoticeMeta[text]
		meta.Kind, meta.Role, meta.Task, meta.Handoff = "acceptance", t.Role, t.ID, d.ID
		meta.QueuedAt = d.QueuedAt
		meta.Session, meta.Since = d.Session, d.SentAt.UnixMilli()
		s.State.NoticeMeta[text] = meta
		if s.State.AcceptanceNotified == nil {
			s.State.AcceptanceNotified = map[string]time.Time{}
		}
		s.State.AcceptanceNotified[key] = d.QueuedAt
		// Informational only: this separate task write is not proof of a committed notice.
		d.AlertedAt = s.Now().UTC()
		if err := s.Tasks.Put(t); err != nil && s.Warn != nil {
			s.Warn(err)
		}
	}
}

// A withdrawn pending notice was not delivered. It must not spend the one alert for a later eligible seat.
func (s *Shift) withdrawAcceptanceNotice(n NoticeMeta) {
	if n.Kind != "acceptance" || s.Tasks == nil {
		return
	}
	t, err := s.Tasks.Get(n.Task)
	if err != nil || t.State != task.Open || t.Delivery == nil || t.Delivery.ID != n.Handoff || !t.Delivery.AcceptedAt.IsZero() {
		return
	}
	d := t.Delivery
	if !n.QueuedAt.IsZero() && !n.QueuedAt.Equal(d.QueuedAt) {
		return
	}
	if d.Session != n.Session || d.SentAt.UnixMilli() != n.Since {
		if n.Session != "" || n.Since != 0 {
			return // Another submitted attempt owns the current alert flag.
		}
		// Legacy pending notices had no session/time binding. Retire their old flag unless a newly bound
		// notice already owns it, so upgrading neither repeats nor permanently swallows the valid alert.
		for _, current := range s.State.NoticeMeta {
			if current.Kind == "acceptance" && current.Task == n.Task && current.Handoff == d.ID &&
				current.Session == d.Session && current.Since == d.SentAt.UnixMilli() &&
				(current.QueuedAt.IsZero() || current.QueuedAt.Equal(d.QueuedAt)) {
				return
			}
		}
	}
	// This withdrawal and marker removal commit with removal of the pending queue entry.
	for _, key := range []string{acceptanceAlertKey(t), t.ID + "/" + d.ID} {
		if queuedAt, ok := s.State.AcceptanceNotified[key]; ok && queuedAt.Equal(d.QueuedAt) {
			delete(s.State.AcceptanceNotified, key)
		}
	}
	d.AlertedAt = time.Time{}
	if err := s.Tasks.Put(t); err != nil && s.Warn != nil {
		s.Warn(err)
	}
}
