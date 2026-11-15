package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

const (
	// assignSeats are the seats of the crews these tests run in; every one is down until zmina starts it.
	assignSeats = "  dev: {}\n  dev2: {}\n"
	// assignOn is reconcile with the one part the commands use: a task or a line for a seat that is down starts it.
	assignOn = "reconcile: {assign: true}\n"
)

// inboxOf is what waits in a role's queue, as the saved state has it.
func inboxOf(t *testing.T, file, role string) []string {
	t.Helper()

	a := &app{out: io.Discard, err: io.Discard, crew: file}

	var got []string

	require.NoError(t, a.readState(func(s *shift.Shift) error {
		got = append(got, s.State.Role(role).Inbox...)

		return nil
	}))

	return got
}

// withRole changes a role's saved state, as a command holding the lock would.
func withRole(t *testing.T, file, role string, fn func(rs *shift.RoleState)) {
	t.Helper()

	a := &app{out: io.Discard, err: io.Discard, crew: file}
	require.NoError(t, a.withState(func(s *shift.Shift) error { fn(s.State.Role(role)); return nil }))
}

// A task given to a seat that is down starts it, in the task's place, with the packet queued: the first line stays what scripts read.
func TestTaskNewStartsADownSeatWithReconcileAssign(t *testing.T) {
	file := testCrew(t, assignSeats+assignOn)

	out, errOut, err := zm(t, file, "task", "new", "dev", "Fix the form")
	require.NoError(t, err)
	assert.Regexp(t, `^T-1 · dev\ndev up as \S+ for T-1\n$`, out)
	assert.Empty(t, errOut, "started: no hint on how to start it")

	assert.True(t, isUp(t, file, "dev"))
	assert.False(t, isUp(t, file, "dev2"), "only the seat the task is for")

	inbox := inboxOf(t, file, "dev")
	require.Len(t, inbox, 1)
	assert.Contains(t, inbox[0], "task accept T-1")

	assert.Len(t, eventsNamed(t, file, "assign-started"), 1)
	assert.NotNil(t, readTask(t, file, "T-1").Delivery, "the hand-out is recorded")

	// A seat that is up is handed the task as always, and nothing is started.
	out, errOut, err = zm(t, file, "task", "new", "dev", "Fix the footer")
	require.NoError(t, err)
	assert.Equal(t, "T-2 · dev\n", out)
	assert.Empty(t, errOut)
	assert.Len(t, eventsNamed(t, file, "assign-started"), 1)
}

func TestTaskNewJSONSaysTheStartOnStderr(t *testing.T) {
	file := testCrew(t, assignSeats+assignOn)

	out, errOut, err := zm(t, file, "task", "new", "dev", "--json", "Fix the form")
	require.NoError(t, err)

	var got task.Task
	require.NoError(t, json.Unmarshal([]byte(out), &got), "stdout is the task and nothing else")
	assert.Equal(t, "dev", got.Role)
	assert.NotNil(t, got.Delivery, "the task as the hand-out left it")
	assert.Regexp(t, `^dev up as \S+ for T-1\n$`, errOut)
	assert.True(t, isUp(t, file, "dev"))
}

// With the key absent, off, or on for the other parts only, a task for a seat that is down is recorded and nothing starts: the
// output is byte for byte what it was before the key existed.
func TestTaskNewWithAssignOffIsAsBefore(t *testing.T) {
	for _, key := range []string{"", "reconcile: off\n", "reconcile: {restart: true, tries: 2, orphans: down}\n",
		"reconcile: {assign: false}\n"} {
		t.Run(strings.TrimSpace(key), func(t *testing.T) {
			file := testCrew(t, assignSeats+key)

			out, errOut, err := zm(t, file, "task", "new", "dev", "Review")
			require.NoError(t, err)
			assert.Equal(t, "T-1 · dev\n", out)
			assert.Equal(t, "zmina: dev is down: zmina --crew "+quoteCommandArg(file)+" up 'dev' --task T-1 starts it on the task\n", errOut)

			assert.False(t, isUp(t, file, "dev"))
			assert.Empty(t, inboxOf(t, file, "dev"), "nothing is queued for a seat that was not started")
			assert.Empty(t, eventsNamed(t, file, "assign-started"))
			assert.Empty(t, eventsNamed(t, file, "assign-refused"))

			out, errOut, err = zm(t, file, "task", "new", "dev2", "--json", "Review")
			require.NoError(t, err)
			assert.Contains(t, out, `"role": "dev2"`)
			assert.Empty(t, errOut, "no word of the start under --json, as before")
		})
	}
}

