package shift

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/registry"

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

	got, inside := f.s.Inside("dispatcher", 900, "")
	assert.True(t, inside, "a shell the role's claude runs")
	assert.Equal(t, pane, got.Pane)
	assert.Equal(t, f.s.State.Role("dispatcher").Started.UnixNano(), got.Started)

	_, inside = f.s.Inside("dispatcher", 700, "")
	assert.False(t, inside, "another terminal")
	_, inside = f.s.Inside("designer", 900, "")
	assert.False(t, inside, "a role that is down")

	f.s.Parent = nil
	_, inside = f.s.Inside("dispatcher", 900, "")
	assert.False(t, inside, "no way to read parents and no session named")
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
	own := Own{Pane: pane, Started: f.s.State.Role("dispatcher").Started.UnixNano()}
	over, gone := f.s.OwnTurnOver("dispatcher", own)
	assert.False(t, over, "the turn that asked still runs")
	assert.False(t, gone)

	f.status("dispatcher", "idle", f.now.Add(time.Second))
	over, gone = f.s.OwnTurnOver("dispatcher", own)
	assert.True(t, over)
	assert.False(t, gone)

	f.status("dispatcher", "shell", f.now.Add(2*time.Second))
	f.rg.endedAt["sid-dispatcher"] = *f.now
	over, _ = f.s.OwnTurnOver("dispatcher", own)
	assert.True(t, over, "its turn is over while a background shell runs")

	f.tm.drafts = map[string]string{"dispatcher": "half a line"}
	over, _ = f.s.OwnTurnOver("dispatcher", own)
	assert.False(t, over, "the owner types into it")
	delete(f.tm.drafts, "dispatcher")

	_, gone = f.s.OwnTurnOver("dispatcher", Own{Pane: pane + 1, Started: own.Started})
	assert.True(t, gone, "another process runs in its pane: restarted meanwhile")
	_, gone = f.s.OwnTurnOver("dispatcher", Own{Pane: pane, Started: own.Started - 1})
	assert.True(t, gone, "another start under the same pid")
}

// A tool of a runtime that names its conversation is inside the role whose current session that is, whatever its
// process ancestry: a shared Codex App Server's tools descend from the daemon, not from the role's pane.
func TestInsideBySession(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	f.s.Parent = func(int) (int, error) { return 1, nil } // the daemon: never the pane

	own, inside := f.s.Inside("dispatcher", 900, "sid-dispatcher")
	assert.True(t, inside)
	assert.Equal(t, f.tm.up["dispatcher"], own.Pane)

	_, inside = f.s.Inside("dispatcher", 900, "sid-designer")
	assert.False(t, inside, "another conversation")
}

// The claim decides under the lock: a turn that runs again, a session that replaced the one that asked and a restart
// that claimed it first are never put down by this one.
func TestClaimOwnRestart(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	f.rg.byPID[f.tm.up["dispatcher"]].Cwd = f.s.Crew.Dir
	own := Own{Pane: f.tm.up["dispatcher"], Started: f.s.State.Role("dispatcher").Started.UnixNano()}

	f.status("dispatcher", "busy", *f.now) // its last turn has not ended
	_, _, _, err := f.s.ClaimOwnRestart("dispatcher", own, false, false, false)
	require.ErrorIs(t, err, ErrOwnTurnRuns, "a turn began again")
	assert.Empty(t, f.tm.typed["dispatcher"], "nothing typed into a busy session")
	assert.True(t, f.s.State.Role("dispatcher").Downing.IsZero())

	f.status("dispatcher", "idle", *f.now)
	f.rg.byPID[f.tm.up["dispatcher"]].Cwd = f.s.Crew.Dir
	opts, wait, claim, err := f.s.ClaimOwnRestart("dispatcher", own, false, false, false)
	require.NoError(t, err)
	assert.True(t, wait)
	assert.Equal(t, "sid-dispatcher", opts.Resume)
	assert.Equal(t, []string{"/exit"}, f.tm.typed["dispatcher"])

	_, _, _, err = f.s.ClaimOwnRestart("dispatcher", own, false, false, false)
	require.ErrorIs(t, err, ErrOwnGone, "a second pending restart of the same session coalesces")
	assert.Equal(t, []string{"/exit"}, f.tm.typed["dispatcher"], "typed once")

	require.ErrorIs(t, f.s.FinishOwnRestart("dispatcher", claim.Add(-time.Second), opts), ErrOwnGone,
		"another command's down is not this claim")
	require.NoError(t, f.s.FinishOwnRestart("dispatcher", claim, opts))
	assert.True(t, f.s.State.Role("dispatcher").Up)
	assert.NotEqual(t, own.Pane, f.tm.up["dispatcher"], "a new process")
	assert.Contains(t, f.tm.argv["dispatcher"], "sid-dispatcher", "on the conversation it was on")

	_, _, _, err = f.s.ClaimOwnRestart("dispatcher", own, false, false, false)
	require.ErrorIs(t, err, ErrOwnGone, "the session that asked was replaced: its replacement is left alone")
}

// A shared Codex role's tool runs under the shared App Server daemon, not under the role's bridge pane: its thread
// (CODEX_THREAD_ID), the bridge's current one, makes the restart its own, and the claim waits for that turn to end.
func TestSharedCodexOwnRestartByThread(t *testing.T) {
	s, _, _, _ := fixture(t)
	r, err := s.Role("designer")
	require.NoError(t, err)
	r.Runtime, r.CodexTransport = "codex", "shared"
	require.NoError(t, s.Up("designer"))
	s.Parent = func(int) (int, error) { return 1, nil } // the daemon's tool: never under the pane
	p, err := s.probe("designer")
	require.NoError(t, err)
	reg := &codex.Registry{Dir: filepath.Join(s.Crew.StateDir(), "codex")}
	rec := codex.Record{Session: registry.Session{PID: p.PID, Name: r.Session, SessionID: "thread-1", Status: "busy", StatusUpdatedAt: s.Now().UnixMilli()}}
	require.NoError(t, reg.Save(rec))

	own, inside := s.Inside("designer", 4242, "thread-1")
	require.True(t, inside, "its own thread")
	_, inside = s.Inside("designer", 4242, "thread-2")
	assert.False(t, inside, "another thread of the same daemon")

	_, _, _, err = s.ClaimOwnRestart("designer", own, true, false, false)
	require.ErrorIs(t, err, ErrOwnTurnRuns, "the asking turn runs: the bridge is not ended under it")
}
