package shift

import (
	"fmt"
	"github.com/iamnikolie/zmina/internal/record"
	"sort"
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
			text := fmt.Sprintf("%s result %s ready; read zmina task result %s --revision %s (worker-reported)", id, ref.Revision[:12], id, ref.Revision)
			if _, err := s.Note(d.Role, text, false); err != nil {
				return
			}
			s.State.RecordNotified[key] = true
		}
	}
}
