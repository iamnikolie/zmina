package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/shift"
)

func TestLockCommandSaysWhoHoldsItAndNeverWaits(t *testing.T) {
	crewFile := testCrew(t, "")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	was := shift.LockWait
	shift.LockWait = 30 * time.Second

	t.Cleanup(func() { shift.LockWait = was })

	out, _, err := zm(t, crewFile, "lock")
	require.NoError(t, err)
	assert.Equal(t, "free\n", out)

	l, err := shift.LockAs(c.StateDir(), shift.LockOpts{Verb: "tick"})
	require.NoError(t, err)
	l.Mark("roles", "dev")

	before := snapshotDir(t, c.StateDir())
	start := time.Now()

	out, _, err = zm(t, crewFile, "lock")
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "a look at a held lock does not queue behind it")
	assert.Contains(t, out, "held by pid "+strconv.Itoa(os.Getpid())+" (tick), held ")
	assert.Contains(t, out, ", in roles (dev) for ")
	assert.Equal(t, before, snapshotDir(t, c.StateDir()), "nothing written, nothing taken away")

	l.Release()

	out, _, err = zm(t, crewFile, "lock")
	require.NoError(t, err)
	assert.Equal(t, "free\n", out)
}

func TestLockCommandCallsALeftoverRecordStale(t *testing.T) {
	crewFile := testCrew(t, "")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	gone := exec.Command("true")
	require.NoError(t, gone.Run())

	raw, err := json.Marshal(shift.Holder{PID: gone.Process.Pid, Verb: "tick", Since: time.Now().Add(-time.Hour)})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(c.Layout().LockHolder(), raw, 0o600))

	out, _, err := zm(t, crewFile, "lock")
	require.NoError(t, err)
	assert.Contains(t, out, "free; a stale record is left")
	assert.Contains(t, out, "pid "+strconv.Itoa(gone.Process.Pid)+" (tick)")
}

// Every command's lock events name the command, never its arguments.
func TestLockEventsNameTheCommand(t *testing.T) {
	crewFile := testCrew(t, "")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	was := shift.LockHeldNote
	shift.LockHeldNote = 0

	t.Cleanup(func() { shift.LockHeldNote = was })

	_, _, err = zm(t, crewFile, "todo", "add", "a secret text that must not reach the log")
	require.NoError(t, err)

	evs, err := eventLog(c).Read(events.Filter{})
	require.NoError(t, err)

	var held []events.Event

	for _, e := range evs {
		if e.Event == "lock-held" {
			held = append(held, e)
		}
	}

	require.Len(t, held, 1)
	assert.Equal(t, "todo add", shift.ParseHoldEvent(held[0].Detail).Verb)
	assert.NotContains(t, held[0].Detail, "secret")
	assert.Contains(t, held[0].Detail, "phases=")
	assert.True(t, strings.Contains(held[0].Detail, "save:"), "the save is a phase of every command")
}

func TestDoctorShowsTheLocksHoldersAndLongHolds(t *testing.T) {
	crewFile := testCrew(t, "")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	now := time.Now()
	log := eventLog(c)
	add := func(ago time.Duration, event, detail string) {
		require.NoError(t, log.Add(events.Event{TS: now.Add(-ago), Event: event, Detail: detail}))
	}

	// Two long holds in the last day, one older than that, one under the threshold (an old zmina's lower bar).
	add(time.Hour, "lock-held", "held=3m2s verb=tick phase=roles phase_for=2m50s step=roles/dev step_for=2m tmux=capture-pane tmux_for=9.8s")
	add(2*time.Hour, "lock-held", "held=13m2s verb=tick phase=deliver phase_for=12m50s")
	add(30*time.Hour, "lock-held", "held=40m verb=tick phase=roles phase_for=39m")
	add(3*time.Hour, "lock-held", "held=1s verb=ack")
	add(time.Hour, "lock-wait", "waited=30.1s holder=tick holder_pid=4242 holder_for=4m gave_up=yes")
	add(time.Hour, "lock-wait", "waited=6s holder=tick holder_pid=4242 holder_for=1m")
	add(time.Hour, "lock-wait", "waited=1s")

	var lines []string

	collect := func(name, level, format string, args ...any) {
		lines = append(lines, name+" "+level+" "+fmt.Sprintf(format, args...))
	}

	checkLockHolds(c, now, collect)

	require.Len(t, lines, 2, "free now: the holds, the waits")
	assert.Equal(t, "lock warn 2 holds over 20s in the last 24 h, longest 13m2s: tick, mostly in deliver (12m50s)", lines[0])
	assert.Equal(t, "lock warn 2 waits over 5s in the last 24 h, longest 30.1s, 1 gave up", lines[1])

	// Held now: the holder first, with its age.
	l, err := shift.LockAs(c.StateDir(), shift.LockOpts{Verb: "tick"})
	require.NoError(t, err)
	l.Mark("roles", "dev")

	lines = nil

	checkLockHolds(c, now.Add(time.Minute), collect)
	require.Len(t, lines, 3)
	assert.Equal(t, "lock warn holder: pid "+strconv.Itoa(os.Getpid())+" (tick), held 1m0s, in roles (dev) for 1m0s", lines[0])

	l.Release()
}

func TestDoctorLockLineWhenNothingWasLong(t *testing.T) {
	crewFile := testCrew(t, "")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	var lines []string

	checkLockHolds(c, time.Now(), func(name, level, format string, args ...any) {
		lines = append(lines, name+" "+level+" "+fmt.Sprintf(format, args...))
	})

	assert.Equal(t, []string{"lock ok no hold over 20s and no wait over 5s in the last 24 h"}, lines)
}
