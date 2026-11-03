package shift

import (
	"sort"
	"strings"

	"github.com/iamnikolie/zmina/internal/record"
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
			text += " (zmina task result " + id + ")"
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
