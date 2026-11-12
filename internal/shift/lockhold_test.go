package shift

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
)

// lockClock is the clock of the lock code under test: time moves only when a test moves it, or when the lock code
// sleeps between two tries (then onSleep may let the holder go).
type lockClock struct {
	now     time.Time
	onSleep func()
}

func fakeLockClock(t *testing.T) *lockClock {
	t.Helper()

	c := &lockClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	oldNow, oldSleep, oldSlowest := lockNow, lockSleep, slowestTmux
	lockNow = func() time.Time { return c.now }
	lockSleep = func(d time.Duration) {
		c.now = c.now.Add(d)

		if c.onSleep != nil {
			c.onSleep()
		}
	}
	slowestTmux = func() (string, time.Duration) { return "", 0 }

	t.Cleanup(func() { lockNow, lockSleep, slowestTmux = oldNow, oldSleep, oldSlowest })

	return c
}

// logged collects the events a lock logs.
type logged struct{ evs []events.Event }

func (l *logged) add(e events.Event) { l.evs = append(l.evs, e) }

// deadPID is the pid of a process that has ended.
func deadPID(t *testing.T) int {
	t.Helper()

	cmd := exec.Command("true")
	require.NoError(t, cmd.Run())

	return cmd.Process.Pid
}

func putHolder(t *testing.T, dir string, h Holder) {
	t.Helper()

	raw, err := json.Marshal(h)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(holderPath(dir), raw, 0o600))
}

func TestHolderRecordIsWrittenAndCleared(t *testing.T) {
	dir := t.TempDir()

	l, err := LockAs(dir, LockOpts{Verb: "task set"})
	require.NoError(t, err)

	h, ok := readHolder(dir)
	require.True(t, ok, "the record is there while the lock is held")
	assert.Equal(t, os.Getpid(), h.PID)
	assert.Equal(t, "task set", h.Verb, "the verb, never its arguments")
	assert.False(t, h.Since.IsZero())

	st := LockLook(dir)
	assert.True(t, st.Held)
	require.NotNil(t, st.Holder)
	assert.Equal(t, os.Getpid(), st.Holder.PID)
	assert.Nil(t, st.Stale)

	require.NoError(t, l.Save())

	_, err = os.Stat(holderPath(dir))
	assert.ErrorIs(t, err, os.ErrNotExist, "cleared with the lock")
	assert.Equal(t, LockStatus{}, LockLook(dir), "free, nothing left")

	l, err = LockAs(dir, LockOpts{Verb: "tick"})
	require.NoError(t, err)
	l.Release()

	_, err = os.Stat(holderPath(dir))
	assert.ErrorIs(t, err, os.ErrNotExist, "a release without a save clears it too")
}

func TestMarkUpdatesTheRecordAndTheLookDescribesIt(t *testing.T) {
	c := fakeLockClock(t)
	dir := t.TempDir()

	l, err := LockAs(dir, LockOpts{Verb: "tick"})
	require.NoError(t, err)

	c.now = c.now.Add(10 * time.Second)
	l.Mark("roles", "dev")

	h, ok := readHolder(dir)
	require.True(t, ok)
	assert.Equal(t, "roles", h.Phase)
	assert.Equal(t, "dev", h.Detail)
	assert.Equal(t, c.now, h.PhaseSince)

	c.now = c.now.Add(3 * time.Minute)
	got := LockLook(dir).Holder
	require.NotNil(t, got)
	assert.Equal(t, "pid "+strconv.Itoa(os.Getpid())+" (tick), held 3m10s, in roles (dev) for 3m0s", got.Describe(c.now))

	l.Release()
	l.Mark("roles", "ops") // a lock let go notes nothing and does not panic
}

func TestARecordThatCannotBeWrittenNeverFailsTheCommand(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(holderPath(dir), 0o700), "a directory where the record goes: it cannot be opened")

	l, err := LockAs(dir, LockOpts{Verb: "tick"})
	require.NoError(t, err)

	l.Mark("roles", "dev")
	l.Role("dev").LastSession = "kept"
	require.NoError(t, l.Save())

	l, err = Lock(dir)
	require.NoError(t, err)
	assert.Equal(t, "kept", l.Role("dev").LastSession)
	l.Release()
}

