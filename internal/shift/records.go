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
		line := resultTold
		if t, err := s.Tasks.Get(id); err == nil {
			srcs = t.RecordSources("result")
			line = s.resultLine(t)
		}
		revs := record.Revisions(s.Crew.StateDir(), "result", srcs)
		for _, ref := range list {
			key := "result/" + id + "/" + ref.Revision
			if s.State.RecordNotified[key] {
				continue
			}
			if line == resultSilent {
				s.State.RecordNotified[key] = true
				continue
			}
			// Held for the close, but not for ever: a worker that never closes is heard of in the end.
			if line == resultHeld && s.Now().Sub(ref.At) < resultHoldMax {
				continue
			}
			d, err := record.Load(s.Crew.StateDir(), "result", id, ref.Revision)
			if err != nil {
				continue
			}
			text := id + resultReady
			if g := resultGist(d); g != "" {
				text += ": " + g
			}
			// The revision is in the notice itself: it is what the queue keeps and what tells two results of one
			// task apart when their gists are the same, and the command opens that one, not the latest.
			text += " (zmina task result " + id + " --revision " + record.Short(revs, ref.Revision) + ")"
			// An info line: it asks for nothing. It is made whatever the role wrote of the task itself: a line of
			// its own that names the task need not be about the result.
			if _, _, err := s.note(d.Role, text, false, true); err != nil {
				return
			}
			s.State.RecordNotified[key] = true
		}
	}
}

// How the dispatcher hears of the results of a task (resultLine).
type resultKind int

const (
	// resultTold: as always, a line for each result.
	resultTold resultKind = iota
	// resultHeld: none yet; the task's close tells of its outcome, or the line goes when the result is resultHoldMax old.
	resultHeld
	// resultSilent: none at all; the loop's own lines tell the outcome.
	resultSilent
)

// resultHoldMax is how long a result's line waits for the task's close when auto_review holds it.
const resultHoldMax = 30 * time.Minute

// reviewApplies reports whether the crew's auto_review reviews t when a role closes it: the key is on (with a review rule to
// pick the reviewer), and t has a worktree and is no review itself.
func (s *Shift) reviewApplies(t task.Task) bool {
	return s.Crew.AutoReviewRounds() > 0 && t.Worktree != "" && t.Review == nil
}

// resultLine is how the dispatcher hears of the results of t. With the crew file's auto_review on, a task with a worktree
// that is being worked on (open: one waiting on the owner's answer is told) is held: the review of it comes next, and the line
// waits for the close. A review task, and a task closed with its review due, are never told (the loop tells how the review
// went, see NoteOnce); any other is told as always, a task closed by hand included. With the key off none of it applies, a
// review task still on the ledger included.
func (s *Shift) resultLine(t task.Task) resultKind {
	if s.Crew.AutoReviewRounds() == 0 {
		return resultTold
	}

	switch {
	case t.Review != nil, t.Closed != nil && t.Closed.Review != nil:
		return resultSilent
	case t.State == task.Open && s.reviewApplies(t):
		return resultHeld
	}

	return resultTold
}

// NoteOnce is Note, or NoteQuiet, told once for each key (RecordNotified); it reports whether this call queued it. A line the
// queue refuses is tried again by the next call.
func (s *Shift) NoteOnce(key, from, text string, quiet bool) (bool, error) {
	if s.State.RecordNotified[key] {
		return false, nil
	}

	var err error

	if quiet {
		_, err = s.NoteQuiet(from, text)
	} else {
		_, err = s.Note(from, text, false)
	}

	if err != nil {
		return false, err
	}

	s.MarkNoted(key)

	return true, nil
}

// MarkNoted records that the line for key needs no telling: what it is about was dealt with another way (NoteOnce).
func (s *Shift) MarkNoted(key string) {
	if s.State.RecordNotified == nil {
		s.State.RecordNotified = map[string]bool{}
	}

	s.State.RecordNotified[key] = true
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

		// A review the crew's auto_review runs, and the task it is of, have lines of the loop's own (NoteOnce): not this one. With
		// the key off a review still on the ledger closes as any task does, and its line says the verdict.
		if (t.Review != nil || c.Review != nil) && s.Crew.AutoReviewRounds() > 0 {
			s.State.RecordNotified[key] = true

			continue
		}

		if t.Result != nil && !t.Result.At.Before(assigned(t)) {
			s.State.RecordNotified[key] = true

			continue
		}

		text := t.ID + " " + string(c.State)
		if t.Review != nil && t.Review.Verdict != "" {
			text += " (" + t.Review.Verdict + ")"
		}
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
