package shift

import (
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestTaskConversationSurvivesCopyEndAndSelectsCompatibleHistory(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.s.Node = "local"
	require.NoError(t, f.s.Up("designer#2"))
	f.status("designer#2", "idle", *f.now)
	f.rg.context["sid-designer#2"] = 100
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer#2", State: task.Open}}
	require.NoError(t, f.s.Down("designer#2", DownOptions{}))
	assert.NotContains(t, f.s.State.Roles, "designer#2")
	got, err := f.tk.Get("T-1")
	require.NoError(t, err)
	require.Len(t, got.Conversations, 1)
	assert.Equal(t, "designer#2", got.Conversations[0].Role)
	opts, mode, err := f.s.TaskStart("designer#3", got, false, true)
	require.NoError(t, err)
	assert.Equal(t, "sid-designer#2", opts.Resume)
	assert.Contains(t, mode, "continue")
	fresh, _, err := f.s.TaskStart("designer", got, true, false)
	require.NoError(t, err)
	assert.True(t, fresh.Fresh)
	assert.Empty(t, fresh.Resume)
	for _, change := range []func(*task.Conversation){func(c *task.Conversation) { c.Runtime = "codex" }, func(c *task.Conversation) { c.Node = "remote" }, func(c *task.Conversation) { c.Home = "/different" }, func(c *task.Conversation) { c.Cwd = "/different" }} {
		copy := got
		copy.Conversations = append([]task.Conversation(nil), got.Conversations...)
		change(&copy.Conversations[0])
		_, _, err := f.s.TaskStart("designer", copy, false, true)
		require.ErrorContains(t, err, "--fresh")
	}
	opts, _, err = f.s.TaskStart("designer", task.Task{ID: "T-2"}, false, false)
	require.NoError(t, err)
	assert.True(t, opts.Fresh)
}
func TestParkKeepsConversationObservedDuringParking(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	f.rg.context["sid-designer"] = 42
	tsk := task.Task{ID: "T-1", Role: "designer", State: task.Open}
	f.tk.list = []task.Task{tsk}
	_, err := f.s.Park(tsk, "Remaining work")
	require.NoError(t, err)
	got, err := f.tk.Get("T-1")
	require.NoError(t, err)
	require.Len(t, got.Conversations, 1)
	assert.True(t, strings.HasPrefix(got.Conversations[0].Session, "sid-"))
}

func TestRestartContinuationRetainsDirectoryAndChecksHomeAfterCopyCleanup(t *testing.T) {
	s, tm, _, _ := fixture(t)
	name := "designer#2"
	require.NoError(t, s.Up(name))
	runtime, home := s.runtimeScope(s.Crew.Roles["designer"])
	dir := t.TempDir()
	rs := s.State.Role(name)
	rs.LastSession = "saved-copy"
	rs.LastRuntime = runtime
	rs.LastHome = home
	rs.LastDir = dir
	opts, err := s.RestartOptions(name, false, true)
	require.NoError(t, err)
	assert.Equal(t, "saved-copy", opts.Resume)
	assert.Equal(t, dir, opts.Dir)
	delete(s.State.Roles, name)
	delete(tm.up, name)
	// Configuration changes while shutdown waited outside the state lock cannot change the target's scope.
	s.Crew.Namespace = t.TempDir()
	require.ErrorContains(t, s.UpWith(name, opts), "continuation scope changed")
	s.Crew.Namespace = home
	require.NoError(t, s.UpWith(name, opts))
	assert.Equal(t, dir, s.State.Role(name).Dir)
}
