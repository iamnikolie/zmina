package shift

import (
	"fmt"
	"slices"

	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/task"
)

// LiveSession is the id of the conversation the role runs now: "" when it is not up under zmina or has not written a
// session. A delivery not yet submitted to a session (task.Delivery.Session) is the live one's.
func (s *Shift) LiveSession(name string) string {
	if sess := s.session(name); sess != nil {
		return sess.SessionID
	}

	return ""
}

// RebindDelivery moves the delivery of t to session, the conversation the role runs now, when the conversation changed under the
// running role since the line was submitted (its own /clear, a /resume in its pane): it is the role's, so it holds the hand-out
// by definition (ledger.Current, Rebind). The hand-out, its number, its acceptance and the task are as they were; the event is
// "rebound", and nobody is told. It returns t as it is kept now, which the caller writes on from.
func (s *Shift) RebindDelivery(t task.Task, session string) (task.Task, error) {
	d := t.Delivery
	if d == nil || session == "" || d.Session == "" || d.Session == session || s.Tasks == nil {
		return t, nil
	}

	from := d.Session
	d.Session = session

	if err := s.Tasks.Put(t); err != nil {
		d.Session = from

		return t, err
	}

	s.log(t.Role, "rebound", t.ID, fmt.Sprintf("hand-out %d: the conversation of %s changed under it, from %s to %s, which is the one it runs now",
		d.Gen, t.Role, clipRunes(from, 12), clipRunes(session, 12)))

	return t, nil
}

// RecordLate keeps a late report on task t: a session reported on it, or tried to close or change it, after it was
// handed to another (ledger.Current). The report is listed in the task's file, whose state, result, delivery and Updated it
// leaves as they are, and logged as a late-report event; the dispatcher is told once for each caller and hand-out, in a note
// that asks for nothing, however often that caller reports again. The revision of a record is marked as told, so that the
// notice of a result that is ready does not announce it as the task's. t is the task as read under the lock. It returns
// whether the report was new: one the task lists already (the same caller, hand-out and revision) is neither logged nor
// told again.
func (s *Shift) RecordLate(t task.Task, l task.Late) (bool, error) {
	if l.At.IsZero() {
		l.At = s.Now().UTC()
	}

	if slices.ContainsFunc(t.Late, l.Same) {
		return false, nil
	}

	// Told once for each caller on a hand-out: a revision of its own or another thing it tried is no news again.
	already := slices.ContainsFunc(t.Late, func(o task.Late) bool { return o.By == l.By && o.Session == l.Session && o.Gen == l.Gen })

	t.Late = append(t.Late, l)
	if len(t.Late) > task.LateMax {
		t.Late = t.Late[len(t.Late)-task.LateMax:]
	}

	if s.Tasks == nil {
		return false, nil
	}

	if err := s.Tasks.Put(t); err != nil {
		return false, err
	}

	s.log(l.By, "late-report", t.ID, lateDetail(t, l))

	if l.What == "result" && l.Revision != "" && (t.Result == nil || t.Result.Revision != l.Revision) {
		if s.State.RecordNotified == nil {
			s.State.RecordNotified = map[string]bool{}
		}

		s.State.RecordNotified["result/"+t.ID+"/"+l.Revision] = true
	}

	if already {
		return true, nil
	}

	if _, _, err := s.note(l.By, lateText(t, l), false, true); err != nil && s.Warn != nil {
		s.Warn(err)
	}

	return true, nil
}

// lateWhat says what the late report did and what became of it.
func lateWhat(l task.Late) string {
	if l.Revision != "" {
		return fmt.Sprintf("its %s %s was kept as a revision, not the task's %s", l.What, record.Short(nil, l.Revision), l.What)
	}

	return "its task " + l.What + " was refused"
}

// lateDetail is what the late-report event says.
func lateDetail(t task.Task, l task.Late) string {
	d := fmt.Sprintf("%s; hand-out %d is current", lateWhat(l), l.Gen)
	if l.Session != "" {
		d += "; session " + clipRunes(l.Session, 12)
	}

	return d + "; " + t.Role + " holds it"
}

// lateText is the dispatcher's note: one line, once for a caller on a hand-out.
func lateText(t task.Task, l task.Late) string {
	if l.Other {
		return fmt.Sprintf("%s is handed to %s (hand-out %d) and %s, which does not hold it, tried to report on it: %s; the task is unchanged (zmina task show %s)",
			t.ID, t.Role, l.Gen, l.By, lateWhat(l), t.ID)
	}

	return fmt.Sprintf("%s was handed out again (hand-out %d, to %s) and a session that held it before reported on it: %s; the task is unchanged "+
		"(zmina task show %s; if that session is the one that should hold it, zmina task handoff %s --queue hands it to the live one)",
		t.ID, l.Gen, t.Role, lateWhat(l), t.ID, t.ID)
}
