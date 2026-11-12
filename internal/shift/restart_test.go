package shift

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/chat"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/handoff"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
)

// restarting is the default crew (restart_at 250k, restart_idle 1h) with designer up past its boot, holding T-1,
// its conversation at the given size, idle since the given time and its last turn begun before that.
func restarting(t *testing.T, extra string, context int, idle time.Duration) fix {
	t.Helper()

	f := fixtureWith(t, crewYAML+extra)
	f.s.Tick() // the dispatcher and harvester start
	require.NoError(t, f.s.Up("designer"))
	*f.now = f.now.Add(5 * time.Minute)
	f.rg.activity = map[string]registry.Activity{}
	f.idle("designer", context, idle)
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open, Title: "draw the logo",
		Updated: f.now.Add(-3 * time.Hour).UTC()}}

	return f
}

// idle marks a role idle for a while with its conversation at a size, its last turn begun before that.
func (f fix) idle(role string, context int, idle time.Duration) {
	f.status(role, "idle", f.now.Add(-idle))
	f.rg.context["sid-"+role] = context
	f.rg.activity["sid-"+role] = registry.Activity{Prompt: f.now.Add(-idle - time.Minute)}
}

// note saves the role's checkpoint of a task at a time, as zmina task checkpoint does.
func (f fix) note(t *testing.T, id string, at time.Time) {
	t.Helper()

	tk, err := f.tk.Get(id)
	require.NoError(t, err)
	r, err := f.s.Role(tk.Role)
	require.NoError(t, err)
	shape := f.s.Crew.HandoffFor(r)
	values := map[string]string{}
	for _, sec := range shape.Sections {
		values[sec.Name] = "logo half drawn"
	}
	ref, err := record.Save(f.s.Crew.StateDir(), record.Document{Version: 1, Kind: "checkpoint", Task: id,
		Role: tk.Role, At: at.UTC(), Shape: shape, Values: values})
	require.NoError(t, err)
	tk.Checkpoint, tk.Updated, tk.CheckpointTaskUpdated = &ref, at.UTC(), at.UTC()
	require.NoError(t, f.tk.Put(tk))
}

// finish ends the downs the tick began and starts each restarted role again, as RunTick does outside the lock.
func (f fix) finish(t *testing.T) []string {
	t.Helper()

	var out []string
	for _, r := range f.s.Restarts {
		out = append(out, f.s.FinishRestart(r))
	}

	f.s.Restarts = nil

	return out
}

