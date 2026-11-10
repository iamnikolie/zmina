package fsx_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/iamnikolie/zmina/internal/chat"
	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/handoff"
	"github.com/iamnikolie/zmina/internal/inbox"
	"github.com/iamnikolie/zmina/internal/lesson"
	"github.com/iamnikolie/zmina/internal/message"
	"github.com/iamnikolie/zmina/internal/recovery"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

// Every kept file is written through fsx, so each one is synced with its directory and never seen half written.
func TestStoresWriteThroughFsx(t *testing.T) {
	writers := map[string]func(dir string) error{
		"state": func(dir string) error {
			l, err := shift.Lock(dir)
			if err != nil {
				return err
			}

			return l.Save()
		},
		"inbox": func(dir string) error {
			return (&inbox.Store{Dir: dir}).Put(inbox.Entry{ID: mustInboxID(t), From: "dispatcher"})
		},
		"codex session": func(dir string) error {
			return (&codex.Registry{Dir: dir}).Save(codex.Record{Session: registry.Session{PID: 42}})
		},
		"codex usage": func(dir string) error { return codex.Registry{Dir: dir}.SaveUsage(codex.Usage{Thread: "thread-1"}) },
		"message": func(dir string) error {
			return message.Store{Dir: dir}.Save(message.Message{Version: 1, ID: strings.Repeat("a", 64)})
		},
		"recovery":      func(dir string) error { return recovery.Save(dir, recovery.State{Version: 1}) },
		"lessons state": func(dir string) error { return lesson.SaveState(dir, lesson.RunState{}) },
		"handoff": func(dir string) error {
			_, err := handoff.Write(dir, handoff.Packet{Task: task.Task{ID: "T-1"}})
			return err
		},
		"chat upload": func(dir string) error {
			_, err := chat.FileStore{Dir: dir, Role: "dev"}.Put(chat.Upload{ID: "abcdefab-1234-1234-1234-123456789abc",
				Name: "image.png", Data: []byte{0, 255, 1, 2}})
			return err
		},
	}

	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			var through []string

			fsx.BeforeCommit = func(_, path string) { through = append(through, filepath.Base(path)) }
			t.Cleanup(func() { fsx.BeforeCommit = nil })

			if err := write(t.TempDir()); err != nil {
				t.Fatal(err)
			}

			if len(through) == 0 {
				t.Fatalf("%s did not write through fsx", name)
			}
		})
	}
}

// Writers of one file that used a fixed name.tmp raced: one renamed the other's half written file, the other's
// rename found nothing.
func TestOneFileManyWriters(t *testing.T) {
	dir := t.TempDir()
	reg := &codex.Registry{Dir: dir}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	for g := range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range 30 {
				rec := codex.Record{Session: registry.Session{PID: 42, Name: fmt.Sprintf("w%d-%d-%s", g, i, strings.Repeat("x", 4096))}}
				if err := reg.Save(rec); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}
		}()
	}

	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d of 240 saves failed, first: %v", len(errs), errs[0])
	}

	if s, err := reg.ByPID(42); err != nil || s == nil {
		t.Fatalf("after the race: %v, %v", s, err)
	}
}

func mustInboxID(t *testing.T) string {
	t.Helper()

	id, err := inbox.NewID()
	if err != nil {
		t.Fatal(err)
	}

	return id
}

// A first write into directories that do not exist yet syncs each new directory into its parent, up to the one that
// was there: otherwise a power loss can drop the new directory with the synced file in it.
func TestFirstWriteSyncsNewDirectories(t *testing.T) {
	cases := map[string]struct {
		write func(dir string) error
		made  []string // new directories, relative to dir
	}{
		"handoff": {func(dir string) error {
			_, err := handoff.Write(dir, handoff.Packet{Task: task.Task{ID: "T-1"}})
			return err
		}, []string{"handoffs", "handoffs/T-1"}},
		"chat upload": {func(dir string) error {
			_, err := chat.FileStore{Dir: filepath.Join(dir, "chat-files"), Role: "dev"}.Put(chat.Upload{
				ID: "abcdefab-1234-1234-1234-123456789abc", Name: "image.png", Data: []byte{0, 255, 1, 2}})
			return err
		}, []string{"chat-files", "chat-files/" + chat.Hash("dev")}},
		"task": {func(dir string) error {
			_, err := (&task.Store{Dir: filepath.Join(dir, "tasks")}).Add(task.Task{Role: "dev", Asked: "x"}, now)
			return err
		}, []string{"tasks"}},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()

			synced := map[string]bool{}
			fsx.Synced = func(d string) { synced[d] = true }
			t.Cleanup(func() { fsx.Synced = nil })

			if err := c.write(dir); err != nil {
				t.Fatal(err)
			}

			for _, m := range c.made {
				parent := filepath.Dir(filepath.Join(dir, m))
				if !synced[parent] {
					t.Errorf("%s was made but %s, which got its entry, was not synced", m, parent)
				}
			}
		})
	}
}
