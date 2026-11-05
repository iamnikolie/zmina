package shift

import (
	"sort"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/task"
)

// NotifyRecords reconciles immutable revisions into the durable dispatcher queue.
func (s *Shift) NotifyRecords() {
	refs, err := record.All(s.Crew.StateDir(), "result")
	if err != nil {
		if s.Warn != nil {
			s.Warn(err)
		}
	}
	if s.State.RecordNotified == nil {
		s.State.RecordNotified = map[string]bool{}
	}
	ids := make([]string, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		list := refs[id]
		// The short form stays apart from every revision task result <id> --revision looks among.
		srcs := []string{id}
		if t, err := s.Tasks.Get(id); err == nil {
			srcs = t.RecordSources("result")
		}
		revs := record.Revisions(s.Crew.StateDir(), "result", srcs)
		for _, ref := range list {
			key := "result/" + id + "/" + ref.Revision
			if s.State.RecordNotified[key] {
				continue
			}
			d, err := record.Load(s.Crew.StateDir(), "result", id, ref.Revision)
			if err != nil {
				continue
			}
			text := id + " result ready (worker-reported)"
			if g := resultGist(d); g != "" {
				text += ": " + g
			}
			// The revision is in the notice itself: it is what the queue keeps and what tells two results of one
			// task apart when their gists are the same, and the command opens that one, not the latest.
			text += " (zmina task result " + id + " --revision " + record.Short(revs, ref.Revision) + ")"
			if _, err := s.Note(d.Role, text, false); err != nil {
				return
			}
			s.State.RecordNotified[key] = true
		}
	}
}

// resultGist is a result's first sentence for its notice: the summary section (the first section when the shape has
// none), one line, at most 120 characters. The whole result stays one command away.
func resultGist(d record.Document) string {
	if len(d.Shape.Sections) == 0 {
		return ""
	}

	name := d.Shape.Sections[0].Name
	for _, sec := range d.Shape.Sections {
		if sec.Name == "summary" {
			name = sec.Name
		}
	}

	g := strings.Join(strings.Fields(d.Values[name]), " ")
	if i := strings.Index(g, ". "); i > 0 {
		g = g[:i+1]
	}

	if r := []rune(g); len(r) > 120 {
		g = string(r[:119]) + "…"
	}

	return g
}

// NotifyClosed reconciles the tasks a role other than the dispatcher closed into the durable dispatcher queue: one
// signal each closure, with its note (`T-1 done: <note>`), unless a result of the same assignment tells it already
// (a result saved since the task's packet was accepted, or since it was made). A closure is noted once
// (RecordNotified "closed/<id>/<time>"); one the queue refuses is tried again the next beat. Tasks closed by the
// owner's hand, by the dispatcher, or by a zmina that did not keep who closed them are left alone.
func (s *Shift) NotifyClosed() {
	if s.Tasks == nil {
		return
	}

	tasks, err := s.Tasks.All()
	if err != nil {
		if s.Warn != nil {
			s.Warn(err)
		}

		return
	}

	for _, t := range tasks {
		c := t.Closed
		if c == nil || t.State.Active() || c.By == "" || c.By == "hand" || c.By == s.Crew.Dispatcher {
			continue
		}

		key := "closed/" + t.ID + "/" + c.At.UTC().Format(time.RFC3339Nano)
		if s.State.RecordNotified[key] {
			continue
		}

		if s.State.RecordNotified == nil {
			s.State.RecordNotified = map[string]bool{}
		}

		if t.Result != nil && !t.Result.At.Before(assigned(t)) {
			s.State.RecordNotified[key] = true

			continue
		}

		text := t.ID + " " + string(c.State)
		if note := strings.Join(strings.Fields(c.Note), " "); note != "" {
			text += ": " + note
		} else {
			text += ", no note"
		}

		if _, err := s.Note(c.By, text+" (zmina task show "+t.ID+")", false); err != nil {
			return
		}

		s.State.RecordNotified[key] = true
	}
}

// assigned is when a task's current assignment began: its packet accepted, sent or queued, else the task made.
func assigned(t task.Task) time.Time {
	if d := t.Delivery; d != nil {
		for _, at := range []time.Time{d.AcceptedAt, d.SentAt, d.QueuedAt} {
			if !at.IsZero() {
				return at
			}
		}
	}

	return t.Created
}
