package shift

import (
	"fmt"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func TestNoticesRevalidateAtDeliveryAndKeepCounts(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	note := "designer: idle for 30 min, no active task"
	f.s.addNotes([]string{note, note})
	require.Len(t, f.s.State.Notes, 1)
	assert.Contains(t, f.s.currentNotes()[0], "repeated 2 times")
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open}}
	assert.Empty(t, f.s.currentNotes(), "assignment resolves old idle notice")
	f.s.addNotes([]string{"legacy: unknown notice"})
	assert.Equal(t, []string{"legacy: unknown notice"}, f.s.currentNotes())
}
func TestKeyedReportsCoalesceAcrossDeliveryAndRefreshOnChange(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	q, err := f.s.Report("designer", "T-1/direction", "Direction ready")
	require.NoError(t, err)
	assert.True(t, q)
	f.s.State.Signals = nil // dispatcher consumed it
	*f.now = f.now.Add(time.Hour)
	q, err = f.s.Report("designer", "T-1/direction", "Direction ready")
	require.NoError(t, err)
	assert.False(t, q)
	assert.Empty(t, f.s.State.Signals)
	assert.Equal(t, 2, f.s.State.Reports["designer/T-1/direction"].Count)
	q, err = f.s.Report("designer", "T-1/direction", "Direction revised")
	require.NoError(t, err)
	assert.True(t, q)
	require.Len(t, f.s.State.Signals, 1)
	_, err = f.s.Note("owner", "Please check again", false)
	require.NoError(t, err)
	_, err = f.s.Note("owner", "Please check again", false)
	require.NoError(t, err)
	assert.Equal(t, 2, f.s.State.SignalCounts["owner: Please check again"])
}

func TestRemoteReportsGetBoundedFairnessWithoutOverwritingDrafts(t *testing.T) {
	f := fixtureWith(t, dispatcherOnly)
	require.NoError(t, f.s.Up("dispatcher"))
	*f.now = f.now.Add(bootPause)
	f.status("dispatcher", "idle", *f.now)
	_, err := f.s.NoteSent("remote box/ops", "remote report", *f.now)
	require.NoError(t, err)
	for i := 0; i < 4; i++ {
		_, err := f.s.Note("watch", fmt.Sprintf("local %d", i), false)
		require.NoError(t, err)
		f.s.deliver(*f.now)
		*f.now = f.now.Add(time.Minute)
	}
	lines := strings.Join(f.tm.typed["dispatcher"], "|")
	assert.Contains(t, lines, "remote box/ops")
	f.s.State.RemoteBatches = 3
	_, err = f.s.NoteSent("remote box/ops", "another report", *f.now)
	require.NoError(t, err)
	f.tm.drafts["dispatcher"] = "owner draft"
	assert.Empty(t, f.s.deliver(*f.now))
	assert.Equal(t, "owner draft", f.tm.drafts["dispatcher"])
}
