package mesh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deadline is the context the stuck-child tests give a call; slack is what they allow after it: the kill, the
// pipes closed, and waitDelay for a pipe that something out of the command's reach still holds.
const (
	deadline = 500 * time.Millisecond
	slack    = waitDelay + 5*time.Second
)

// Every ssh call that only collects output asks the far side whether it is alive, so a dead link ends ssh itself.
func TestSSHCallsAskIfTheLinkIsAlive(t *testing.T) {
	alive := []string{"-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=3"}

	c := Command(context.Background(), Addr{SSH: "box"}, "node", "serve", "--stdio")
	assert.Equal(t, []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}, c.Args[:5])
	assert.Equal(t, alive, c.Args[5:9])
	assert.Equal(t, []string{"--", "box"}, c.Args[9:11])
	assert.True(t, strings.HasSuffix(c.Args[11], " zmina node serve --stdio"))

	own, err := OwnerCommand(context.Background(), Addr{SSH: "box"}, false, "ls")
	require.NoError(t, err)
	assert.Equal(t, alive, own.Args[5:9])
	assert.Equal(t, waitDelay, own.WaitDelay)

	// A command line is the owner's own: nothing is added to it.
	line := Command(context.Background(), Addr{Cmd: "ssh box zmina"}, "ls")
	assert.Equal(t, []string{"sh", "-c", "ssh box zmina ls"}, line.Args)
}

// A command that never answers and never exits ends with the call's context: Read returns, Close returns.
func TestDialEndsAtTheDeadlineOfAChildThatNeverExits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	// exec turns the shell into sleep; the words Command adds are a comment.
	rw, err := Exec{}.Dial(ctx, Addr{Cmd: "exec sleep 60 #"})
	require.NoError(t, err)

	start := time.Now()

	readErr := make(chan error, 1)

	go func() {
		_, err := rw.Read(make([]byte, 1))
		readErr <- err
	}()

	select {
	case err := <-readErr:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(slack):
		t.Fatal("Read is still blocked long after the deadline")
	}

	closed := make(chan error, 1)

	go func() { closed <- rw.Close() }()

	select {
	case <-closed:
	case <-time.After(slack):
		t.Fatal("Close is still waiting for the command long after the deadline")
	}

	assert.Less(t, time.Since(start), slack)
}

// A grandchild in the command's group holding its pipes dies with it, so nothing waits on it.
func TestDialKillsWhatTheCommandStarted(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rw, err := Exec{}.Dial(ctx, Addr{Cmd: "sleep 60 & echo $! > '" + pidFile + "'; wait #"})
	require.NoError(t, err)

	pid := readPid(t, pidFile)
	start := time.Now()

	endsWithin(t, "the stream", rw, cancel)

	assert.Less(t, time.Since(start), slack)
	assert.Eventually(t, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }, 3*time.Second,
		20*time.Millisecond, "the grandchild is still running")
}

// A process that left the command's group (its own session) and holds the pipes cannot keep Read or Close waiting.
func TestDialEndsWhileAnEscapedGrandchildHoldsThePipes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr, pidFile := escaper(t)

	rw, err := Exec{}.Dial(ctx, addr)
	require.NoError(t, err)

	pid := readPid(t, pidFile)
	start := time.Now()

	endsWithin(t, "the stream", rw, cancel)

	assert.Less(t, time.Since(start), slack)
	// The scenario held: the child is out of the group's reach, and still there.
	assert.NoError(t, syscall.Kill(pid, 0), "the escaped child is gone, so the test proved nothing")
}

// What runs a peer's command and collects its output (Command, then Wait) ends at the context too, whatever the
// command left behind: the command is killed, and Wait gives up on the pipes after waitDelay.
func TestCommandRunEndsAtTheDeadline(t *testing.T) {
	escaped, escapedPid := escaper(t)

	for name, tc := range map[string]struct {
		line    string
		pidFile string
	}{
		"never exits":             {line: "exec sleep 60 #"},
		"grandchild in its group": {line: "sleep 60 & wait #"},
		"grandchild out of reach": {line: escaped.Cmd, pidFile: escapedPid},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			c := Command(ctx, Addr{Cmd: tc.line}, "x")

			var out, errb bytes.Buffer

			c.Stdout, c.Stderr = &out, &errb

			require.NoError(t, c.Start())

			if tc.pidFile != "" {
				readPid(t, tc.pidFile)
			}

			start := time.Now()

			cancel()

			require.Error(t, c.Wait())
			assert.Less(t, time.Since(start), slack)
		})
	}
}

// endsWithin ends the context of a stream and fails the test when its Read or Close is still waiting after slack.
func endsWithin(t *testing.T, what string, rw io.ReadWriteCloser, end func()) {
	t.Helper()

	readErr := make(chan error, 1)

	go func() {
		_, err := rw.Read(make([]byte, 1))
		readErr <- err
	}()

	end()

	select {
	case err := <-readErr:
		require.Error(t, err)
	case <-time.After(slack):
		t.Fatalf("Read of %s is still blocked long after its context ended", what)
	}

	closed := make(chan struct{})

	go func() {
		_ = rw.Close() // it reports the kill; the test is about when it returns
		close(closed)
	}()

	select {
	case <-closed:
	case <-time.After(slack):
		t.Fatalf("Close of %s is still waiting long after its context ended", what)
	}
}

// escaper is a command line whose process starts a child in a session of its own, which holds the command's stdout
// and stderr, and then waits: killing the command's group does not reach the child.
// It also returns the file the child's pid is written to once it runs.
func escaper(t *testing.T) (Addr, string) {
	t.Helper()

	exe, err := os.Executable()
	require.NoError(t, err)

	pidFile := filepath.Join(t.TempDir(), "pid")

	return Addr{Cmd: "ZMINA_MESH_ESCAPER='" + pidFile + "' '" + exe + "' -test.run='^TestEscaperHelper$' #"}, pidFile
}

// TestEscaperHelper is the process escaper starts; it does nothing in a normal run of the tests.
func TestEscaperHelper(t *testing.T) {
	pidFile := os.Getenv("ZMINA_MESH_ESCAPER")
	if pidFile == "" {
		t.Skip("helper of the tests above")
	}

	// A short sleep: this child outlives the test by design, and nothing here kills it.
	c := exec.Command("sleep", "20")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	require.NoError(t, c.Start())
	require.NoError(t, os.WriteFile(pidFile, []byte(strconv.Itoa(c.Process.Pid)), 0o600))

	time.Sleep(time.Minute)
}

func readPid(t *testing.T, path string) int {
	t.Helper()

	var raw []byte

	require.Eventually(t, func() bool {
		var err error

		raw, err = os.ReadFile(path)

		return err == nil && strings.TrimSpace(string(raw)) != ""
	}, 3*time.Second, 20*time.Millisecond, "the command wrote no pid")

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)

	return pid
}
