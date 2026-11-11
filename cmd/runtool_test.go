package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

// hangingTools puts programs of these names first on PATH that never answer, and shortens the deadlines so a test
// that fails by waiting out the real ones is not mistaken for a pass.
func hangingTools(t *testing.T, names ...string) {
	t.Helper()

	dir := t.TempDir()
	for _, name := range names {
		// exec: the shell becomes the sleep, so ending the program ends all of it.
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755))
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	keepPS, keepService := psTimeout, serviceTimeout
	psTimeout, serviceTimeout = 150*time.Millisecond, 150*time.Millisecond

	t.Cleanup(func() { psTimeout, serviceTimeout = keepPS, keepService })
}

// A program that does not answer is killed at its deadline, and the error says so.
func TestRunToolEndsAProgramThatDoesNotAnswer(t *testing.T) {
	start := time.Now()
	_, err := runTool(100*time.Millisecond, true, "sh", "-c", "exec sleep 30")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no answer in 100ms")
	assert.Less(t, time.Since(start), 5*time.Second)
}

// A program that answers keeps its output (with combined, its standard error too) and its own exit status.
func TestRunToolReturnsWhatTheProgramPrinted(t *testing.T) {
	out, err := runTool(5*time.Second, false, "sh", "-c", "echo out; echo err >&2")
	require.NoError(t, err)
	assert.Equal(t, "out\n", string(out))

	out, err = runTool(5*time.Second, true, "sh", "-c", "echo out; echo err >&2; exit 3")
	require.Error(t, err)
	assert.Contains(t, string(out), "out")
	assert.Contains(t, string(out), "err")
	assert.NotContains(t, err.Error(), "no answer")
}

// ps, which the beat runs to read a process's command line and parent, ends instead of holding the beat.
func TestProcessReadsEndWhenPSDoesNotAnswer(t *testing.T) {
	hangingTools(t, "ps")

	_, err := procArgs(os.Getpid())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no answer")

	_, err = procParent(os.Getpid())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no answer")
}

// The ticker's service manager calls (launchctl, systemctl, loginctl) end when the manager does not answer.
func TestServiceManagerCallsEndWhenItDoesNotAnswer(t *testing.T) {
	hangingTools(t, "launchctl", "systemctl", "loginctl")

	err := launchctl("print", "gui/1/none")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no answer")

	err = systemctl("daemon-reload")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no answer")

	assert.False(t, lingering(), "a logind that does not answer is no linger")

	var out bytes.Buffer

	c := &crew.Crew{Path: filepath.Join(t.TempDir(), "crew.yaml")}
	require.NoError(t, systemdStatus(&out, c, ""))
	assert.Contains(t, out.String(), "ticker not installed", "a timer that cannot be asked is not installed")

	out.Reset()

	require.NoError(t, launchdStatus(&out, c, ""))
	assert.Contains(t, out.String(), "ticker not loaded")
}
