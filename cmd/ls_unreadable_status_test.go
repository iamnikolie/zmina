package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// pidTmux answers for a crew whose roles all run as one pid, with nothing on their screens.
type pidTmux struct {
	shift.Tmux
	pid int
}

func (t pidTmux) Probe(string) (tmux.Pane, error)   { return tmux.Pane{State: tmux.Up, PID: t.pid}, nil }
func (pidTmux) Prompt(string) (string, bool, error) { return "", true, nil }
func (pidTmux) Capture(string, int) (string, error) { return "", nil }
func (pidTmux) KeepScreens(string) error            { return nil }
func (pidTmux) Attached(string) (bool, error)       { return false, nil }

// An up role whose session file in Claude Code's registry is empty or cut short shows as "up unknown"; ls says why in
// a few words, and --json carries the reason in full (what zmina doctor names, with the role to restart).
func TestLsSaysWhyAnUpRoleIsUnknown(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n")
	ns := filepath.Join(filepath.Dir(crewFile), "ns")
	pid := 4242

	require.NoError(t, os.MkdirAll(filepath.Join(ns, "sessions"), 0o700))

	file := filepath.Join(ns, "sessions", strconv.Itoa(pid)+".json")

	ls := func(args ...string) string {
		t.Helper()

		var out, errb bytes.Buffer

		a := &app{out: &out, err: &errb, shiftSeams: func(s *shift.Shift) {
			s.Tmux, s.Reg = pidTmux{pid: pid}, &registry.Registry{Namespace: ns}
		}}
		root := newRoot(a)
		root.SetArgs(append([]string{"--crew", crewFile, "ls"}, args...))
		require.NoError(t, root.Execute())

		return out.String()
	}

	for name, body := range map[string]string{"empty": "", "cut short": `{"pid": 4242, "sessionId": "sid-`} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(file, []byte(body), 0o600))

			assert.Contains(t, ls(), "dispatcher *  up unknown (session file unreadable; zmina doctor)")
			assert.Contains(t, ls("--wide"), "unknown (session file unreadable; zmina doctor)")

			var views []shift.View

			require.NoError(t, json.Unmarshal([]byte(ls("--json")), &views))
			require.NotEmpty(t, views)
			assert.Equal(t, "unknown", views[0].Status)
			assert.Contains(t, views[0].StatusError, "4242.json")
			assert.Contains(t, views[0].StatusError, "restart the role")
		})
	}

	t.Run("a session file that reads", func(t *testing.T) {
		require.NoError(t, os.WriteFile(file, []byte(`{"pid": 4242, "sessionId": "sid-1", "status": "idle", "statusUpdatedAt": 1}`), 0o600))

		out := ls()
		assert.Contains(t, out, "dispatcher *  up idle")
		assert.NotContains(t, out, "unreadable")
		assert.NotContains(t, ls("--json"), "status_error")
	})
}
