package shift

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// listingTmux is the fake tmux that can also list its sessions: the roles that are up or dead (all started by zmina) and the extra ones
// a test puts there, each tagged or not.
type listingTmux struct {
	*fakeTmux
	// extra puts a session on the list whether or not a role is up under its name: true is tagged, false is not. A name that is up or
	// dead and not here is tagged.
	extra   map[string]bool
	err     error
	killErr error
	asked   int
}

func (l *listingTmux) Sessions() ([]tmux.SessionInfo, error) {
	l.asked++

	if l.err != nil {
		return nil, l.err
	}

	names := map[string]bool{}

	for _, group := range [][]string{mapKeys(l.up), mapKeys(l.dead), mapKeys(l.extra)} {
		for _, name := range group {
			names[name] = true
		}
	}

	var out []tmux.SessionInfo

	for _, name := range mapKeys(names) {
		tagged := true
		if v, ok := l.extra[name]; ok {
			tagged = v
		}

		out = append(out, tmux.SessionInfo{Name: name, Tagged: tagged})
	}

	return out, nil
}

func (l *listingTmux) Kill(name string) error {
	if l.killErr != nil {
		return l.killErr
	}

	return l.fakeTmux.Kill(name)
}

func mapKeys[V any](m map[string]V) []string {
	var out []string

	for k := range m {
		out = append(out, k)
	}

	slices.Sort(out)

	return out
}

// orphaned is a crew with the key as given, its seats up and its dispatcher booted, and an orphan session: no role of the crew file is
// named ghost, its pane is tagged, its process runs, and its registry entry says it has been idle for an hour.
func orphaned(t *testing.T, key string) (fix, *listingTmux) {
	t.Helper()

	f := fixtureWith(t, key+crewYAML+"  dev:\n")

	lt := &listingTmux{fakeTmux: f.tm, extra: map[string]bool{}}
	f.s.Tmux = lt

	f.s.Tick()

	f.tm.up["ghost"] = 4242
	lt.extra["ghost"] = true
	f.rg.byPID[4242] = &registry.Session{PID: 4242, Status: "idle", StatusUpdatedAt: f.now.Add(-time.Hour).UnixMilli(), SessionID: "sid-ghost"}

	return f, lt
}

func eventsOf(f fix, event string) int {
	n := 0

	for _, e := range f.ev.list {
		if e.Event == event {
			n++
		}
	}

	return n
}

// A session no role names is reported once, with the key on in report mode: it is left alone, however many beats; told again when it has been gone
// and returns.
func TestOrphanIsReportedOnceAndAgainAfterItReturns(t *testing.T) {
	f, lt := orphaned(t, "reconcile: {orphans: report}\n")

	for range 3 {
		f.beat(time.Minute)
	}

	assert.Equal(t, 1, eventsOf(f, "orphan"))
	assert.True(t, f.tm.Has("ghost"), "report only")
	assert.True(t, f.s.State.RecordNotified[orphanKey("ghost")])

	lines := strings.Join(f.dispatcherLines(), "|")
	assert.Contains(t, lines, "ghost: a tmux session no role of the crew file names; left alone (reconcile: orphans: report); zmina attach ghost to look")
	assert.NotContains(t, lines, "taken down")

	// Gone: the report may be told again.
	delete(f.tm.up, "ghost")
	delete(lt.extra, "ghost")
	f.beat(time.Minute)
	assert.False(t, f.s.State.RecordNotified[orphanKey("ghost")])

	f.tm.up["ghost"] = 4242
	lt.extra["ghost"] = true
	f.beat(time.Minute)
	assert.Equal(t, 2, eventsOf(f, "orphan"))
}