// A seat zmina must not start is not started, and the task is recorded with the hint it always had, and why not.
func TestTaskNewNotStartedKeepsTheHintAndSaysWhy(t *testing.T) {
	file := func(t *testing.T) string { return testCrew(t, assignSeats+assignOn) }

	for name, c := range map[string]struct {
		setup func(t *testing.T, file string)
		why   string
	}{
		"put down by hand": {func(t *testing.T, file string) {
			withRole(t, file, "dev", func(rs *shift.RoleState) { rs.Held = true })
		},
			"it was put down by hand"},
		"broken": {func(t *testing.T, file string) {
			withRole(t, file, "dev", func(rs *shift.RoleState) { rs.Broken = true })
		},
			"it is broken (zmina ls)"},
		"asleep": {func(t *testing.T, file string) {
			_, _, err := zm(t, file, "sleep", "dev", "--until", "+1h")
			require.NoError(t, err)
		}, "it is asleep"},
		"the shift is stopped": {func(t *testing.T, file string) {
			a := &app{out: io.Discard, err: io.Discard, crew: file}
			require.NoError(t, a.withState(func(s *shift.Shift) error { s.State.Stopped = time.Now(); return nil }))
		}, "the shift is stopped"},
		"its plan is held": {func(t *testing.T, file string) {
			holdBook(t, mustCrew(t, file), time.Now().UTC().Add(2*time.Hour), "")
		}, "its plan is held (zmina limits)"},
		"it holds other work": {func(t *testing.T, file string) {
			_, err := taskStore(mustCrew(t, file)).Add(task.Task{Role: "dev", Asked: "an earlier task"}, time.Now())
			require.NoError(t, err)
		}, "it holds T-1, which comes first"},
	} {
		t.Run(name, func(t *testing.T) {
			f := file(t)
			c.setup(t, f)

			out, errOut, err := zm(t, f, "task", "new", "dev", "Fix the form")
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(out, "T-"), out)
			assert.NotContains(t, out, "up as", "the first line only: nothing was started")
			assert.Contains(t, errOut, "zmina: dev is down: zmina --crew "+quoteCommandArg(f)+" up 'dev' --task T-")
			assert.Contains(t, errOut, " starts it on the task (not started: "+c.why+")\n")

			assert.False(t, isUp(t, f, "dev"))
			assert.Empty(t, inboxOf(t, f, "dev"))
			assert.Len(t, eventsNamed(t, f, "assign-refused"), 1)
			assert.Empty(t, eventsNamed(t, f, "assign-started"))
		})
	}
}

// The dispatcher is the ticker's to open, on its own resume policy: a task for it is recorded as before, with the hint and no refusal.
func TestTaskNewForTheDispatcherIsNotStartedByAssign(t *testing.T) {
	file := testCrew(t, assignSeats+assignOn)

	out, errOut, err := zm(t, file, "task", "new", "dispatcher", "Plan the week")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dispatcher\n", out)
	assert.Equal(t, "zmina: dispatcher is down: zmina --crew "+quoteCommandArg(file)+" up 'dispatcher' --task T-1 starts it on the task\n", errOut)
	assert.False(t, isUp(t, file, "dispatcher"))
}

// A copy the routes name is started as zmina up --as 2 starts it.
func TestTaskNewRouteCopyIsStartedAsACopy(t *testing.T) {
	file := routeCrew(t, "")
	routeLimits(t, file, 41, 50)

	_, _, err := zm(t, file, "task", "new", "dev", "the first piece")
	require.NoError(t, err)

	editCrew(t, file, "roles:\n", assignOn+"roles:\n")

	out, errOut, err := zm(t, file, "task", "new", "--route", "--after", "T-1", "the second piece")
	require.NoError(t, err)
	assert.Contains(t, out, "T-2 · dev#2 (rule 2: goes on from T-1, done by dev;")
	assert.Regexp(t, `\ndev#2 up as \S+ for T-2\n$`, out)
	assert.Empty(t, errOut)

	assert.True(t, isUp(t, file, "dev#2"))
	assert.False(t, isUp(t, file, "dev"), "the base seat is left as it was")
	assert.Equal(t, "dev#2", readTaskFile(t, file, "T-2")["role"])
}

