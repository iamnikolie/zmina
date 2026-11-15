package shift

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
)

// reopenCrew is the default crew (dispatcher, designer, harvester: keep: always) with two seats that are kept manual, dev and dev2.
const reopenCrew = crewYAML + "  dev:\n  dev2:\n"

// A crew of the dispatcher and one seat, for the tests that watch what happens to that seat alone.
const soloCrew = "namespace: /ns\nclaude: claude\nroles:\n  dispatcher:\n    boot: read d and boot\n  dev:\n"

// reconciling is a crew with the key on and the beat's first look behind it: whoever is wanted up is up.
func reconciling(t *testing.T, key, body string) fix {
	t.Helper()

	f := fixtureWith(t, key+body)
	f.s.Tick()

	return f
}

func (f fix) holds(role string, tasks ...task.Task) {
	for _, t := range tasks {
		if t.Role == "" {
			t.Role = role
		}

		f.tk.list = append(f.tk.list, t)
	}
}

// wait moves the clock and takes a beat.
func (f fix) beat(after time.Duration) []string {
	*f.now = f.now.Add(after)

	return f.s.Tick()
}

// dispatcherLines are what waits for the dispatcher: signals and notes, as written.
func (f fix) dispatcherLines() []string {
	var out []string

	for _, sig := range f.s.State.Signals {
		out = append(out, SignalBody(sig))
	}

	return append(out, f.s.State.Notes...)
}

func started(f fix, role string) bool {
	for _, n := range f.tm.starts {
		if n == role {
			return true
		}
	}

	return false
}

// A seat that holds an open task and whose session ended is started again by the beat, with a note that says why and the task handed
// again, as a keep: always seat is.
func TestReconcileRestartStartsASeatThatHoldsAnOpenTask(t *testing.T) {
	f := reconciling(t, "reconcile: {restart: true}\n", soloCrew)
	require.NoError(t, f.s.Up("dev"))
	f.status("dev", "idle", *f.now)
	f.holds("dev", openTask("T-1", "dev"))

	f.beat(11 * time.Minute)
	require.Equal(t, "sid-dev", f.s.State.Role("dev").LastSession, "the conversation it was on is known")

	f.tm.die("dev")
	did := f.beat(time.Minute)

	assert.Contains(t, did, "started dev")
	assert.True(t, f.tm.Has("dev"))
	assert.Contains(t, f.s.State.Notes, "dev: ended (held T-1)")
	assert.Contains(t, f.s.State.Notes, "dev: started again (holds T-1)")
	assert.True(t, f.s.isInfo("dev: started again (holds T-1)"), "an info line")
	assert.Contains(t, strings.Join(f.s.State.Role("dev").Inbox, "|"), "T-1", "the task is handed again")
}

// The seat is started where it ran when that is still a directory (a task's worktree), else in its own.
func TestReconcileRestartStartsInTheDirectoryTheSeatRanIn(t *testing.T) {
	f := reconciling(t, "reconcile: {restart: true}\n", soloCrew)
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, os.MkdirAll(wt, 0o700))

	f.holds("dev", openTask("T-1", "dev"))
	require.NoError(t, f.s.UpWith("dev", UpOptions{Dir: wt}))
	f.tm.die("dev")
	f.beat(11 * time.Minute)
	assert.Equal(t, wt, f.tm.dirs["dev"], "back in the worktree")

	f.tm.die("dev")
	require.NoError(t, os.RemoveAll(wt))
	f.beat(30 * time.Minute)
	assert.Equal(t, f.s.Crew.Roles["dev"].Dir, f.tm.dirs["dev"], "the worktree is gone: its own directory")
}

