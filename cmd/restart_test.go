package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRestartExplicitModes(t *testing.T) {
	for _, mode := range []string{"--fresh", "--continue"} {
		t.Run(mode, func(t *testing.T) {
			file := testCrew(t, "  dev:\n    resume: always\n")
			a := &app{crew: file}
			require.NoError(t, a.withState(func(s *shift.Shift) error { s.State.Role("dev").LastSession = "previous-session"; return nil }))
			_, _, err := zm(t, file, "restart", "dev", mode)
			require.NoError(t, err)
			var screen string
			require.Eventually(t, func() bool { screen, _, _ = zm(t, file, "peek", "dev"); return strings.Contains(screen, "claude ") }, 15*time.Second, 50*time.Millisecond)
			if mode == "--fresh" {
				assert.NotContains(t, screen, "--resume")
			} else {
				assert.Contains(t, screen, "--resume previous-session")
			}
		})
	}
	file := testCrew(t, "  dev: {}\n")
	_, _, err := zm(t, file, "restart", "dev", "--fresh", "--continue")
	require.Error(t, err)
}

func TestRestartCopyPreservesContinuationBeforeTeardown(t *testing.T) {
	for _, mode := range []string{"--continue", "policy", "--fresh"} {
		t.Run(mode, func(t *testing.T) {
			file := testCrew(t, "  dev:\n    resume: always\n")
			_, _, err := zm(t, file, "up", "dev", "--as", "2")
			require.NoError(t, err)
			require.Eventually(t, func() bool { out, _, _ := zm(t, file, "peek", "dev#2"); return strings.Contains(out, "claude ") }, 15*time.Second, 50*time.Millisecond)
			a := &app{crew: file}
			require.NoError(t, a.withState(func(s *shift.Shift) error { s.State.Role("dev#2").LastSession = "saved-copy-session"; return nil }))
			args := []string{"restart", "dev#2"}
			if mode != "policy" {
				args = append(args, mode)
			}
			_, _, err = zm(t, file, args...)
			require.NoError(t, err)
			var screen string
			require.Eventually(t, func() bool { screen, _, _ = zm(t, file, "peek", "dev#2"); return strings.Contains(screen, "claude ") }, 15*time.Second, 50*time.Millisecond)
			if mode == "--fresh" {
				assert.NotContains(t, screen, "--resume")
			} else {
				assert.Contains(t, screen, "--resume saved-copy-session")
			}
		})
	}
}

func TestRestartIncompatibleCopyFailsBeforeStopping(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	_, _, err := zm(t, file, "up", "dev", "--as", "2")
	require.NoError(t, err)
	require.Eventually(t, func() bool { out, _, _ := zm(t, file, "peek", "dev#2"); return strings.Contains(out, "claude ") }, 15*time.Second, 50*time.Millisecond)
	a := &app{crew: file}
	require.NoError(t, a.withState(func(s *shift.Shift) error {
		rs := s.State.Role("dev#2")
		rs.LastSession = "saved"
		rs.LastRuntime = "codex"
		return nil
	}))
	_, _, err = zm(t, file, "restart", "dev#2", "--continue")
	require.ErrorContains(t, err, "last session belongs to codex")
	_, _, err = zm(t, file, "peek", "dev#2")
	require.NoError(t, err, "invalid continuation must leave the live copy running")
}

// ownCrew is a crew whose dev is up and booted, with its pane's pid and a way to publish its registry entry.
func ownCrew(t *testing.T) (file string, pane func() int, session func(pid int, status string)) {
	t.Helper()
	ownRestartPoll = 50 * time.Millisecond
	t.Cleanup(func() { ownRestartPoll, ownRestartWait = time.Second, 30*time.Minute })

	file = testCrew(t, "  dev: {}\n")
	dir := filepath.Dir(file)
	_, _, err := zm(t, file, "up", "dev")
	require.NoError(t, err)
	require.Eventually(t, func() bool { out, _, _ := zm(t, file, "peek", "dev"); return strings.Contains(out, "claude ") }, 15*time.Second, 50*time.Millisecond)

	pane = func() int {
		t.Helper()
		out, _, err := zm(t, file, "ls", "dev", "--json")
		require.NoError(t, err)
		var views []struct {
			PID int `json:"pid"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &views))
		require.Len(t, views, 1)
		return views[0].PID
	}
	session = func(pid int, status string) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"pid": pid, "sessionId": "conv-1", "cwd": dir, "status": status})
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "ns", "sessions"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "ns", "sessions", strconv.Itoa(pid)+".json"), raw, 0o600))
	}
	a := &app{crew: file}
	require.NoError(t, a.withState(func(s *shift.Shift) error { s.State.Role("dev").Booted = time.Now(); return nil }))

	return file, pane, session
}

// askOwnRestart runs `zmina restart dev` as the role's own tool would (its claude's session named in the environment)
// and returns the detached half's arguments, without the crew flag.
func askOwnRestart(t *testing.T, file string, extra ...string) []string {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "conv-1")
	t.Setenv("CODEX_THREAD_ID", "")

	var spawned []string
	var outb, errb bytes.Buffer
	root := newRoot(&app{out: &outb, err: &errb, spawn: func(args []string) error { spawned = args; return nil },
		parent: func(int) (int, error) { return 1, nil }}) // ancestry alone would not find it (a shared daemon's tool)
	root.SetArgs(append([]string{"--crew", file, "restart", "dev"}, extra...))
	require.NoError(t, root.Execute())
	assert.Contains(t, outb.String(), "dev restarts once this turn ends, on this conversation")
	require.GreaterOrEqual(t, len(spawned), 8)
	require.Equal(t, []string{"--crew", file, "restart", "dev", "--after-turn"}, spawned[:5])

	return spawned[2:]
}

func devEvents(t *testing.T, file, event string) int {
	t.Helper()
	out, _, err := zm(t, file, "log", "--role", "dev", "--json")
	require.NoError(t, err)
	var events []struct {
		Event string `json:"event"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &events))
	n := 0
	for _, e := range events {
		if e.Event == event {
			n++
		}
	}
	return n
}

