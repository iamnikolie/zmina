package shift

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/watch"
)

// releasing is a crew whose manual roles are released after 30 min idle, with designer up past its boot and idle
// for 31 min.
func releasing(t *testing.T, extra string) fix {
	t.Helper()

	f := fixtureWith(t, "release_idle_minutes: 30\n"+crewYAML+extra)
	f.s.Tick() // the dispatcher and harvester start
	require.NoError(t, f.s.Up("designer"))
	*f.now = f.now.Add(5 * time.Minute)
	f.status("designer", "idle", f.now.Add(-31*time.Minute))

	return f
}

func TestReleaseParksWhatTheRoleLeaves(t *testing.T) {
	f := releasing(t, "")
	f.tm.screen = "⏺ Drawing the second variant\n"
	f.tm.suggestions["designer"] = "commit the logo" // dim: no draft
	f.tk.list = []task.Task{
		{ID: "T-1", Role: "designer", State: task.Open, Title: "draw the logo"},
		{ID: "T-2", Role: "designer", State: task.Waiting, Title: "pick the palette"},
		{ID: "T-3", Role: "harvester", State: task.Open},
	}

	f.s.Tick()
	require.Len(t, f.tm.typed["designer"], 1, "the release line, nothing else")
	line := f.tm.typed["designer"][0]
	assert.Contains(t, line, "[zmina] idle 31m: you are released")
	assert.Contains(t, line, "(T-1, T-2)")
	assert.Contains(t, line, `zmina task park <id> --note "<what remains>"`)
	assert.NotContains(t, strings.Join(f.s.State.Notes, "|"), "designer: idle", "the idle notice is replaced")
	assert.True(t, f.tm.Has("designer"))

	// The role writes what remains of one task itself.
	it, err := f.s.Park(f.tk.list[0], "two variants left;\n the second is half drawn")
	require.NoError(t, err)
	assert.Equal(t, "L-1", it.ID)

	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	assert.True(t, f.tm.Has("designer"), "a task still held waits for the role")

	*f.now = f.now.Add(releaseWait)
	f.s.Tick()
	assert.False(t, f.tm.Has("designer"), "put down")
	assert.Equal(t, []string{"designer"}, f.s.Released)
	assert.Contains(t, f.s.State.Notes, "released designer after 31m idle → L-1, L-2")
	assert.True(t, f.ev.has("designer", "released"))
	require.NoError(t, f.s.FinishDown("designer"))

	require.Len(t, f.lt.list, 2)
	assert.Equal(t, "two variants left; the second is half drawn", f.lt.list[0].Text)
	assert.Equal(t, "pick the palette; last: Drawing the second variant", f.lt.list[1].Text, "the fallback")

	for i, id := range []string{"T-1", "T-2"} {
		it := f.lt.list[i]
		assert.Equal(t, id, it.From)
		assert.Equal(t, "designer", it.Role)
		assert.Equal(t, "sid-designer", it.Session, "later take --resume continues this conversation")
		assert.Equal(t, "from "+id+" (designer)", it.Note)

		tk, _ := f.tk.Get(id)
		assert.Equal(t, task.Parked, tk.State)
		assert.Equal(t, "parked "+it.ID, tk.Status())
	}

	tk, _ := f.tk.Get("T-3")
	assert.Equal(t, task.Open, tk.State, "another role's task is not touched")
}

func TestReleaseWithoutTasksGoesDownAtOnce(t *testing.T) {
	f := releasing(t, "")

	f.s.Tick()
	assert.False(t, f.tm.Has("designer"))
	assert.Equal(t, []string{"/exit"}, f.tm.typed["designer"])
	assert.Contains(t, f.s.State.Notes, "released designer after 31m idle")
	assert.Empty(t, f.lt.list)
}

func TestReleaseIsCalledOff(t *testing.T) {
	f := releasing(t, "")
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open}}

	f.s.Tick()
	require.False(t, f.s.State.Role("designer").Releasing.IsZero())

	f.tm.drafts["designer"] = "wait, one more thing"
	*f.now = f.now.Add(releaseWait)
	f.s.Tick()
	assert.True(t, f.tm.Has("designer"), "the owner types into it")
	assert.True(t, f.s.State.Role("designer").Releasing.IsZero())
	assert.True(t, f.ev.has("designer", "release-off"))

	// Released again after another idle stretch; a new task then calls it off as well.
	delete(f.tm.drafts, "designer")
	f.status("designer", "idle", f.now.Add(-31*time.Minute))
	f.s.Tick()
	require.False(t, f.s.State.Role("designer").Releasing.IsZero())

	f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: "designer", State: task.Open})
	*f.now = f.now.Add(releaseWait)
	f.s.Tick()
	assert.True(t, f.tm.Has("designer"))
	assert.True(t, f.s.State.Role("designer").Releasing.IsZero())
	assert.Empty(t, f.lt.list)
}

func TestWhoIsNeverReleased(t *testing.T) {
	f := releasing(t, `  dev:
    release_idle_minutes: off
  ops:
    release_idle_minutes: 90
`)

	for _, name := range []string{"dev", "ops"} {
		require.NoError(t, f.s.Up(name))
	}

	*f.now = f.now.Add(5 * time.Minute)

	for _, name := range []string{"dispatcher", "harvester", "dev"} {
		f.status(name, "idle", f.now.Add(-2*time.Hour))
	}

	f.status("ops", "idle", f.now.Add(-89*time.Minute))

	f.status("designer", "busy", f.now.Add(-2*time.Hour))
	f.s.Tick()

	for _, name := range []string{"dispatcher", "harvester", "designer", "dev", "ops"} {
		assert.True(t, f.tm.Has(name), name)
		assert.True(t, f.s.State.Role(name).Releasing.IsZero(), name)
	}

	assert.True(t, f.ev.has("dev", "idle"), "off keeps the idle notice")
	assert.False(t, f.ev.has("ops", "idle"))

	f.status("designer", "idle", f.now.Add(-time.Hour))
	f.tm.drafts["designer"] = "-" // a dialog
	f.s.Tick()
	assert.True(t, f.tm.Has("designer"), "a dialog is open")

	delete(f.tm.drafts, "designer")
	f.tm.attached["designer"] = true
	f.s.Tick()
	assert.True(t, f.tm.Has("designer"), "a terminal is attached")

	f.tm.attached["designer"] = false
	f.s.Tick()
	assert.False(t, f.tm.Has("designer"))

	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	assert.False(t, f.tm.Has("ops"), "its own 90 min")
}

func TestParkRefusesAClosedTask(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Done}}

	_, err := f.s.Park(f.tk.list[0], "")
	require.ErrorContains(t, err, "T-1 is done")

	f.tk.list[0].State = task.Open
	f.tk.list[0].Asked = "make the  site\nfaster"
	it, err := f.s.Park(f.tk.list[0], " ")
	require.NoError(t, err)
	assert.Equal(t, "make the site faster", it.Text, "no note: the task's title")
}

func TestRunTickFinishesTheReleasedDowns(t *testing.T) {
	f := releasing(t, "")

	locked := func(fn func(s *Shift) error) error { return fn(f.s) }

	var lines []string

	require.NoError(t, RunTick(locked, func([]WatchRun) []watch.Result { return nil },
		func(l []string) { lines = append(lines, l...) }))
	assert.Contains(t, lines, "released designer after 31m idle")
	assert.False(t, f.tm.Has("designer"))
	assert.True(t, f.s.State.Role("designer").Downing.IsZero(), "the down is finished")
}