// What stands in the way of a start: a seat that is held, asleep, broken, of a held plan, in a stopped shift or a retired crew, past its
// expiry, going down or being released is not started, nor one whose tasks wait for someone, are parked or closed, nor a copy.
func TestReconcileRestartLeavesDownWhatStandsInTheWay(t *testing.T) {
	open := openTask("T-1", "dev")

	for name, c := range map[string]struct {
		tasks []task.Task
		setup func(f fix)
		body  string
	}{
		"put down by hand": {[]task.Task{open}, func(f fix) { f.s.State.Role("dev").Held = true }, ""},
		"broken":           {[]task.Task{open}, func(f fix) { f.s.State.Role("dev").Broken = true }, ""},
		"asleep": {[]task.Task{open}, func(f fix) {
			f.s.Alarms = &alarm.Store{Dir: f.s.Crew.Layout().Alarms()}
			_, err := f.s.Alarms.Add(alarm.Alarm{Kind: alarm.Sleep, Role: "dev", At: f.now.Add(48 * time.Hour)}, *f.now)
			require.NoError(t, err)
		}, ""},
		"its plan is held": {[]task.Task{open}, func(f fix) {
			f.claudeLimits(97, 10)
			f.s.limitsStep(*f.now)
			require.True(t, f.s.planHeld("dev", *f.now))
		}, "timezone: UTC\n"},
		"the shift is stopped": {[]task.Task{open}, func(f fix) { f.s.State.Stopped = *f.now }, ""},
		"the crew is retired":  {[]task.Task{open}, func(f fix) { f.s.Crew.Retired = "wound down" }, ""},
		"a down is begun":      {[]task.Task{open}, func(f fix) { f.s.State.Role("dev").Downing = *f.now }, ""},
		"a release is under way": {[]task.Task{open}, func(f fix) {
			f.s.State.Role("dev").Releasing = *f.now
		}, ""},
		"waiting for the owner": {[]task.Task{{ID: "T-1", Role: "dev", State: task.Waiting}}, nil, ""},
		"waiting on a seat":     {[]task.Task{{ID: "T-1", Role: "dev", State: "waiting-designer"}}, nil, ""},
		"parked":                {[]task.Task{{ID: "T-1", Role: "dev", State: task.Parked}}, nil, ""},
		"done":                  {[]task.Task{{ID: "T-1", Role: "dev", State: task.Done}}, nil, ""},
		"another seat's task":   {[]task.Task{openTask("T-1", "dev2")}, nil, ""},
		"no task at all":        {nil, nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, c.body+"reconcile: {restart: true}\n"+reopenCrew)
			f.tk.list = c.tasks

			if c.setup != nil {
				c.setup(f)
			}

			f.s.Tick()
			assert.False(t, started(f, "dev"), "dev is left down")
			assert.False(t, f.tm.Has("dev"))

		})
	}

	t.Run("a temporary seat past its expiry", func(t *testing.T) {
		f := fixtureWith(t, "reconcile: {restart: true}\n"+crewYAML+"  old:\n    expires: 2026-10-24T11:00:00Z\n")
		f.holds("old", openTask("T-1", "old"))

		f.s.Tick()
		assert.False(t, started(f, "old"))
	})

	t.Run("a copy is gone once down: its task is not reconciled", func(t *testing.T) {
		f := reconciling(t, "reconcile: {restart: true}\n", reopenCrew)
		f.holds("dev#2", openTask("T-1", "dev#2"))
		require.NoError(t, f.s.UpWith("dev#2", UpOptions{}))
		f.tm.die("dev#2")

		f.beat(time.Hour)
		assert.False(t, f.tm.Has("dev#2"))
		assert.Len(t, f.tm.starts, 3, "dispatcher, harvester and the copy's own up: nothing from the beat")
	})
}

