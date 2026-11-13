package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	assert.Contains(t, lines[0], "an account problem that needs the owner: the lines queued for it wait 30m, or less when its next turn ends or it is restarted")

	// A second failure is a second hold, and its own event.
	root = newRoot(&app{out: &out, err: &errb})
	root.SetIn(strings.NewReader(stopFailure))
	root.SetArgs([]string{"--crew", crewFile, "_stop-failure", "--role", "dev"})
	require.NoError(t, root.Execute())
	assert.Len(t, eventLines(t, crewFile), 2)
}

// The event the hook logs says how long the queue behind the failed turn waits, by the kind of the error: five minutes for a
// transient one, as it always said; until the reset time for a limit that gave one (never under five minutes nor over six
// hours); thirty minutes, or less when the role's next turn comes first, for an account problem.
func TestStopFailureHookEventSaysHowLongTheQueueWaits(t *testing.T) {
	for name, tc := range map[string]struct{ input, want string }{
		"transient": {`{"session_id":"s-1","error":"server_error","last_assistant_message":"API Error: 529 Overloaded"}`,
			"the lines queued for it wait 5m0s"},
		"a limit with its time": {`{"session_id":"s-1","error":"rate_limit","last_assistant_message":"You've hit your limit · resets ` +
			time.Now().UTC().Add(2*time.Hour).Format("Jan 2, 3:04pm") + ` (UTC)"}`,
			"a usage limit: the lines queued for it wait until "},
		"a limit with none": {`{"session_id":"s-1","error":"rate_limit"}`,
			"a usage limit with no reset time given: the lines queued for it wait 5m0s"},
		"account": {`{"session_id":"s-1","error":"billing_error"}`,
			"an account problem that needs the owner: the lines queued for it wait 30m, or less when its next turn ends or it is restarted"},
	} {
		t.Run(name, func(t *testing.T) {
			crewFile := testCrew(t, "")

			var out, errb bytes.Buffer

			root := newRoot(&app{out: &out, err: &errb})
			root.SetIn(strings.NewReader(tc.input))
			root.SetArgs([]string{"--crew", crewFile, "_stop-failure", "--role", "dev"})
			require.NoError(t, root.Execute())

			lines := eventLines(t, crewFile)
			require.Len(t, lines, 1)
			assert.Contains(t, lines[0], tc.want)
		})
	}
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

	// A model that is not there is an account problem: the owner mends it, and ls says so.
	write("idle", 1_000_000)
	assert.Contains(t, ls(), "dispatcher *  up idle (last turn ended in an API error, account problem, needs the owner: "+
		clip(failureText, 60)+")")
	assert.Contains(t, ls("--wide"), "idle (last turn ended in an API error, account problem, needs the owner: model_not_found: There's an issue")

	var views []shift.View

	require.NoError(t, json.Unmarshal([]byte(ls("--json")), &views))
	require.NotEmpty(t, views)
	assert.Equal(t, failureText, views[0].TurnError)
	assert.Equal(t, "account", views[0].TurnErrorKind)

	// An error that goes by itself says only that: as it always did, and with no kind in --json.
	record := func(input string) {
		_, err := shift.RecordFailure(crew.Layout{Dir: dir}, "dispatcher", []byte(input), time.UnixMilli(1_000_000))
		require.NoError(t, err)
	}

	record(`{"session_id":"s-1","error":"server_error","last_assistant_message":"API Error: 529 Overloaded"}`)
	assert.Contains(t, ls(), "dispatcher *  up idle (last turn ended in an API error: server_error: API Error: 529 Overloaded)")
	assert.NotContains(t, ls("--json"), "turn_error_kind")

	// A limit says so, and when it lifts when the message said (the zone is named in it: nothing here is the host's).
	record(`{"session_id":"s-1","error":"rate_limit","last_assistant_message":"You've hit your limit · resets 3pm (UTC)"}`)
	assert.Contains(t, ls(), "dispatcher *  up idle (last turn ended in an API error, usage limit, resets Thu 15:00 UTC: ")

	record(`{"session_id":"s-1","error":"rate_limit","last_assistant_message":"API Error: 429 Too Many Requests"}`)
	assert.Contains(t, ls(), "dispatcher *  up idle (last turn ended in an API error, usage limit: rate_limit: API Error: 429")

	record(stopFailure)

	// The next turn started.
	write("busy", 1_060_000)

	out := ls()
	assert.Contains(t, out, "dispatcher *  up busy")
	assert.NotContains(t, out, "API error")
	assert.NotContains(t, ls("--json"), "turn_error")
}

