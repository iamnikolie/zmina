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

// A role that runs `zmina restart <itself>` from its own session (a tool shell its claude runs) is not put down
// under the command: the restart goes to a detached zmina that waits for that turn to end, then restarts the role on
// the conversation it was on, though its policy (no resume: always, as the dispatchers) would start it fresh.
func TestRestartFromOwnSessionWaitsForTurnAndResumes(t *testing.T) {
	ownRestartPoll = 50 * time.Millisecond
	defer func() { ownRestartPoll = time.Second }()

	file := testCrew(t, "  dev: {}\n")
	dir := filepath.Dir(file)
	_, _, err := zm(t, file, "up", "dev")
	require.NoError(t, err)
	require.Eventually(t, func() bool { out, _, _ := zm(t, file, "peek", "dev"); return strings.Contains(out, "claude ") }, 15*time.Second, 50*time.Millisecond)

	out, _, err := zm(t, file, "ls", "dev", "--json")
	require.NoError(t, err)
	var views []struct {
		PID int `json:"pid"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &views))
	require.Len(t, views, 1)
	pane := views[0].PID
	require.Positive(t, pane)

	session := func(status string) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"pid": pane, "sessionId": "conv-1", "cwd": dir, "status": status})
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "ns", "sessions"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "ns", "sessions", strconv.Itoa(pane)+".json"), raw, 0o600))
	}
	session("busy") // its turn runs the restart
	a := &app{crew: file}
	require.NoError(t, a.withState(func(s *shift.Shift) error { s.State.Role("dev").Booted = time.Now(); return nil }))

	var spawned []string
	var outb, errb bytes.Buffer
	me := os.Getpid()
	root := newRoot(&app{out: &outb, err: &errb, spawn: func(args []string) error { spawned = args; return nil },
		parent: func(pid int) (int, error) {
			if pid == me {
				return pane, nil // this test is a shell the role's claude runs
			}
			return 1, nil
		}})
	root.SetArgs([]string{"--crew", file, "restart", "dev"})
	require.NoError(t, root.Execute())
	assert.Contains(t, outb.String(), "dev restarts once this turn ends, on this conversation")
	require.Equal(t, []string{"--crew", file, "restart", "dev", "--after-turn", strconv.Itoa(pane)}, spawned)
	typed, _ := os.ReadFile(filepath.Join(dir, "typed.log"))
	assert.NotContains(t, string(typed), "/exit", "nothing is put down under the command that asked")

	done := make(chan error, 1)
	go func() { _, _, err := zm(t, file, spawned[2:]...); done <- err }()

	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("the detached half went before the turn ended: %v", err)
	default:
	}

	session("idle") // the turn is over
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("the detached half never restarted the role")
	}

	var screen string
	require.Eventually(t, func() bool { screen, _, _ = zm(t, file, "peek", "dev"); return strings.Contains(screen, "claude ") }, 15*time.Second, 50*time.Millisecond)
	assert.Contains(t, screen, "--resume conv-1", "back on the conversation it was on")
	assert.Contains(t, screen, "up", "the role is up")
	out, _, err = zm(t, file, "ls", "dev", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &views))
	assert.NotEqual(t, pane, views[0].PID, "a new process")

	// A detached half whose role was restarted meanwhile leaves it alone.
	_, _, err = zm(t, file, "restart", "dev", "--after-turn", strconv.Itoa(pane))
	require.NoError(t, err)
	log, _, err := zm(t, file, "log", "--role", "dev")
	require.NoError(t, err)
	assert.Contains(t, log, "restart-skipped")
	assert.Contains(t, log, "restart-pending")
}

// Inside walks real parents with ps.
func TestProcParentReadsPS(t *testing.T) {
	got, err := procParent(os.Getpid())
	require.NoError(t, err)
	assert.Equal(t, os.Getppid(), got)
}