// A seat that cannot be started is tried again on a growing pause, then marked broken with one note; zmina up re-arms it.
func TestReconcileRestartFailingStartsClimbThePauseThenBreak(t *testing.T) {
	f := reconciling(t, "reconcile: {restart: true}\n", soloCrew)
	f.holds("dev", openTask("T-1", "dev"))
	f.tm.startErr = errors.New("no such directory\nanywhere")
	rs := f.s.State.Role("dev")

	// First try: at once. The pause before the next is reopenPause twice over.
	did := f.beat(time.Minute)
	assert.Contains(t, strings.Join(did, "|"), "start dev: dev: no such directory\nanywhere")
	assert.Equal(t, 1, rs.QuickEnds)
	assert.False(t, rs.Broken)

	f.tm.starts = nil

	f.beat(9 * time.Minute)
	assert.Equal(t, 1, rs.QuickEnds, "no try inside the pause")

	f.beat(time.Minute)
	assert.Equal(t, 2, rs.QuickEnds, "the second try: ten minutes after the first")
	assert.False(t, rs.Broken)

	f.beat(19 * time.Minute)
	assert.Equal(t, 2, rs.QuickEnds, "twenty minutes now")

	f.beat(time.Minute)
	assert.Equal(t, 3, rs.QuickEnds)
	assert.True(t, rs.Broken, "tries, three, failed in a row")

	var told []string

	for _, n := range f.s.State.Notes {
		if strings.Contains(n, "could not be started") {
			told = append(told, n)
		}
	}

	require.Len(t, told, 1, "one note")
	assert.Equal(t, "dev: could not be started 3 times (last: no such directory anywhere); not tried again until zmina up dev", told[0])
	assert.Equal(t, "no such directory anywhere", clipRunes(oneLine("no such directory\nanywhere"), startReasonMax))
	assert.True(t, f.ev.has("dev", "broken"))

	// Broken: never tried again, however long, and not told again.
	for range 4 {
		f.beat(12 * time.Hour)
	}

	assert.Equal(t, 3, rs.QuickEnds)

	count := 0

	for _, n := range f.s.State.Notes {
		if strings.Contains(n, "could not be started") {
			count++
		}
	}

	assert.Equal(t, 1, count)

	// A hand's up re-arms it: broken and the count are cleared, and the seat is wanted again when it falls.
	f.tm.startErr = nil
	require.NoError(t, f.s.Up("dev"))
	assert.False(t, rs.Broken)
	assert.Zero(t, rs.QuickEnds)

	f.tm.die("dev")
	f.beat(11 * time.Minute)
	assert.True(t, f.tm.Has("dev"), "wanted up again")
}

// The dispatcher is never marked broken: it is told once, when it is up to hear, and tried again on the growing pause.
func TestReconcileRestartNeverBreaksTheDispatcher(t *testing.T) {
	f := fixtureWith(t, "reconcile: {restart: true, tries: 2}\n"+soloCrew)
	f.tm.startErr = errors.New("tmux: no server")
	rs := f.s.State.Role("dispatcher")

	f.s.Tick()
	assert.Equal(t, 1, rs.QuickEnds)

	f.beat(10 * time.Minute)
	assert.Equal(t, 2, rs.QuickEnds)
	assert.False(t, rs.Broken, "never")

	told := 0

	for _, n := range f.s.State.Notes {
		if strings.Contains(n, "dispatcher: could not be started 2 times") {
			told++
		}
	}

	assert.Equal(t, 1, told, "once, at the count")

	for range 6 {
		f.beat(40 * time.Minute)
	}

	assert.False(t, rs.Broken)
	assert.Greater(t, rs.QuickEnds, 2, "still tried")

	f.tm.startErr = nil
	f.beat(40 * time.Minute)
	assert.True(t, f.tm.Has("dispatcher"))
}

// A seat that dies soon after a start is a try too: tries ends in a row mark it broken, with the keep: always note.
func TestReconcileRestartQuickEndsCountTowardTheTries(t *testing.T) {
	f := reconciling(t, "reconcile: {restart: true, tries: 2}\n", soloCrew)
	f.holds("dev", openTask("T-1", "dev"))
	f.beat(0) // dev is wanted: started
	require.True(t, f.tm.Has("dev"))

	f.tm.die("dev")
	f.beat(time.Minute)
	rs := f.s.State.Role("dev")
	assert.Equal(t, 1, rs.QuickEnds)
	assert.False(t, f.tm.Has("dev"), "the pause is ten minutes since its start now, not five")

	f.beat(10 * time.Minute)
	require.True(t, f.tm.Has("dev"), "started again")

	f.tm.die("dev")
	f.beat(time.Minute)

	assert.True(t, rs.Broken)
	assert.Contains(t, strings.Join(f.s.State.Notes, "|"), "dev: dies on boot (2 ends within 10 min of a start), not started again until zmina up dev")

	f.beat(time.Hour)
	assert.False(t, f.tm.Has("dev"))
}

