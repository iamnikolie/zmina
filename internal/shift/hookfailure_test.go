package shift

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/chat"
	"github.com/iamnikolie/zmina/internal/crew"
)

// failedInput is what Claude Code hands a StopFailure hook (a live payload): a short code in error, the words it showed
// in last_assistant_message, no other field of the error.
const failedInput = `{"session_id":"sid-designer","hook_event_name":"StopFailure","error":"model_not_found",` +
	`"last_assistant_message":"There's an issue with the selected model (…). It may not exist or you may not have access ` +
	`to it. Run /model to pick a different model."}`

// failedText is what the record says of it in a few words.
const failedText = "model_not_found: There's an issue with the selected model (…). It may not exist or you may not have " +
	"access to it. Run /model to pick a different model."

// transientInput is a StopFailure of an error that goes by itself (the API overloaded), and transientText what the record
// says of it: the queue behind it waits FailHold, and the dispatcher's note is the plain line.
const transientInput = `{"session_id":"sid-designer","hook_event_name":"StopFailure","error":"server_error",` +
	`"last_assistant_message":"API Error: 529 Overloaded"}`

const transientText = "server_error: API Error: 529 Overloaded"

// limitInput is a StopFailure of a usage limit that says when it lifts: the time is at, in UTC, to the minute, as Claude Code
// shows it.
func limitInput(reset time.Time) string {
	return `{"session_id":"sid-designer","hook_event_name":"StopFailure","error":"rate_limit",` +
		`"last_assistant_message":"You've hit your limit · resets ` + reset.UTC().Format("3:04pm") + ` (UTC)"}`
}

// limitNoTimeInput is one that says nothing of when.
const limitNoTimeInput = `{"session_id":"sid-designer","hook_event_name":"StopFailure","error":"rate_limit",` +
	`"last_assistant_message":"API Error: 429 Too Many Requests"}`

// live lets the role's session be found by its name in the session files, as Waiting finds it without tmux.
func (f fix) live(role string) {
	r, err := f.s.Role(role)
	if err != nil {
		panic(err)
	}

	f.rg.outside[r.Session] = f.rg.byPID[f.tm.up[role]]
}

// failed records that the role's turn ended on an API error at the given time, as its hook does.
func (f fix) failed(t *testing.T, role, input string, at time.Time) {
	t.Helper()

	got, err := RecordFailure(f.s.Crew.Layout(), role, []byte(input), at)
	require.NoError(t, err)
	require.NotNil(t, got)
}

func turnErrorOf(t *testing.T, f fix, role string) string {
	t.Helper()

	for _, v := range f.s.Views() {
		if v.Role == role {
			return v.TurnError
		}
	}

	t.Fatalf("no view of %s", role)

	return ""
}

