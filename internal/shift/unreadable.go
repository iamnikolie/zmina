package shift

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// unreadableStore is a store that names the files it cannot read (task.Store, asks.Store).
type unreadableStore interface {
	Unreadable() (map[string]string, error)
}

// unreadableNamed is how many files one note names; it counts the rest (doctor and the log name every one).
const unreadableNamed = 10

// reportUnreadable tells the dispatcher about task and ask files that do not read: such a record is in no list, so
// without this it would vanish in silence. Each breakage is logged once and told once, in one note per beat for all
// files found broken since the last; the note goes to the dispatcher's signal queue, which is never trimmed. A
// note the queue refuses (it is full) leaves the files untold, to be told on a later beat. A file that reads again
// is forgotten, so it is told again if it breaks again.
func (s *Shift) reportUnreadable(now time.Time) []string {
	found := map[string]string{}

	for _, store := range []struct {
		dir string
		of  any
	}{{"tasks", s.Tasks}, {"asks", s.Asks}} {
		u, ok := store.of.(unreadableStore)
		if !ok {
			continue
		}

		bad, err := u.Unreadable()
		if err != nil {
			continue
		}

		for key := range s.State.Unreadable {
			if dir, file, _ := strings.Cut(key, "/"); dir == store.dir && bad[file] == "" {
				delete(s.State.Unreadable, key)
			}
		}

		for file, why := range bad {
			found[store.dir+"/"+file] = why
		}
	}

	var fresh []string

	for _, key := range slices.Sorted(maps.Keys(found)) {
		at, logged := s.State.Unreadable[key]
		if !at.IsZero() {
			continue
		}

		fresh = append(fresh, key)

		if !logged {
			if s.State.Unreadable == nil {
				s.State.Unreadable = map[string]time.Time{}
			}

			// Logged now, told when the note is taken (a zero time until then).
			s.State.Unreadable[key] = time.Time{}

			taskID := ""
			if dir, file, _ := strings.Cut(key, "/"); dir == "tasks" {
				taskID = strings.TrimSuffix(file, ".json")
			}

			s.log("", "unreadable", taskID, key+": "+found[key])
		}
	}

	if len(fresh) == 0 {
		return nil
	}

	named := strings.Join(fresh[:min(len(fresh), unreadableNamed)], ", ")
	if more := len(fresh) - unreadableNamed; more > 0 {
		named += fmt.Sprintf(" and %d more", more)
	}

	text := "files that do not read, so they are in no list and hold no role: " + named + "; zmina doctor and " +
		"zmina log name each with why; mend each file or move it out of its directory"
	if _, _, err := s.note("zmina", text, false, false); err != nil {
		return []string{"unreadable files not told yet (" + err.Error() + "): " + strings.Join(fresh, ", ")}
	}

	for _, key := range fresh {
		s.State.Unreadable[key] = now
	}

	return []string{strings.Join(fresh, ", ") + ": do not read; told the dispatcher"}
}