// A session that ended with a terminal attached was most likely ended by whoever sat there: the seat is left down, held, and
// the note says how to start it. The dispatcher is not: it is always started again.
func TestReconcileRestartLeavesASeatDownThatEndedWithATerminalAttached(t *testing.T) {
	f := reconciling(t, "reconcile: {restart: true}\n", soloCrew)
	f.holds("dev", openTask("T-1", "dev"))
	f.beat(time.Minute)
	require.True(t, f.tm.Has("dev"))

	f.tm.attached["dev"] = true
	f.tm.die("dev")
	did := f.beat(12 * time.Minute)

	assert.Contains(t, strings.Join(did, "|"), "dev: ended (held T-1); ended with a terminal attached: left down (zmina up dev)")
	assert.True(t, f.s.State.Role("dev").Held)
	assert.True(t, f.ev.has("dev", "held-attached"))
	assert.False(t, f.tm.Has("dev"))

	f.beat(2 * time.Hour)
	assert.False(t, f.tm.Has("dev"), "held until a hand's up")

	// The dispatcher with a terminal attached is started again as ever.
	f.tm.attached["dispatcher"] = true
	f.tm.die("dispatcher")
	f.beat(11 * time.Minute)
	assert.True(t, f.tm.Has("dispatcher"))
	assert.False(t, f.s.State.Role("dispatcher").Held)
}

// At most reconcileBeatMax seats other than the dispatcher are started in a beat, the keep: always ones counted: the rest wait for the next.
func TestReconcileRestartStartsAtMostTwoSeatsABeat(t *testing.T) {
	f := fixtureWith(t, "reconcile: {restart: true}\n"+crewYAML+"  dev:\n  dev2:\n  dev3:\n")
	f.holds("dev", openTask("T-1", "dev"))
	f.holds("dev2", openTask("T-2", "dev2"))
	f.holds("dev3", openTask("T-3", "dev3"))

	f.s.Tick()
	assert.Equal(t, []string{"dispatcher", "dev", "dev2"}, f.tm.starts, "the dispatcher is not counted: two seats, in the order of the crew")

	f.beat(time.Minute)
	assert.Equal(t, []string{"dispatcher", "dev", "dev2", "dev3", "harvester"}, f.tm.starts, "the rest on the next beat, in order, keep: always included")
}

// A session of the seat's name that runs outside zmina is not taken: the seat stays down, and the dispatcher is told once, again only
// after that session has been gone.
func TestReconcileRestartTellsOnceOfASessionOutsideZmina(t *testing.T) {
	f := reconciling(t, "reconcile: {restart: true}\n", soloCrew)
	f.rg.outside["zm-dev"] = &registry.Session{PID: 77, Name: "zm-dev", Cwd: "/work/api"}
	f.holds("dev", openTask("T-1", "dev"))

	told := func() int {
		n := 0

		for _, e := range f.ev.list {
			if e.Event == "reconcile-outside" && e.Role == "dev" {
				n++
			}
		}

		return n
	}

	for range 3 {
		f.beat(10 * time.Minute)
	}

	assert.False(t, f.tm.Has("dev"))
	assert.Equal(t, 1, told(), "once")
	assert.True(t, f.s.State.RecordNotified[outsideKey("dev")])
	assert.Contains(t, strings.Join(f.dispatcherLines(), "|"),
		"dev should be up (holds T-1), but a session named zm-dev already runs outside zmina (pid 77, /work/api): end it, then zmina up dev")

	// Gone: the key is cleared and the seat starts.
	delete(f.rg.outside, "zm-dev")
	f.beat(10 * time.Minute)
	assert.True(t, f.tm.Has("dev"))
	assert.False(t, f.s.State.RecordNotified[outsideKey("dev")])

	// And it may be told again when it comes back.
	f.tm.die("dev")
	f.rg.outside["zm-dev"] = &registry.Session{PID: 78, Name: "zm-dev", Cwd: "/work/api"}
	f.beat(11 * time.Minute)
	f.beat(10 * time.Minute)
	assert.Equal(t, 2, told())
}