// A look that tmux does not answer forgets no report and takes nothing down; a Tmux that cannot list at all has no orphans.
func TestOrphansAreLeftAloneWhenTmuxCannotBeAsked(t *testing.T) {
	f, lt := orphaned(t, "reconcile: {orphans: report}\n")
	f.beat(time.Minute)
	require.True(t, f.s.State.RecordNotified[orphanKey("ghost")])

	lt.err = errors.New("tmux: no answer in 10s")
	f.beat(time.Minute)
	assert.True(t, f.s.State.RecordNotified[orphanKey("ghost")], "an unanswered look forgets no report")
	assert.Equal(t, 1, eventsOf(f, "orphan"))

	g, glt := orphaned(t, "reconcile: {orphans: down}\n")
	glt.err = errors.New("tmux: no answer in 10s")
	g.beat(time.Minute)
	assert.True(t, g.tm.Has("ghost"), "nothing is taken down on a look that failed")

	h := fixtureWith(t, "reconcile: {orphans: down}\n"+crewYAML)
	h.s.Tick()
	h.tm.up["ghost"] = 4242
	h.beat(time.Minute)
	assert.True(t, h.tm.Has("ghost"), "the plain fake lists nothing")
}

// With the key off, or orphans off, nothing is asked of tmux and nothing is told.
func TestOrphansOffAsksNothing(t *testing.T) {
	for _, key := range []string{"", "reconcile: off\n", "reconcile: {restart: true, assign: true, orphans: off}\n"} {
		f, lt := orphaned(t, key)
		f.beat(time.Hour)

		assert.Zero(t, lt.asked, "%q: tmux is not asked to list", key)
		assert.True(t, f.tm.Has("ghost"), key)
		assert.Zero(t, eventsOf(f, "orphan"), key)
		assert.Empty(t, f.s.State.RecordNotified, key)
	}
}

// What the ticker never takes for an orphan: a role of the crew file, a copy of one, and a seat the state has.
func TestOrphansAreNoRoleOfTheCrewFileAndNoCopyOfOne(t *testing.T) {
	f, lt := orphaned(t, "reconcile: {orphans: report}\n")

	// A recorded copy, a copy the state lost, and a role's own session: none is an orphan.
	require.NoError(t, f.s.UpWith("dev#2", UpOptions{}))
	f.tm.up["dev#3"] = 5000
	lt.extra["dev#3"] = true
	require.NoError(t, f.s.UpWith("dev", UpOptions{}))

	f.beat(time.Minute)

	assert.Equal(t, 1, eventsOf(f, "orphan"), "only ghost")

	for _, n := range []string{"dev", "dev#2", "dev#3", "dispatcher", "harvester"} {
		assert.False(t, f.s.State.RecordNotified[orphanKey(n)], n)
	}

	// A copy of a role that is not in the crew file is an orphan like any session.
	f.tm.up["old#2"] = 5001
	lt.extra["old#2"] = true
	f.beat(time.Minute)
	assert.True(t, f.s.State.RecordNotified[orphanKey("old#2")])
}

// A session that holds tasks is reported with them, and never taken down.
func TestAnOrphanThatHoldsTasksIsReportedWithThemAndKept(t *testing.T) {
	f, _ := orphaned(t, "reconcile: {orphans: down}\n")
	f.holds("ghost", openTask("T-1", "ghost"), task.Task{ID: "T-2", Role: "ghost", State: task.Waiting}, task.Task{ID: "T-3", Role: "ghost", State: task.Done})

	f.beat(time.Minute)
	f.beat(time.Hour)

	assert.True(t, f.tm.Has("ghost"), "it holds work: report only")
	assert.Equal(t, 1, eventsOf(f, "orphan"))
	assert.Zero(t, eventsOf(f, "orphan-down"))

	lines := strings.Join(f.dispatcherLines(), "|")
	assert.Contains(t, lines, "ghost: a tmux session no role of the crew file names, and it holds T-1, T-2: not taken down")
	assert.NotContains(t, lines, "T-3")
}

