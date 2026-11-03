package shift

import (
	"fmt"
	"github.com/iamnikolie/zmina/internal/recovery"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecoveryIncludesOpenThreadsAndKeepsOverflowIndex(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open, Note: "Latest decision"}}
	st := recovery.State{Version: 1, Followups: map[string]recovery.Followup{}}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("F-%d", i)
		st.Followups[id] = recovery.Followup{ID: id, Text: strings.Repeat("promise", 100)}
	}
	require.NoError(t, recovery.Save(f.s.Crew.StateDir(), st))
	path, p, err := f.s.RecoveryPack()
	require.NoError(t, err)
	assert.FileExists(t, path)
	assert.Equal(t, 12, p.Counts["followups"])
	require.Len(t, p.Entries["followups"], 8)
	assert.NotEmpty(t, p.Index)
	raw, err := os.ReadFile(p.Index)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "F-11")
	assert.Contains(t, p.Entries["tasks"][0].Text, "Latest decision")
	r, _ := f.s.Role("dispatcher")
	assert.Contains(t, f.s.BootText(r), "[zmina recovery]")
	require.NoError(t, os.WriteFile(filepath.Join(f.s.Crew.StateDir(), "dispatcher.json"), []byte("broken"), 0600))
	_, _, err = f.s.RecoveryPack()
	require.Error(t, err)
	raw, err = os.ReadFile(filepath.Join(f.s.Crew.StateDir(), "dispatcher.json"))
	require.NoError(t, err)
	assert.Equal(t, "broken", string(raw))
}
func TestNativeContextReductionQueuesRecoveryOnce(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("dispatcher"))
	f.status("dispatcher", "idle", *f.now)
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open}}
	f.rg.context["sid-dispatcher"] = 200000
	f.s.observeRecovery("dispatcher")
	f.rg.context["sid-dispatcher"] = 20000
	f.s.observeRecovery("dispatcher")
	require.Empty(t, f.s.State.Role("dispatcher").Inbox)
	f.s.observeRecovery("dispatcher")
	require.Len(t, f.s.State.Role("dispatcher").Inbox, 1)
	f.s.observeRecovery("dispatcher")
	assert.Len(t, f.s.State.Role("dispatcher").Inbox, 1)
}

func TestRecoveryIgnoresUnknownAndTransientContext(t *testing.T) {
	for _, readings := range [][]int{{340000, 0, 340000}, {340000, 120000, 340000}, {340000, -1, 340000}} {
		t.Run(fmt.Sprint(readings), func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("dispatcher"))
			f.status("dispatcher", "idle", *f.now)
			f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open}}
			for _, context := range readings {
				f.rg.context["sid-dispatcher"] = context
				f.s.observeRecovery("dispatcher")
				require.Empty(t, f.s.State.Role("dispatcher").Inbox)
				require.Equal(t, 340000, f.s.State.Role("dispatcher").RecoveryContext)
			}
			require.Empty(t, f.s.State.Role("dispatcher").Inbox, "unknown or one transient low sample is not compaction")
			require.Equal(t, 340000, f.s.State.Role("dispatcher").RecoveryContext)
		})
	}
}

// A trim zmina made queues its own recovery line; the drop in context it causes is not a native compaction too.
func TestManagedTrimQueuesRecoveryOnce(t *testing.T) {
	f := fixtureWith(t, trimCrew)
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open}}
	f.s.Tick()

	recoveries := func() int {
		n := 0
		for _, line := range append(append([]string{}, f.tm.typed["dispatcher"]...), f.s.State.Role("dispatcher").Inbox...) {
			if strings.Contains(line, "[zmina recovery]") {
				n++
			}
		}
		return n
	}

	f.rg.context["sid-dispatcher"] = 156_000
	f.status("dispatcher", "idle", f.now.Add(-10*time.Minute))
	*f.now = f.now.Add(6 * time.Minute)
	f.s.Tick()
	f.s.Tick()
	require.Equal(t, 1, compactCount(f.tm.typed["dispatcher"]))
	require.Equal(t, 1, recoveries())

	f.rg.context["sid-dispatcher"] = 0 // compacted: unknown until the next turn
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	for range 5 {
		f.rg.context["sid-dispatcher"] = 40_000
		*f.now = f.now.Add(time.Minute)
		f.status("dispatcher", "idle", f.now.Add(-10*time.Minute))
		f.s.Tick()
	}
	assert.Equal(t, 1, recoveries(), "the trim's own drop in context is no second compaction")
	assert.Equal(t, 40_000, f.s.State.Role("dispatcher").RecoveryContext)

	f.rg.context["sid-dispatcher"] = 120_000
	f.s.Tick()
	f.rg.context["sid-dispatcher"] = 15_000
	f.s.Tick()
	f.s.Tick()
	assert.Equal(t, 2, recoveries(), "a native compaction after it still queues its own")
}