// With the key off, absent or on for the other parts only, the beat is what it always was: a keep: always seat is started again on
// the same pause, a failed start is tried again each five minutes for ever with no note and no count, a manual seat that holds an
// open task is never started, none is held for a terminal, no session outside zmina is told, and the state carries nothing new.
func TestReconcileKeyOffLeavesTheBeatAsItWas(t *testing.T) {
	var states []string

	for name, key := range map[string]string{
		"absent":                    "",
		"off":                       "reconcile: off\n",
		"the other parts only":      "reconcile: {assign: true, orphans: off}\n",
		"the block with nothing on": "reconcile: {tries: 5}\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, key+reopenCrew)
			f.holds("dev", openTask("T-1", "dev"))

			did := f.s.Tick()
			assert.ElementsMatch(t, []string{"dispatcher", "harvester"}, f.tm.starts, "dev holds a task and is not started")
			assert.Contains(t, f.s.State.Notes, "harvester: started again (keep: always)")
			assert.Len(t, f.s.State.Notes, 2, "and the note about the task dev has not taken, as ever")
			assert.Contains(t, did, "started harvester")

			// A manual seat that holds an open task and whose session ended: noted, not started, not held, however long.
			require.NoError(t, f.s.Up("dev"))
			f.tm.starts = nil
			f.tm.attached["dev"] = true
			f.tm.die("dev")
			did = f.beat(12 * time.Minute)
			assert.Contains(t, did, "dev: ended (held T-1)")
			assert.False(t, f.s.State.Role("dev").Held, "no terminal rule without the key")
			f.beat(3 * time.Hour)
			assert.False(t, started(f, "dev"))

			// A keep: always seat that was attached when it ended is started again all the same, five minutes after its start.
			f.tm.attached["harvester"] = true
			f.tm.die("harvester")
			f.beat(time.Minute)
			assert.False(t, f.s.State.Role("harvester").Held)
			require.True(t, f.tm.Has("harvester"))

			// A failed start: the pause is five minutes however many fail, no count, never broken, nothing told.
			f.beat(20 * time.Minute)
			f.tm.startErr = errors.New("boom")
			f.tm.die("harvester")
			f.tm.starts = nil
			f.beat(time.Minute)

			hs := f.s.State.Role("harvester")
			assert.Zero(t, hs.QuickEnds, "a failed start counts for nothing")

			for range 8 {
				f.beat(5 * time.Minute)
				assert.False(t, hs.Broken)
			}

			assert.Zero(t, hs.QuickEnds)
			assert.Empty(t, f.tm.starts, "nothing starts while it fails")

			for _, n := range f.s.State.Notes {
				assert.NotContains(t, n, "could not be started")
			}

			// A session outside zmina keeps a keep: always seat down without a word.
			f.tm.startErr = nil
			f.rg.outside["zm-harvester"] = &registry.Session{PID: 9, Name: "zm-harvester"}
			f.beat(time.Hour)
			assert.False(t, f.tm.Has("harvester"))
			assert.Empty(t, f.s.State.RecordNotified, "no key of the new kinds")

			for _, e := range f.ev.list {
				assert.NotContains(t, []string{"held-attached", "reconcile-outside", "orphan", "orphan-down", "start-failing"}, e.Event)
			}

			raw, err := json.Marshal(f.s.State.Roles["dev"])
			require.NoError(t, err)
			states = append(states, string(raw))
		})
	}

	for _, s := range states[1:] {
		assert.JSONEq(t, states[0], s, "every spelling of off leaves the same state")
	}
}

// The pause before a keep: always seat is started again is five minutes with the key off: the same crash is the same wait, and with
// restart on it grows by the ends that came soon after a start.
func TestReconcileRestartLadderStartsAtTheOldPause(t *testing.T) {
	for _, key := range []string{"", "reconcile: {restart: true}\n"} {
		f := fixtureWith(t, key+crewYAML)
		f.s.Tick()
		f.beat(30 * time.Minute) // well past its quick end

		f.tm.die("harvester")
		f.tm.starts = nil
		f.beat(time.Minute)
		assert.True(t, started(f, "harvester"), "after a long run it is started again at once: %q", key)
	}
}

