package handoff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/task"
)

// A packet is built from a task whose words came from anyone: whatever they hold, the packet is one JSON file inside
// the handoffs directory of its task, or it is refused. A task id that names a path never leaves the directory. Run it
// for a while with
//
//	go test -fuzz=FuzzWritePacket -fuzztime=30s -run '^$' ./internal/handoff
//
// Plain `go test` runs the seeds below and every input kept in testdata/fuzz as an ordinary test.

func FuzzWritePacket(f *testing.F) {
	f.Add("T-1", "dev", "First line\nSecond line", "a note", "do it")
	f.Add("T-12", "", "", "", "")
	f.Add("../escape", "dev", "x", "x", "x")
	f.Add("T-1/../T-2", "dev", "x", "x", "x")
	f.Add("/abs/T-1", "dev", "x", "x", "x")
	f.Add("T-1", "dev", "\xff\xfe invalid", " \x00", "\r\n")
	f.Add("", "dev", "x", "x", "x")
	f.Add("T-1\x00", "dev", "x", "x", "x")

	f.Fuzz(func(t *testing.T, id, role, asked, note, instructions string) {
		dir := t.TempDir()
		p := Packet{Version: 1, Role: role, Instructions: instructions,
			Shape: crew.HandoffShape{Sections: []crew.Section{{Name: "summary", Guidance: note}}},
			Task:  task.Task{ID: id, Role: role, Asked: asked, Note: note, Criteria: []string{asked}}}

		path, err := Write(dir, p)
		if err != nil {
			return
		}

		want := filepath.Join(dir, "handoffs", id)
		if filepath.Dir(path) != want && !strings.HasPrefix(path, want+string(filepath.Separator)) {
			// The temporary directory may be reached through a link (macOS /var): compare resolved paths.
			a, _ := filepath.EvalSymlinks(filepath.Dir(path))
			b, _ := filepath.EvalSymlinks(want)

			if a != b {
				t.Fatalf("task id %q wrote %s, outside %s", id, path, want)
			}
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the file a packet was written to: %v", err)
		}

		var got Packet
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("a packet is not JSON: %v", err)
		}

		if got.Task.ID != id {
			t.Fatalf("a packet for %q says it is for %q", id, got.Task.ID)
		}

		again, err := Write(dir, p)
		if err != nil || again != path {
			t.Fatalf("the same packet again: %v, %s and %s", err, again, path)
		}
	})
}
