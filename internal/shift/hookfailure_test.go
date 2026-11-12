package shift

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

// failedInput is what Claude Code hands a StopFailure hook.
const failedInput = `{"session_id":"sid-designer","hook_event_name":"StopFailure","error":"rate_limit",` +
	`"error_details":"429 too many requests","last_assistant_message":"API Error: Rate limit reached"}`

// failedTurn puts a role's session idle since the fixture's now, as it is when a turn has ended.
func (f fix) failedTurn(role string) { f.status(role, "idle", *f.now) }

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

// A StopFailure becomes the role's record: the session, the time, and Claude Code's words for the error, as they came.
func TestRecordFailureWritesWhatTheHookSaid(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	sid, err := RecordFailure(l, "designer", []byte(failedInput), at)
	require.NoError(t, err)
	assert.Equal(t, "sid-designer", sid)

	var got TurnFailure
	require.True(t, readRecord(l.HookFailure("designer"), &got))
	assert.Equal(t, TurnFailure{SessionID: "sid-designer", At: at, Error: "rate_limit", Details: "429 too many requests"}, got)
	assert.Equal(t, "rate_limit: 429 too many requests", got.Text())

	info, err := os.Stat(l.HookFailure("designer"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// The words of the error are passed on as they are, never sorted: a string, an object with a message, the last reply when
// there are no details, nothing at all.
func TestRecordFailureKeepsWhateverTheErrorCameAs(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	for name, tc := range map[string]struct {
		input string
		text  string
	}{
		"a kind only":             {`{"session_id":"s","error":"server_error"}`, "server_error"},
		"a kind and details":      {`{"session_id":"s","error":"server_error","error_details":"500 overloaded"}`, "server_error: 500 overloaded"},
		"the reply as details":    {`{"session_id":"s","error":"unknown","last_assistant_message":"API Error: gone"}`, "unknown: API Error: gone"},
		"an object":               {`{"session_id":"s","error":{"message":"connection lost"}}`, "connection lost"},
		"an object of no message": {`{"session_id":"s","error":{"code":7}}`, `{"code":7}`},
		"details that repeat":     {`{"session_id":"s","error":"x","error_details":"x"}`, "x"},
		"multiline":               {`{"session_id":"s","error":"a\nb"}`, "a b"},
		"no words at all":         {`{"session_id":"s"}`, "no text given"},
	} {
		t.Run(name, func(t *testing.T) {
			l := crew.Layout{Dir: t.TempDir()}

			sid, err := RecordFailure(l, "designer", []byte(tc.input), at)
			require.NoError(t, err)
			assert.Equal(t, "s", sid)

			var got TurnFailure
			require.True(t, readRecord(l.HookFailure("designer"), &got))
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

// Input that is not a StopFailure's records nothing and names no session; a role that is no file name records nothing
// but its session is still named, for the turn is over.
func TestRecordFailureLeavesBadInputAlone(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	for name, input := range map[string]string{"not json": "not json", "nothing": "", "no session": `{"error":"x"}`} {
		t.Run(name, func(t *testing.T) {
			l := crew.Layout{Dir: t.TempDir()}

			sid, err := RecordFailure(l, "designer", []byte(input), at)
			require.NoError(t, err)
			assert.Empty(t, sid)
			assert.NoDirExists(t, l.Hooks())
		})
	}

	l := crew.Layout{Dir: t.TempDir()}
	sid, err := RecordFailure(l, "../designer", []byte(failedInput), at)
	require.NoError(t, err)
	assert.Equal(t, "sid-designer", sid)
	assert.NoDirExists(t, l.Hooks())

	gone := crew.Layout{Dir: t.TempDir() + "/gone"}
	sid, err = RecordFailure(gone, "designer", []byte(failedInput), at)
	require.Error(t, err)
	assert.Equal(t, "sid-designer", sid, "named though it could not be written")
	assert.NoDirExists(t, gone.Dir)
}

// The view says the last turn ended on an API error from the record, until the next turn starts; a record that is not
// of this turn, or a role without the hook, shows nothing.
func TestViewSaysTheLastTurnEndedOnAnAPIError(t *testing.T) {
	record := func(f fix, input string, at time.Time) {
		_, err := RecordFailure(f.s.Crew.Layout(), "designer", []byte(input), at)
		require.NoError(t, err)
	}

	for name, tc := range map[string]struct {
		set  func(f fix)
		want string
	}{
		"the turn just ended on it": {func(f fix) { record(f, failedInput, *f.now) }, "rate_limit: 429 too many requests"},
		"the hook a moment before the status": {func(f fix) { record(f, failedInput, f.now.Add(-time.Second)) },
			"rate_limit: 429 too many requests"},
		"the next turn started": {func(f fix) {
			record(f, failedInput, *f.now)
			f.rg.byPID[f.tm.up["designer"]].Status = "busy"
		}, ""},
		"it waits on a question now": {func(f fix) {
			record(f, failedInput, *f.now)
			f.rg.byPID[f.tm.up["designer"]].Status = "waiting"
		}, ""},
		"the turn after it ended well": {func(f fix) {
			record(f, failedInput, *f.now)
			*f.now = f.now.Add(time.Minute)
			f.rg.byPID[f.tm.up["designer"]].StatusUpdatedAt = f.now.UnixMilli()
		}, ""},
		"another session": {func(f fix) { record(f, strings.Replace(failedInput, "sid-designer", "sid-before-a-clear", 1), *f.now) }, ""},
		"no record":       {func(f fix) {}, ""},
		"a record that does not read": {func(f fix) {
			require.NoError(t, os.MkdirAll(f.s.Crew.Layout().Hooks(), 0o700))
			require.NoError(t, os.WriteFile(f.s.Crew.Layout().HookFailure("designer"), []byte(`{"session_id":`), 0o600))
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("designer"))
			f.failedTurn("designer")

			tc.set(f)
			assert.Equal(t, tc.want, turnErrorOf(t, f, "designer"))
		})
	}

	t.Run("json", func(t *testing.T) {
		f := fixtureWith(t, crewYAML)
		require.NoError(t, f.s.Up("designer"))
		f.failedTurn("designer")
		record(f, failedInput, *f.now)

		for _, v := range f.s.Views() {
			if v.Role == "designer" {
				raw, err := json.Marshal(v)
				require.NoError(t, err)
				assert.Contains(t, string(raw), `"turn_error":"rate_limit: 429 too many requests"`)
			}
		}
	})
}

// The dispatcher is told once, as soon as the hook says the turn ended on an error, whether or not the transcript shows
// it yet; and not again when it does. A role without the hook is told from the transcript as before.
func TestAPIErrorFromTheHookIsToldOnce(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.failedTurn("designer")
	*f.now = f.now.Add(3 * time.Minute)

	rs := f.s.State.Role("designer")

	_, err := RecordFailure(f.s.Crew.Layout(), "designer", []byte(failedInput), f.now.Add(-3*time.Minute))
	require.NoError(t, err)

	want := "designer: its last turn ended in an API error: rate_limit: 429 too many requests"
	assert.Equal(t, want, f.s.apiErrorNote("designer", rs))
	assert.True(t, f.ev.has("designer", "api-error"))
	assert.Empty(t, f.s.apiErrorNote("designer", rs), "once")

	// The transcript shows the error afterwards: the same error, not told again, and not when the record goes stale.
	f.rg.errors["sid-designer"] = "API Error: Rate limit reached"
	assert.Empty(t, f.s.apiErrorNote("designer", rs))
	assert.Empty(t, f.s.apiErrorNote("designer", rs))

	f.rg.byPID[f.tm.up["designer"]].Status = "busy" // the next turn: the record is not current, the transcript line still is
	assert.Empty(t, f.s.apiErrorNote("designer", rs))

	// A new error is news.
	f.rg.errors["sid-designer"] = "API Error: Connection lost mid-response."
	assert.Equal(t, "designer: its last turn ended in an API error: API Error: Connection lost mid-response.",
		f.s.apiErrorNote("designer", rs))
}

// With the transcript's words at hand they are the ones told, as for a role without the hook; the record's are for
// when the transcript has none yet.
func TestAPIErrorWordsAreTheTranscriptsWhenItHasThem(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.failedTurn("designer")

	_, err := RecordFailure(f.s.Crew.Layout(), "designer", []byte(failedInput), *f.now)
	require.NoError(t, err)

	f.rg.errors["sid-designer"] = "API Error: Rate limit reached"
	assert.Equal(t, "designer: its last turn ended in an API error: API Error: Rate limit reached",
		f.s.apiErrorNote("designer", f.s.State.Role("designer")))
}

// A failed turn is over once the session is idle, though the transcript shows no turn closed: the hook says so. Its end
// posts what waits, as any turn's end does, and nothing is typed or retried.
func TestAFailedTurnsEndPostsWhatWaits(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	start := *f.now
	f.status("designer", "idle", start)
	f.rg.endedAt["sid-designer"] = start.Add(time.Second) // the boot turn is over
	require.NoError(t, f.s.Queue("designer", "[dispatcher] next"))

	*f.now = start.Add(time.Minute)

	// The turn after the boot ended on an error: idle again, and no line of the transcript closes it.
	f.status("designer", "idle", *f.now)
	delete(f.rg.endedAt, "sid-designer")
	f.rg.errors["sid-designer"] = "API Error: Rate limit reached"

	assert.False(t, f.s.TurnOver("designer", "sid-designer"), "a Stop hook would wait for this in vain")
	assert.True(t, f.s.FailedTurnOver("designer", "sid-designer"))
	assert.False(t, f.s.FailedTurnOver("designer", "sid-other"), "another session")

	f.status("designer", "busy", *f.now)
	assert.False(t, f.s.FailedTurnOver("designer", "sid-designer"), "a turn that is running")
	f.status("designer", "idle", *f.now)

	// The boot was over before: the line waiting goes now.
	f.s.State.Role("designer").Booted = start
	assert.Equal(t, []string{"posted to designer: [dispatcher] next"}, f.s.TurnEnd("designer", "sid-designer"))
	assert.Equal(t, []string{"[dispatcher] next"}, p.posted["sid-designer"])
	assert.Empty(t, f.tm.typed["designer"], "nothing typed")
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
