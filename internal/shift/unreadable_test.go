package shift

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/task"
)

func unreadableFixture(t *testing.T) (fix, *task.Store, *asks.Store) {
	t.Helper()

	f := fixtureWith(t, crewYAML)
	tasks := &task.Store{Dir: filepath.Join(t.TempDir(), "tasks")}
	ask := &asks.Store{Dir: filepath.Join(t.TempDir(), "asks")}
	f.s.Tasks, f.s.Asks = tasks, ask
	require.NoError(t, os.MkdirAll(tasks.Dir, 0o700))
	require.NoError(t, os.MkdirAll(ask.Dir, 0o700))

	return f, tasks, ask
}

func unreadableSignals(s *Shift) []string {
	var out []string

	for _, sig := range s.State.Signals {
		if strings.Contains(sig, "files that do not read") {
			out = append(out, sig)
		}
	}

	return out
}

// A task or ask file that does not read is in no list. The ticker says so once per breakage: a log event and a
// signal for the dispatcher; again only after the file was mended and broke again.
func TestUnreadableTaskOrAskIsToldOnce(t *testing.T) {
	f, tasks, ask := unreadableFixture(t)

	_, err := tasks.Add(task.Task{Role: "designer", Asked: "the landing page"}, *f.now)
	require.NoError(t, err)

	// An empty T-2.json is what a writer killed after claiming the name used to leave.
	broken := filepath.Join(tasks.Dir, "T-2.json")
	require.NoError(t, os.WriteFile(broken, nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(ask.Dir, "A-1.json"), []byte("{"), 0o600))

	f.s.Tick()
	assert.Equal(t, 2, f.ev.count("", "unreadable"))
	assert.Len(t, tasks.Active(), 1, "the broken task is still in no list")

	sigs := unreadableSignals(f.s)
	require.Len(t, sigs, 1, "one signal for both files")
	assert.Contains(t, sigs[0], "zmina: files that do not read, so they are in no list and hold no role: "+
		"asks/A-1.json, tasks/T-2.json; zmina doctor and zmina log name each with why")

	var logged []string

	for _, e := range f.ev.list {
		if e.Event == "unreadable" {
			logged = append(logged, e.Task+" "+e.Detail)
		}
	}

	assert.Contains(t, logged, "T-2 tasks/T-2.json: task T-2: unexpected end of JSON input", "zmina log --task T-2 finds it")

	f.s.State.Signals = nil // delivered
	f.s.Tick()
	assert.Equal(t, 2, f.ev.count("", "unreadable"), "logged once")
	assert.Empty(t, unreadableSignals(f.s), "told once")

	// Mended, then broken again: told again.
	require.NoError(t, tasks.Put(task.Task{ID: "T-2", Role: "designer", Asked: "the logo", State: task.Open}))
	f.s.Tick()
	assert.NotContains(t, f.s.State.Unreadable, "tasks/T-2.json")
	assert.Contains(t, f.s.State.Unreadable, "asks/A-1.json", "the ask is still broken")

	require.NoError(t, os.WriteFile(broken, []byte("not json"), 0o600))
	f.s.Tick()
	assert.Equal(t, 3, f.ev.count("", "unreadable"))
	require.Len(t, unreadableSignals(f.s), 1)
	assert.NotContains(t, unreadableSignals(f.s)[0], "A-1", "the ask was told already")
}

// Many broken files are one signal, none lost to a cap; a full signal queue leaves them untold until it has room.
func TestUnreadableFilesWaitForRoom(t *testing.T) {
	f, tasks, _ := unreadableFixture(t)

	for i := 1; i <= 21; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(tasks.Dir, fmt.Sprintf("T-%d.json", i)), nil, 0o600))
	}

	for i := range signalsMax {
		f.s.State.Signals = append(f.s.State.Signals, fmt.Sprintf("dev: line %d", i))
	}

	f.s.Tick()
	assert.Empty(t, unreadableSignals(f.s), "the queue is full")
	assert.Equal(t, 21, f.ev.count("", "unreadable"), "logged at once")

	for key, at := range f.s.State.Unreadable {
		assert.True(t, at.IsZero(), "%s marked told while untold", key)
	}

	f.s.Tick()
	assert.Equal(t, 21, f.ev.count("", "unreadable"), "not logged again while it waits")

	f.s.State.Signals = nil
	f.s.Tick()

	sigs := unreadableSignals(f.s)
	require.Len(t, sigs, 1)

	// Named in the order of their file names, ten of them, then the count.
	assert.Contains(t, sigs[0], "tasks/T-1.json, tasks/T-10.json,")
	assert.Contains(t, sigs[0], fmt.Sprintf("tasks/T-%d.json and 11 more;", 10+8), "ten named, the rest counted")

	assert.Len(t, f.s.State.Unreadable, 21)

	for key, at := range f.s.State.Unreadable {
		assert.False(t, at.IsZero(), "%s not marked told", key)
	}
}