func TestAStaleRecordIsNeverTakenForTheHolder(t *testing.T) {
	dir := t.TempDir()
	dead := deadPID(t)

	// A holder killed without cleaning up: the lock is free, its record stays.
	putHolder(t, dir, Holder{PID: dead, Verb: "tick", Since: time.Now()})

	st := LockLook(dir)
	assert.False(t, st.Held)
	assert.Nil(t, st.Holder)
	require.NotNil(t, st.Stale)
	assert.Equal(t, dead, st.Stale.PID)

	// A new taker overwrites it whole.
	l, err := LockAs(dir, LockOpts{Verb: "x"})
	require.NoError(t, err)

	h, ok := readHolder(dir)
	require.True(t, ok)
	assert.Equal(t, os.Getpid(), h.PID)

	// The lock is held now, but the record is of a process that is gone.
	putHolder(t, dir, Holder{PID: dead, Verb: "tick", Since: time.Now()})

	st = LockLook(dir)
	assert.True(t, st.Held)
	assert.Nil(t, st.Holder, "a dead pid holds nothing")
	require.NotNil(t, st.Stale)
	assert.Equal(t, os.Getpid(), st.PID, "the lock file still names who took it")

	// A live pid that is not the one the lock file names is no match either (a pid reused, an older zmina holding).
	putHolder(t, dir, Holder{PID: os.Getppid(), Verb: "tick", Since: time.Now()})

	st = LockLook(dir)
	assert.Nil(t, st.Holder)
	assert.NotNil(t, st.Stale)

	_, ok = liveHolder(dir)
	assert.False(t, ok)

	l.Release()
}

func TestAWaiterLogsLockWaitWithTheHolderItSaw(t *testing.T) {
	c := fakeLockClock(t)
	dir := t.TempDir()

	holder, err := LockAs(dir, LockOpts{Verb: "tick"})
	require.NoError(t, err)

	c.now = c.now.Add(2 * time.Minute)
	holder.Mark("roles", "dev")

	released := c.now.Add(8 * time.Second)
	c.onSleep = func() {
		if !c.now.Before(released) && holder != nil {
			holder.Release()
			holder = nil
		}
	}

	var log logged

	l, err := LockAs(dir, LockOpts{Verb: "task set", Log: log.add})
	require.NoError(t, err)

	h, ok := readHolder(dir)
	require.True(t, ok)
	assert.Equal(t, "task set", h.Verb, "now the record is the waiter's own")
	l.Release()

	require.Len(t, log.evs, 1, "one event, whatever the number of tries")
	assert.Equal(t, "lock-wait", log.evs[0].Event)

	w := ParseWaitEvent(log.evs[0].Detail)
	assert.False(t, w.GaveUp)
	assert.GreaterOrEqual(t, w.Waited, 8*time.Second)
	assert.Less(t, w.Waited, 9*time.Second)
	assert.Equal(t, os.Getpid(), w.HolderPID)
	assert.Equal(t, "tick", w.HolderVerb)
	assert.Equal(t, "roles/dev", w.HolderPhase)
	assert.GreaterOrEqual(t, w.HolderFor, 2*time.Minute)
}

func TestAShortWaitLogsNothing(t *testing.T) {
	c := fakeLockClock(t)
	dir := t.TempDir()

	holder, err := Lock(dir)
	require.NoError(t, err)

	released := c.now.Add(time.Second)
	c.onSleep = func() {
		if !c.now.Before(released) && holder != nil {
			holder.Release()
			holder = nil
		}
	}

	var log logged

	l, err := LockAs(dir, LockOpts{Verb: "ack", Log: log.add})
	require.NoError(t, err)
	l.Release()

	assert.Empty(t, log.evs)
}

