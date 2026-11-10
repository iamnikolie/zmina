package shift

import (
	"maps"
	"slices"
	"strings"
	"time"
)

// unreadableStore is a store that names the files it cannot read (task.Store, asks.Store).
type unreadableStore interface {
	Unreadable() (map[string]string, error)
}

// reportUnreadable tells the dispatcher, once per file, about a task or ask file that does not read: such a record
// is in no list, so without this it would vanish in silence. A file that reads again is forgotten, so it is told
// again if it breaks again.
func (s *Shift) reportUnreadable(now time.Time) []string {
	var did []string

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

		for _, file := range slices.Sorted(maps.Keys(bad)) {
			key := store.dir + "/" + file
			if _, told := s.State.Unreadable[key]; told {
				continue
			}

			if s.State.Unreadable == nil {
				s.State.Unreadable = map[string]time.Time{}
			}

			s.State.Unreadable[key] = now

			taskID := ""
			if store.dir == "tasks" {
				taskID = strings.TrimSuffix(file, ".json")
			}

			s.log("", "unreadable", taskID, key+": "+bad[file])
			s.addNotes([]string{"zmina: " + key + " does not read (" + bad[file] + "), so it is in no list and " +
				"holds no role; zmina doctor names it; mend the file or move it out of " + store.dir + "/"})
			did = append(did, key+": does not read; told the dispatcher")
		}
	}

	return did
}
