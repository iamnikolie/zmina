package shift

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/task"
)

// A task or ask file that does not read is in no list. The ticker says so once per breakage: a log event and a
// note for the dispatcher; again only after the file was mended and broke again.
func TestUnreadableTaskOrAskIsToldOnce(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	tasks := &task.Store{Dir: filepath.Join(t.TempDir(), "tasks")}
	ask := &asks.Store{Dir: filepath.Join(t.TempDir(), "asks")}
	f.s.Tasks, f.s.Asks = tasks, ask

	_, err := tasks.Add(task.Task{Role: "designer", Asked: "the landing page"}, *f.now)
	require.NoError(t, err)

	// An empty T-2.json is what a writer killed after claiming the name used to leave.
	broken := filepath.Join(tasks.Dir, "T-2.json")
	require.NoError(t, os.WriteFile(broken, nil, 0o600))
	require.NoError(t, os.MkdirAll(ask.Dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ask.Dir, "A-1.json"), []byte("{"), 0o600))

	f.s.Tick()
	assert.Equal(t, 2, f.ev.count("", "unreadable"))
	assert.Len(t, tasks.Active(), 1, "the broken task is still in no list")

	notes := strings.Join(f.s.State.Notes, "|")
	assert.Contains(t, notes, "zmina: tasks/T-2.json does not read (task T-2: unexpected end of JSON input)")
	assert.Contains(t, notes, "zmina: asks/A-1.json does not read")

	var logged []string
	for _, e := range f.ev.list {
		if e.Event == "unreadable" {
			logged = append(logged, e.Task+" "+e.Detail)
		}
	}

	assert.Contains(t, logged, "T-2 tasks/T-2.json: task T-2: unexpected end of JSON input", "zmina log --task T-2 finds it")

	f.s.Tick()
	assert.Equal(t, 2, f.ev.count("", "unreadable"), "told once")

	// Mended, then broken again: told again.
	require.NoError(t, tasks.Put(task.Task{ID: "T-2", Role: "designer", Asked: "the logo", State: task.Open}))
	f.s.Tick()
	assert.NotContains(t, f.s.State.Unreadable, "tasks/T-2.json")
	assert.Contains(t, f.s.State.Unreadable, "asks/A-1.json", "the ask is still broken")

	require.NoError(t, os.WriteFile(broken, []byte("not json"), 0o600))
	f.s.Tick()
	assert.Equal(t, 3, f.ev.count("", "unreadable"))
}