// A try takes a free lock as a command does, and finds a held one at once: nothing slept, written or logged, and the
// state is not read.
func TestATryNeverWaitsForAHeldLock(t *testing.T) {
	c := fakeLockClock(t)
	c.onSleep = func() { t.Error("a try never sleeps") }

	dir := t.TempDir()

	var log logged

	free, err := TryLockAs(dir, LockOpts{Verb: "tick --deliver", Log: log.add})
	require.NoError(t, err)

	h, ok := readHolder(dir)
	require.True(t, ok, "the lock it took has its holder record")
	assert.Equal(t, os.Getpid(), h.PID)
	assert.Equal(t, "tick --deliver", h.Verb)

	free.Role("dev").Up = true
	require.NoError(t, free.Save())

	_, err = os.Stat(holderPath(dir))
	assert.ErrorIs(t, err, os.ErrNotExist, "cleared with the lock")

	// Another command holds the lock now.
	holder, err := LockAs(dir, LockOpts{Verb: "tick"})
	require.NoError(t, err)
	holder.Mark("roles", "dev")

	state := crew.Layout{Dir: dir}.State()
	lock := crew.Layout{Dir: dir}.Lock()
	require.NoError(t, os.WriteFile(lock, []byte("99999\n"), 0o600), "a pid no try may overwrite")

	stateBefore, err := os.ReadFile(state)
	require.NoError(t, err)

	// A state file that does not read would fail a try that took the lock: this one must not even look.
	require.NoError(t, os.WriteFile(state, []byte("not json"), 0o600))

	got, err := TryLockAs(dir, LockOpts{Verb: "tick --deliver", Log: log.add})
	require.ErrorIs(t, err, ErrLockHeld)
	assert.Nil(t, got)
	assert.Empty(t, log.evs, "no lock-wait event")

	h, ok = readHolder(dir)
	require.True(t, ok)
	assert.Equal(t, "tick", h.Verb, "the holder's record is the holder's")
	assert.Equal(t, "roles", h.Phase)

	pid, err := os.ReadFile(lock)
	require.NoError(t, err)
	assert.Equal(t, "99999\n", string(pid), "the lock file is not written")

	raw, err := os.ReadFile(state)
	require.NoError(t, err)
	assert.Equal(t, "not json", string(raw), "the state file is not read or written")

	require.NoError(t, os.WriteFile(state, stateBefore, 0o600))

	// The holder lets go: the next try takes it.
	holder.Release()

	again, err := TryLockAs(dir, LockOpts{Verb: "tick --deliver", Log: log.add})
	require.NoError(t, err)
	assert.True(t, again.Role("dev").Up, "it reads the state the holder left")
	again.Release()
	assert.Empty(t, log.evs)
}

func TestATimeoutNamesTheHolderAndLogsTheWait(t *testing.T) {
	c := fakeLockClock(t)
	dir := t.TempDir()

	old := LockWait
	LockWait = 30 * time.Second

	t.Cleanup(func() { LockWait = old })

	holder, err := LockAs(dir, LockOpts{Verb: "tick"})
	require.NoError(t, err)

	c.now = c.now.Add(4 * time.Minute)
	holder.Mark("roles", "dev")

	var log logged

	_, err = LockAs(dir, LockOpts{Verb: "ack", Log: log.add})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zmina pid "+strconv.Itoa(os.Getpid())+" (tick)")
	assert.Contains(t, err.Error(), "held 4m30.1s")
	assert.Contains(t, err.Error(), "in roles (dev) for 30.1s")
	assert.Contains(t, err.Error(), "waited 30.1s")

	require.Len(t, log.evs, 1)
	w := ParseWaitEvent(log.evs[0].Detail)
	assert.True(t, w.GaveUp)
	assert.Equal(t, 30*time.Second+100*time.Millisecond, w.Waited, "the try after the deadline")
	assert.Equal(t, "tick", w.HolderVerb)

	// A holder record that is stale is not named: the pid the lock file names is.
	putHolder(t, dir, Holder{PID: deadPID(t), Verb: "ghost", Since: c.now})

	_, err = Lock(dir)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "ghost")
	assert.Contains(t, err.Error(), "zmina pid "+strconv.Itoa(os.Getpid()))

	holder.Release()
}