// A StopFailure becomes the role's record: the session, the time, the error's code and the words Claude Code showed.
func TestRecordFailureWritesWhatTheHookSaid(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	got, err := RecordFailure(l, "designer", []byte(failedInput), at)
	require.NoError(t, err)
	require.NotNil(t, got)

	var read TurnFailure
	require.True(t, readRecord(l.HookFailure("designer"), &read))
	assert.Equal(t, *got, read)
	assert.Equal(t, "sid-designer", read.SessionID)
	assert.Equal(t, at, read.At)
	assert.Equal(t, "model_not_found", read.Error)
	assert.True(t, strings.HasPrefix(read.Details, "There's an issue with the selected model"), read.Details)
	assert.Equal(t, failedText, read.Text())

	info, err := os.Stat(l.HookFailure("designer"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// The words of the error are passed on as they are, never sorted: the code and the words shown, a code alone, an
// object, nothing at all.
func TestRecordFailureKeepsWhateverTheErrorCameAs(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	for name, tc := range map[string]struct {
		input string
		text  string
	}{
		"a code only":              {`{"session_id":"s","error":"server_error"}`, "server_error"},
		"the code and the words":   {`{"session_id":"s","error":"server_error","last_assistant_message":"API Error: 500 overloaded"}`, "server_error: API Error: 500 overloaded"},
		"words repeating the code": {`{"session_id":"s","error":"x","last_assistant_message":"x"}`, "x"},
		"words only":               {`{"session_id":"s","last_assistant_message":"API Error: gone"}`, "API Error: gone"},
		"an object":                {`{"session_id":"s","error":{"message":"connection lost"}}`, "connection lost"},
		"an object of no message":  {`{"session_id":"s","error":{"code":7}}`, `{"code":7}`},
		"multiline":                {`{"session_id":"s","error":"a\nb"}`, "a b"},
		"no words at all":          {`{"session_id":"s"}`, "no text given"},
	} {
		t.Run(name, func(t *testing.T) {
			l := crew.Layout{Dir: t.TempDir()}

			got, err := RecordFailure(l, "designer", []byte(tc.input), at)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.text, got.Text())
		})
	}

	t.Run("a long text is cut", func(t *testing.T) {
		l := crew.Layout{Dir: t.TempDir()}
		_, err := RecordFailure(l, "designer", []byte(`{"session_id":"s","error":"`+strings.Repeat("é", 2000)+`"}`), at)
		require.NoError(t, err)

		var got TurnFailure
		require.True(t, readRecord(l.HookFailure("designer"), &got))
		assert.LessOrEqual(t, len(got.Error), failureTextMax)
		assert.True(t, strings.HasPrefix(strings.Repeat("é", 2000), got.Error), "cut on a character")
	})
}

// Input that is not a StopFailure's, or a role that is no file name, records nothing and is no error; a crew directory
// that is not there is an error and none is made.
func TestRecordFailureLeavesBadInputAlone(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	for name, tc := range map[string]struct{ role, input string }{
		"not json":   {"designer", "not json"},
		"nothing":    {"designer", ""},
		"no session": {"designer", `{"error":"x"}`},
		"a path":     {"../designer", failedInput},
		"no role":    {"", failedInput},
	} {
		t.Run(name, func(t *testing.T) {
			l := crew.Layout{Dir: t.TempDir()}

			got, err := RecordFailure(l, tc.role, []byte(tc.input), at)
			require.NoError(t, err)
			assert.Nil(t, got)
			assert.NoDirExists(t, l.Hooks())
		})
	}

	gone := crew.Layout{Dir: t.TempDir() + "/gone"}
	got, err := RecordFailure(gone, "designer", []byte(failedInput), at)
	require.Error(t, err)
	assert.Nil(t, got)
	assert.NoDirExists(t, gone.Dir)
}

// The view says the last turn ended on an API error from the record, until the next turn starts. The record counts only
// as the moment of the session's last status change: within a second of it.
func TestViewSaysTheLastTurnEndedOnAnAPIError(t *testing.T) {
	for name, tc := range map[string]struct {
		set  func(f fix)
		want string
	}{
		"the turn just ended on it":           {func(f fix) { f.failed(t, "designer", failedInput, *f.now) }, failedText},
		"the hook a moment before the status": {func(f fix) { f.failed(t, "designer", failedInput, f.now.Add(-hookSkew/2)) }, failedText},
		"the hook a moment after the status":  {func(f fix) { f.failed(t, "designer", failedInput, f.now.Add(hookSkew/2)) }, failedText},
		"the hook long before it":             {func(f fix) { f.failed(t, "designer", failedInput, f.now.Add(-hookSkew-time.Second)) }, ""},
		"the hook long after it":              {func(f fix) { f.failed(t, "designer", failedInput, f.now.Add(hookSkew+time.Second)) }, ""},
		"the next turn started": {func(f fix) {
			f.failed(t, "designer", failedInput, *f.now)
			f.rg.byPID[f.tm.up["designer"]].Status = "busy"
		}, ""},
		"it waits on a question now": {func(f fix) {
			f.failed(t, "designer", failedInput, *f.now)
			f.rg.byPID[f.tm.up["designer"]].Status = "waiting"
		}, ""},
		"the turn after it ended well": {func(f fix) {
			f.failed(t, "designer", failedInput, *f.now)
			*f.now = f.now.Add(time.Minute)
			f.rg.byPID[f.tm.up["designer"]].StatusUpdatedAt = f.now.UnixMilli()
		}, ""},
		"another session": {func(f fix) {
			f.failed(t, "designer", strings.Replace(failedInput, "sid-designer", "sid-before-a-clear", 1), *f.now)
		}, ""},
		"no record": {func(f fix) {}, ""},
		"a record that does not read": {func(f fix) {
			require.NoError(t, os.MkdirAll(f.s.Crew.Layout().Hooks(), 0o700))
			require.NoError(t, os.WriteFile(f.s.Crew.Layout().HookFailure("designer"), []byte(`{"session_id":`), 0o600))
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("designer"))
			f.status("designer", "idle", *f.now)

			tc.set(f)
			assert.Equal(t, tc.want, turnErrorOf(t, f, "designer"))
		})
	}

	t.Run("json", func(t *testing.T) {
		f := fixtureWith(t, crewYAML)
		require.NoError(t, f.s.Up("designer"))
		f.status("designer", "idle", *f.now)
		f.failed(t, "designer", failedInput, *f.now)

		for _, v := range f.s.Views() {
			if v.Role == "designer" {
				raw, err := json.Marshal(v)
				require.NoError(t, err)
				assert.Contains(t, string(raw), `"turn_error":"model_not_found: `)
			}
		}
	})
}

// The dispatcher is told once, as soon as the hook says the turn ended on an error, whether or not the transcript shows
// it yet; and not again when it does. A role without the hook is told from the transcript as before.
func TestAPIErrorFromTheHookIsToldOnce(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	f.failed(t, "designer", transientInput, *f.now)
	*f.now = f.now.Add(3 * time.Minute)

	rs := f.s.State.Role("designer")

	assert.Equal(t, "designer: its last turn ended in an API error: "+transientText, f.s.apiErrorNote("designer", rs))
	assert.True(t, f.ev.has("designer", "api-error"))
	assert.Empty(t, f.s.apiErrorNote("designer", rs), "once")

	// The transcript shows the error afterwards: the same error, not told again.
	f.rg.errors["sid-designer"] = "API Error: Rate limit reached"
	assert.Empty(t, f.s.apiErrorNote("designer", rs))
	assert.Empty(t, f.s.apiErrorNote("designer", rs))

	f.rg.byPID[f.tm.up["designer"]].Status = "busy" // the next turn: the record is not current, the transcript line still is
	assert.Empty(t, f.s.apiErrorNote("designer", rs))

	// A new error is news.
	f.rg.errors["sid-designer"] = "API Error: Connection lost mid-response."
	f.rg.errorAt["sid-designer"] = f.now.Add(time.Hour)
	assert.Equal(t, "designer: its last turn ended in an API error: API Error: Connection lost mid-response.",
		f.s.apiErrorNote("designer", rs))
}

// A failed turn is told once even when the beat saw the record first and the role had left idle before it saw the
// transcript's error: the next turn started, the status is no longer the failure's, and the transcript's last error is still
// that turn's.
func TestAPIErrorToldFromTheHookIsNotToldAgainFromTheTranscript(t *testing.T) {
	failedAt := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	for name, tc := range map[string]struct {
		errorAt time.Time
		told    bool
	}{
		"the error line of that turn":         {failedAt.Add(-200 * time.Millisecond), false},
		"an error line with no time":          {time.Time{}, false},
		"an error far from the record's turn": {failedAt.Add(time.Hour), true},
	} {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("designer"))
			*f.now = failedAt
			f.status("designer", "idle", failedAt)
			f.failed(t, "designer", transientInput, failedAt)

			rs := f.s.State.Role("designer")

			require.Equal(t, "designer: its last turn ended in an API error: "+transientText, f.s.apiErrorNote("designer", rs),
				"the hook's record is told first: the transcript does not show the error yet")

			// The next turn started before the beat saw both: not idle since the failure, and the transcript's last error is
			// still the failed turn's.
			*f.now = failedAt.Add(time.Minute)
			f.status("designer", "busy", *f.now)
			f.rg.errors["sid-designer"] = "API Error: Connection lost mid-response."
			f.rg.errorAt["sid-designer"] = tc.errorAt

			got := f.s.apiErrorNote("designer", rs)
			if tc.told {
				assert.Contains(t, got, "its last turn ended in an API error: API Error: Connection lost")
			} else {
				assert.Empty(t, got, "the same failed turn")
			}

			assert.Empty(t, f.s.apiErrorNote("designer", rs), "and never twice")
		})
	}
}

