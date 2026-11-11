package lesson

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGit puts a git first in PATH that runs script (a shell script body; $3 is git's command, after -C <dir>).
func fakeGit(t *testing.T, script string) {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+script+"\n"), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// shortTimeouts shortens the deadlines for a test, and has no terminal drive the run, however the tests were started.
func shortTimeouts(t *testing.T, local, remote time.Duration) {
	t.Helper()

	l, r, d := GitTimeout, GitNetTimeout, terminalDriven
	GitTimeout, GitNetTimeout, terminalDriven = local, remote, func() bool { return false }

	t.Cleanup(func() { GitTimeout, GitNetTimeout, terminalDriven = l, r, d })
}

func TestGitTimeoutByCommand(t *testing.T) {
	shortTimeouts(t, time.Second, time.Minute)

	for _, c := range []string{"fetch", "push", "ls-remote", "pull"} {
		assert.Equal(t, time.Minute, gitTimeout([]string{c, "-q"}), c+" talks to the remote")
	}

	for _, c := range []string{"status", "rev-parse", "worktree", "commit", "show", "merge-base"} {
		assert.Equal(t, time.Second, gitTimeout([]string{c}), c+" does not")
	}

	assert.Equal(t, time.Second, gitTimeout(nil))
}

// A git that never answers ends at its deadline, with an error that says so, whatever it holds.
func TestGitEndsAtItsDeadline(t *testing.T) {
	shortTimeouts(t, 300*time.Millisecond, 300*time.Millisecond)
	fakeGit(t, "sleep 30")

	begin := time.Now()
	_, err := Git(t.TempDir(), "status")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "git status: no answer in 300ms")
	assert.Less(t, time.Since(begin), 5*time.Second)

	begin = time.Now()
	_, err = gitCombined(context.Background(), t.TempDir(), "push", "origin")

	require.ErrorContains(t, err, "no answer in 300ms")
	assert.Less(t, time.Since(begin), 5*time.Second)
}

// The call's own context ends it too, and says that is why.
func TestGitContextEndsWithItsContext(t *testing.T) {
	shortTimeouts(t, time.Minute, time.Minute)
	fakeGit(t, "sleep 30")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	begin := time.Now()
	_, err := GitContext(ctx, t.TempDir(), "fetch")

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ErrorContains(t, err, "stopped")
	assert.Less(t, time.Since(begin), 5*time.Second)
}

// What git started (ssh, a hook) is killed with it when nothing drives the run from a terminal: it does not outlive
// the deadline holding the pipes, and the call returns at the deadline, not when that process ends.
func TestGitKillsWhatItStartedAtItsDeadline(t *testing.T) {
	shortTimeouts(t, 500*time.Millisecond, 500*time.Millisecond)

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	fakeGit(t, "sleep 30 &\necho $! > '"+pidFile+"'\nwait")

	begin := time.Now()
	_, err := Git(t.TempDir(), "fetch")

	require.ErrorContains(t, err, "no answer in 500ms")
	assert.Less(t, time.Since(begin), 5*time.Second, "the child's pipes did not hold the call")

	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err)

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)

	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) }) // the pid this test started, whatever happened

	require.Eventually(t, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }, 5*time.Second,
		20*time.Millisecond, "the process git started is still running")
}

// A remote command gets the longer deadline: one that takes longer than a local command may is not cut.
func TestGitRemoteCommandsGetTheLongerDeadline(t *testing.T) {
	shortTimeouts(t, 200*time.Millisecond, 20*time.Second)
	fakeGit(t, "sleep 1\necho ok")

	out, err := Git(t.TempDir(), "fetch", "-q", "origin")
	require.NoError(t, err)
	assert.Equal(t, "ok", out)

	_, err = Git(t.TempDir(), "status")
	require.ErrorContains(t, err, "no answer in 200ms", "a local command is not given as long")
}

// A failed git still says what it printed; a push's reply is on its standard error.
func TestGitSaysWhatFailed(t *testing.T) {
	shortTimeouts(t, time.Minute, time.Minute)
	fakeGit(t, "echo 'remote: no' >&2\nexit 3")

	_, err := Git(t.TempDir(), "rev-parse", "x")
	require.ErrorContains(t, err, "git rev-parse x: exit status 3: remote: no")

	out, err := gitCombined(context.Background(), t.TempDir(), "push", "origin")
	require.Error(t, err)
	assert.Equal(t, "remote: no\n", out, "the reply comes back with the failure")
}

// A triage whose fetch never answers fails at the deadline instead of holding the run for good.
func TestTriageFailsWhenTheRemoteNeverAnswers(t *testing.T) {
	shortTimeouts(t, time.Minute, 300*time.Millisecond)
	fakeGit(t, `[ "$3" = fetch ] && sleep 30
echo ok`)

	begin := time.Now()
	_, err := Triage(context.Background(), Config{Repo: t.TempDir(), Branch: "main", Now: time.Now()}, nil, nil)

	require.ErrorContains(t, err, "no answer in 300ms")
	assert.Less(t, time.Since(begin), 5*time.Second)
}
