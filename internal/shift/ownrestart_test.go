package shift

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A command is inside a role's own session when it descends from the process in the role's pane — a claude's tool
// shell — and not when it runs anywhere else.
func TestInsideReadsAncestors(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	pane := f.tm.up["dispatcher"]

	parents := map[int]int{900: 901, 901: pane, pane: 1, 700: 1}
	f.s.Parent = func(pid int) (int, error) {
		if p, ok := parents[pid]; ok {
			return p, nil
		}

		return 0, errors.New("no such process")
	}

	got, inside := f.s.Inside("dispatcher", 900)
	assert.True(t, inside, "a shell the role's claude runs")
	assert.Equal(t, pane, got)

	_, inside = f.s.Inside("dispatcher", 700)
	assert.False(t, inside, "another terminal")
	_, inside = f.s.Inside("designer", 900)
	assert.False(t, inside, "a role that is down")

	f.s.Parent = nil
	_, inside = f.s.Inside("dispatcher", 900)
	assert.False(t, inside, "no way to read parents: never inside")
}

// A role's restart from its own session goes on with the conversation it is on, though its policy would start fresh;
// --fresh still starts a new one.
func TestOwnRestartGoesOnWithItsConversation(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	f.rg.byPID[f.tm.up["dispatcher"]].Cwd = f.s.Crew.Dir
	f.rg.endedAt["sid-dispatcher"] = *f.now

	o, err := f.s.RestartOptions("dispatcher", false, false)
	require.NoError(t, err)
	assert.Empty(t, o.Resume, "from elsewhere a role without resume: always starts as its policy says")

	o, err = f.s.OwnRestartOptions("dispatcher", false, false)
	require.NoError(t, err)
	assert.Equal(t, "sid-dispatcher", o.Resume)

	o, err = f.s.OwnRestartOptions("dispatcher", true, false)
	require.NoError(t, err)
	assert.Empty(t, o.Resume, "--fresh")
}

// The detached half waits for the turn that asked to end, and leaves a role restarted meanwhile alone.
func TestOwnTurnOver(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	pane := f.tm.up["dispatcher"]

	f.status("dispatcher", "busy", *f.now)
	over, gone := f.s.OwnTurnOver("dispatcher", pane)
	assert.False(t, over, "the turn that asked still runs")
	assert.False(t, gone)

	f.status("dispatcher", "idle", f.now.Add(time.Second))
	over, gone = f.s.OwnTurnOver("dispatcher", pane)
	assert.True(t, over)
	assert.False(t, gone)

	f.status("dispatcher", "shell", f.now.Add(2*time.Second))
	f.rg.endedAt["sid-dispatcher"] = *f.now
	over, _ = f.s.OwnTurnOver("dispatcher", pane)
	assert.True(t, over, "its turn is over while a background shell runs")

	f.tm.drafts = map[string]string{"dispatcher": "half a line"}
	over, _ = f.s.OwnTurnOver("dispatcher", pane)
	assert.False(t, over, "the owner types into it")
	delete(f.tm.drafts, "dispatcher")

	_, gone = f.s.OwnTurnOver("dispatcher", pane+1)
	assert.True(t, gone, "another process runs in its pane: restarted meanwhile")
}