// With the transcript's words at hand they are the ones told, as for a role without the hook; the record's are for
// when the transcript has none yet.
func TestAPIErrorWordsAreTheTranscriptsWhenItHasThem(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	f.failed(t, "designer", transientInput, *f.now)

	f.rg.errors["sid-designer"] = "API Error: Connection lost mid-response."
	assert.Equal(t, "designer: its last turn ended in an API error: API Error: Connection lost mid-response.",
		f.s.apiErrorNote("designer", f.s.State.Role("designer")))
}

// The failure a hook recorded holds the role's queue for FailHold: its lines stay queued, posted or typed by nobody — not
// the beat, not the delivery pass, not a turn's end — and Waiting does not name the role, so the courier does not ask
// it. After the hold one line goes; its turn failing too is a record of its own, and holds the role again.
func TestAFailedTurnHoldsTheQueueForAWhile(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	start := *f.now
	f.status("designer", "idle", start)
	f.rg.endedAt["sid-designer"] = start.Add(time.Second) // the boot turn is over
	f.s.State.Role("designer").Booted = start
	require.NoError(t, f.s.Queue("designer", "[dispatcher] first"))
	require.NoError(t, f.s.Queue("designer", "[dispatcher] second"))

	// A turn ended on an error a minute later.
	failedAt := start.Add(time.Minute)
	*f.now = failedAt
	f.status("designer", "idle", failedAt)
	f.live("designer")
	f.failed(t, "designer", transientInput, failedAt)

	queued := func() []string { return f.s.State.Role("designer").Inbox }
	waiting := func() bool { return slicesContain(f.s.Waiting(), "designer") }

	for _, after := range []time.Duration{time.Second, time.Minute, FailHold - time.Second} {
		*f.now = failedAt.Add(after)

		assert.False(t, waiting(), "%v in: Waiting leaves the role out, so the courier does not ask it", after)

		tried, err := f.s.DeliverRoles([]string{"designer"})
		require.NoError(t, err)
		assert.Empty(t, tried, "the delivery pass tries nothing")

		changed, err := f.s.DeliverPass()
		require.NoError(t, err)
		assert.False(t, changed)

		assert.Empty(t, f.s.DeliverTo("designer"), "the beat's delivery")
		assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"), "a turn's end")
		assert.Empty(t, f.s.FlushInbox("designer"))
		assert.Empty(t, p.posted["sid-designer"])
		assert.Empty(t, f.tm.typed["designer"])
		assert.Equal(t, []string{"[dispatcher] first", "[dispatcher] second"}, queued())
	}

	// The hold is over: what waits goes, by whichever path asks first (the two lines as one post, once).
	*f.now = failedAt.Add(FailHold + time.Second)
	assert.True(t, waiting())
	assert.Equal(t, []string{"posted to designer: 2 lines joined: 1. [dispatcher] first 2. [dispatcher] second"}, f.s.TurnEnd("designer", "sid-designer"))
	assert.Equal(t, []string{"1. [dispatcher] first\n2. [dispatcher] second"}, p.posted["sid-designer"])
	assert.Empty(t, queued())

	// Its turn fails too: the new record holds the role again, a line queued meanwhile stays.
	p.showAll()
	again := *f.now
	f.status("designer", "busy", again)
	f.live("designer")
	*f.now = again.Add(10 * time.Second)
	f.status("designer", "idle", *f.now)
	f.live("designer")
	f.failed(t, "designer", transientInput, *f.now)
	require.NoError(t, f.s.Queue("designer", "[dispatcher] third"))

	*f.now = f.now.Add(time.Minute)
	assert.False(t, waiting())
	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"))
	assert.Empty(t, f.s.DeliverTo("designer"))
	assert.Equal(t, []string{"1. [dispatcher] first\n2. [dispatcher] second"}, p.posted["sid-designer"])
	assert.Equal(t, []string{"[dispatcher] third"}, queued())

	*f.now = f.now.Add(FailHold)
	assert.True(t, waiting())
}