// A task handed to another seat starts that seat when it is down, and queues the packet as before when the key is off.
func TestTaskSetRoleStartsADownSeat(t *testing.T) {
	file := testCrew(t, assignSeats+assignOn)

	_, err := taskStore(mustCrew(t, file)).Add(task.Task{Role: "dev", Asked: "fix the form"}, time.Now())
	require.NoError(t, err)

	out, errOut, err := zm(t, file, "task", "set", "T-1", "open", "--role", "dev2")
	require.NoError(t, err)
	assert.Regexp(t, `^T-1 · dev2 · open\ndev2 up as \S+ for T-1\n$`, out)
	assert.Empty(t, errOut)
	assert.True(t, isUp(t, file, "dev2"))
	require.Len(t, inboxOf(t, file, "dev2"), 1, "the packet once: the start hands it, the command does not")
	assert.Contains(t, inboxOf(t, file, "dev2")[0], "task accept T-1")

	// Up already: the packet is queued as always, and nothing is said of a start.
	out, _, err = zm(t, file, "task", "set", "T-1", "open", "--role", "dev2")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev2 · open\n", out)

	// A copy.
	_, err = taskStore(mustCrew(t, file)).Add(task.Task{Role: "dev", Asked: "and the footer"}, time.Now())
	require.NoError(t, err)

	out, _, err = zm(t, file, "task", "set", "T-2", "open", "--role", "dev#2")
	require.NoError(t, err)
	assert.Regexp(t, `\ndev#2 up as \S+ for T-2\n$`, out)
	assert.True(t, isUp(t, file, "dev#2"))
}

func TestTaskSetRoleWithAssignOffQueuesAsBefore(t *testing.T) {
	file := testCrew(t, assignSeats)

	_, err := taskStore(mustCrew(t, file)).Add(task.Task{Role: "dev", Asked: "fix the form"}, time.Now())
	require.NoError(t, err)

	out, errOut, err := zm(t, file, "task", "set", "T-1", "open", "--role", "dev2")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev2 · open\n", out)
	assert.Empty(t, errOut)
	assert.False(t, isUp(t, file, "dev2"))
	require.Len(t, inboxOf(t, file, "dev2"), 1, "queued for the seat, which waits")
}

func TestTaskSetRoleNotStartedKeepsTheQueueAndSaysWhy(t *testing.T) {
	file := testCrew(t, assignSeats+assignOn)

	gone := filepath.Join(filepath.Dir(file), "no-such-worktree")
	_, err := taskStore(mustCrew(t, file)).Add(task.Task{Role: "dev", Asked: "fix the form", Worktree: gone}, time.Now())
	require.NoError(t, err)

	out, errOut, err := zm(t, file, "task", "set", "T-1", "open", "--role", "dev2")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev2 · open\n", out)
	assert.Contains(t, errOut, "(not started: its worktree "+gone+" is gone)")
	assert.False(t, isUp(t, file, "dev2"))
	require.Len(t, inboxOf(t, file, "dev2"), 1, "the packet waits for the seat, as it always did")

	// A seat put down by hand stays down.
	withRole(t, file, "dev2", func(rs *shift.RoleState) { rs.Held = true })
	_, errOut, err = zm(t, file, "task", "set", "T-1", "open", "--role", "dev2")
	require.NoError(t, err)
	assert.Contains(t, errOut, "(not started: it was put down by hand)", "Held is looked at before the worktree")

	// A task that waits on the owner is not started for: it waits.
	ok := filepath.Join(filepath.Dir(file), "worktree-here")
	require.NoError(t, os.MkdirAll(ok, 0o700))

	_, err = taskStore(mustCrew(t, file)).Add(task.Task{Role: "dev", Asked: "pick a colour", Worktree: ok}, time.Now())
	require.NoError(t, err)
	withRole(t, file, "dev2", func(rs *shift.RoleState) { rs.Held = false })

	out, errOut, err = zm(t, file, "task", "set", "T-2", "waiting", "--on", "alex", "--role", "dev2")
	require.NoError(t, err)
	assert.Contains(t, out, "T-2 · dev2 · waiting")
	assert.NotContains(t, errOut, "not started")
	assert.False(t, isUp(t, file, "dev2"))
}

