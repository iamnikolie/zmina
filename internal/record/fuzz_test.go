package record

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
)

// Results and checkpoints are written by a role and read back by the dispatcher: what a role wrote, in any bytes, is
// saved and read back the same, and a file that is not a record is refused. Run one for a while with
//
//	go test -fuzz=FuzzSaveLoad -fuzztime=30s -run '^$' ./internal/record
//
// Plain `go test` runs the seeds below and every input kept in testdata/fuzz as an ordinary test.

var fuzzShape = crew.HandoffShape{Sections: []crew.Section{{Name: "summary"}, {Name: "next_step"}}}

func FuzzSaveLoad(f *testing.F) {
	f.Add(false, "T-1", "dev", "dev", "Shipped.", "none")
	f.Add(true, "T-2", "designer", "dispatcher", "line one\nline two\ttab", "")
	f.Add(false, "T-1", "", "", "\xff\xfe invalid utf-8", " ")
	f.Add(false, "t-1", "dev", "dev", "bad task id", "x")
	f.Add(true, "T-1", "dev", "dev", strings.Repeat("long ", 2000), "x")

	f.Fuzz(func(t *testing.T, checkpoint bool, task, role, by, summary, next string) {
		kind := "result"
		if checkpoint {
			kind = "checkpoint"
		}

		dir := t.TempDir()
		at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		doc := Document{Version: 1, Kind: kind, Task: task, Role: role, By: by, At: at, Shape: fuzzShape,
			Values: map[string]string{"summary": summary, "next_step": next}}

		ref, err := Save(dir, doc)
		if err != nil {
			return
		}

		got, err := Load(dir, kind, task, ref.Revision)
		if err != nil {
			t.Fatalf("a saved record does not load: %v", err)
		}

		if got.Task != task || got.Kind != kind || !got.At.Equal(at) {
			t.Fatalf("a record read back is another: %+v", got)
		}

		if again, err := Save(dir, doc); err != nil || again.Revision != ref.Revision {
			t.Fatalf("the same record saved again: %v, %+v", err, again)
		}
	})
}

func FuzzLoad(f *testing.F) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	seed, _ := json.Marshal(Document{Version: 1, Kind: "result", Task: "T-1", Role: "dev", By: "dev", At: at, Shape: fuzzShape,
		Values: map[string]string{"summary": "x", "next_step": "y"}})

	for _, s := range []string{string(seed), "", "{}", "null", "[]", `{"task":"T-1","kind":"result"}`, `{"task":"T-1","kind":"result","values":null}`,
		`{"task":"T-1","kind":"result","at":"x"}`} {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		dir := t.TempDir()
		rev := strings.Repeat("ab", 32)
		p := filepath.Join(dir, "results", "T-1", rev+".json")

		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Skip(err)
		}

		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Skip(err)
		}

		if doc, err := Load(dir, "result", "T-1", rev); err == nil && (doc.Task != "T-1" || doc.Kind != "result") {
			t.Fatalf("a record accepted under the name of another: %+v", doc)
		}

		_ = Revisions(dir, "result", []string{"T-1", "T-1", "", "../x"})
		_, _ = All(dir, "result")
		_, _, _ = Find(dir, "result", []string{"T-1"}, rev[:8])
	})
}