// The hold is of the failure of the role's current session and of how it stands now: a record of another session (a
// restart, a /clear), one the role has been through a turn since, or none holds nothing.
func TestARecordThatIsNotTheRolesFailureHoldsNothing(t *testing.T) {
	for name, set := range map[string]func(f fix, failedAt time.Time){
		"a record of another session": func(f fix, at time.Time) {
			f.failed(t, "designer", strings.Replace(failedInput, "sid-designer", "sid-before-a-clear", 1), at)
		},
		"the role went through a turn since": func(f fix, at time.Time) {
			f.failed(t, "designer", failedInput, at)
			f.status("designer", "idle", at.Add(30*time.Second))
			f.live("designer")
		},
		"the role is busy again": func(f fix, at time.Time) {
			f.failed(t, "designer", failedInput, at)
			f.status("designer", "busy", at.Add(30*time.Second))
			f.live("designer")
		},
		"a record that does not read": func(f fix, at time.Time) {
			require.NoError(t, os.MkdirAll(f.s.Crew.Layout().Hooks(), 0o700))
			require.NoError(t, os.WriteFile(f.s.Crew.Layout().HookFailure("designer"), []byte(`{"session_id":`), 0o600))
		},
		"no record": func(f fix, at time.Time) {},
	} {
		t.Run(name, func(t *testing.T) {
			f, p := nativeFixture(t, crewYAML)
			require.NoError(t, f.s.Up("designer"))
			start := *f.now
			f.status("designer", "idle", start)
			f.rg.endedAt["sid-designer"] = start.Add(time.Second)
			f.s.State.Role("designer").Booted = start
			require.NoError(t, f.s.Queue("designer", "[dispatcher] first"))

			failedAt := start.Add(time.Minute)
			*f.now = failedAt
			f.status("designer", "idle", failedAt)
			f.live("designer")
			set(f, failedAt)

			if name == "the role is busy again" {
				f.status("designer", "idle", failedAt.Add(time.Minute)) // so the line can go: the point is the hold
				f.live("designer")
			}

			*f.now = failedAt.Add(time.Minute)
			assert.True(t, slicesContain(f.s.Waiting(), "designer"))
			assert.Equal(t, []string{"posted to designer: [dispatcher] first"}, f.s.TurnEnd("designer", "sid-designer"))
			assert.Equal(t, []string{"[dispatcher] first"}, p.posted["sid-designer"])
		})
	}
}

