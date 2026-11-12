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
)

// A role that waits shows what on, from Claude Code's session file: `up waiting (question)`; --json has it as
// waiting_for. A role that does not wait shows nothing of the kind, whatever the file still holds.
func TestLsSaysWhatAWaitingRoleWaitsOn(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n")
	ns := filepath.Join(filepath.Dir(crewFile), "ns")
	pid := 4343

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

	write := func(status, waitingFor string) {
		body := `{"pid": 4343, "sessionId": "sid-1", "status": "` + status + `", "statusUpdatedAt": 1`
		if waitingFor != "" {
			body += `, "waitingFor": "` + waitingFor + `"`
		}

		require.NoError(t, os.WriteFile(file, []byte(body+"}"), 0o600))
	}

	for word, waitingFor := range map[string]string{"question": "input needed", "permission": "permission prompt"} {
		t.Run(word, func(t *testing.T) {
			write("waiting", waitingFor)

			assert.Contains(t, ls(), "dispatcher *  up waiting ("+word+")")
			assert.Contains(t, ls("--wide"), "waiting ("+word+")")

			var views []shift.View

			require.NoError(t, json.Unmarshal([]byte(ls("--json")), &views))
			require.NotEmpty(t, views)
			assert.Equal(t, word, views[0].WaitingFor)
		})
	}

	t.Run("waiting, the file says nothing more", func(t *testing.T) {
		write("waiting", "")

		out := ls()
		assert.Contains(t, out, "dispatcher *  up waiting")
		assert.NotContains(t, out, "waiting (")
		assert.NotContains(t, ls("--json"), "waiting_for")
	})

	t.Run("not waiting any more", func(t *testing.T) {
		write("idle", "input needed")

		out := ls()
		assert.Contains(t, out, "dispatcher *  up idle")
		assert.NotContains(t, out, "(question)")
		assert.NotContains(t, ls("--json"), "waiting_for")
	})
}
