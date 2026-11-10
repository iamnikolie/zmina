package fsx_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/lesson"
	"github.com/iamnikolie/zmina/internal/task"
)

// recordStore is one of the stores that keeps a file per record and numbers a new one above every file there.
type recordStore struct {
	first string // the id the first record gets
	add   func(dir string) (string, error)
	scan  func(dir string) (problems []string, err error)
}

var now = time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)

func recordStores() map[string]recordStore {
	return map[string]recordStore{
		"task": {"T-1",
			func(dir string) (string, error) {
				t, err := (&task.Store{Dir: dir}).Add(task.Task{Role: "dev", Asked: "fix the login"}, now)
				return t.ID, err
			},
			func(dir string) ([]string, error) { _, p, err := (&task.Store{Dir: dir}).Scan(); return p, err }},
		"asks": {"A-1",
			func(dir string) (string, error) {
				a, err := (&asks.Store{Dir: dir}).Add(asks.Ask{Kind: asks.Check, Text: "check the login"}, now)
				return a.ID, err
			},
			func(dir string) ([]string, error) { _, p, err := (&asks.Store{Dir: dir}).Scan(); return p, err }},
		"later": {"L-1",
			func(dir string) (string, error) {
				it, err := (&later.Store{Dir: dir}).Add(later.Item{Text: "the login, later"}, now)
				return it.ID, err
			},
			func(dir string) ([]string, error) { _, p, err := (&later.Store{Dir: dir}).Scan(); return p, err }},
		"alarm": {"R-1",
			func(dir string) (string, error) {
				a, err := (&alarm.Store{Dir: dir}).Add(alarm.Alarm{Kind: alarm.Remind, At: now.Add(time.Hour), Text: "look"}, now)
				return a.ID, err
			},
			func(dir string) ([]string, error) { _, p, err := (&alarm.Store{Dir: dir}).Scan(); return p, err }},
		"lesson": {"LS-1",
			func(dir string) (string, error) {
				l, err := (&lesson.Store{Dir: dir}).Add(lesson.Lesson{Rule: "quote tmux targets"}, now)
				return l.ID, err
			},
			func(dir string) ([]string, error) { _, err := (&lesson.Store{Dir: dir}).All(); return nil, err }},
	}
}

// TestHelperAdd is the process the store kill test stops between a new record's write and its rename.
func TestHelperAdd(t *testing.T) {
	name, dir := os.Getenv("FSX_ADD_STORE"), os.Getenv("FSX_ADD_DIR")
	if name == "" {
		t.Skip("run by TestAddKilledMidWriteLeavesNoRecord")
	}

	fsx.BeforeCommit = func(string, string) { os.Exit(7) }

	id, err := recordStores()[name].add(dir)
	t.Fatalf("%s: added %s (%v) without stopping before the rename", name, id, err)
}

// A process killed while it adds a record leaves no file under the record's name: no empty file that reads as
// damaged, burns the id and drops out of the store's lists. The next add takes the same id.
func TestAddKilledMidWriteLeavesNoRecord(t *testing.T) {
	for name, s := range recordStores() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()

			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperAdd$")
			cmd.Env = append(os.Environ(), "FSX_ADD_STORE="+name, "FSX_ADD_DIR="+dir)

			var exit *exec.ExitError
			if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 7 {
				t.Fatalf("helper: %v (the store does not write through fsx)", err)
			}

			if _, err := os.Stat(filepath.Join(dir, s.first+".json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s.json is there after a kill mid write (%v)", s.first, err)
			}

			if problems, err := s.scan(dir); err != nil || len(problems) > 0 {
				t.Fatalf("scan after the kill: %v, %v", problems, err)
			}

			id, err := s.add(dir)
			if err != nil || id != s.first {
				t.Fatalf("next add: %s, %v; want %s", id, err, s.first)
			}
		})
	}
}
