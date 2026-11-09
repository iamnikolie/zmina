package shift

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/chat"
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
	did = f.s.Tick()
	require.Len(t, f.s.Restarts, 1)
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
