package lesson

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClaude puts a claude first on PATH that runs script.
func fakeClaude(t *testing.T, script string) {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"+script+"\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func shortSeatTimeout(t *testing.T, d time.Duration) {
	t.Helper()

	keep, driven := SeatTimeout, terminalDriven
	SeatTimeout, terminalDriven = d, func() bool { return false }

	t.Cleanup(func() { SeatTimeout, terminalDriven = keep, driven })
}

// A seat that never answers ends at its deadline, and takes what it started with it, so the daily run that nothing
// watches does not hang on it.
func TestClaudeSeatEndsWhenClaudeDoesNotAnswer(t *testing.T) {
	// The shell starts a sleep of its own: only a kill of the whole group ends both before the deadline's wait delay.
	fakeClaude(t, "sleep 30 &\nwait")
	shortSeatTimeout(t, 150*time.Millisecond)

	start := time.Now()
	_, err := ClaudeSeat("m")(context.Background(), t.TempDir(), "prompt", "{}", 1)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no answer in 150ms")
	assert.Less(t, time.Since(start), 2500*time.Millisecond, "ended at the deadline, not at the sleep or the wait delay")
}

// A seat that answers keeps its answer, and one the caller stops says that, not that it did not answer.
func TestClaudeSeatAnswersAndStops(t *testing.T) {
	fakeClaude(t, `cat >/dev/null; printf '{"ok":true}'`)
	shortSeatTimeout(t, 30*time.Second)

	out, err := ClaudeSeat("m")(context.Background(), t.TempDir(), "prompt", "{}", 1)
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true}`, string(out))

	fakeClaude(t, "exec sleep 30")

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()

	_, err = ClaudeSeat("m")(ctx, t.TempDir(), "prompt", "{}", 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stopped")
	assert.NotContains(t, err.Error(), "no answer")
}