// The dispatcher's signals and notes are held as a role's queue is: a dispatcher whose turns fail is not told a note a
// second.
func TestAFailedDispatcherTurnHoldsItsSignalsToo(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("dispatcher"))
	f.status("dispatcher", "idle", *f.now)
	f.live("dispatcher")
	f.rg.endedAt["sid-dispatcher"] = f.now.Add(time.Second)
	f.s.State.Role("dispatcher").Booted = *f.now

	_, err := f.s.Note("mrs", "acme/site!12 approved", false)
	require.NoError(t, err)

	failedAt := f.now.Add(time.Minute)
	*f.now = failedAt
	f.status("dispatcher", "idle", failedAt)
	f.live("dispatcher")
	f.failed(t, "dispatcher", strings.Replace(transientInput, "sid-designer", "sid-dispatcher", 1), failedAt)

	*f.now = failedAt.Add(time.Minute)
	assert.NotContains(t, f.s.Waiting(), "dispatcher")
	assert.Empty(t, f.s.TurnEnd("dispatcher", "sid-dispatcher"))
	assert.Empty(t, f.s.Deliver())
	assert.Empty(t, p.posted["sid-dispatcher"])
	assert.Len(t, f.s.State.Signals, 1, "still queued")

	*f.now = failedAt.Add(FailHold + time.Second)
	assert.Contains(t, f.s.Waiting(), "dispatcher")
	assert.Len(t, f.s.TurnEnd("dispatcher", "sid-dispatcher"), 1)
	assert.Len(t, p.posted["sid-dispatcher"], 1)
}