// The take-down: only when every condition holds. The first beat of this is the one that does it; each case spoils one condition.
func TestAnOrphanIsTakenDownOnlyWhenEveryConditionHolds(t *testing.T) {
	type setup func(f fix, lt *listingTmux)

	cases := map[string]struct {
		key   string
		setup setup
		down  bool
	}{
		"all hold":                        {"reconcile: {orphans: down}\n", func(fix, *listingTmux) {}, true},
		"on: the same, the default of on": {"reconcile: on\n", func(fix, *listingTmux) {}, true},
		"report mode":                     {"reconcile: {orphans: report}\n", func(fix, *listingTmux) {}, false},
		"the shift is stopped": {"reconcile: {orphans: down}\n", func(f fix, _ *listingTmux) {
			f.s.State.Stopped = *f.now
		}, false},
		"zmina did not start it (no tag)": {"reconcile: {orphans: down}\n", func(_ fix, lt *listingTmux) {
			lt.extra["ghost"] = false
		}, false},
		"it was a role lately: the crew file may have a typo": {"reconcile: {orphans: down}\n", func(f fix, _ *listingTmux) {
			f.s.State.Role("ghost").AbsentBeats = 3
		}, false},
		"it has a task": {"reconcile: {orphans: down}\n", func(f fix, _ *listingTmux) {
			f.holds("ghost", openTask("T-1", "ghost"))
		}, false},
		"busy": {"reconcile: {orphans: down}\n", func(f fix, _ *listingTmux) {
			f.rg.byPID[4242].Status = "busy"
		}, false},
		"idle for less than idle_minutes": {"reconcile: {orphans: down}\n", func(f fix, _ *listingTmux) {
			f.rg.byPID[4242].StatusUpdatedAt = f.now.Add(-10 * time.Minute).UnixMilli()
		}, false},
		"no registry entry to say it is idle": {"reconcile: {orphans: down}\n", func(f fix, _ *listingTmux) {
			delete(f.rg.byPID, 4242)
		}, false},
		"a terminal is attached": {"reconcile: {orphans: down}\n", func(f fix, _ *listingTmux) {
			f.tm.attached["ghost"] = true
		}, false},
		"its process has ended": {"reconcile: {orphans: down}\n", func(f fix, _ *listingTmux) {
			f.tm.die("ghost")
		}, false},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, lt := orphaned(t, c.key)
			c.setup(f, lt)

			did := f.beat(time.Minute)

			if !c.down {
				assert.Zero(t, eventsOf(f, "orphan-down"))
				assert.True(t, f.tm.Has("ghost") || f.tm.Dead("ghost"), "left as it is")

				return
			}

			assert.Equal(t, 1, eventsOf(f, "orphan-down"))
			assert.False(t, f.tm.Has("ghost"))
			assert.Contains(t, did, "took down orphan session ghost")

			// An FYI that waits for a line to go with, and no report beside it.
			text := "ghost: a tmux session no role of the crew file names, idle 30 min with nothing queued, taken down"
			assert.True(t, slices.ContainsFunc(f.s.State.Notes, func(n string) bool { return strings.HasPrefix(n, text) }), "%v", f.s.State.Notes)
			assert.True(t, f.s.isInfo(f.s.State.Notes[len(f.s.State.Notes)-1]))
			assert.Zero(t, eventsOf(f, "orphan"), "taken down at once: no report first")
		})
	}
}