// courierHold is a role that is up with a line queued and a session idle since failedAt, found by its session name, and the
// clock a test moves: what the courier's look at a held queue needs.
func courierHold(t *testing.T) (s *shift.Shift, now *time.Time, layout crew.Layout, failedAt time.Time, session string) {
	t.Helper()

	crewFile := testCrew(t, "  dev:\n")
	dir := filepath.Dir(crewFile)
	ns := filepath.Join(dir, "ns")

	failedAt = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	s, err := a.newShift()
	require.NoError(t, err)

	s.Reg = &registry.Registry{Namespace: ns}
	s.State = shift.NewState()

	rs := s.State.Role("dev")
	rs.Up, rs.Inbox = true, []string{"[dispatcher] next"}

	// dev's session, alive (this process stands for it), idle since the failure, found by its session name.
	require.NoError(t, os.MkdirAll(filepath.Join(ns, "sessions"), 0o700))

	session = filepath.Join(ns, "sessions", strconv.Itoa(os.Getpid())+".json")
	idleSince := func(at time.Time) []byte {
		return []byte(`{"pid": ` + strconv.Itoa(os.Getpid()) + `, "sessionId": "s-1", "name": "` + s.Crew.Roles["dev"].Session +
			`", "status": "idle", "statusUpdatedAt": ` + strconv.FormatInt(at.UnixMilli(), 10) + `}`)
	}
	require.NoError(t, os.WriteFile(session, idleSince(failedAt), 0o600))

	clock := failedAt.Add(time.Minute)
	s.Now = func() time.Time { return clock }

	assert.Equal(t, []string{"dev"}, s.Waiting(), "no failure: the role is waiting")

	return s, &clock, crew.Layout{Dir: dir}, failedAt, session
}

// While a failed turn holds a role's queue, Waiting leaves the role out and the courier's look at the queue (queueGated) says
// it is gated, so the courier looks again every few seconds and finds the role when the hold ends: not spinning on it
// meanwhile, not forgetting it after.
func TestCourierLooksAgainWhenAFailedTurnsHoldEnds(t *testing.T) {
	s, now, layout, failedAt, _ := courierHold(t)

	f, err := shift.RecordFailure(layout, "dev", []byte(`{"session_id":"s-1","error":"server_error","last_assistant_message":"API Error: 529 Overloaded"}`), failedAt)
	require.NoError(t, err)
	require.NotNil(t, f)

	assert.Empty(t, s.Waiting(), "held: Waiting leaves the role out")
	assert.True(t, queueGated(s, s.Waiting()), "and the queue reads as gated, so the courier looks again")

	*now = failedAt.Add(shift.FailHold - time.Second)
	assert.True(t, queueGated(s, s.Waiting()), "still held a second before the end")

	*now = failedAt.Add(shift.FailHold + time.Second)
	assert.Equal(t, []string{"dev"}, s.Waiting(), "the hold is over: the role is waiting again")
	assert.False(t, queueGated(s, s.Waiting()))
}