func parkItem(t *testing.T, file string, it later.Item) {
	t.Helper()

	_, err := laterStore(mustCrew(t, file)).Add(it, time.Now())
	require.NoError(t, err)
}

// later take starts a seat that is down, with the key on, as up --task does; with --resume it is the old start, once.
func TestLaterTakeStartsADownSeat(t *testing.T) {
	file := testCrew(t, assignSeats+assignOn)
	parkItem(t, file, later.Item{Text: "rotate the deploy key"})

	out, errOut, err := zm(t, file, "later", "take", "L-1", "dev")
	require.NoError(t, err)
	assert.Regexp(t, `^T-1 · dev · L-1 taken · dev up as \S+\n$`, out)
	assert.Empty(t, errOut)
	assert.True(t, isUp(t, file, "dev"))
	require.Len(t, inboxOf(t, file, "dev"), 1)
	assert.Contains(t, inboxOf(t, file, "dev")[0], "task accept T-1")

	// A seat that is up is handed the task as before.
	parkItem(t, file, later.Item{Text: "and the staging key"})

	out, _, err = zm(t, file, "later", "take", "L-2", "dev")
	require.NoError(t, err)
	assert.Equal(t, "T-2 · dev · L-2 taken\n", out)
}

func TestLaterTakeWithResumeStaysTheOldStartOnce(t *testing.T) {
	file := testCrew(t, assignSeats+assignOn)
	parkItem(t, file, later.Item{Text: "rotate the deploy key", Role: "dev", Session: "abc-123"})

	out, _, err := zm(t, file, "later", "take", "L-1", "dev", "--resume")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev · L-1 taken · dev up on abc-123\n", out)
	assert.True(t, isUp(t, file, "dev"))

	handed := 0

	for _, line := range inboxOf(t, file, "dev") {
		if strings.Contains(line, "task accept T-1") {
			handed++
		}
	}

	assert.Equal(t, 1, handed, "one hand-out: the key does not start it a second time")
	assert.Empty(t, eventsNamed(t, file, "assign-started"), "the start was the --resume's")
}

func TestLaterTakeNotStartedQueuesAndSaysWhy(t *testing.T) {
	file := testCrew(t, assignSeats+assignOn)
	parkItem(t, file, later.Item{Text: "rotate the deploy key"})
	withRole(t, file, "dev", func(rs *shift.RoleState) { rs.Held = true })

	out, _, err := zm(t, file, "later", "take", "L-1", "dev")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev · L-1 taken · dev not started: it was put down by hand\n", out)
	assert.False(t, isUp(t, file, "dev"))
	assert.Len(t, inboxOf(t, file, "dev"), 1, "queued for the seat, as before")
}

func TestLaterTakeWithAssignOffIsAsBefore(t *testing.T) {
	file := testCrew(t, assignSeats)
	parkItem(t, file, later.Item{Text: "rotate the deploy key"})

	out, errOut, err := zm(t, file, "later", "take", "L-1", "dev")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev · L-1 taken\n", out)
	assert.Empty(t, errOut)
	assert.False(t, isUp(t, file, "dev"))
	assert.Len(t, inboxOf(t, file, "dev"), 1)
}

// The ticker's sweep task starts a seat that is down with the key on; its beat line then does not say the seat is down.
func TestAutomaticSweepStartsADownSeatWithAssign(t *testing.T) {
	file := testCrew(t, "  dev:\n"+autoSweep+assignOn)
	now := time.Now()

	for range 3 {
		parked(t, file, "old", 40*day, nil)
	}

	out := sweepBeat(t, file, now)
	assert.Contains(t, out, "later sweep: T-1 for dev, 3 of 3 stale parked items")
	assert.NotContains(t, out, "dev is down")
	assert.True(t, isUp(t, file, "dev"))
	assert.Len(t, eventsNamed(t, file, "assign-started"), 1)
}