// A state entry that stays after the crew file lost the role is not taken down until it has been missing for pruneAfterBeats; one with
// lines in its queue is never, though its state is kept.
func TestAnOrphanWithAStateEntryWaitsForItsPrune(t *testing.T) {
	f, _ := orphaned(t, "reconcile: {orphans: down}\n")
	rs := f.s.State.Role("ghost")
	rs.AbsentBeats = pruneAfterBeats - 3

	for i := 1; i < 3; i++ {
		f.beat(time.Minute)
		require.True(t, f.tm.Has("ghost"), "beat %d: still missing from the crew file only %d beats", i, rs.AbsentBeats)
	}

	f.beat(time.Minute) // pruned this beat: its state is archived and gone, nothing is queued for it
	assert.False(t, f.tm.Has("ghost"))
	assert.NotContains(t, f.s.State.Roles, "ghost")

	// A state that could not be archived stays; its queue is a reason to keep the session.
	g, _ := orphaned(t, "reconcile: {orphans: down}\n")
	require.NoError(t, os.MkdirAll(g.s.Crew.StateDir(), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(g.s.Crew.StateDir(), "pruned"), []byte("a file, not a directory"), 0o600))

	grs := g.s.State.Role("ghost")
	grs.AbsentBeats = pruneAfterBeats
	grs.Inbox = []string{"[dev] one more thing"}

	g.beat(time.Minute)
	assert.True(t, g.tm.Has("ghost"), "a line waits for it")
	assert.Contains(t, g.s.State.Roles, "ghost", "and its state was kept")

	grs.Inbox = nil
	g.beat(time.Minute)
	assert.False(t, g.tm.Has("ghost"), "nothing waits any more")
}

// A kill that fails is said and tried again, and nothing is told as done.
func TestAnOrphanThatCannotBeKilledIsTriedAgain(t *testing.T) {
	f, lt := orphaned(t, "reconcile: {orphans: down}\n")
	lt.killErr = errors.New("tmux: server exited")

	did := f.beat(time.Minute)
	assert.Contains(t, strings.Join(did, "|"), "orphan ghost: could not be taken down: tmux: server exited")
	assert.True(t, f.tm.Has("ghost"))
	assert.Zero(t, eventsOf(f, "orphan-down"))

	lt.killErr = nil
	f.beat(time.Minute)
	assert.False(t, f.tm.Has("ghost"))
	assert.Equal(t, 1, eventsOf(f, "orphan-down"))
}

// A take-down keeps the last screen, as an ended role's is kept.
func TestAnOrphanTakenDownLeavesItsLastScreen(t *testing.T) {
	f, _ := orphaned(t, "reconcile: {orphans: down}\n")
	f.tm.screen = "⏺ Done with the migration\n"

	f.beat(time.Minute)

	files, err := filepath.Glob(filepath.Join(f.s.ScreensDir(), "ghost-*.txt"))
	require.NoError(t, err)
	require.Len(t, files, 1)

	raw, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Contains(t, string(raw), "Done with the migration")
	assert.Contains(t, strings.Join(f.dispatcherLines(), "|"), ", last screen screens/ghost-")
}

// The shift stopped is a reason for the take-down to wait, not to forget: the report still comes.
func TestAStoppedShiftStillReportsAnOrphan(t *testing.T) {
	f, _ := orphaned(t, "reconcile: {orphans: down}\n")
	f.s.State.Stopped = *f.now

	f.beat(time.Minute)
	assert.True(t, f.tm.Has("ghost"))
	assert.Equal(t, 1, eventsOf(f, "orphan"))
	assert.Contains(t, strings.Join(f.dispatcherLines(), "|"), "taken down once it has been idle 30 min with nothing queued for it and no terminal attached")
}

// An orphan zmina did not start is reported as such, and never taken down.
func TestAnUntaggedSessionIsReportedAsNotZminas(t *testing.T) {
	f, lt := orphaned(t, "reconcile: {orphans: down}\n")
	lt.extra["ghost"] = false
	delete(f.tm.up, "ghost")
	lt.up["by-hand"] = 4343
	lt.extra["by-hand"] = false
	f.rg.byPID[4343] = &registry.Session{PID: 4343, Status: "idle", StatusUpdatedAt: f.now.Add(-time.Hour).UnixMilli()}

	f.beat(time.Minute)
	assert.True(t, f.tm.Has("by-hand"))
	assert.Contains(t, strings.Join(f.dispatcherLines(), "|"), "by-hand: a tmux session no role of the crew file names, and zmina did not start it: left alone")
}