// A seat that dies after more than ten minutes each time is no quick end, and the pause of a failed start does not bound it: the beat
// starts it again only so many times in a day, then marks it broken with one note; a hand's up begins the count afresh.
func TestReconcileRestartCapsStartsOfASeatThatNeverFinishes(t *testing.T) {
	f := reconciling(t, "reconcile: {restart: true}\n", soloCrew)
	f.holds("dev", openTask("T-1", "dev"))
	rs := f.s.State.Role("dev")

	f.beat(0)
	require.True(t, f.tm.Has("dev"), "the first start")

	for i := 1; i < reconcileRestartCap; i++ {
		f.tm.die("dev")
		f.beat(11 * time.Minute)
		require.True(t, f.tm.Has("dev"), "start %d", i+1)
		require.False(t, rs.Broken)
	}

	assert.Equal(t, reconcileRestartCap, f.s.restartsSince("dev", *f.now))

	f.tm.die("dev")
	f.beat(11 * time.Minute)
	assert.False(t, f.tm.Has("dev"), "no more")
	assert.True(t, rs.Broken)
	assert.True(t, f.ev.has("dev", "broken"))

	count := func() int {
		n := 0

		for _, l := range f.dispatcherLines() {
			if strings.Contains(l, "started again 6 times in 24h and still holds T-1") {
				n++
			}
		}

		return n
	}

	assert.Equal(t, 1, count())
	assert.Contains(t, strings.Join(f.dispatcherLines(), "|"), "dev: started again 6 times in 24h and still holds T-1, so it is not making progress; not started again until zmina up dev")

	f.beat(3 * time.Hour)
	f.beat(3 * time.Hour)
	assert.Equal(t, 1, count(), "told once")
	assert.False(t, f.tm.Has("dev"))

	// A hand's up begins afresh: the count of starts is forgotten with the mark.
	require.NoError(t, f.s.Up("dev"))
	assert.False(t, rs.Broken)
	assert.Zero(t, f.s.restartsSince("dev", *f.now))

	f.tm.die("dev")
	f.beat(11 * time.Minute)
	assert.True(t, f.tm.Has("dev"), "wanted up again")
}

// Starts older than a day are not counted, and the beat forgets their records; a keep: always seat has no such cap.
func TestReconcileRestartCapForgetsADayAndSparesKeepAlways(t *testing.T) {
	f := reconciling(t, "reconcile: {restart: true}\n", soloCrew)
	f.holds("dev", openTask("T-1", "dev"))

	for i := range reconcileRestartCap {
		f.s.State.MarkRestart("dev", f.now.Add(-time.Duration(25+i)*time.Hour))
	}

	f.beat(time.Minute)
	assert.True(t, f.tm.Has("dev"), "all of them are past the day")
	assert.Equal(t, 1, f.s.restartsSince("dev", *f.now))

	for key := range f.s.State.RecordNotified {
		if strings.HasPrefix(key, restartPrefix) {
			ts := strings.TrimPrefix(key, restartPrefix+"dev/")
			assert.NotEmpty(t, ts)
			assert.Equal(t, strconvInt(f.now.Unix()), ts, "only this beat's start is kept: %s", key)
		}
	}

	h := fixtureWith(t, "reconcile: {restart: true}\n"+crewYAML)
	h.s.Tick()

	for range reconcileRestartCap + 2 {
		h.tm.die("harvester")
		h.beat(11 * time.Minute)
		require.True(t, h.tm.Has("harvester"))
	}

	assert.False(t, h.s.State.Role("harvester").Broken)
	assert.Zero(t, h.s.restartsSince("harvester", *h.now), "a keep: always seat is not counted")
}

// A keep: always seat is started again whoever sat in its terminal, with the key on too: only a seat that is not keep: always is held
// for a terminal attached when it ended.
func TestReconcileRestartHoldsNoKeepAlwaysSeatForATerminal(t *testing.T) {
	f := fixtureWith(t, "reconcile: {restart: true}\n"+crewYAML+"  dev:\n")
	f.holds("dev", openTask("T-1", "dev"))
	f.s.Tick()
	f.beat(11 * time.Minute)
	require.True(t, f.tm.Has("harvester"))
	require.True(t, f.tm.Has("dev"))

	f.tm.attached["harvester"] = true
	f.tm.attached["dev"] = true
	f.tm.die("harvester")
	f.tm.die("dev")
	did := f.beat(time.Minute)

	assert.False(t, f.s.State.Role("harvester").Held, "keep: always: started again")
	assert.True(t, f.tm.Has("harvester"))
	assert.NotContains(t, strings.Join(did, "|"), "harvester: ended; ended with a terminal attached")

	assert.True(t, f.s.State.Role("dev").Held, "a seat kept up for its task alone is held")
	assert.False(t, f.tm.Has("dev"))
}

func strconvInt(n int64) string { return strconv.FormatInt(n, 10) }