// send --when-idle starts a seat that is down for the owner's line or the dispatcher's, never for a role's, a slash command, the
// dispatcher, a copy, or a seat that was put down by hand.
func TestSendWhenIdleStartsADownSeatForTheOwnerAndTheDispatcher(t *testing.T) {
	for who, role := range map[string]string{"the owner": "", "the dispatcher": "dispatcher"} {
		t.Run(who, func(t *testing.T) {
			file := testCrew(t, assignSeats+assignOn)
			as(t, role)

			out, _, err := zm(t, file, "send", "dev", "--when-idle", "look at the login form")
			require.NoError(t, err)
			assert.True(t, strings.HasSuffix(out, "; dev is down: started it\n"), out)
			assert.True(t, isUp(t, file, "dev"))
			assert.Len(t, eventsNamed(t, file, "line-started"), 1)
			assert.NotEmpty(t, inboxOf(t, file, "dev"), "the line waits for the seat to boot, as it does for any seat")
		})
	}
}

func TestSendWhenIdleDoesNotStartForARole(t *testing.T) {
	file := testCrew(t, assignSeats+assignOn)
	as(t, "dev2")

	out, _, err := zm(t, file, "send", "dev", "--when-idle", "look at the login form")
	require.NoError(t, err)
	assert.NotContains(t, out, "is down")
	assert.True(t, strings.HasPrefix(out, "queued for dev"), out)
	assert.False(t, isUp(t, file, "dev"), "a line from one seat to another never starts a seat")
	assert.Empty(t, eventsNamed(t, file, "line-started"))
}

func TestSendWhenIdleStartsNothingForTheRest(t *testing.T) {
	file := testCrew(t, assignSeats+assignOn)

	// A slash command is a command to a session, not work.
	out, _, err := zm(t, file, "send", "dev", "--when-idle", "/compact")
	require.NoError(t, err)
	assert.NotContains(t, out, "is down")
	assert.False(t, isUp(t, file, "dev"))

	// The dispatcher is the ticker's.
	out, _, err = zm(t, file, "send", "dispatcher", "--when-idle", "good morning")
	require.NoError(t, err)
	assert.NotContains(t, out, "is down")
	assert.False(t, isUp(t, file, "dispatcher"))

	// A copy is started with up --as.
	out, _, err = zm(t, file, "send", "dev#2", "--when-idle", "look at the login form")
	require.NoError(t, err)
	assert.Contains(t, out, "; dev#2 is down: not started (a copy is started with zmina up dev --as 2)")
	assert.False(t, isUp(t, file, "dev#2"))

	// A seat put down by hand stays down, and the line waits as it did.
	withRole(t, file, "dev", func(rs *shift.RoleState) { rs.Held = true })

	out, _, err = zm(t, file, "send", "dev", "--when-idle", "look at the login form")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(out, "; dev is down: not started (it was put down by hand)\n"), out)
	assert.False(t, isUp(t, file, "dev"))
	assert.NotEmpty(t, inboxOf(t, file, "dev"))
	assert.Len(t, eventsNamed(t, file, "line-refused"), 1)
}

// With the key off, or on for the other parts, the reply of send --when-idle is what it was, and no seat starts.
func TestSendWhenIdleWithAssignOffIsAsBefore(t *testing.T) {
	for _, key := range []string{"", "reconcile: {restart: true}\n", "reconcile: off\n"} {
		t.Run(strings.TrimSpace(key), func(t *testing.T) {
			file := testCrew(t, assignSeats+key)

			out, errOut, err := zm(t, file, "send", "dev", "--when-idle", "look at the login form")
			require.NoError(t, err)
			assert.Regexp(t, `^queued for dev as S-\d+: delivered once it is up and between turns; said again until it acks\n$|^queued for dev: typed the first time it is up and idle\n$`, out)
			assert.Empty(t, errOut)
			assert.False(t, isUp(t, file, "dev"))
			assert.Empty(t, eventsNamed(t, file, "line-started"))
			assert.Empty(t, eventsNamed(t, file, "line-refused"))
		})
	}
}