func slicesContain(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}

	return false
}

// The StopFailure hook is in the settings of a Claude role on its own channel: silent, exit 0 always, a short timeout.
func TestStopFailureHookInSettings(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)

	type settings struct {
		Hooks struct {
			StopFailure []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
					Timeout int    `json:"timeout"`
				} `json:"hooks"`
			}
		} `json:"hooks"`
	}

	var got settings
	require.NoError(t, json.Unmarshal([]byte(f.s.roleSettings("designer")), &got))
	assert.Empty(t, got.Hooks.StopFailure, "no executable known: no hook")

	f.s.Self = "/opt/zmina"

	require.NoError(t, json.Unmarshal([]byte(f.s.roleSettings("designer")), &got))
	require.Len(t, got.Hooks.StopFailure, 1)
	require.Len(t, got.Hooks.StopFailure[0].Hooks, 1)
	h := got.Hooks.StopFailure[0].Hooks[0]
	assert.Empty(t, got.Hooks.StopFailure[0].Matcher, "every error kind")
	assert.Equal(t, "command", h.Type)
	assert.Equal(t, "'/opt/zmina' --crew '"+f.s.Crew.Path+"' _stop-failure --role 'designer' >/dev/null 2>&1 || true", h.Command)
	assert.Equal(t, recordTimeout, h.Timeout)
}

// A role that starts a new session has none of the last one's records: its question and its failure go, and nothing
// else in the hooks directory, nor another role's.
func TestAStartDropsTheRecordsOfTheSessionBefore(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	l := f.s.Crew.Layout()
	at := *f.now

	require.NoError(t, RecordQuestion(l, "designer", []byte(askedInput), at))
	f.failed(t, "designer", failedInput, at)
	require.NoError(t, RecordQuestion(l, "harvester", []byte(strings.Replace(askedInput, "sid-designer", "sid-harvester", 1)), at))
	require.NoError(t, os.WriteFile(l.Hooks()+"/notes.txt", []byte("not a record"), 0o600))

	require.NoError(t, f.s.Up("designer"))

	assert.NoFileExists(t, l.HookQuestion("designer"))
	assert.NoFileExists(t, l.HookFailure("designer"))
	assert.FileExists(t, l.HookQuestion("harvester"), "another role's")
	assert.FileExists(t, l.Hooks()+"/notes.txt", "nothing else")
}

// The hold is for what zmina would feed the role by itself: the owner's own message, next in the queue, goes during it,
// as their typing into the pane would.
func TestTheOwnersOwnMessageIsNotHeldAfterAFailedTurn(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	start := *f.now
	f.status("designer", "idle", start)
	f.rg.endedAt["sid-designer"] = start.Add(time.Second)
	f.s.State.Role("designer").Booted = start

	const owner = "try that again, please"
	require.NoError(t, f.s.Queue("designer", owner))
	require.NoError(t, f.s.Queue("designer", "[dispatcher] second"))

	failedAt := start.Add(time.Minute)
	*f.now = failedAt
	f.status("designer", "idle", failedAt)
	f.live("designer")
	f.failed(t, "designer", failedInput, failedAt)

	*f.now = failedAt.Add(time.Second)
	require.False(t, slicesContain(f.s.Waiting(), "designer"), "held while the next line is not the owner's")

	rs := f.s.State.Role("designer")
	rs.ChatReceipts = append(rs.ChatReceipts, chat.Receipt{ID: "c1", Line: owner, At: *f.now})

	assert.True(t, slicesContain(f.s.Waiting(), "designer"), "the owner's message is next: the role is asked")
	assert.True(t, f.s.ownerNext("designer"))
	assert.NotEmpty(t, f.s.DeliverTo("designer"), "and it goes")
	assert.Equal(t, []string{"[dispatcher] second"}, rs.Inbox, "what zmina queued behind it waits for that turn")
}
