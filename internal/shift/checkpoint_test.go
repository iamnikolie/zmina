package shift

import (
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReleasePreservesRoleCheckpointAndMarksIncompleteFallbacks(t *testing.T) {
	for _, mode := range []string{"current", "missing", "stale", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			f := releasing(t, "")
			r, _ := f.s.Role("designer")
			r.Handoff = &crew.HandoffShape{Sections: []crew.Section{{Name: "decisions", Guidance: "Chosen direction and unresolved decisions"}}}
			tsk := task.Task{ID: "T-1", Role: "designer", State: task.Open, Updated: f.now.UTC()}
			if mode != "missing" {
				at := f.now.UTC()
				if mode == "stale" {
					at = at.Add(-time.Hour)
				}
				ref, err := record.Save(f.s.Crew.StateDir(), record.Document{Version: 1, Kind: "checkpoint", Task: tsk.ID, Role: tsk.Role, At: at, Shape: f.s.Crew.HandoffFor(r), Values: map[string]string{"decisions": "Direction A; question remains"}})
				require.NoError(t, err)
				tsk.Checkpoint = &ref
				if mode == "unreadable" {
					require.NoError(t, os.WriteFile(filepath.Join(f.s.Crew.StateDir(), "checkpoints", "T-1", ref.Revision+".json"), []byte("broken"), 0600))
				}
			}
			f.tk.list = []task.Task{tsk}
			f.s.Tick()
			files, err := filepath.Glob(filepath.Join(f.s.Crew.StateDir(), "releases", "*.json"))
			require.NoError(t, err)
			require.Len(t, files, 1)
			raw, err := os.ReadFile(files[0])
			require.NoError(t, err)
			assert.Contains(t, string(raw), "Chosen direction")
			*f.now = f.now.Add(releaseWait + time.Minute)
			f.s.Tick()
			require.Len(t, f.lt.list, 1)
			assert.Equal(t, mode != "current", f.lt.list[0].Incomplete)
			assert.Equal(t, tsk.Checkpoint, f.lt.list[0].Checkpoint)
			assert.False(t, f.tm.Has("designer"), "missing checkpoint does not strand idle role")
		})
	}
}