func TestRestartAsksForTheNoteThenStartsFresh(t *testing.T) {
	f := restarting(t, "", 262_000, 6*time.Minute)

	did := f.s.Tick()
	assert.Contains(t, did, "designer: context 262k over restart_at 250k; asked for its note to restart fresh")
	require.Len(t, f.tm.typed["designer"], 1)
	line := f.tm.typed["designer"][0]
	assert.True(t, strings.HasPrefix(line, "[zmina] restart: "), line)
	assert.True(t, f.ev.has("designer", "restart-asked"))

	paths, err := filepath.Glob(filepath.Join(f.s.Crew.StateDir(), "restarts", "*.json"))
	require.NoError(t, err)
	require.Len(t, paths, 1)
	assert.Contains(t, line, paths[0])
	raw, err := os.ReadFile(paths[0])
	require.NoError(t, err)
	var request struct {
		Tasks []handoff.Packet `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal(raw, &request))
	require.Len(t, request.Tasks, 1)
	assert.Contains(t, request.Tasks[0].Instructions, "task checkpoint T-1")
	assert.Contains(t, request.Tasks[0].Instructions, "Do not park")

	// It writes the note in a turn of its own; until that turn is over it is not put down.
	*f.now = f.now.Add(time.Minute)
	f.note(t, "T-1", *f.now)
	f.status("designer", "busy", *f.now)
	f.s.Tick()
	assert.Empty(t, f.s.Restarts, "never mid-turn")
	assert.Zero(t, compactCount(f.tm.typed["designer"]), "no trim while the restart waits")

	*f.now = f.now.Add(time.Minute)
	f.status("designer", "idle", *f.now)
	f.s.Tick()
	assert.Empty(t, f.s.Restarts, "the note's turn just ended: a beat settles first")

	*f.now = f.now.Add(restartDueSettle)
	did = f.s.Tick()
	require.Len(t, f.s.Restarts, 1)
	require.NotEmpty(t, did)
	assert.Contains(t, did[len(did)-1], "designer: context 262k over restart_at 250k; restarting fresh with its note (T-1 ")
	assert.Contains(t, f.tm.typed["designer"], "/exit")

	out := f.finish(t)
	require.Len(t, out, 1)
	assert.Contains(t, out[0], "designer: restarted fresh (context 262k over restart_at 250k); note T-1 ")
	assert.True(t, f.tm.Has("designer"))
	assert.True(t, f.ev.has("designer", "restarted"))
	rs := f.s.State.Role("designer")
	assert.Empty(t, f.s.Resumed("designer"), "a fresh conversation")
	require.Len(t, rs.Inbox, 2)
	assert.True(t, strings.HasPrefix(rs.Inbox[0], "[zmina] restarted fresh: context 262k over restart_at 250k. Your note: T-1 "),
		"the first line Talk shows says why: %s", rs.Inbox[0])
	assert.True(t, strings.HasPrefix(rs.Inbox[1], "[zmina T-1] Read handoff packet "), rs.Inbox[1])

	tk, err := f.tk.Get("T-1")
	require.NoError(t, err)
	packet, err := handoff.Write(f.s.Crew.StateDir(), mustPacket(t, f, tk))
	require.NoError(t, err)
	body, err := os.ReadFile(packet)
	require.NoError(t, err)
	assert.Contains(t, string(body), tk.Checkpoint.Revision, "the packet carries the note")
}

func mustPacket(t *testing.T, f fix, tk task.Task) handoff.Packet {
	t.Helper()

	p, err := f.s.TaskPacket(tk.Role, tk)
	require.NoError(t, err)

	return p
}

// A role that wrote its checkpoint after its last turn began is restarted at once, not asked again.
func TestRestartTakesANoteAlreadyWritten(t *testing.T) {
	f := restarting(t, "", 300_000, 10*time.Minute)
	wt := t.TempDir()
	f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: "designer", State: task.Waiting, Worktree: wt})
	f.note(t, "T-1", f.now.Add(-10*time.Minute))
	f.note(t, "T-2", f.now.Add(-9*time.Minute))

	did := f.s.Tick()
	require.Len(t, f.s.Restarts, 1, "%v", did)
	assert.Equal(t, []string{"/exit"}, f.tm.typed["designer"], "no ask: the notes are there")
	assert.False(t, f.ev.has("designer", "restart-asked"))

	f.finish(t)
	rs := f.s.State.Role("designer")
	assert.Equal(t, wt, rs.Dir, "fresh in the worktree of a task it holds")
	require.Len(t, rs.Inbox, 3, "the Talk line, then a packet per task")
	assert.Contains(t, rs.Inbox[0], "Your note: T-1 ")
	assert.Contains(t, rs.Inbox[0], ", T-2 ")
	assert.True(t, strings.HasPrefix(rs.Inbox[2], "[zmina T-2] Read handoff packet "))
}

func TestRestartWaitsForBetweenTurns(t *testing.T) {
	cases := map[string]func(f fix){
		"settling":         func(f fix) { f.idle("designer", 262_000, 2*time.Minute) },
		"busy":             func(f fix) { f.status("designer", "busy", f.now.Add(-time.Hour)) },
		"background shell": func(f fix) { f.rg.activity["sid-designer"] = registry.Activity{Background: []string{"b1"}} },
		"draft":            func(f fix) { f.tm.drafts["designer"] = "one more thing" },
		"attached":         func(f fix) { f.tm.attached["designer"] = true },
		"queued":           func(f fix) { f.s.State.Role("designer").Inbox = []string{"[zmina] hello"} },
		"owner in the terminal": func(f fix) {
			f.rg.activity["sid-designer"] = registry.Activity{Prompt: f.now.Add(-10 * time.Minute), Owner: f.now.Add(-10 * time.Minute)}
		},
		"owner in Talk": func(f fix) {
			f.s.State.Role("designer").ChatReceipts = []chat.Receipt{{ID: "c1", At: f.now.Add(-14 * time.Minute)}}
		},
		"no task":    func(f fix) { f.tk.list = nil },
		"small":      func(f fix) { f.idle("designer", 240_000, 6*time.Minute) },
		"restart at": func(fix) {},
	}

	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			f := restarting(t, "", 262_000, 6*time.Minute)
			if name == "restart at" {
				f = restarting(t, "restart_at: 300k\n", 262_000, 6*time.Minute)
			} else {
				set(f)
			}

			f.s.Tick()
			assert.False(t, f.ev.has("designer", "restart-asked"))
			assert.Empty(t, f.s.Restarts)
		})
	}
}

func TestRestartAfterTheOwnerWentQuiet(t *testing.T) {
	f := restarting(t, "", 262_000, 16*time.Minute)
	f.rg.activity["sid-designer"] = registry.Activity{Prompt: f.now.Add(-17 * time.Minute), Owner: f.now.Add(-17 * time.Minute)}
	f.s.State.Role("designer").ChatReceipts = []chat.Receipt{{ID: "c1", At: f.now.Add(-17 * time.Minute)}}

	f.s.Tick()
	assert.True(t, f.ev.has("designer", "restart-asked"))
}

func TestRestartNeverTheDispatcher(t *testing.T) {
	f := restarting(t, "", 262_000, 6*time.Minute)
	f.idle("dispatcher", 400_000, 2*time.Hour)
	f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: "dispatcher", State: task.Open})

	f.s.Tick()
	assert.False(t, f.ev.has("dispatcher", "restart-asked"))
	assert.NotContains(t, strings.Join(f.tm.typed["dispatcher"], "|"), "restart")
}

func TestRestartOffForTheCrewOrASeat(t *testing.T) {
	for _, extra := range []string{"restart_at: off\nrestart_idle: off\n", "    restart_at: off\n"} {
		body := extra + crewYAML
		if strings.HasPrefix(extra, "    ") {
			body = strings.Replace(crewYAML, "  designer:\n", "  designer:\n"+extra, 1)
		}

		f := fixtureWith(t, body)
		f.s.Tick()
		require.NoError(t, f.s.Up("designer"))
		*f.now = f.now.Add(5 * time.Minute)
		f.rg.activity = map[string]registry.Activity{}
		f.idle("designer", 262_000, 6*time.Minute)
		f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open}}

		f.s.Tick()
		assert.False(t, f.ev.has("designer", "restart-asked"), extra)
	}
}

func TestRestartBeforeACacheGoesCold(t *testing.T) {
	f := restarting(t, "", 140_000, 49*time.Minute)

	f.s.Tick()
	assert.False(t, f.ev.has("designer", "restart-asked"), "idle 49m: the cache is warm, nothing to save yet")

	f.idle("designer", 140_000, 51*time.Minute)
	did := f.s.Tick()
	assert.Contains(t, did, "designer: idle 51m at 140k, near restart_idle 1h; asked for its note to restart fresh")

	f.idle("designer", 90_000, 3*time.Hour)
	f.s.State.Role("designer").RestartAsked = time.Time{}
	f.s.State.Role("designer").RestartOff = f.now.Add(-time.Hour)
	f.s.State.Role("designer").Inbox = nil
	f.s.Tick()
	assert.Equal(t, 1, f.ev.count("designer", "restart-asked"), "under 100k a cold wake rewrites little")
}

func TestRestartCalledOff(t *testing.T) {
	cases := map[string]func(f fix){
		"a new task": func(f fix) { f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: "designer", State: task.Open}) },
		"a draft":    func(f fix) { f.tm.drafts["designer"] = "wait" },
		"the owner": func(f fix) {
			f.s.State.Role("designer").ChatReceipts = []chat.Receipt{{ID: "c1", At: *f.now}}
		},
	}

	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			f := restarting(t, "", 262_000, 6*time.Minute)
			f.s.Tick()
			require.True(t, f.ev.has("designer", "restart-asked"))

			*f.now = f.now.Add(time.Minute)
			set(f)
			f.s.Tick()
			assert.True(t, f.ev.has("designer", "restart-off"))
			assert.True(t, f.s.State.Role("designer").RestartAsked.IsZero())
			assert.Empty(t, f.s.Restarts)
		})
	}
}

func TestRestartGivesUpWithoutANote(t *testing.T) {
	f := restarting(t, "", 262_000, 6*time.Minute)
	f.s.Tick()

	*f.now = f.now.Add(restartNoteWait)
	f.idle("designer", 270_000, 6*time.Minute)
	f.s.Tick()
	assert.True(t, f.ev.has("designer", "restart-off"))
	assert.Contains(t, strings.Join(f.s.State.Notes, "|"), "designer: wrote no note within 10m0s")
	assert.Equal(t, "sid-designer", f.s.State.Role("designer").RestartGaveUp)

	*f.now = f.now.Add(time.Hour)
	f.idle("designer", 280_000, 6*time.Minute)
	f.s.Tick()
	assert.Equal(t, 1, f.ev.count("designer", "restart-asked"), "not asked again in this conversation")
}

func TestRestartTakenOverMeanwhile(t *testing.T) {
	f := restarting(t, "", 300_000, 10*time.Minute)
	f.note(t, "T-1", f.now.Add(-10*time.Minute))
	f.s.Tick()
	require.Len(t, f.s.Restarts, 1)

	// zmina up by hand while the down waited outside the lock.
	require.NoError(t, f.s.FinishDown("designer"))
	require.NoError(t, f.s.Up("designer"))
	out := f.finish(t)
	assert.Equal(t, []string{"designer: restart: another command took it over meanwhile"}, out)
	assert.Empty(t, f.s.State.Role("designer").Inbox)
}

func (f *fakeEvents) count(role, event string) int {
	n := 0

	for _, e := range f.list {
		if e.Role == role && e.Event == event {
			n++
		}
	}

	return n
}

// Background work whose end never came holds the restart; the log says so once, not every beat.
func TestRestartHeldByBackgroundSaidOnce(t *testing.T) {
	f := restarting(t, "", 262_000, 6*time.Minute)
	act := f.rg.activity["sid-designer"]
	act.Background = []string{"b1"}
	f.rg.activity["sid-designer"] = act

	did := f.s.Tick()
	assert.Contains(t, did, "designer: restart held: background b1 running")
	*f.now = f.now.Add(time.Minute)
	f.idle("designer", 262_000, 7*time.Minute)
	f.rg.activity["sid-designer"] = registry.Activity{Prompt: act.Prompt, Background: []string{"b1"}}
	did = f.s.Tick()
	assert.NotContains(t, did, "designer: restart held: background b1 running")
	assert.Equal(t, 1, f.ev.count("designer", "restart-held"))
	assert.Empty(t, f.s.Restarts)

	// Another launch is said again; once it all ended the restart goes on.
	f.rg.activity["sid-designer"] = registry.Activity{Prompt: act.Prompt, Background: []string{"b1", "m2"}}
	f.s.Tick()
	assert.Equal(t, 2, f.ev.count("designer", "restart-held"))

	f.rg.activity["sid-designer"] = registry.Activity{Prompt: act.Prompt}
	f.s.Tick()
	assert.True(t, f.ev.has("designer", "restart-asked"))
}

// A copy of a role holding its own task is restarted as itself: the same copy, fresh, with its task's packet.
func TestRestartACopy(t *testing.T) {
	f := restarting(t, "", 100, time.Minute)
	f.tk.list = nil
	require.NoError(t, f.s.Up("designer#2"))
	*f.now = f.now.Add(5 * time.Minute)
	f.idle("designer#2", 300_000, 10*time.Minute)
	f.tk.list = []task.Task{{ID: "T-7", Role: "designer#2", State: task.Open, Title: "draw the icon"}}
	f.note(t, "T-7", f.now.Add(-10*time.Minute))

	did := f.s.Tick()
	require.Len(t, f.s.Restarts, 1, "%v", did)
	assert.Equal(t, "designer#2", f.s.Restarts[0].Name)
	assert.Contains(t, f.tm.typed["designer#2"], "/exit")
	assert.NotContains(t, f.tm.typed["designer"], "/exit", "the base role stays up")

	out := f.finish(t)
	require.Len(t, out, 1)
	assert.Contains(t, out[0], "designer#2: restarted fresh (context 300k over restart_at 250k); note T-7 ")
	assert.True(t, f.tm.Has("designer#2"))
	assert.True(t, f.ev.has("designer#2", "restarted"))
	rs := f.s.State.Role("designer#2")
	require.Len(t, rs.Inbox, 2)
	assert.True(t, strings.HasPrefix(rs.Inbox[1], "[zmina T-7] Read handoff packet "), rs.Inbox[1])
	assert.Contains(t, rs.Inbox[1], "--role 'designer#2'")
}

// doneRestarting is restarting with designer's T-1 closed done 5 minutes after its start, then the given time
// passed with designer idle since the close and its conversation at the given size.
func doneRestarting(t *testing.T, extra string, context int, after time.Duration) fix {
	t.Helper()

	f := restarting(t, extra, context, 0)
	closed := f.now.UTC()
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Done, Title: "draw the logo", Updated: closed,
		Closed: &task.Closure{By: "dispatcher", At: closed, State: task.Done}}}
	*f.now = f.now.Add(after)
	f.idle("designer", context, after)

	return f
}

// A role whose task is done and that holds none is restarted fresh once restartDoneGrace has passed: no note is
// asked for, it starts in its own dir, its first line says why, and it is restarted once for that task.
func TestRestartAfterTheTaskIsDone(t *testing.T) {
	f := doneRestarting(t, "", 180_000, 11*time.Minute)

	did := f.s.Tick()
	require.Len(t, f.s.Restarts, 1, "%v", did)
	job := f.s.Restarts[0]
	assert.Equal(t, "T-1", job.Done)
	assert.Empty(t, job.Tasks)
	assert.Empty(t, job.Opts.Dir, "its own dir, not the closed task's worktree")
	assert.True(t, job.Opts.Fresh)
	assert.Contains(t, did, "designer: task done: T-1, context 180k over restart_done 150k; restarting fresh")
	assert.False(t, f.ev.has("designer", "restart-asked"), "no note is asked for")
	assert.Contains(t, f.tm.typed["designer"], "/exit")

	out := f.finish(t)
	require.Len(t, out, 1)
	assert.Equal(t, "designer: restarted fresh (task done: T-1, context 180k over restart_done 150k)", out[0])
	rs := f.s.State.Role("designer")
	require.Len(t, rs.Inbox, 1, "no task packet: it holds none")
	assert.Equal(t, "[zmina] restarted fresh: task done: T-1, context 180k over restart_done 150k. You hold no "+
		"task; what it ended with: zmina task show T-1.", rs.Inbox[0])

	var restarted []string
	for _, e := range f.ev.list {
		if e.Role == "designer" && e.Event == "restarted" {
			restarted = append(restarted, e.Task+" "+e.Detail)
		}
	}
	assert.Equal(t, []string{"T-1 task done: T-1, context 180k over restart_done 150k"}, restarted)

	// The fresh conversation grows past restart_done again: it started after the close, so no second restart.
	*f.now = f.now.Add(30 * time.Minute)
	rs.Inbox = nil
	f.idle("designer", 200_000, 20*time.Minute)
	f.s.Tick()
	assert.Empty(t, f.s.Restarts, "once a task")
}

func TestRestartAfterTheTaskIsDoneWaits(t *testing.T) {
	for name, c := range map[string]struct {
		extra   string
		context int
		after   time.Duration
		change  func(f fix)
	}{
		"within the grace":    {context: 180_000, after: 9 * time.Minute},
		"under restart_done":  {context: 140_000, after: 11 * time.Minute},
		"restart_done off":    {extra: "restart_done: \"off\"\n", context: 400_000, after: 30 * time.Minute},
		"a turn in the grace": {context: 180_000, after: 11 * time.Minute, change: func(f fix) { f.status("designer", "idle", f.now.Add(-5*time.Minute)) }},
		"the owner wrote": {context: 180_000, after: 30 * time.Minute, change: func(f fix) {
			f.rg.activity["sid-designer"] = registry.Activity{Prompt: f.now.Add(-20 * time.Minute), Owner: f.now.Add(-10 * time.Minute)}
		}},
		"reopened": {context: 180_000, after: 11 * time.Minute, change: func(f fix) {
			f.tk.list[0].State, f.tk.list[0].Closed = task.Open, nil
		}},
		"a new task": {context: 180_000, after: 11 * time.Minute, change: func(f fix) {
			f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: "designer", State: task.Open, Title: "the icon"})
		}},
		"closed before its start": {context: 180_000, after: 11 * time.Minute, change: func(f fix) {
			f.tk.list[0].Closed.At = f.now.Add(-3 * time.Hour)
			f.tk.list[0].Updated = f.tk.list[0].Closed.At
		}},
		"dropped, not done": {context: 180_000, after: 11 * time.Minute, change: func(f fix) {
			f.tk.list[0].State, f.tk.list[0].Closed.State = task.Dropped, task.Dropped
		}},
		"another role's task": {context: 180_000, after: 11 * time.Minute, change: func(f fix) {
			f.tk.list[0].Role = "harvester"
		}},
		"a draft": {context: 180_000, after: 11 * time.Minute, change: func(f fix) { f.tm.drafts["designer"] = "and the" }},
		"a line after the close": {context: 180_000, after: 30 * time.Minute, change: func(f fix) {
			// A peer's line 30 s after the close began a turn (a review loop); it has sat 29 minutes since.
			f.rg.activity["sid-designer"] = registry.Activity{Prompt: f.now.Add(-30*time.Minute + 30*time.Second)}
		}},
		"the owner's Chat after the close": {context: 180_000, after: 40 * time.Minute, change: func(f fix) {
			f.s.State.Role("designer").ChatReceipts = []chat.Receipt{{ID: "c1", At: f.now.Add(-20 * time.Minute)}}
		}},
		"a line queued for it": {context: 180_000, after: 11 * time.Minute, change: func(f fix) {
			f.s.State.Role("designer").Inbox = []string{"[zmina] a line from the dispatcher"}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := doneRestarting(t, c.extra, c.context, c.after)
			if c.change != nil {
				c.change(f)
			}

			did := f.s.Tick()
			assert.Empty(t, f.s.Restarts, "%v", did)
			assert.NotContains(t, f.tm.typed["designer"], "/exit")
		})
	}
}

// Background work holds the restart after a task off, and the log says so once.
func TestRestartAfterTheTaskIsDoneHeldByBackground(t *testing.T) {
	f := doneRestarting(t, "", 180_000, 11*time.Minute)
	act := f.rg.activity["sid-designer"]
	act.Background = []string{"b7"}
	f.rg.activity["sid-designer"] = act

	f.s.Tick()
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	assert.Empty(t, f.s.Restarts)
	assert.Equal(t, 1, f.ev.count("designer", "restart-held"))
}

// A beat that dies between beginning a restart and finishing it (the ticker killed during the wait): the next beat
// past downingFor finishes it from the saved job, once.
func TestRestartFinishedAfterItsBeatDied(t *testing.T) {
	f := restarting(t, "", 262_000, 6*time.Minute)
	f.s.Tick() // asks for the note
	*f.now = f.now.Add(time.Minute)
	f.note(t, "T-1", *f.now)
	f.status("designer", "idle", *f.now)
	*f.now = f.now.Add(restartDueSettle)
	f.s.Tick()
	require.Len(t, f.s.Restarts, 1)

	rs := f.s.State.Role("designer")
	require.NotNil(t, rs.Pending, "the job is saved with the down")
	assert.Equal(t, pendingRestart, rs.Pending.Kind)
	assert.Equal(t, []string{"T-1"}, rs.Pending.Tasks)

	// The process dies: its in-memory job is gone; the session ended on the /exit.
	f.s.Restarts = nil
	f.tm.die("designer")

	*f.now = f.now.Add(30 * time.Second)
	f.s.Tick()
	assert.False(t, f.tm.Has("designer"), "inside downingFor the beat that began it may still finish it")

	*f.now = f.now.Add(downingFor)
	starts := len(f.tm.starts)
	did := f.s.Tick()
	assert.Contains(t, strings.Join(did, "|"), "designer: restarted fresh")
	assert.True(t, f.tm.Has("designer"))
	assert.Len(t, f.tm.starts, starts+1)
	assert.Empty(t, f.s.Resumed("designer"), "fresh, as the restart meant")
	assert.Nil(t, rs.Pending)
	assert.True(t, rs.Downing.IsZero())
	require.Len(t, rs.Inbox, 2)
	assert.True(t, strings.HasPrefix(rs.Inbox[1], "[zmina T-1] Read handoff packet "), rs.Inbox[1])
	assert.True(t, f.ev.has("designer", "restart-late"))

	*f.now = f.now.Add(downingFor)
	f.s.Tick()
	assert.Len(t, f.tm.starts, starts+1, "finished once")
}

// A down whose own command died during the wait, with nothing saved to finish it: after staleDown the role stops
// being treated as going down, and the dispatcher hears it stayed down.
func TestStaleDownIsCleared(t *testing.T) {
	f := restarting(t, "", 100_000, time.Minute)

	_, err := f.s.BeginDown("designer", DownOptions{})
	require.NoError(t, err)
	f.tm.die("designer")
	rs := f.s.State.Role("designer")
	require.Nil(t, rs.Pending, "a command's down saves no job")

	*f.now = f.now.Add(downingFor + time.Second)
	f.s.Tick()
	assert.False(t, rs.Downing.IsZero(), "not yet stale")

	*f.now = f.now.Add(staleDown)
	f.s.Tick()
	assert.True(t, rs.Downing.IsZero())
	assert.True(t, rs.Held, "it stays down: the owner put it down")
	assert.Contains(t, strings.Join(f.s.State.Notes, "|"), "designer: went down halfway")
	assert.True(t, f.ev.has("designer", "down-stale"))
}

// turnEnding is restarting on the role's own channel (its Stop hook runs), designer between turns: its last turn
// ended a few seconds ago, far inside restartSettle.
func turnEnding(t *testing.T, extra string, context int) (fix, *fakePeer) {
	t.Helper()

	f := restarting(t, extra, context, 5*time.Second)
	p := newFakePeer()
	f.s.Peer = p
	rs := f.s.State.Role("designer")
	rs.Inbound, rs.Tag = true, "abcdefghijkl"
	f.rg.endedAt["sid-designer"] = f.now.Add(-5 * time.Second)

	return f, p
}

// A busy role never sits idle restartSettle: the beat alone leaves it, its turn's end flags it and the next beat
// restarts it from the note it saved in that turn.
func TestRestartAtTheFirstTurnEndWithItsNote(t *testing.T) {
	f, p := turnEnding(t, "", 262_000)
	f.note(t, "T-1", f.now.Add(-30*time.Second))

	f.s.Tick()
	require.Empty(t, f.s.Restarts, "idle 5s: the beat alone waits for the settle")

	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"), "the note is there: nothing to ask for")
	assert.Equal(t, "sid-designer", f.s.State.Role("designer").RestartDue)
	assert.Equal(t, 1, f.ev.count("designer", "restart-due"))
	assert.Empty(t, f.s.Restarts, "the down is the beat's")
	assert.Empty(t, p.posted["sid-designer"])

	*f.now = f.now.Add(restartDueSettle)
	did := f.s.Tick()
	require.Len(t, f.s.Restarts, 1, "%v", did)
	assert.Equal(t, []string{"T-1"}, f.s.Restarts[0].Tasks)
	assert.False(t, f.ev.has("designer", "restart-asked"))
	assert.Empty(t, f.s.State.Role("designer").RestartDue, "the flag goes with the restart")
}

// Without a note the turn's end asks for it at once; the role is restarted at the first beat after it is saved and
// settled (restartDueSettle).
func TestRestartAsksAtTheTurnEnd(t *testing.T) {
	f, p := turnEnding(t, "", 262_000)

	did := f.s.TurnEnd("designer", "sid-designer")
	require.Len(t, did, 2, "%v", did)
	assert.Equal(t, "designer: context 262k over restart_at 250k; asked for its note to restart fresh", did[0])
	require.Len(t, p.posted["sid-designer"], 1)
	assert.True(t, strings.HasPrefix(p.posted["sid-designer"][0], "[zmina] restart: "))
	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"), "asked once")

	p.showAll()
	*f.now = f.now.Add(40 * time.Second)
	f.note(t, "T-1", *f.now)
	f.status("designer", "idle", *f.now)
	f.rg.endedAt["sid-designer"] = *f.now
	f.s.TurnEnd("designer", "sid-designer")
	assert.Empty(t, f.s.Restarts)

	*f.now = f.now.Add(10 * time.Second)
	f.s.Tick()
	assert.Empty(t, f.s.Restarts, "the note's turn ended 10s ago: it settles first")

	*f.now = f.now.Add(restartDueSettle)
	did = f.s.Tick()
	require.Len(t, f.s.Restarts, 1, "%v", did)
	assert.Equal(t, 1, f.ev.count("designer", "restart-asked"))
}

// At 80% of restart_at the role is asked for its note once a conversation, and nothing restarts it yet.
func TestRestartAsksForTheNoteAhead(t *testing.T) {
	f, p := turnEnding(t, "", 205_000)

	did := f.s.TurnEnd("designer", "sid-designer")
	require.Len(t, did, 2, "%v", did)
	assert.Equal(t, "designer: context 205k near restart_at 250k; asked for its note ahead of the restart", did[0])
	require.Len(t, p.posted["sid-designer"], 1)
	line := p.posted["sid-designer"][0]
	assert.True(t, strings.HasPrefix(line, "[zmina] restart ahead: your conversation is at 205k; past restart_at 250k "), line)
	paths, err := filepath.Glob(filepath.Join(f.s.Crew.StateDir(), "restarts", "*.json"))
	require.NoError(t, err)
	require.Len(t, paths, 1)
	assert.Contains(t, line, paths[0])

	rs := f.s.State.Role("designer")
	assert.Equal(t, "sid-designer", rs.RestartNoted)
	assert.True(t, rs.RestartAsked.IsZero(), "no wait runs: nothing to give up or call off")
	assert.Empty(t, rs.RestartDue)

	p.showAll()
	*f.now = f.now.Add(time.Minute)
	f.idle("designer", 220_000, 5*time.Second)
	f.rg.endedAt["sid-designer"] = f.now.Add(-5 * time.Second)
	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"), "once a conversation")
	f.s.Tick()
	assert.Empty(t, f.s.Restarts)
	assert.Equal(t, 1, f.ev.count("designer", "restart-note"))

	// Under 80% nothing is asked.
	f, p = turnEnding(t, "", 199_000)
	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"))
	assert.Empty(t, p.posted["sid-designer"])
}

// The early ask is made once a climb: a compaction or a trim that drops the conversation well under restart_at lets
// its next climb to 80% be asked again, and a size that only dips does not.
func TestRestartAsksAheadAgainAfterACompaction(t *testing.T) {
	f, p := turnEnding(t, "", 205_000)
	ends := func(context int) []string { // a turn ends at a size, a minute after the last
		p.showAll()
		*f.now = f.now.Add(time.Minute)
		f.idle("designer", context, 5*time.Second)
		f.rg.endedAt["sid-designer"] = f.now.Add(-5 * time.Second)

		return f.s.TurnEnd("designer", "sid-designer")
	}

	require.Len(t, f.s.TurnEnd("designer", "sid-designer"), 2)
	require.Equal(t, 1, f.ev.count("designer", "restart-note"))

	assert.Empty(t, ends(215_000), "once a climb")
	assert.Empty(t, ends(150_000), "a dip to 60% is no compaction")
	assert.Empty(t, ends(210_000), "the dip asks no second time")
	assert.Equal(t, 1, f.ev.count("designer", "restart-note"))

	assert.Empty(t, ends(30_000), "compacted: nothing to ask at 12%")
	assert.Empty(t, f.s.State.Role("designer").RestartNoted, "and the ask is forgotten")
	assert.Empty(t, ends(190_000), "the climb is under 80% yet")
	assert.Equal(t, 1, f.ev.count("designer", "restart-note"))

	did := ends(205_000)
	require.Len(t, did, 2, "%v", did)
	assert.Equal(t, "designer: context 205k near restart_at 250k; asked for its note ahead of the restart", did[0])
	assert.Equal(t, 2, f.ev.count("designer", "restart-note"))
	assert.Equal(t, "sid-designer", f.s.State.Role("designer").RestartNoted)

	assert.Empty(t, ends(230_000), "once this climb too")
	assert.Equal(t, 2, f.ev.count("designer", "restart-note"))
}

// A size not known (0, right after a compaction before a turn shows it) and a turn not over forget nothing.
func TestRestartAheadIsKeptWhereTheSizeIsNotKnown(t *testing.T) {
	f, p := turnEnding(t, "", 205_000)
	require.Len(t, f.s.TurnEnd("designer", "sid-designer"), 2)
	p.showAll()

	f.rg.context["sid-designer"] = 0
	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"))
	assert.Equal(t, "sid-designer", f.s.State.Role("designer").RestartNoted)

	f.rg.context["sid-designer"] = 30_000
	delete(f.rg.endedAt, "sid-designer")
	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"))
	assert.Equal(t, "sid-designer", f.s.State.Role("designer").RestartNoted, "mid-turn: not a turn's end")
}

// The flag of a conversation over restart_at that nothing could restart goes with a compaction, so its next climb is
// flagged (and logged) anew.
func TestRestartDueIsForgottenAfterACompaction(t *testing.T) {
	f, _ := turnEnding(t, "", 262_000)
	f.rg.activity["sid-designer"] = registry.Activity{Background: []string{"b1"}} // holds the restart off

	f.s.TurnEnd("designer", "sid-designer")
	require.Equal(t, "sid-designer", f.s.State.Role("designer").RestartDue)
	require.Equal(t, 1, f.ev.count("designer", "restart-due"))

	*f.now = f.now.Add(time.Minute)
	f.idle("designer", 30_000, 5*time.Second)
	f.rg.activity["sid-designer"] = registry.Activity{Background: []string{"b1"}}
	f.rg.endedAt["sid-designer"] = f.now.Add(-5 * time.Second)
	f.s.TurnEnd("designer", "sid-designer")
	assert.Empty(t, f.s.State.Role("designer").RestartDue)

	*f.now = f.now.Add(time.Minute)
	f.idle("designer", 262_000, 5*time.Second)
	f.rg.activity["sid-designer"] = registry.Activity{Background: []string{"b1"}}
	f.rg.endedAt["sid-designer"] = f.now.Add(-5 * time.Second)
	f.s.TurnEnd("designer", "sid-designer")
	assert.Equal(t, 2, f.ev.count("designer", "restart-due"))
}

// A note asked for at a turn's end and not written within restartNoteWait gives the restart up, as from a beat.
func TestRestartAskedAtTheTurnEndGivesUpWithoutANote(t *testing.T) {
	f, p := turnEnding(t, "", 262_000)
	f.s.TurnEnd("designer", "sid-designer")
	require.True(t, f.ev.has("designer", "restart-asked"))
	p.showAll()

	*f.now = f.now.Add(restartNoteWait)
	f.idle("designer", 270_000, 5*time.Second)
	f.rg.endedAt["sid-designer"] = f.now.Add(-5 * time.Second)
	f.s.Tick()
	assert.Equal(t, "sid-designer", f.s.State.Role("designer").RestartGaveUp)
	assert.Contains(t, strings.Join(f.s.State.Notes, "|"), "designer: wrote no note within 10m0s")

	*f.now = f.now.Add(time.Hour)
	f.idle("designer", 280_000, 5*time.Second)
	f.rg.endedAt["sid-designer"] = f.now.Add(-5 * time.Second)
	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"), "not in this conversation again")
	f.s.Tick()
	assert.Equal(t, 1, f.ev.count("designer", "restart-asked"))
	assert.Empty(t, f.s.Restarts)
}

// What holds a restart at a beat holds it at a turn's end and at the beat after it: the flag shortens the settle only.
func TestRestartAtTheTurnEndKeepsItsGuards(t *testing.T) {
	cases := map[string]func(f fix){
		"owner in the terminal": func(f fix) {
			f.rg.activity["sid-designer"] = registry.Activity{Prompt: f.now.Add(-10 * time.Minute), Owner: f.now.Add(-10 * time.Minute)}
		},
		"owner in Talk": func(f fix) {
			f.s.State.Role("designer").ChatReceipts = []chat.Receipt{{ID: "c1", At: f.now.Add(-10 * time.Minute)}}
		},
		"draft":            func(f fix) { f.tm.drafts["designer"] = "one more thing" },
		"attached":         func(f fix) { f.tm.attached["designer"] = true },
		"background shell": func(f fix) { f.rg.activity["sid-designer"] = registry.Activity{Background: []string{"b1"}} },
		"mid-turn":         func(f fix) { delete(f.rg.endedAt, "sid-designer") },
		"busy":             func(f fix) { f.status("designer", "busy", *f.now) },
	}

	for _, context := range []int{262_000, 205_000} {
		for name, set := range cases {
			t.Run(name+" "+Tokens(context), func(t *testing.T) {
				f, p := turnEnding(t, "", context)
				f.note(t, "T-1", f.now.Add(-30*time.Second))
				f.rg.activity["sid-designer"] = registry.Activity{Prompt: f.now.Add(-20 * time.Minute)} // the note is older than the turn
				set(f)

				f.s.TurnEnd("designer", "sid-designer")
				*f.now = f.now.Add(restartDueSettle)
				f.s.Tick()
				assert.Empty(t, p.posted["sid-designer"], "nothing asked")
				assert.False(t, f.ev.has("designer", "restart-asked"))
				assert.False(t, f.ev.has("designer", "restart-note"))
				assert.Empty(t, f.s.Restarts)
				assert.Empty(t, f.s.State.Role("designer").RestartNoted, "asked ahead at a later turn's end")
			})
		}
	}
}

// A flag is its conversation's: the beat ignores one left by another, and one whose turn is no longer over.
func TestRestartDueIsItsConversations(t *testing.T) {
	f, _ := turnEnding(t, "", 262_000)
	f.note(t, "T-1", f.now.Add(-30*time.Second))
	f.s.State.Role("designer").RestartDue = "sid-before"

	*f.now = f.now.Add(restartDueSettle)
	f.s.Tick()
	assert.Empty(t, f.s.Restarts)
}

// A flagged conversation is put down only once its last turn has ended restartDueSettle ago and no turn began since:
// a role that ended its turn waiting for a peer's reply is not taken at the very next beat, and the wait starts over
// with each turn the reply begins.
func TestRestartDueSettlesAFullBeat(t *testing.T) {
	f, _ := turnEnding(t, "", 262_000)
	f.note(t, "T-1", f.now.Add(-30*time.Second))
	f.s.TurnEnd("designer", "sid-designer") // flagged; the turn ended 5s ago
	require.Equal(t, "sid-designer", f.s.State.Role("designer").RestartDue)

	// The first beat after the turn's end, and every one inside the settle: nothing.
	*f.now = f.now.Add(20 * time.Second)
	f.s.Tick()
	assert.Empty(t, f.s.Restarts, "25s after the turn's end")

	*f.now = f.now.Add(restartDueSettle - 26*time.Second) // 89s since the turn ended
	f.s.Tick()
	assert.Empty(t, f.s.Restarts, "one second short of the settle")
	assert.False(t, f.ev.has("designer", "restart-asked"), "the note is there: nothing is asked")

	// The peer's reply begins a turn at 89s and it ends at 100s: the settle counts from that end.
	f.status("designer", "busy", *f.now)
	delete(f.rg.endedAt, "sid-designer")
	*f.now = f.now.Add(11 * time.Second)
	f.status("designer", "idle", *f.now)
	f.rg.endedAt["sid-designer"] = *f.now
	f.s.TurnEnd("designer", "sid-designer")
	*f.now = f.now.Add(restartDueSettle - time.Second)
	f.s.Tick()
	assert.Empty(t, f.s.Restarts, "89s after the new turn's end, 178s after the first")

	*f.now = f.now.Add(time.Second)
	did := f.s.Tick()
	require.Len(t, f.s.Restarts, 1, "%v", did)
	assert.Equal(t, []string{"T-1"}, f.s.Restarts[0].Tasks)
}

// A task closed while the role holds another is a turn's end like any: over restart_at it restarts then, for the
// task it still holds.
func TestRestartAtATaskBoundary(t *testing.T) {
	f, _ := turnEnding(t, "", 262_000)
	f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: "designer", State: task.Open, Title: "draw the icon"})
	f.note(t, "T-2", f.now.Add(-30*time.Second))
	closed := f.now.Add(-10 * time.Second).UTC()
	f.tk.list[0].State, f.tk.list[0].Updated = task.Done, closed
	f.tk.list[0].Closed = &task.Closure{By: "designer", At: closed, State: task.Done}

	f.s.TurnEnd("designer", "sid-designer")
	*f.now = f.now.Add(restartDueSettle)
	did := f.s.Tick()
	require.Len(t, f.s.Restarts, 1, "%v", did)
	assert.Equal(t, []string{"T-2"}, f.s.Restarts[0].Tasks)
	assert.Empty(t, f.s.Restarts[0].Done)
	assert.False(t, f.ev.has("designer", "restart-asked"))
}

// A role asked for its note that closes its last task instead: the ask ends without the pause, so restart_done
// applies to that close after its own grace.
func TestRestartAskEndedByTheTaskDone(t *testing.T) {
	f, p := turnEnding(t, "", 262_000)
	f.s.TurnEnd("designer", "sid-designer")
	require.True(t, f.ev.has("designer", "restart-asked"))
	p.showAll()

	*f.now = f.now.Add(time.Minute)
	closed := f.now.UTC()
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Done, Title: "draw the logo", Updated: closed,
		Closed: &task.Closure{By: "designer", At: closed, State: task.Done}}}
	f.idle("designer", 262_000, 0)
	f.s.Tick()
	rs := f.s.State.Role("designer")
	assert.True(t, rs.RestartAsked.IsZero())
	assert.True(t, rs.RestartOff.IsZero(), "no pause")
	assert.Empty(t, f.s.Restarts, "inside the grace")

	*f.now = f.now.Add(restartDoneGrace + time.Minute)
	did := f.s.Tick()
	require.Len(t, f.s.Restarts, 1, "%v", did)
	assert.Equal(t, "T-1", f.s.Restarts[0].Done)
}

// restart_at off, and the dispatcher: a turn's end leaves both alone.
func TestRestartAtTheTurnEndOffAndTheDispatcher(t *testing.T) {
	f, p := turnEnding(t, "restart_at: off\n", 400_000)
	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"))
	rs := f.s.State.Role("designer")
	assert.Empty(t, rs.RestartDue)
	assert.Empty(t, rs.RestartNoted)
	assert.Empty(t, p.posted["sid-designer"])

	f, p = turnEnding(t, "", 262_000)
	d := f.s.State.Role("dispatcher")
	d.Inbound, d.Tag = true, "mnopqrstuvwx"
	f.idle("dispatcher", 400_000, 5*time.Second)
	f.rg.endedAt["sid-dispatcher"] = f.now.Add(-5 * time.Second)
	f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: "dispatcher", State: task.Open})

	f.s.TurnEnd("dispatcher", "sid-dispatcher")
	*f.now = f.now.Add(20 * time.Second)
	f.s.Tick()
	assert.Empty(t, d.RestartDue)
	assert.False(t, f.ev.has("dispatcher", "restart-due"))
	assert.False(t, f.ev.has("dispatcher", "restart-asked"))
	assert.NotContains(t, strings.Join(p.posted["sid-dispatcher"], "|"), "restart")
}

// A state.json written before the flag has none: it loads, and the role is restarted by the beat's gate as before.
func TestRestartStateWithoutTheFlagLoads(t *testing.T) {
	var rs RoleState
	require.NoError(t, json.Unmarshal([]byte(`{"up":true,"restart_asked":"2026-01-02T03:04:05Z","restart_session":"s1",`+
		`"restart_gave_up":"s0"}`), &rs))
	assert.Empty(t, rs.RestartDue)
	assert.Empty(t, rs.RestartNoted)
	assert.Equal(t, "s1", rs.RestartSession)

	raw, err := json.Marshal(RoleState{})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "restart_due")
	assert.NotContains(t, string(raw), "restart_noted")

	f := restarting(t, "", 262_000, 6*time.Minute)
	f.s.Tick()
	assert.True(t, f.ev.has("designer", "restart-asked"), "no flag: the settle, as before")
}

// A role that wrote the note it was asked for and went on working (a line reached it as the note's turn ended) is
// called off, not given up: it is asked again later in the same conversation.
func TestRestartWithItsNoteButBusyIsCalledOff(t *testing.T) {
	f := restarting(t, "", 262_000, 6*time.Minute)
	f.s.Tick()
	require.True(t, f.ev.has("designer", "restart-asked"))

	*f.now = f.now.Add(time.Minute)
	f.note(t, "T-1", *f.now)
	f.status("designer", "busy", *f.now)
	*f.now = f.now.Add(restartNoteWait)
	did := f.s.Tick()
	assert.Contains(t, did, "designer: restart called off, its note is there, but it was not between turns within 10m0s")
	rs := f.s.State.Role("designer")
	assert.Empty(t, rs.RestartGaveUp)
	assert.NotContains(t, strings.Join(f.s.State.Notes, "|"), "wrote no note")
	assert.Empty(t, f.s.Restarts)

	*f.now = f.now.Add(trimPause)
	f.idle("designer", 280_000, 6*time.Minute)
	f.s.Tick()
	assert.Equal(t, 2, f.ev.count("designer", "restart-asked"), "asked again in this conversation")
}

// startsFailing is the fake tmux whose first fail starts fail, as a tmux that cannot make a session does; the starts
// after it go through. Every start asked for is kept (dir, command), and onStart runs before it is answered.
type startsFailing struct {
	*fakeTmux
	fail    int
	asked   []startAsked
	onStart func()
}

type startAsked struct {
	dir  string
	argv []string
}

func (s *startsFailing) Start(n, dir string, env, argv []string) error {
	s.asked = append(s.asked, startAsked{dir: dir, argv: argv})

	if s.onStart != nil {
		s.onStart()
	}

	if s.fail > 0 {
		s.fail--

		return errors.New("tmux: no server running")
	}

	return s.fakeTmux.Start(n, dir, env, argv)
}

// restartBegun is the moment after the ticker put designer down for its restart: its note is there, T-1 is held,
// the old session is gone and the job waits for FinishRestart. Its starts go through startsFailing.
func restartBegun(t *testing.T, fail int) (fix, *startsFailing) {
	t.Helper()

	f := restarting(t, "", 300_000, 10*time.Minute)
	f.note(t, "T-1", f.now.Add(-10*time.Minute))
	f.s.Tick()
	require.Len(t, f.s.Restarts, 1)
	require.False(t, f.tm.Has("designer"), "put down, the old session is gone")
	require.Equal(t, "sid-designer", f.s.State.Role("designer").LastSession, "the conversation it was on")

	sf := &startsFailing{fakeTmux: f.tm, fail: fail}
	f.s.Tmux = sf

	return f, sf
}

// newSession registers the session of the pane a start made, as the registry does once it runs, idle and past its boot.
func (f fix) newSession(role, id string) {
	*f.now = f.now.Add(bootPause)
	f.rg.byPID[f.tm.up[role]] = &registry.Session{Status: "idle", StatusUpdatedAt: f.now.UnixMilli(), SessionID: id}
}

// typedAfter ticks the crew until the role has been typed the given number of lines after its start, and returns them:
// one line a beat, each beat a minute on, the role idle between.
func (f fix) typedAfter(role, id string, n int) []string {
	typed := len(f.tm.typed[role])

	for range 3 * n {
		if len(f.tm.typed[role])-typed >= n {
			break
		}

		f.s.Tick()
		*f.now = f.now.Add(time.Minute)
		f.rg.byPID[f.tm.up[role]] = &registry.Session{Status: "idle", StatusUpdatedAt: f.now.UnixMilli(), SessionID: id}
	}

	return f.tm.typed[role][typed:]
}

// savedStates records every state the shift saved while fn ran, as the file would hold it (the state's own decode).
func savedStates(t *testing.T, f fix, fn func()) []*State {
	t.Helper()

	var saved []*State

	f.s.Flush = func() error {
		raw, err := json.Marshal(f.s.State)
		require.NoError(t, err)

		st, _, err := decode(raw)
		require.NoError(t, err)

		saved = append(saved, st)

		return nil
	}

	fn()

	f.s.Flush = nil

	return saved
}

// The line its Talk shows and each packet go to the head of the queue, ahead of what waited while the role was down,
// and reach the fresh session in that order: its context first.
func TestRestartHandsOutAheadOfWhatWaited(t *testing.T) {
	f, _ := restartBegun(t, 0)
	wt := t.TempDir()
	f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: "designer", State: task.Waiting, Worktree: wt})
	f.note(t, "T-2", f.now.Add(-9*time.Minute))
	f.s.Restarts[0].Tasks = []string{"T-1", "T-2"}
	f.s.Restarts[0].Notes = "T-1 a, T-2 b"

	// Lines that came while it was down: a peer's, and one of zmina's own.
	require.NoError(t, f.s.QueueFrom("designer", "[dev] look at the palette", Facts{From: "dev"}))
	require.NoError(t, f.s.QueueFrom("designer", "[zmina] FYI the build is green", Facts{From: "zmina"}))

	out := f.finish(t)
	require.Len(t, out, 1)
	assert.Contains(t, out[0], "restarted fresh")

	rs := f.s.State.Role("designer")
	require.Len(t, rs.Inbox, 5, "%v", rs.Inbox)
	assert.True(t, strings.HasPrefix(rs.Inbox[0], "[zmina] restarted fresh: "), rs.Inbox[0])
	assert.True(t, strings.HasPrefix(rs.Inbox[1], "[zmina T-1] Read handoff packet "), rs.Inbox[1])
	assert.True(t, strings.HasPrefix(rs.Inbox[2], "[zmina T-2] Read handoff packet "), rs.Inbox[2])
	assert.Equal(t, []string{"[dev] look at the palette", "[zmina] FYI the build is green"}, rs.Inbox[3:],
		"what waited comes after, in its order")

	// And that is the order the session is told them in.
	f.newSession("designer", "sid-new")
	typed := f.typedAfter("designer", "sid-new", 5)
	require.Len(t, typed, 5, "%v", typed)
	assert.True(t, strings.HasPrefix(typed[0], "[zmina] restarted fresh: "), typed[0])
	assert.True(t, strings.HasPrefix(typed[1], "[zmina T-1] Read handoff packet "), typed[1])
	assert.True(t, strings.HasPrefix(typed[2], "[zmina T-2] Read handoff packet "), typed[2])
	assert.Equal(t, "[dev] look at the palette", typed[3])
	assert.Equal(t, "[zmina] FYI the build is green", typed[4])
}

// A line that a session left posted and never showed goes back to the head of the queue when the session is settled: it
// is settled before the hand-out is queued, so that the hand-out is ahead of it too.
func TestRestartHandsOutAheadOfALineGivenBack(t *testing.T) {
	f, _ := restartBegun(t, 0)
	rs := f.s.State.Role("designer")
	rs.Inbox = []string{"[dev] waited"}
	rs.Posted = []Posted{{Line: "[dev] posted, never shown", MsgID: "m1", Session: "sid-designer", At: f.now.Add(-time.Minute)}}
	f.s.Peer = newFakePeer()

	f.finish(t)

	require.GreaterOrEqual(t, len(rs.Inbox), 3, "%v", rs.Inbox)
	assert.True(t, strings.HasPrefix(rs.Inbox[0], "[zmina] restarted fresh: "), "%v", rs.Inbox)
	assert.True(t, strings.HasPrefix(rs.Inbox[1], "[zmina T-1] Read handoff packet "), "%v", rs.Inbox)
}

// What the start saves already holds the lines: a process that dies after the start finds the fresh session with its
// restart line and packet in its queue, never with only its boot. No state saved with the fresh session up lacks them.
func TestRestartSavesTheHandOutWithTheStart(t *testing.T) {
	f, sf := restartBegun(t, 0)
	require.NoError(t, f.s.QueueFrom("designer", "[dev] waited", Facts{From: "dev"}))

	// The fake's start looks at the state the shift holds as it asks tmux to make the session: the lines are in
	// the queue before it, so the flush that follows saves them.
	var atStart []string

	sf.onStart = func() { atStart = slices.Clone(f.s.State.Role("designer").Inbox) }

	saved := savedStates(t, f, func() { f.finish(t) })

	require.Len(t, atStart, 3, "%v", atStart)
	assert.True(t, strings.HasPrefix(atStart[0], "[zmina] restarted fresh: "), "%v", atStart)
	assert.True(t, strings.HasPrefix(atStart[1], "[zmina T-1] Read handoff packet "), "%v", atStart)

	require.NotEmpty(t, saved, "the start saves the state")

	for i, st := range saved {
		rs := st.Role("designer")
		if !rs.Up {
			continue
		}

		require.Len(t, rs.Inbox, 3, "save %d, up: %v", i, rs.Inbox)
		assert.True(t, strings.HasPrefix(rs.Inbox[0], "[zmina] restarted fresh: "), "save %d: %v", i, rs.Inbox)
		assert.True(t, strings.HasPrefix(rs.Inbox[1], "[zmina T-1] Read handoff packet "), "save %d: %v", i, rs.Inbox)
		assert.Equal(t, "[dev] waited", rs.Inbox[2])
		assert.Nil(t, rs.Pending, "save %d", i)
		assert.True(t, rs.Downing.IsZero(), "save %d", i)
		assert.False(t, rs.Started.IsZero(), "save %d", i)
	}

	assert.True(t, saved[0].Role("designer").Up, "the first save is the start's own")
}

// A fresh start that fails does not leave the role down and held: it is started again on the conversation it was on,
// the restart is given up for that conversation as a called-off one is, and the dispatcher hears it once.
func TestRestartFailedStartGoesBackToItsConversation(t *testing.T) {
	f, sf := restartBegun(t, 1)
	wt := t.TempDir()
	f.s.Restarts[0].Opts.Dir = wt
	require.NoError(t, f.s.QueueFrom("designer", "[dev] waited", Facts{From: "dev"}))

	before, err := f.tk.Get("T-1")
	require.NoError(t, err)

	out := f.finish(t)
	require.Len(t, out, 1)
	assert.Contains(t, out[0], "tmux: no server running")
	assert.Contains(t, out[0], "back on its conversation")

	require.Len(t, sf.asked, 2, "the fresh start, then the old conversation")
	assert.Equal(t, wt, sf.asked[0].dir, "the fresh start was meant for the task's worktree")
	assert.NotContains(t, sf.asked[0].argv, "--resume")
	assert.Contains(t, sf.asked[1].argv, "--resume")
	assert.Contains(t, sf.asked[1].argv, "sid-designer", "the conversation it was on")
	assert.NotEqual(t, wt, sf.asked[1].dir, "in the directory it ran in")
	assert.Equal(t, "sid-designer", f.s.Resumed("designer"))
	assert.True(t, f.tm.Has("designer"))

	rs := f.s.State.Role("designer")
	assert.True(t, rs.Up)
	assert.False(t, rs.Held)
	assert.Nil(t, rs.Pending)
	assert.True(t, rs.Downing.IsZero())
	assert.Equal(t, []string{"[dev] waited"}, rs.Inbox, "the restart's line and packet are out again; what waited stays")
	assert.Equal(t, "sid-designer", rs.RestartGaveUp, "given up for that conversation")
	assert.False(t, rs.RestartOff.IsZero())

	assert.False(t, f.ev.has("designer", "restarted"), "nothing was restarted")
	assert.True(t, f.ev.has("designer", "restart-failed"))
	assert.True(t, f.ev.has("designer", "restart-off"))
	assert.Equal(t, 1, countNotes(f, "designer: restart for its context failed"), "%v", f.s.State.Notes)
	assert.Contains(t, strings.Join(f.s.State.Notes, "|"), "it is back on its conversation")

	// The task's delivery is what it was: the old conversation's, not a hand-out that never went.
	after, err := f.tk.Get("T-1")
	require.NoError(t, err)
	assert.Equal(t, before.Delivery, after.Delivery)

	// The cards of the lines that came out are closed as dropped, with the reason.
	f.s.FlushCards()

	var dropped []string

	for _, c := range cardsOn(f, "designer") {
		if c.Closed != nil && c.Closed.How == ClosedDropped {
			assert.Contains(t, c.Closed.Why, "fresh start", c.Head)
			dropped = append(dropped, c.Head)
		}
	}

	require.Len(t, dropped, 2, "%v", dropped)
	assert.True(t, strings.HasPrefix(dropped[0], "[zmina] restarted fresh: "), "%v", dropped)
	assert.True(t, strings.HasPrefix(dropped[1], "[zmina T-1] Read handoff packet "), "%v", dropped)

	// Not rotated again in that conversation, however long it is past restart_at: the restart is given up for it.
	f.newSession("designer", "sid-designer")

	for range 3 {
		*f.now = f.now.Add(trimPause + time.Minute)
		f.idle("designer", 310_000, 10*time.Minute)
		f.rg.byPID[f.tm.up["designer"]] = &registry.Session{Status: "idle", StatusUpdatedAt: f.now.Add(-10 * time.Minute).UnixMilli(),
			SessionID: "sid-designer"}
		f.s.Tick()
		assert.Empty(t, f.s.Restarts)
	}

	assert.Zero(t, f.ev.count("designer", "restart-asked"), "no note asked for: it was given up")
	assert.Len(t, sf.asked, 2, "no start after the second")
	assert.Equal(t, 1, countNotes(f, "designer: restart for its context failed"), "told once")
}

func countNotes(f fix, prefix string) int {
	n := 0

	for _, note := range f.s.State.Notes {
		if strings.HasPrefix(note, prefix) {
			n++
		}
	}

	return n
}

// Both starts fail, the fresh one and the one on the old conversation: the role is left down and not held, so that
// a role with keep: always is opened again by the ticker as after a crash, and the dispatcher hears it once. The
// packets stay queued for whoever starts it next; the restart's own line, which says "restarted fresh", does not.
func TestRestartBothStartsFailLeaveTheRoleOpenable(t *testing.T) {
	f, sf := restartBegun(t, 2)
	f.s.Crew.Roles["designer"].Keep = crew.KeepAlways
	require.NoError(t, f.s.QueueFrom("designer", "[dev] waited", Facts{From: "dev"}))

	out := f.finish(t)
	require.Len(t, out, 1)
	assert.Contains(t, out[0], "tmux: no server running")
	assert.Len(t, sf.asked, 2)

	rs := f.s.State.Role("designer")
	assert.False(t, f.tm.Has("designer"), "down")
	assert.False(t, rs.Up)
	assert.False(t, rs.Held, "not held: the ticker may open it")
	assert.Nil(t, rs.Pending)
	assert.True(t, rs.Downing.IsZero())
	assert.Equal(t, "sid-designer", rs.RestartGaveUp, "given up for the conversation it was on")
	assert.False(t, f.ev.has("designer", "restarted"))

	require.Len(t, rs.Inbox, 2, "%v", rs.Inbox)
	assert.True(t, strings.HasPrefix(rs.Inbox[0], "[zmina T-1] Read handoff packet "),
		"the packet is kept, the line that says restarted fresh is not: %v", rs.Inbox)
	assert.Equal(t, "[dev] waited", rs.Inbox[1])

	tk, err := f.tk.Get("T-1")
	require.NoError(t, err)
	require.NotNil(t, tk.Delivery)
	assert.Equal(t, rs.Inbox[0], tk.Delivery.Line, "the task's delivery is the packet that waits")

	assert.Equal(t, 1, countNotes(f, "designer: restart for its context failed"), "%v", f.s.State.Notes)
	assert.Contains(t, strings.Join(f.s.State.Notes, "|"), "starting it on its conversation failed too")
	assert.Contains(t, strings.Join(f.s.State.Notes, "|"), "up 'designer' --task T-1 --fresh")

	// The ticker opens it again as after a crash, and the packet that waited is its context.
	*f.now = f.now.Add(reopenPause)
	did := f.s.Tick()
	assert.Contains(t, did, "started designer")
	assert.True(t, f.tm.Has("designer"))
	assert.Empty(t, f.s.Resumed("designer"), "fresh")
	assert.Len(t, sf.asked, 3)

	f.newSession("designer", "sid-new")
	typed := f.typedAfter("designer", "sid-new", 2)
	require.Len(t, typed, 2, "%v", typed)
	assert.True(t, strings.HasPrefix(typed[0], "[zmina T-1] Read handoff packet "), typed[0])
	assert.Equal(t, "[dev] waited", typed[1])
}

// A role with no conversation of its own known has nothing to go back to: the one start failed, the role is left down
// and not held, and the note says so.
func TestRestartWithNoConversationToGoBackTo(t *testing.T) {
	f, sf := restartBegun(t, 1)
	f.s.State.Role("designer").LastSession = ""

	f.finish(t)

	assert.Len(t, sf.asked, 1, "no second start")

	rs := f.s.State.Role("designer")
	assert.False(t, rs.Up)
	assert.False(t, rs.Held)
	assert.Contains(t, strings.Join(f.s.State.Notes, "|"), "no conversation of its own is known to go back to")
}

// With no room for its line and a packet per task the restart is not begun, before the old session is put down.
func TestRestartNotBegunWithAFullQueue(t *testing.T) {
	f := restarting(t, "", 300_000, 10*time.Minute)
	f.note(t, "T-1", f.now.Add(-10*time.Minute))

	rs := f.s.State.Role("designer")
	for i := range inboxMax {
		rs.Inbox = append(rs.Inbox, fmt.Sprintf("[dev] line %d", i))
	}

	// plainIdle lets only an empty queue through, so the begin is called as the gates would for a role whose tasks
	// alone fill the queue.
	line := f.s.beginRestart("designer", f.s.heldTasks("designer"), "context 300k over restart_at 250k", "")

	assert.Contains(t, line, "20 lines already wait for it and its restart needs room for 2 more")
	assert.Empty(t, f.s.Restarts)
	assert.NotContains(t, f.tm.typed["designer"], "/exit", "the old session was not put down")
	assert.True(t, f.tm.Has("designer"))
	assert.True(t, rs.Up)
	assert.True(t, rs.Downing.IsZero())
	assert.Nil(t, rs.Pending)
	assert.Len(t, rs.Inbox, inboxMax, "nothing lost")
	assert.True(t, f.ev.has("designer", "restart-failed"))
	assert.False(t, rs.RestartOff.IsZero(), "asked again after the usual pause, not at every beat")
}

// Lines that came while the role was down fill its queue; the restart's hand-out goes in ahead of them and none is
// dropped to make room.
func TestRestartHandsOutIntoAQueueThatFilledWhileItWasDown(t *testing.T) {
	f, _ := restartBegun(t, 0)

	for i := range inboxMax {
		require.NoError(t, f.s.QueueFrom("designer", fmt.Sprintf("[dev] line %d", i), Facts{From: "dev"}))
	}

	require.Error(t, f.s.QueueFrom("designer", "[dev] one too many", Facts{From: "dev"}), "full for a sender")

	out := f.finish(t)
	require.Len(t, out, 1)
	assert.Contains(t, out[0], "restarted fresh")

	rs := f.s.State.Role("designer")
	require.Len(t, rs.Inbox, inboxMax+2, "%v", rs.Inbox)
	assert.True(t, strings.HasPrefix(rs.Inbox[0], "[zmina] restarted fresh: "))
	assert.True(t, strings.HasPrefix(rs.Inbox[1], "[zmina T-1] Read handoff packet "))
	assert.Equal(t, "[dev] line 0", rs.Inbox[2])
	assert.Equal(t, "[dev] line 19", rs.Inbox[inboxMax+1])
	assert.False(t, f.ev.has("designer", "restart-failed"))
}

// A restart finished by a later beat, its own having died during the wait, hands out in the same order and rolls back
// the same way: the conversation it goes back to is the one the state kept, not the dead beat's memory.
func TestRestartFinishedByALaterBeatHandsOutAndRollsBack(t *testing.T) {
	for name, fail := range map[string]int{"hands out": 0, "rolls back": 1} {
		t.Run(name, func(t *testing.T) {
			f, sf := restartBegun(t, fail)
			require.NotNil(t, f.s.State.Role("designer").Pending)
			require.NoError(t, f.s.QueueFrom("designer", "[dev] waited", Facts{From: "dev"}))

			// The beat dies: its in-memory job is gone.
			f.s.Restarts = nil

			*f.now = f.now.Add(30 * time.Second)
			f.s.Tick()
			assert.Empty(t, sf.asked, "inside downingFor the beat that began it may still finish it")

			*f.now = f.now.Add(downingFor)
			did := f.s.Tick()
			assert.True(t, f.ev.has("designer", "restart-late"))
			assert.True(t, f.tm.Has("designer"))

			rs := f.s.State.Role("designer")
			assert.Nil(t, rs.Pending)
			assert.True(t, rs.Downing.IsZero())

			if fail == 0 {
				assert.Contains(t, strings.Join(did, "|"), "designer: restarted fresh")
				require.GreaterOrEqual(t, len(rs.Inbox), 3, "%v", rs.Inbox)
				assert.True(t, strings.HasPrefix(rs.Inbox[0], "[zmina] restarted fresh: "), "%v", rs.Inbox)
				assert.True(t, strings.HasPrefix(rs.Inbox[1], "[zmina T-1] Read handoff packet "), "%v", rs.Inbox)
				assert.Equal(t, "[dev] waited", rs.Inbox[2])

				return
			}

			assert.Equal(t, "sid-designer", f.s.Resumed("designer"), "back on its conversation")
			assert.Equal(t, "sid-designer", rs.RestartGaveUp)
			assert.Equal(t, []string{"[dev] waited"}, rs.Inbox)
		})
	}
}

// A restart that a later beat or a command finished meanwhile is not done twice: the job in memory finds the role
// started since it began, hands out nothing and starts nothing.
func TestRestartSupersededHandsOutNothing(t *testing.T) {
	f, sf := restartBegun(t, 0)
	job := f.s.Restarts[0]
	f.s.Restarts = nil

	*f.now = f.now.Add(downingFor + 30*time.Second)
	f.s.Tick()
	require.Len(t, sf.asked, 1, "the later beat finished it")

	rs := f.s.State.Role("designer")
	inbox := slices.Clone(rs.Inbox)
	require.Len(t, inbox, 2)

	out := f.s.FinishRestart(job)

	assert.Equal(t, "designer: restart: a later beat or a command started it meanwhile", out)
	assert.True(t, f.ev.has("designer", "restart-superseded"))
	assert.Len(t, sf.asked, 1, "no second start")
	assert.Equal(t, inbox, rs.Inbox, "no second hand-out")
}

// A restart after a task was done has no packet; a failed fresh start goes back to the conversation as well, and is
// not asked again for that task (the role's start after the close is later than it).
func TestRestartAfterTheTaskIsDoneRollsBack(t *testing.T) {
	f := doneRestarting(t, "", 180_000, 11*time.Minute)
	f.s.Tick()
	require.Len(t, f.s.Restarts, 1)

	sf := &startsFailing{fakeTmux: f.tm, fail: 1}
	f.s.Tmux = sf

	out := f.finish(t)
	require.Len(t, out, 1)
	assert.Contains(t, out[0], "back on its conversation")

	rs := f.s.State.Role("designer")
	assert.True(t, rs.Up)
	assert.Empty(t, rs.Inbox, "no restart line on the conversation it went back to")
	assert.Equal(t, "sid-designer", rs.RestartGaveUp)
	assert.Equal(t, "sid-designer", f.s.Resumed("designer"))
	assert.Equal(t, 1, countNotes(f, "designer: restart after T-1 was done failed"), "%v", f.s.State.Notes)

	f.newSession("designer", "sid-designer")
	*f.now = f.now.Add(trimPause + time.Hour)
	f.idle("designer", 200_000, 20*time.Minute)
	f.rg.byPID[f.tm.up["designer"]] = &registry.Session{Status: "idle", StatusUpdatedAt: f.now.Add(-20 * time.Minute).UnixMilli(),
		SessionID: "sid-designer"}
	f.s.Tick()
	assert.Empty(t, f.s.Restarts, "no loop")
	assert.Len(t, sf.asked, 2)
}

// deliveryOf is the delivery the restart left on T-1.
func deliveryOf(t *testing.T, f fix) *task.Delivery {
	t.Helper()

	tk, err := f.tk.Get("T-1")
	require.NoError(t, err)
	require.NotNil(t, tk.Delivery)

	return tk.Delivery
}

// packetLineOf is the line of T-1's packet that waits in the role's queue.
func packetLineOf(t *testing.T, f fix) string {
	t.Helper()

	rs := f.s.State.Role("designer")
	i := slices.IndexFunc(rs.Inbox, func(l string) bool { return strings.HasPrefix(l, "[zmina T-1] Read handoff packet ") })
	require.GreaterOrEqual(t, i, 0, "%v", rs.Inbox)

	return rs.Inbox[i]
}

// The packet is handed while the role is down, before its start: its delivery is queued and not sent, so the 5 min of
// the acceptance alert do not run while it waits; the new session is typed it once, and the packet is not handed again.
// What a packet not accepted comes to after that is the restart's rotation to say, once: the alert of the packet is
// the rotation's while it is open and after it was lost and told, so the dispatcher does not hear it twice (the rotation
// tests); a packet no rotation handed is told by the alert as ever (TestAcceptanceAlertFiresWhenNoRotationOwnsTheTask).
func TestRestartHandOutIsQueuedBeforeTheStartAndItsAcceptanceIsTheRotationsToTell(t *testing.T) {
	f, _ := restartBegun(t, 0)
	f.finish(t)

	d, packet := deliveryOf(t, f), packetLineOf(t, f)
	assert.Equal(t, packet, d.Line)
	assert.True(t, d.SentAt.IsZero(), "queued, not sent to any session")
	assert.Empty(t, d.Session)
	assert.True(t, d.AcceptedAt.IsZero())
	assert.False(t, d.QueuedAt.After(f.s.State.Role("designer").Started), "handed before the start")

	// It waits for a booted session; however long, nothing is due: a submission is what the alert counts from.
	f.newSession("designer", "sid-new")
	*f.now = f.now.Add(3 * time.Minute)
	f.rg.byPID[f.tm.up["designer"]] = &registry.Session{Status: "idle", StatusUpdatedAt: f.now.UnixMilli(), SessionID: "sid-new"}
	tk, err := f.tk.Get("T-1")
	require.NoError(t, err)
	assert.False(t, f.s.acceptanceDue(tk), "a waiting packet is no submission")

	typed := f.typedAfter("designer", "sid-new", 2)
	require.Len(t, typed, 2, "%v", typed)
	assert.Equal(t, packet, typed[1], "the new session is typed the packet")

	d = deliveryOf(t, f)
	require.False(t, d.SentAt.IsZero(), "the typing is the submission")
	assert.Equal(t, "sid-new", d.Session, "to the new session, not the old")
	sent := d.SentAt

	// Its boot turn is over, its restart line was shown to it, and it goes idle without accepting.
	f.rg.endedAt["sid-new"] = sent.Add(-time.Second)
	f.s.cardSeen("designer", lastRotation(t, f, "designer").Lines[0], SeenTranscript, sent)

	alerts := func() int { return countNotes(f, "designer: T-1 has no acceptance after 5 min") }

	idle := func(at time.Time) {
		*f.now = at
		f.rg.byPID[f.tm.up["designer"]] = &registry.Session{Status: "idle", StatusUpdatedAt: sent.UnixMilli(), SessionID: "sid-new"}
		f.s.Tick()
	}

	idle(sent.Add(5 * time.Minute))
	assert.Zero(t, alerts(), "the rotation that handed the packet is open: it tells it, not the alert: %v", f.s.State.Notes)
	assert.Zero(t, rotationNotes(f))

	idle(sent.Add(rotationAcceptWait + time.Minute))
	assert.Zero(t, alerts(), "%v", f.s.State.Notes)
	assert.Equal(t, 1, rotationNotes(f), "it is told once, by the rotation: %v", f.s.State.Notes)
	assert.Contains(t, strings.Join(f.s.State.Notes, "|"), "lost: it did not accept T-1")

	idle(sent.Add(rotationAcceptWait + 4*time.Minute))
	assert.Zero(t, alerts(), "nor after it was lost and told")
	assert.Equal(t, 1, rotationNotes(f), "once")

	// Nothing was handed twice: one typing of the packet in all, and nothing left in the queue.
	n := 0

	for _, l := range f.tm.typed["designer"] {
		if l == packet {
			n++
		}
	}

	assert.Equal(t, 1, n)
	assert.Empty(t, f.s.State.Role("designer").Inbox)
}

// The new session accepts the packet it was typed, and no alert follows.
func TestRestartHandOutIsAcceptedByTheNewSession(t *testing.T) {
	f, _ := restartBegun(t, 0)
	f.finish(t)
	f.newSession("designer", "sid-new")
	f.typedAfter("designer", "sid-new", 2)

	d := deliveryOf(t, f)

	f.s.By = "designer"
	require.NoError(t, f.s.AcceptTask("T-1", d.ID))
	f.s.By = "hand"

	*f.now = f.now.Add(10 * time.Minute)
	f.rg.byPID[f.tm.up["designer"]] = &registry.Session{Status: "idle", StatusUpdatedAt: f.now.UnixMilli(), SessionID: "sid-new"}
	f.s.Tick()
	assert.Zero(t, countNotes(f, "designer: T-1 has no acceptance"), "%v", f.s.State.Notes)
	assert.True(t, f.ev.has("designer", "accepted"))
}

// A delivery of the very packet the restart hands out, sent to the conversation that is gone and not accepted, is
// renewed as a start after it always renewed it: not sent, no session, queued now. Its old session's submission is not the
// new session's, and the line goes once.
func TestRestartRenewsADeliveryOfTheSamePacket(t *testing.T) {
	f, _ := restartBegun(t, 0)

	tk, err := f.tk.Get("T-1")
	require.NoError(t, err)

	path, err := handoff.Write(f.s.Crew.StateDir(), mustPacket(t, f, tk))
	require.NoError(t, err)

	r, err := f.s.Role("designer")
	require.NoError(t, err)

	runtime, home := f.s.runtimeScope(r)
	queued := f.s.State.Role("designer").Started.Add(time.Minute).UTC()
	f.tk.list[0].Delivery = &task.Delivery{ID: strings.TrimSuffix(filepath.Base(path), ".json"), Role: "designer",
		Runtime: runtime, Home: home, Line: "an earlier line", TaskUpdated: tk.Updated, QueuedAt: queued,
		SentAt: queued.Add(time.Second), Session: "sid-designer", AlertedAt: queued.Add(6 * time.Minute)}

	f.finish(t)

	d, packet := deliveryOf(t, f), packetLineOf(t, f)
	assert.Equal(t, packet, d.Line)
	assert.True(t, d.SentAt.IsZero(), "the old session's submission is not the new one's")
	assert.Empty(t, d.Session)
	assert.True(t, d.AlertedAt.IsZero())
	assert.True(t, d.QueuedAt.After(queued))

	n := 0

	for _, l := range f.s.State.Role("designer").Inbox {
		if l == packet {
			n++
		}
	}

	assert.Equal(t, 1, n, "queued once")
}

// A copy of a role is forgotten when it goes down, and a restart's down is no exception: the copy whose fresh start fails
// goes back to its conversation (its state is made anew by that start), and one that cannot start at all is not kept
// down with a queue, as no copy is.
func TestRestartACopyWhoseFreshStartFails(t *testing.T) {
	for name, fail := range map[string]int{"back on its conversation": 1, "cannot start": 2} {
		t.Run(name, func(t *testing.T) {
			f := restarting(t, "", 100, time.Minute)
			f.tk.list = nil
			require.NoError(t, f.s.Up("designer#2"))
			*f.now = f.now.Add(5 * time.Minute)
			f.idle("designer#2", 300_000, 10*time.Minute)
			f.tk.list = []task.Task{{ID: "T-7", Role: "designer#2", State: task.Open, Title: "draw the icon"}}
			f.note(t, "T-7", f.now.Add(-10*time.Minute))

			f.s.Tick()
			require.Len(t, f.s.Restarts, 1)

			sf := &startsFailing{fakeTmux: f.tm, fail: fail}
			f.s.Tmux = sf

			out := f.finish(t)
			require.Len(t, out, 1)
			assert.Len(t, sf.asked, 2, "the fresh start, then the old conversation")
			assert.Equal(t, 1, countNotes(f, "designer#2: restart for its context failed"), "%v", f.s.State.Notes)

			rs, kept := f.s.State.Roles["designer#2"]
			if fail == 1 {
				require.True(t, kept)
				assert.True(t, rs.Up)
				assert.Equal(t, "sid-designer#2", f.s.Resumed("designer#2"), "its own conversation")
				assert.Equal(t, "sid-designer#2", rs.RestartGaveUp)
				assert.Empty(t, rs.Inbox)

				return
			}

			assert.False(t, kept, "a copy that is down is not kept")
			assert.False(t, f.tm.Has("designer#2"))
		})
	}
}

// ticking makes the shift's clock go a second on at every look, as the real one does between a hand-out and a start;
// frozen stops it again. A frozen clock reads the same at the hand-out and at the start, which hides every comparison
// of the two.
func (f fix) ticking() {
	f.s.Now = func() time.Time {
		*f.now = f.now.Add(time.Second)

		return *f.now
	}
}

func (f fix) frozen() { f.s.Now = func() time.Time { return *f.now } }

// packetLines counts the lines of T-1's packet in a role's queue.
func packetLines(f fix, role string) int {
	n := 0

	for _, l := range f.s.State.Role(role).Inbox {
		if strings.HasPrefix(l, "[zmina T-1] Read handoff packet ") {
			n++
		}
	}

	return n
}

// The hand-out is queued before the start, so the delivery is older than the session's start for the whole life of the
// session. A hand of the unchanged task again, after the session accepted its packet, is as it was before restarts: no
// new delivery, the acceptance kept, no second packet line.
func TestReHandAfterARestartKeepsTheAcceptedPacket(t *testing.T) {
	f, _ := restartBegun(t, 0)
	f.ticking()
	f.finish(t)
	f.frozen()

	rs := f.s.State.Role("designer")
	require.True(t, rs.Started.After(deliveryOf(t, f).QueuedAt), "the hand-out is older than the start")

	f.newSession("designer", "sid-new")
	f.typedAfter("designer", "sid-new", 2)

	d := deliveryOf(t, f)
	f.s.By = "designer"
	require.NoError(t, f.s.AcceptTask("T-1", d.ID))
	f.s.By = "hand"

	accepted := *deliveryOf(t, f)
	require.False(t, accepted.AcceptedAt.IsZero())

	tk, err := f.tk.Get("T-1")
	require.NoError(t, err)

	for range 2 {
		*f.now = f.now.Add(time.Minute)
		_, err = f.s.HandTask("designer", tk)
		require.NoError(t, err)

		assert.Equal(t, accepted, *deliveryOf(t, f), "no new delivery: accepted by the live session")
		assert.Empty(t, f.s.State.Role("designer").Inbox, "no second packet line")
	}

	// And it is still the session's to accept again.
	f.s.By = "designer"
	require.NoError(t, f.s.AcceptTask("T-1", d.ID))
	f.s.By = "hand"
}

// A hand of the unchanged task while the packet still waits in the queue, or after it was sent and not accepted, leaves
// one packet line and nothing for the session to be told twice.
func TestReHandAfterARestartQueuesNoSecondLine(t *testing.T) {
	f, _ := restartBegun(t, 0)
	f.ticking()
	f.finish(t)
	f.frozen()

	tk, err := f.tk.Get("T-1")
	require.NoError(t, err)

	before := slices.Clone(f.s.State.Role("designer").Inbox)

	_, err = f.s.HandTask("designer", tk)
	require.NoError(t, err)
	assert.Equal(t, before, f.s.State.Role("designer").Inbox, "still the restart's line, then the one packet")
	assert.Equal(t, 1, packetLines(f, "designer"))

	f.newSession("designer", "sid-new")
	typed := f.typedAfter("designer", "sid-new", 2)
	require.Len(t, typed, 2, "%v", typed)

	n := 0

	for _, l := range f.tm.typed["designer"] {
		if strings.HasPrefix(l, "[zmina T-1] Read handoff packet ") {
			n++
		}
	}

	assert.Equal(t, 1, n, "told once")
	assert.Equal(t, "sid-new", deliveryOf(t, f).Session)
}

// A restart that fails both starts is a restart given up for the conversation too: a role that is opened again on it (resume:
// always) is not asked for its note and put down again at once, a dispatcher note per round.
func TestRestartBothStartsFailGiveItUpForTheConversation(t *testing.T) {
	f, _ := restartBegun(t, 2)
	f.s.Crew.Roles["designer"].Keep = crew.KeepAlways
	f.s.Crew.Roles["designer"].Resume = crew.ResumeAlways

	f.finish(t)

	rs := f.s.State.Role("designer")
	assert.Equal(t, "sid-designer", rs.RestartGaveUp)
	assert.False(t, rs.RestartOff.IsZero())

	*f.now = f.now.Add(reopenPause)
	f.s.Tick()
	require.True(t, f.tm.Has("designer"))
	assert.Equal(t, "sid-designer", f.s.Resumed("designer"), "on the same conversation")

	f.newSession("designer", "sid-designer")
	f.typedAfter("designer", "sid-designer", 1)

	for range 3 {
		*f.now = f.now.Add(trimPause + time.Minute)
		f.idle("designer", 310_000, 10*time.Minute)
		f.rg.byPID[f.tm.up["designer"]] = &registry.Session{Status: "idle",
			StatusUpdatedAt: f.now.Add(-10 * time.Minute).UnixMilli(), SessionID: "sid-designer"}
		f.s.Tick()
		assert.Empty(t, f.s.Restarts)
	}

	assert.Zero(t, f.ev.count("designer", "restart-asked"), "not asked for a note in that conversation")
	assert.Equal(t, 1, countNotes(f, "designer: restart for its context failed"), "%v", f.s.State.Notes)
}

// The dispatcher hears of a role that cannot be started once: the next beat's "down, has not taken" hint would be the same
// news twice, the packet being unaccepted after the hand-out, and the note already says what task it holds and how to
// start it.
func TestRestartBothStartsFailTellsTheDispatcherOnce(t *testing.T) {
	f, _ := restartBegun(t, 2)
	f.finish(t)

	for range 2 {
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
	}

	n := 0

	for _, note := range f.s.State.Notes {
		if strings.HasPrefix(note, "designer: ") {
			n++
		}
	}

	assert.Equal(t, 1, n, "%v", f.s.State.Notes)
}

// A task's worktree the role ran in may be gone when the restart fails: the old conversation is not resumed in some other
// place, and the role is left down as when the resume fails.
func TestRestartRollbackNotInADirectoryThatIsGone(t *testing.T) {
	f, sf := restartBegun(t, 1)
	gone := filepath.Join(t.TempDir(), "worktree")
	f.s.State.Role("designer").Dir = gone

	f.finish(t)

	assert.Len(t, sf.asked, 1, "no start where the conversation did not run")

	rs := f.s.State.Role("designer")
	assert.False(t, rs.Up)
	assert.False(t, rs.Held)
	assert.False(t, f.tm.Has("designer"))
	assert.Contains(t, strings.Join(f.s.State.Notes, "|"), "the directory it ran in is gone: "+gone)
	assert.Equal(t, 1, countNotes(f, "designer: restart for its context failed"), "%v", f.s.State.Notes)

	// A directory that is there is resumed in.
	f2, sf2 := restartBegun(t, 1)
	here := t.TempDir()
	f2.s.State.Role("designer").Dir = here
	f2.finish(t)
	require.Len(t, sf2.asked, 2)
	assert.Equal(t, here, sf2.asked[1].dir)
	assert.True(t, f2.tm.Has("designer"))
}

// The older packet of a task that waits in a queue is dropped when the role runs on the newer one, not before: a start
// that fails goes back to the conversation it was for.
func TestRestartReplacesAnOlderPacketOnlyOnceStarted(t *testing.T) {
	older := "[zmina T-1] Read handoff packet \"/old.json\"; before work run: zmina task accept T-1 --handoff old"

	t.Run("started", func(t *testing.T) {
		f, _ := restartBegun(t, 0)
		require.NoError(t, f.s.QueueFrom("designer", older, Facts{From: "dev", Task: "T-1"}))

		f.finish(t)

		rs := f.s.State.Role("designer")
		assert.NotContains(t, rs.Inbox, older)
		assert.Equal(t, 1, packetLines(f, "designer"))
	})

	t.Run("fails", func(t *testing.T) {
		f, _ := restartBegun(t, 1)
		require.NoError(t, f.s.QueueFrom("designer", older, Facts{From: "dev", Task: "T-1"}))

		f.finish(t)

		assert.Equal(t, []string{older}, f.s.State.Role("designer").Inbox, "what waited, as it was")
	})
}
