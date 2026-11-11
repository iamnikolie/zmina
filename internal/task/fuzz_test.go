package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A task file is read by every command and every beat, and a handoff packet carries it: a file that is hand-edited, cut
// short or written by another zmina must read or be refused, never stop the reader. Run it for a while with
//
//	go test -fuzz=FuzzReadTask -fuzztime=30s -run '^$' ./internal/task
//
// Plain `go test` runs the seeds below and every input kept in testdata/fuzz as an ordinary test.

func FuzzReadTask(f *testing.F) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	full, _ := json.Marshal(Task{ID: "T-1", Role: "dev", Asked: "fix the login", State: Open, Created: now, Updated: now,
		Note: "half done", Criteria: []string{"tests pass"}, Delivery: &Delivery{AcceptedAt: now}, Closed: &Closure{},
		Conversations: []Conversation{{}}, Route: &Route{}})

	for _, s := range []string{string(full), "", "{}", "null", "[]", `{"id":"T-1"}`, `{"id":"T-2","state":"waiting-alex"}`,
		`{"id":"T-1","state":"parked","created":"not a time"}`, `{"id":"T-1","delivery":null,"closed":null}`,
		`{"id":"T-1","criteria":[1,2]}`, "\xff\xfe"} {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		dir := t.TempDir()
		path := filepath.Join(dir, "T-1.json")

		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Skip(err)
		}

		task, err := readTask(path)
		if err == nil {
			if task.ID != "T-1" {
				t.Fatalf("a task read from T-1.json says it is %q", task.ID)
			}

			if _, err := json.Marshal(task); err != nil {
				t.Fatalf("a task that was read does not encode: %v", err)
			}

			_ = task.State.Active()
			_ = task.State.Known()
		}

		s := &Store{Dir: dir}
		tasks, problems, err := s.Scan()

		if err != nil {
			t.Fatalf("a scan of one file: %v", err)
		}

		if len(tasks)+len(problems) == 0 {
			t.Fatalf("the file is neither a task nor a problem")
		}

		_, _ = s.Unreadable()
		_ = s.Active()
	})
}