// A sleeping seat is not woken by a line either: its alarm does that.
func TestSendWhenIdleDoesNotWakeASleepingSeat(t *testing.T) {
	file := testCrew(t, assignSeats+assignOn)

	_, _, err := zm(t, file, "sleep", "dev", "--until", "+1h")
	require.NoError(t, err)
	require.NotEmpty(t, func() []alarm.Alarm {
		var pending []alarm.Alarm

		a := &app{out: io.Discard, err: io.Discard, crew: file}
		require.NoError(t, a.readState(func(s *shift.Shift) error { pending = s.Alarms.Pending(); return nil }))

		return pending
	}())

	out, _, err := zm(t, file, "send", "dev", "--when-idle", "look at the login form")
	require.NoError(t, err)
	assert.Contains(t, out, "; dev is down: not started (it is asleep)")
	assert.False(t, isUp(t, file, "dev"))
}

// A seat that starts others by giving them work would start them in a chain: only the owner's hand, the cockpit, the ticker (the sweep) and
// the dispatcher have what they give a seat that is down start it. What any other role gives is recorded as with the key off: the hint,
// no start, no event.
func TestARolesTaskForADownSeatStartsNothing(t *testing.T) {
	file := testCrew(t, assignSeats+"  dev3: {}\n"+assignOn)
	parkItem(t, file, later.Item{Text: "rotate the deploy key"})

	_, err := taskStore(mustCrew(t, file)).Add(task.Task{Role: "dev", Asked: "fix the form"}, time.Now())
	require.NoError(t, err)

	as(t, "dev3")

	out, errOut, err := zm(t, file, "task", "new", "dev2", "Review")
	require.NoError(t, err)
	assert.Equal(t, "T-2 · dev2\n", out)
	assert.Equal(t, "zmina: dev2 is down: zmina --crew "+quoteCommandArg(file)+" up 'dev2' --task T-2 starts it on the task\n", errOut, "the hint of the key off, no word of a refusal")
	assert.False(t, isUp(t, file, "dev2"))

	out, errOut, err = zm(t, file, "task", "set", "T-1", "open", "--role", "dev2")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev2 · open\n", out)
	assert.Empty(t, errOut)
	assert.False(t, isUp(t, file, "dev2"))

	out, _, err = zm(t, file, "later", "take", "L-1", "dev2")
	require.NoError(t, err)
	assert.Equal(t, "T-3 · dev2 · L-1 taken\n", out)
	assert.False(t, isUp(t, file, "dev2"))

	assert.Empty(t, eventsNamed(t, file, "assign-started"))
	assert.Empty(t, eventsNamed(t, file, "assign-refused"))
	assert.Len(t, inboxOf(t, file, "dev2"), 2, "queued for the seat, as with the key off: T-1 and T-3")

	// The dispatcher starts it (the owner's hand does, as the other tests show).
	f := testCrew(t, assignSeats+assignOn)
	as(t, "dispatcher")

	out, _, err = zm(t, f, "task", "new", "dev", "Review")
	require.NoError(t, err)
	assert.Regexp(t, `\ndev up as \S+ for T-1\n$`, out)
	assert.True(t, isUp(t, f, "dev"))
}

// Who may have a seat started by what they give it: the owner's hand, the cockpit, the ticker (its sweep) and the dispatcher.
func TestStartsSeatsNamesWhoMay(t *testing.T) {
	file := testCrew(t, assignSeats+assignOn)
	s := &shift.Shift{Crew: mustCrew(t, file), State: shift.NewState()}

	for by, want := range map[string]bool{"hand": true, "ui": true, "ticker": true, "dispatcher": true, "dev": false, "dev2#2": false,
		"codex-thread": false, "": false} {
		s.By = by
		assert.Equal(t, want, startsSeats(s), "%q", by)
	}
}