// An account hold is a timed one like the rest: Waiting leaves the role out for AccountHold, the courier's look at the queue says
// it is gated all that time, so that it looks again every few seconds and finds the role when the probe is due, and the
// role's next turn frees it sooner. The courier's gate reads no record for a role with nothing queued.
func TestCourierLooksAgainWhenAnAccountHoldEnds(t *testing.T) {
	s, now, layout, failedAt, session := courierHold(t)

	_, err := shift.RecordFailure(layout, "dev", []byte(stopFailure), failedAt)
	require.NoError(t, err)

	for _, after := range []time.Duration{time.Minute, shift.FailHold + time.Second, shift.AccountHold - time.Second} {
		*now = failedAt.Add(after)

		assert.Empty(t, s.Waiting(), "%v on: held", after)
		assert.True(t, queueGated(s, s.Waiting()), "%v on: gated, so the courier looks again", after)
	}

	*now = failedAt.Add(shift.AccountHold + time.Second)
	assert.Equal(t, []string{"dev"}, s.Waiting(), "the probe: one line may go")
	assert.False(t, queueGated(s, s.Waiting()))

	// The role goes through a turn since, well before the hold is over: the record is not how it stands, and it is waiting.
	*now = failedAt.Add(10 * time.Minute)
	require.Empty(t, s.Waiting())
	require.NoError(t, os.WriteFile(session, []byte(`{"pid": `+strconv.Itoa(os.Getpid())+`, "sessionId": "s-1", "name": "`+
		s.Crew.Roles["dev"].Session+`", "status": "idle", "statusUpdatedAt": `+strconv.FormatInt(failedAt.Add(9*time.Minute).UnixMilli(), 10)+
		`}`), 0o600))

	assert.Equal(t, []string{"dev"}, s.Waiting())
}

// A role with nothing queued has no record read for the courier's gate, whatever the record says.
func TestCourierGateReadsNoRecordForAnEmptyQueue(t *testing.T) {
	s, now, layout, failedAt, _ := courierHold(t)

	_, err := shift.RecordFailure(layout, "dev", []byte(stopFailure), failedAt)
	require.NoError(t, err)

	s.State.Role("dev").Inbox = nil

	// The record is unreadable now: a gate that read it would be the only thing that could notice.
	require.NoError(t, os.WriteFile(layout.HookFailure("dev"), []byte(`{`), 0o600))

	assert.Empty(t, s.Waiting())
	assert.False(t, queueGated(s, s.Waiting()))

	// And a record past the longest hold is not sorted: stale, it holds nothing.
	*now = failedAt.Add(shift.LimitHoldMax + time.Minute)
	s.State.Role("dev").Inbox = []string{"[dispatcher] next"}
	_, err = shift.RecordFailure(layout, "dev", []byte(stopFailure), failedAt)
	require.NoError(t, err)

	assert.Equal(t, []string{"dev"}, s.Waiting())
}

