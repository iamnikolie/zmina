package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
)

const stopFailure = `{"session_id":"s-1","hook_event_name":"StopFailure","error":"rate_limit","error_details":"429 too many requests"}`

// The StopFailure hook records the error for the role, starts the detached copy that posts what waits once the session is
// idle (a failed turn runs no Stop hook), and says nothing.
func TestStopFailureHookRecordsAndStartsTheTurnEnd(t *testing.T) {
	crewFile := testCrew(t, "")
	layout := crew.Layout{Dir: filepath.Dir(crewFile)}

	var out, errb bytes.Buffer

	var spawned []string

	a := &app{out: &out, err: &errb, spawn: func(args []string) error { spawned = args; return nil }}
	root := newRoot(a)
	root.SetIn(strings.NewReader(stopFailure))
	root.SetArgs([]string{"--crew", crewFile, "_stop-failure", "--role", "dev"})
	require.NoError(t, root.Execute())

	assert.Empty(t, out.String(), "nothing on stdout: no decision")
	assert.Empty(t, errb.String())
	assert.Equal(t, []string{"--crew", crewFile, "_turn-end", "--role", "dev", "--session", "s-1", "--wait", "--failed"}, spawned)

	raw, err := os.ReadFile(layout.HookFailure("dev"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"session_id":"s-1"`)
	assert.Contains(t, string(raw), `"error":"rate_limit"`)
	assert.Contains(t, string(raw), `"details":"429 too many requests"`)
}

// Whatever it is given, the hook returns 0 and prints nothing; it starts the turn-end copy only for a session it was told,
// and a record that cannot be written does not stop that.
func TestStopFailureHookIsSilentWhateverItIsGiven(t *testing.T) {
	for name, tc := range map[string]struct {
		stdin   string
		args    []string
		spawned bool
		record  bool
	}{
		"a failure":        {stopFailure, []string{"--role", "dev"}, true, true},
		"not json":         {"not json", []string{"--role", "dev"}, false, false},
		"nothing":          {"", []string{"--role", "dev"}, false, false},
		"no session":       {`{"error":"x"}`, []string{"--role", "dev"}, false, false},
		"no role":          {stopFailure, nil, false, false},
		"a path as a role": {stopFailure, []string{"--role", "../dev"}, true, false},
		"an unknown flag":  {stopFailure, []string{"--role", "dev", "--nope"}, true, true},
	} {
		t.Run(name, func(t *testing.T) {
			crewFile := testCrew(t, "")
			layout := crew.Layout{Dir: filepath.Dir(crewFile)}

			var out, errb bytes.Buffer

			var spawned []string

			root := newRoot(&app{out: &out, err: &errb, spawn: func(args []string) error { spawned = args; return nil }})
			root.SetIn(strings.NewReader(tc.stdin))
			root.SetArgs(append([]string{"--crew", crewFile, "_stop-failure"}, tc.args...))
			require.NoError(t, root.Execute())

			assert.Empty(t, out.String()+errb.String())
			assert.Equal(t, tc.spawned, spawned != nil)

			if tc.record {
				assert.FileExists(t, layout.HookFailure("dev"))
			} else {
				assert.NoFileExists(t, layout.HookFailure("dev"))
			}
		})
	}

	t.Run("a crew directory that is not there", func(t *testing.T) {
		gone := filepath.Join(t.TempDir(), "gone", "crew.yaml")

		var spawned []string

		var out, errb bytes.Buffer

		root := newRoot(&app{out: &out, err: &errb, spawn: func(args []string) error { spawned = args; return nil }})
		root.SetIn(strings.NewReader(stopFailure))
		root.SetArgs([]string{"--crew", gone, "_stop-failure", "--role", "dev"})
		require.NoError(t, root.Execute())

		assert.Empty(t, out.String()+errb.String())
		assert.NoDirExists(t, filepath.Dir(gone))
		assert.Empty(t, spawned, "no crew: nothing to post for")
	})
}

// The detached copy of a failed turn's end gives up silently when the role's session is not the one the hook ran for.
func TestFailedTurnEndWaitForAnotherSessionPostsNothing(t *testing.T) {
	crewFile := testCrew(t, "")

	out, errOut, err := zm(t, crewFile, "_turn-end", "--role", "dev", "--session", "s-1", "--wait", "--failed")
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.Empty(t, errOut)
}

// ls says the last turn ended in an API error, from the record, while the role is idle since it; --json carries the words.
func TestLsSaysTheLastTurnEndedInAnAPIError(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n")
	dir := filepath.Dir(crewFile)
	ns := filepath.Join(dir, "ns")
	pid := 4444

	require.NoError(t, os.MkdirAll(filepath.Join(ns, "sessions"), 0o700))

	file := filepath.Join(ns, "sessions", strconv.Itoa(pid)+".json")
	write := func(status string, at int64) {
		require.NoError(t, os.WriteFile(file, []byte(`{"pid": 4444, "sessionId": "s-1", "status": "`+status+
			`", "statusUpdatedAt": `+strconv.FormatInt(at, 10)+`}`), 0o600))
	}

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

	// The role went idle as the hook ran, an hour ago.
	_, err := shift.RecordFailure(crew.Layout{Dir: dir}, "dispatcher", []byte(stopFailure), time.UnixMilli(1_000_000))
	require.NoError(t, err)

	write("idle", 1_000_000)
	assert.Contains(t, ls(), "dispatcher *  up idle (last turn ended in an API error: rate_limit: 429 too many requests)")
	assert.Contains(t, ls("--wide"), "idle (last turn ended in an API error: rate_limit: 429 too many requests)")

	var views []shift.View

	require.NoError(t, json.Unmarshal([]byte(ls("--json")), &views))
	require.NotEmpty(t, views)
	assert.Equal(t, "rate_limit: 429 too many requests", views[0].TurnError)

	// The next turn started.
	write("busy", 1_060_000)

	out := ls()
	assert.Contains(t, out, "dispatcher *  up busy")
	assert.NotContains(t, out, "API error")
	assert.NotContains(t, ls("--json"), "turn_error")
}