// A task given to a seat that is down starts it; when its packet cannot then be handed the seat is up, and every word says so: it is not
// "not started".
func TestTaskNewThatCannotHandThePacketSaysTheSeatIsUp(t *testing.T) {
	file := testCrew(t, assignSeats+assignOn)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(file), "handoffs"), []byte("a file where the packets go"), 0o600))

	out, errOut, err := zm(t, file, "task", "new", "dev", "Fix the form")
	require.NoError(t, err)
	assert.Regexp(t, `^T-1 · dev\ndev up as \S+ for T-1\n$`, out)
	assert.Contains(t, errOut, "zmina: dev is up, but its packet was not handed:")
	assert.Contains(t, errOut, "(zmina task handoff T-1 --queue hands it)")
	assert.NotContains(t, errOut, "not started")
	assert.True(t, isUp(t, file, "dev"))
	assert.Len(t, eventsNamed(t, file, "assign-started"), 1)
	assert.Empty(t, eventsNamed(t, file, "assign-refused"))
}

// zmina down by the dispatcher of a seat that holds no open task is routine: with assign on the seat is not held, and a task given to it
// later starts it. The owner's down, and a down of a seat that still holds an open task, or of a keep: always seat, stay held; with the key
// off nothing changes.
func TestADownByTheDispatcherIsRoutineOnlyWithAssign(t *testing.T) {
	seats := assignSeats + "  keeper:\n    keep: always\n"

	for name, c := range map[string]struct {
		key  string
		who  string
		role string
		task bool
		held bool
	}{
		"the dispatcher, no task": {assignOn, "dispatcher", "dev", false, false},
		"the owner's hand":        {assignOn, "", "dev", false, true},
		"it holds an open task":   {assignOn, "dispatcher", "dev", true, true},
		"a keep: always seat":     {assignOn, "dispatcher", "keeper", false, true},
		"the key off":             {"", "dispatcher", "dev", false, true},
		"assign off, restart on":  {"reconcile: {restart: true}\n", "dispatcher", "dev", false, true},
	} {
		t.Run(name, func(t *testing.T) {
			file := testCrew(t, seats+c.key)

			putDownBy(t, file, c.role, c.who, c.task)

			a := &app{out: io.Discard, err: io.Discard, crew: file}
			require.NoError(t, a.readState(func(s *shift.Shift) error {
				assert.Equal(t, c.held, s.State.Role(c.role).Held)

				return nil
			}))

			if c.key == assignOn && !c.held {
				assert.Len(t, eventsNamed(t, file, "down-routine"), 1)

				as(t, "")

				out, _, err := zm(t, file, "task", "new", c.role, "Fix the form")
				require.NoError(t, err)
				assert.Regexp(t, `dev up as \S+ for T-\d+`, out, "a later task starts it")
			}
		})
	}
}

// zmina stop is no routine down: with assign on the seats stay held, as a stop and a start always left them.
func TestAStopByTheDispatcherLeavesSeatsHeld(t *testing.T) {
	file := testCrew(t, assignSeats+assignOn)

	as(t, "")
	_, _, err := zm(t, file, "up", "dev")
	require.NoError(t, err)
	awaitPrompt(t, file, "dev")

	as(t, "dispatcher")
	_, _, err = zm(t, file, "stop", "--clear")
	require.NoError(t, err)

	a := &app{out: io.Discard, err: io.Discard, crew: file}
	require.NoError(t, a.readState(func(s *shift.Shift) error {
		assert.True(t, s.State.Role("dev").Held)

		return nil
	}))
}

// putDownBy starts a seat (with an open task when task is set), waits for its prompt and puts it down as who ("" the owner's hand).
func putDownBy(t *testing.T, file, role, who string, hasTask bool) {
	t.Helper()
	as(t, "")

	if hasTask {
		_, err := taskStore(mustCrew(t, file)).Add(task.Task{Role: role, Asked: "fix the form"}, time.Now())
		require.NoError(t, err)
	}

	_, _, err := zm(t, file, "up", role)
	require.NoError(t, err)
	awaitPrompt(t, file, role)

	as(t, who)

	_, _, err = zm(t, file, "down", role)
	require.NoError(t, err)

	as(t, "")
}

// awaitPrompt waits for the fake claude of a seat to draw its input line.
func awaitPrompt(t *testing.T, file, role string) {
	t.Helper()
	require.Eventually(t, func() bool {
		out, _, _ := zm(t, file, "peek", role, "-n", "5")

		return strings.Contains(out, "❯")
	}, 15*time.Second, 100*time.Millisecond)
}
