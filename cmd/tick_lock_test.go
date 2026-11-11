package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

// The beat holds no state lock while it talks to a peer: a session that does not answer for a while must not make
// every other zmina command fail with "lock held".
func TestTickHoldsNoStateLockWhileItTalksToAPeer(t *testing.T) {
	crewFile := testCrew(t, "  dev: {}\n")
	dialed := filepath.Join(t.TempDir(), "dialed")

	_, _, err := zm(t, crewFile, "node", "init", "--name", "laptop", "--owner")
	require.NoError(t, err)

	// A peer whose command starts, says nothing and ends a few seconds later.
	_, _, err = zm(t, crewFile, "node", "peer", "add", "far", "--cmd", "touch '"+dialed+"'; sleep 4 #")
	require.NoError(t, err)

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	was := shift.LockWait
	shift.LockWait = time.Second

	t.Cleanup(func() { shift.LockWait = was })

	done := make(chan error, 1)

	go func() {
		_, _, err := zm(t, crewFile, "tick")
		done <- err
	}()

	require.Eventually(t, func() bool { _, err := os.Stat(dialed); return err == nil }, 10*time.Second,
		20*time.Millisecond, "the beat never called the peer")

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err, "the beat holds the state lock while its session with the peer is open")
	l.Release()

	select {
	case err := <-done:
		t.Fatalf("the beat ended (%v) before the peer's command did: the test proved nothing", err)
	default:
	}

	assert.NoError(t, <-done)
}
