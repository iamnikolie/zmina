package cmd

import (
	"bytes"
	"encoding/json"
	"io"
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

// stopFailure is what Claude Code hands a StopFailure hook (a live payload).
const stopFailure = `{"session_id":"s-1","hook_event_name":"StopFailure","error":"model_not_found",` +
	`"last_assistant_message":"There's an issue with the selected model (…). It may not exist or you may not have access ` +
	`to it. Run /model to pick a different model."}`

// failureText is what the record says of it.
const failureText = "model_not_found: There's an issue with the selected model (…). It may not exist or you may not have " +
	"access to it. Run /model to pick a different model."

// eventLines are the lines of the crew's event log.
func eventLines(t *testing.T, crewFile string) []string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(filepath.Dir(crewFile), "events.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}

	require.NoError(t, err)

	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// The StopFailure hook only records: the error for the role, one event for the hold it starts, and nothing else. It starts
// no process, posts nothing and says nothing: no decision.
func TestStopFailureHookOnlyRecords(t *testing.T) {
	crewFile := testCrew(t, "")
	layout := crew.Layout{Dir: filepath.Dir(crewFile)}

	var out, errb bytes.Buffer

	spawned := false

	a := &app{out: &out, err: &errb, spawn: func([]string) error { spawned = true; return nil }}
	root := newRoot(a)
	root.SetIn(strings.NewReader(stopFailure))
	root.SetArgs([]string{"--crew", crewFile, "_stop-failure", "--role", "dev"})
	require.NoError(t, root.Execute())

	assert.Empty(t, out.String(), "nothing on stdout: no decision")
	assert.Empty(t, errb.String())
	assert.False(t, spawned, "nothing is started: the hook records only")

	raw, err := os.ReadFile(layout.HookFailure("dev"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"session_id":"s-1"`)
	assert.Contains(t, string(raw), `"error":"model_not_found"`)
	assert.Contains(t, string(raw), `"details":"There's an issue with the selected model`)

	lines := eventLines(t, crewFile)
	require.Len(t, lines, 1, "one event, the hold's start")
	assert.Contains(t, lines[0], `"event":"failure-hold"`)
	assert.Contains(t, lines[0], `"role":"dev"`)
	assert.Contains(t, lines[0], "model_not_found")
	assert.Contains(t, lines[0], "wait 5m0s")

	// A second failure is a second hold, and its own event.
	root = newRoot(&app{out: &out, err: &errb})
	root.SetIn(strings.NewReader(stopFailure))
	root.SetArgs([]string{"--crew", crewFile, "_stop-failure", "--role", "dev"})
	require.NoError(t, root.Execute())
	assert.Len(t, eventLines(t, crewFile), 2)
}

// Whatever it is given, the hook returns 0 and prints nothing; it records, and logs the hold, only for a StopFailure of a
// role.
func TestStopFailureHookIsSilentWhateverItIsGiven(t *testing.T) {
	for name, tc := range map[string]struct {
		stdin  string
		args   []string
		record bool
	}{
		"a failure":        {stopFailure, []string{"--role", "dev"}, true},
		"not json":         {"not json", []string{"--role", "dev"}, false},
		"nothing":          {"", []string{"--role", "dev"}, false},
		"no session":       {`{"error":"x"}`, []string{"--role", "dev"}, false},
		"no role":          {stopFailure, nil, false},
		"a path as a role": {stopFailure, []string{"--role", "../dev"}, false},
		"an unknown flag":  {stopFailure, []string{"--role", "dev", "--nope"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			crewFile := testCrew(t, "")
			layout := crew.Layout{Dir: filepath.Dir(crewFile)}

			var out, errb bytes.Buffer

			root := newRoot(&app{out: &out, err: &errb})
			root.SetIn(strings.NewReader(tc.stdin))
			root.SetArgs(append([]string{"--crew", crewFile, "_stop-failure"}, tc.args...))
			require.NoError(t, root.Execute())

			assert.Empty(t, out.String()+errb.String())

			if tc.record {
				assert.FileExists(t, layout.HookFailure("dev"))
				assert.Len(t, eventLines(t, crewFile), 1)
			} else {
				assert.NoFileExists(t, layout.HookFailure("dev"))
				assert.Empty(t, eventLines(t, crewFile), "no hold, no event")
			}
		})
	}

	t.Run("a crew directory that is not there", func(t *testing.T) {
		gone := filepath.Join(t.TempDir(), "gone", "crew.yaml")

		var out, errb bytes.Buffer

		root := newRoot(&app{out: &out, err: &errb})
		root.SetIn(strings.NewReader(stopFailure))
		root.SetArgs([]string{"--crew", gone, "_stop-failure", "--role", "dev"})
		require.NoError(t, root.Execute())

		assert.Empty(t, out.String()+errb.String())
		assert.NoDirExists(t, filepath.Dir(gone))
	})
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
	assert.Contains(t, ls(), "dispatcher *  up idle (last turn ended in an API error: "+clip(failureText, 60)+")")
	assert.Contains(t, ls("--wide"), "idle (last turn ended in an API error: model_not_found: There's an issue")

	var views []shift.View

	require.NoError(t, json.Unmarshal([]byte(ls("--json")), &views))
	require.NotEmpty(t, views)
	assert.Equal(t, failureText, views[0].TurnError)

	// The next turn started.
	write("busy", 1_060_000)

	out := ls()
	assert.Contains(t, out, "dispatcher *  up busy")
	assert.NotContains(t, out, "API error")
	assert.NotContains(t, ls("--json"), "turn_error")
}

// While a failed turn holds a role's queue, Waiting leaves the role out and the courier's look at the queue (queueGated) says
// it is gated, so the courier looks again every few seconds and finds the role when the hold ends: not spinning on it
// meanwhile, not forgetting it after.
func TestCourierLooksAgainWhenAFailedTurnsHoldEnds(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n")
	dir := filepath.Dir(crewFile)
	ns := filepath.Join(dir, "ns")

	var failedAt = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	s, err := a.newShift()
	require.NoError(t, err)

	s.Reg = &registry.Registry{Namespace: ns}
	s.State = shift.NewState()

	rs := s.State.Role("dev")
	rs.Up, rs.Inbox = true, []string{"[dispatcher] next"}

	// dev's session, alive (this process stands for it), idle since the failure, found by its session name.
	require.NoError(t, os.MkdirAll(filepath.Join(ns, "sessions"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ns, "sessions", strconv.Itoa(os.Getpid())+".json"), []byte(
		`{"pid": `+strconv.Itoa(os.Getpid())+`, "sessionId": "s-1", "name": "`+s.Crew.Roles["dev"].Session+
			`", "status": "idle", "statusUpdatedAt": `+strconv.FormatInt(failedAt.UnixMilli(), 10)+`}`), 0o600))

	now := failedAt.Add(time.Minute)
	s.Now = func() time.Time { return now }

	assert.Equal(t, []string{"dev"}, s.Waiting(), "no failure: the role is waiting")

	f, err := shift.RecordFailure(crew.Layout{Dir: dir}, "dev", []byte(stopFailure), failedAt)
	require.NoError(t, err)
	require.NotNil(t, f)

	assert.Empty(t, s.Waiting(), "held: Waiting leaves the role out")
	assert.True(t, queueGated(s, s.Waiting()), "and the queue reads as gated, so the courier looks again")

	now = failedAt.Add(shift.FailHold - time.Second)
	assert.True(t, queueGated(s, s.Waiting()), "still held a second before the end")

	now = failedAt.Add(shift.FailHold + time.Second)
	assert.Equal(t, []string{"dev"}, s.Waiting(), "the hold is over: the role is waiting again")
	assert.False(t, queueGated(s, s.Waiting()))
}