func TestALongHoldLogsLockHeldWithItsPhases(t *testing.T) {
	c := fakeLockClock(t)
	slowestTmux = func() (string, time.Duration) { return "capture-pane", 9 * time.Second }

	dir := t.TempDir()

	var log logged

	l, err := LockAs(dir, LockOpts{Verb: "tick", Log: log.add})
	require.NoError(t, err)

	c.now = c.now.Add(200 * time.Millisecond)
	l.Mark("reconcile", "")
	c.now = c.now.Add(2 * time.Second)
	l.Mark("notices", "")
	c.now = c.now.Add(100 * time.Millisecond)
	l.Mark("roles", "dev")
	c.now = c.now.Add(3 * time.Minute)
	l.Mark("roles", "ops")
	c.now = c.now.Add(10 * time.Second)
	l.Mark("deliver", "")
	c.now = c.now.Add(5 * time.Second)
	require.NoError(t, l.Save())

	require.Len(t, log.evs, 1)
	assert.Equal(t, "lock-held", log.evs[0].Event)

	e := ParseHoldEvent(log.evs[0].Detail)
	assert.Equal(t, "tick", e.Verb)
	assert.Equal(t, 3*time.Minute+17*time.Second+300*time.Millisecond, e.Held)
	assert.Equal(t, "roles", e.Phase, "the phase it spent longest in")
	assert.Equal(t, 3*time.Minute+10*time.Second, e.PhaseFor, "both roles' stretches")
	assert.Equal(t, "roles/dev", e.Step, "and the one thing in it that took longest")
	assert.Equal(t, 3*time.Minute, e.StepFor)
	assert.Equal(t, "capture-pane", e.Tmux)
	assert.Equal(t, 9*time.Second, e.TmuxFor)
	assert.Contains(t, log.evs[0].Detail, "phases=roles:3m10s,deliver:5s,reconcile:2s,run:200ms,notices:100ms,save:0s")
}

func TestAShortHoldLogsNothing(t *testing.T) {
	c := fakeLockClock(t)

	var log logged

	l, err := LockAs(t.TempDir(), LockOpts{Verb: "ack", Log: log.add})
	require.NoError(t, err)

	c.now = c.now.Add(LockHeldNote - time.Millisecond)
	require.NoError(t, l.Save())

	assert.Empty(t, log.evs)
}

func TestAVerbWithBlanksSurvivesTheEvent(t *testing.T) {
	c := fakeLockClock(t)

	var log logged

	l, err := LockAs(t.TempDir(), LockOpts{Verb: "task set", Log: log.add})
	require.NoError(t, err)

	c.now = c.now.Add(time.Minute)
	l.Release()

	require.Len(t, log.evs, 1)
	assert.Equal(t, "task set", ParseHoldEvent(log.evs[0].Detail).Verb)
	assert.Empty(t, ParseHoldEvent(log.evs[0].Detail).Step, "no step when nothing was named")
}

// The marks are bookkeeping: a beat with them runs as one without, line for line, and states what it leaves are equal.
func TestMarksChangeNothingOfTheBeat(t *testing.T) {
	plain := fixtureWith(t, crewYAML)
	marked := fixtureWith(t, crewYAML)

	var phases []string

	marked.s.Mark = func(phase, detail string) {
		if len(phases) == 0 || phases[len(phases)-1] != phase {
			phases = append(phases, phase)
		}
	}

	for i := 0; i < 3; i++ {
		*plain.now = plain.now.Add(time.Minute)
		*marked.now = marked.now.Add(time.Minute)

		assert.Equal(t, plain.s.Tick(), marked.s.Tick())
	}

	a, err := json.Marshal(plain.s.State)
	require.NoError(t, err)

	b, err := json.Marshal(marked.s.State)
	require.NoError(t, err)

	assert.JSONEq(t, string(a), string(b))
	assert.Equal(t, plain.tm.starts, marked.tm.starts)
	assert.Equal(t, plain.tm.typed, marked.tm.typed)

	assert.Equal(t, []string{"reconcile", "notices", "alarms", "roles", "notes", "deliver", "trim", "reconcile-end",
		"reconcile", "notices", "alarms", "roles", "notes", "deliver", "trim", "reconcile-end",
		"reconcile", "notices", "alarms", "roles", "notes", "deliver", "trim", "reconcile-end"}, phases)
}

func TestTickNamesTheRoleItIsOn(t *testing.T) {
	f := fixtureWith(t, crewYAML)

	var marks []string

	f.s.Mark = func(phase, detail string) { marks = append(marks, phase+"/"+detail) }
	f.s.Tick()

	assert.Contains(t, marks, "roles/dispatcher")
	assert.Contains(t, marks, "roles/designer")
	assert.Contains(t, marks, "roles/harvester")
}
