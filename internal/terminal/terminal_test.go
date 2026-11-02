package terminal

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDisconnectKeepsTmuxSessionAlive(t *testing.T) {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	socket := fmt.Sprintf("zmina-term-test-%d", time.Now().UnixNano())
	run := func(args ...string) string {
		cmd := exec.Command(bin, append([]string{"-L", socket}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return string(out)
	}
	run("new-session", "-d", "-s", "dev", "-x", "80", "-y", "24", "cat")
	defer func() { _ = exec.Command(bin, "-L", socket, "kill-server").Run() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tty, err := Attach(ctx, []string{bin, "-L", socket, "attach-session", "-t", "=dev"}, 80, 24)
	require.NoError(t, err)
	require.NoError(t, tty.Resize(42, 16))
	_, err = tty.Write([]byte("TERMINAL_ALIVE\r"))
	require.NoError(t, err)
	// PTY output is asynchronous; wait for the input to reach the existing pane.
	require.Eventually(t, func() bool { return strings.Contains(run("capture-pane", "-p", "-t", "=dev:"), "TERMINAL_ALIVE") }, 3*time.Second, 20*time.Millisecond)
	require.NoError(t, tty.Close())
	require.Equal(t, "0", strings.TrimSpace(run("display-message", "-p", "-t", "=dev:", "#{pane_dead}")))
	next, err := Attach(ctx, []string{bin, "-L", socket, "attach-session", "-t", "=dev"}, 80, 24)
	require.NoError(t, err)
	require.NoError(t, next.Close())
}