// zmina doctor says which roles' queues wait for a usage limit or an account error, and for the dispatcher what that keeps from
// the owner: its queue, the crew's signals and the notes. It lists a hold only while the delivery would still hold: the same
// test zmina ls uses, a record of the role's live session that still stands. A hold the owner's message went through, a turn
// that ended since, a session that changed (a /clear, a restart) and a hold that ran out are not lines. A transient error is
// over in minutes and is no line. The times are in the crew's location.
func TestDoctorSaysWhichQueuesAreStillHeldForAFailedTurn(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n")
	raw, err := os.ReadFile(crewFile)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(crewFile, append([]byte("timezone: Asia/Kolkata\n"), raw...), 0o600))

	layout := crew.Layout{Dir: filepath.Dir(crewFile)}
	ns := filepath.Join(filepath.Dir(crewFile), "ns")
	failedAt := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	// The two roles' sessions are alive (this process and its parent stand for them), idle since the failures.
	require.NoError(t, os.MkdirAll(filepath.Join(ns, "sessions"), 0o700))

	session := func(role string, pid int, id, status string, since time.Time) {
		t.Helper()

		require.NoError(t, os.WriteFile(filepath.Join(ns, "sessions", strconv.Itoa(pid)+".json"), []byte(`{"pid": `+strconv.Itoa(pid)+
			`, "sessionId": "`+id+`", "name": "`+c.Roles[role].Session+`", "status": "`+status+`", "statusUpdatedAt": `+
			strconv.FormatInt(since.UnixMilli(), 10)+`}`), 0o600))
	}

	self, parent := os.Getpid(), os.Getppid()

	_, err = shift.RecordFailure(layout, "dispatcher", []byte(strings.Replace(stopFailure, "s-1", "s-d", 1)), failedAt)
	require.NoError(t, err)

	_, err = shift.RecordFailure(layout, "dev", []byte(`{"session_id":"s-2","error":"server_error","last_assistant_message":"API Error: 529 Overloaded"}`), failedAt)
	require.NoError(t, err)

	st := shift.NewState()
	st.Signals = []string{"one", "two"}
	st.Notes = []string{"a note"}
	st.Role("dispatcher").Inbox = []string{"[zmina] a line"}

	lines := func(now time.Time) []check {
		t.Helper()

		var got []check

		checkFailureHolds(c, st, failureHolds(c, st, now), func(name, level, format string, args ...any) {
			got = append(got, check{Name: name, Level: level, Detail: fmt.Sprintf(format, args...)})
		})

		return got
	}

	// In force: the dispatcher's account hold is a line, the transient one of dev is not, and the time is the crew's.
	session("dispatcher", self, "s-d", "idle", failedAt)
	session("dev", parent, "s-2", "idle", failedAt)

	got := lines(failedAt.Add(time.Minute))
	require.Len(t, got, 1, "the transient error is no line: %v", got)
	assert.Equal(t, "holds", got[0].Name)
	assert.Equal(t, "warn", got[0].Level)
	assert.Contains(t, got[0].Detail, "dispatcher: its last turn ended in an API error, account problem, needs the owner (model_not_found:")
	assert.Contains(t, got[0].Detail, "its queue, the crew's signals and the health notes wait (2 signals of at most 20, 1 notes, 1 lines of its own waiting; a new signal past the most is refused) until 18:00 IST,")
	assert.NotContains(t, got[0].Detail, "UTC")
	assert.Contains(t, got[0].Detail, "this is the owner's to look at: zmina peek dispatcher")

	// No longer in force: each of these makes the delivery let the queue go, so that doctor says nothing of it.
	for name, move := range map[string]func(){
		"the role went through a turn since (the owner's message went in, and it answered)": func() {
			session("dispatcher", self, "s-d", "idle", failedAt.Add(10*time.Minute))
		},
		"the role is busy again":                    func() { session("dispatcher", self, "s-d", "busy", failedAt.Add(time.Second)) },
		"the session changed (a /clear, a restart)": func() { session("dispatcher", self, "s-new", "idle", failedAt) },
		"the role is not up":                        func() { require.NoError(t, os.Remove(filepath.Join(ns, "sessions", strconv.Itoa(self)+".json"))) },
	} {
		t.Run(name, func(t *testing.T) {
			session("dispatcher", self, "s-d", "idle", failedAt)
			require.Len(t, lines(failedAt.Add(time.Minute)), 1, "in force before")

			move()

			assert.Empty(t, lines(failedAt.Add(time.Minute)))
		})
	}

	// The hold ran out.
	session("dispatcher", self, "s-d", "idle", failedAt)
	require.Len(t, lines(failedAt.Add(time.Minute)), 1)
	assert.Empty(t, lines(failedAt.Add(shift.AccountHold+time.Minute)))
}

// ls says what kind of error ended the turn, when it is not a transient one, and until when the queue waits for it.
func TestStatusWordsSayTheKindAndTheHold(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	for name, tc := range map[string]struct {
		view shift.View
		want string
	}{
		"transient": {shift.View{Status: "idle", TurnError: "server_error: Overloaded"},
			"idle (last turn ended in an API error: server_error: Overloaded)"},
		"account": {shift.View{Status: "idle", TurnError: "billing_error", TurnErrorKind: "account", TurnErrorHeldUntil: at.Add(shift.AccountHold)},
			"idle (last turn ended in an API error, account problem, needs the owner, queue held until 12:30 UTC: billing_error)"},
		"a limit": {shift.View{Status: "idle", TurnError: "rate_limit", TurnErrorKind: "limit", TurnErrorReset: at.Add(3 * time.Hour),
			TurnErrorHeldUntil: at.Add(3 * time.Hour)},
			"idle (last turn ended in an API error, usage limit, resets Sat 15:00 UTC, queue held until 15:00 UTC: rate_limit)"},
		"a limit, the hold over": {shift.View{Status: "idle", TurnError: "rate_limit", TurnErrorKind: "limit"},
			"idle (last turn ended in an API error, usage limit: rate_limit)"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, statusWords(tc.view))
		})
	}
}