// A role that runs `zmina restart <itself>` from its own session is not put down under the command: the restart goes
// to a detached zmina that waits for that turn to end, then restarts the role on the conversation it was on, though
// its policy (no resume: always, as the dispatchers) would start it fresh.
func TestRestartFromOwnSessionWaitsForTurnAndResumes(t *testing.T) {
	file, pane, session := ownCrew(t)
	first := pane()
	session(first, "busy") // its turn runs the restart

	worker := askOwnRestart(t, file)
	typed, _ := os.ReadFile(filepath.Join(filepath.Dir(file), "typed.log"))
	assert.NotContains(t, string(typed), "/exit", "nothing is put down under the command that asked")

	done := make(chan error, 1)
	go func() { _, _, err := zm(t, file, worker...); done <- err }()

	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("the detached half went before the turn ended: %v", err)
	default:
	}

	session(first, "idle") // the turn is over
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the detached half never restarted the role")
	}

	var screen string
	require.Eventually(t, func() bool { screen, _, _ = zm(t, file, "peek", "dev"); return strings.Contains(screen, "claude ") }, 15*time.Second, 50*time.Millisecond)
	assert.Contains(t, screen, "--resume conv-1", "back on the conversation it was on")
	assert.NotEqual(t, first, pane(), "a new process, up")
	assert.Equal(t, 1, devEvents(t, file, "restart-pending"))
}

// The detached half decides under the lock it begins the down in. A session that replaced the one that asked while
// the half waited for the lock is left alone, busy as it is.
func TestOwnRestartLeavesAReplacementAlone(t *testing.T) {
	file, pane, session := ownCrew(t)
	first := pane()
	session(first, "idle")
	worker := askOwnRestart(t, file)

	a := &app{crew: file}
	s, err := a.newShift()
	require.NoError(t, err)
	l, err := shift.Lock(s.Crew.StateDir())
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { _, _, err := zm(t, file, worker...); done <- err }()
	time.Sleep(500 * time.Millisecond) // its unlocked look saw the turn over: it waits for the lock

	s.State = l.State
	require.NoError(t, s.Down("dev", shift.DownOptions{}))
	require.NoError(t, s.UpWith("dev", shift.UpOptions{}))
	require.NoError(t, l.Save())
	var second int
	require.Eventually(t, func() bool { second = pane(); return second != 0 && second != first }, 15*time.Second, 50*time.Millisecond)
	session(second, "busy")

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the detached half never returned")
	}
	assert.Equal(t, second, pane(), "the replacement is not put down")
	assert.Equal(t, 1, devEvents(t, file, "restart-skipped"))
}

// A turn that begins again while the half waits for the lock holds the restart until it ends.
func TestOwnRestartWaitsForATurnThatBeganAgain(t *testing.T) {
	file, pane, session := ownCrew(t)
	first := pane()
	session(first, "idle")
	worker := askOwnRestart(t, file)

	a := &app{crew: file}
	s, err := a.newShift()
	require.NoError(t, err)
	l, err := shift.Lock(s.Crew.StateDir())
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { _, _, err := zm(t, file, worker...); done <- err }()
	time.Sleep(500 * time.Millisecond)
	session(first, "busy") // a line started a turn
	require.NoError(t, l.Save())

	time.Sleep(500 * time.Millisecond)
	typed, _ := os.ReadFile(filepath.Join(filepath.Dir(file), "typed.log"))
	assert.NotContains(t, string(typed), "/exit", "a busy session is not put down")
	assert.Equal(t, first, pane())

	session(first, "idle")
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the detached half never restarted the role")
	}
	assert.NotEqual(t, first, pane())
}

// Two restarts asked from the same session restart it once.
func TestOwnRestartsOfOneSessionCoalesce(t *testing.T) {
	file, pane, session := ownCrew(t)
	first := pane()
	session(first, "idle")
	w1 := askOwnRestart(t, file)
	w2 := askOwnRestart(t, file)
	ups := devEvents(t, file, "up")

	done := make(chan error, 2)
	for _, w := range [][]string{w1, w2} {
		go func() { _, _, err := zm(t, file, w...); done <- err }()
	}
	for range 2 {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(40 * time.Second):
			t.Fatal("a detached half never returned")
		}
	}

	assert.Equal(t, ups+1, devEvents(t, file, "up"), "one restart")
	assert.Equal(t, 1, devEvents(t, file, "restart-skipped"))
	assert.NotEqual(t, first, pane())
}

// A turn that never ends gives up the restart: the role stays as it is and the dispatcher hears it.
func TestOwnRestartGivesUp(t *testing.T) {
	file, pane, session := ownCrew(t)
	first := pane()
	session(first, "busy")
	worker := askOwnRestart(t, file)
	ownRestartWait = 300 * time.Millisecond

	_, _, err := zm(t, file, worker...)
	require.ErrorContains(t, err, "its turn did not end")
	assert.Equal(t, first, pane())
	assert.Equal(t, 1, devEvents(t, file, "restart-failed"))
	q, _, err := zm(t, file, "queue", "ls", "--full")
	require.NoError(t, err)
	assert.Contains(t, q, "zmina restart dev --continue")
}

// Inside walks real parents with ps.
func TestProcParentReadsPS(t *testing.T) {
	got, err := procParent(os.Getpid())
	require.NoError(t, err)
	assert.Equal(t, os.Getppid(), got)
}
