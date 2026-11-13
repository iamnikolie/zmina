package shift

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

// heldFixture is a role with two lines queued, whose turn ended on the error the input says a minute after its boot turn,
// with the session idle since. It returns the moment the turn failed.
func heldFixture(t *testing.T, input string) (fix, *fakePeer, time.Time) {
	t.Helper()

	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	start := *f.now
	f.status("designer", "idle", start)
	f.rg.endedAt["sid-designer"] = start.Add(time.Second) // the boot turn is over
	f.s.State.Role("designer").Booted = start
	require.NoError(t, f.s.Queue("designer", "[dispatcher] first"))
	require.NoError(t, f.s.Queue("designer", "[dispatcher] second"))

	failedAt := start.Add(time.Minute)
	*f.now = failedAt
	f.status("designer", "idle", failedAt)
	f.live("designer")
	f.failed(t, "designer", input, failedAt)

	return f, p, failedAt
}

// heldAt says whether the role's queue is held at the moment: Waiting leaves it out, and no path delivers to it.
func heldAt(t *testing.T, f fix, p *fakePeer, at time.Time) bool {
	t.Helper()

	*f.now = at

	held := !slicesContain(f.s.Waiting(), "designer")

	if held {
		assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"), "%v in: a turn's end", at)
		assert.Empty(t, f.s.DeliverTo("designer"), "%v in: the beat's delivery", at)
		assert.Empty(t, p.posted["sid-designer"])
		assert.Equal(t, []string{"[dispatcher] first", "[dispatcher] second"}, f.s.State.Role("designer").Inbox)
	}

	return held
}

// How long the queue waits is the kind of the error. A transient one waits FailHold, as ever; a limit until its reset time,
// at most LimitHoldMax, and FailHold when it gave none; an account problem for as long as the record is how the session
// stands, which a turn the role goes through since is not.
func TestHowLongAFailedTurnHoldsTheQueueIsItsKind(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 1, 0, 0, time.UTC) // the moment the turn fails in heldFixture

	for _, tc := range []struct {
		name  string
		input string
		// held are offsets from the failure at which the queue is held, free those at which it goes.
		held, free []time.Duration
	}{
		{name: "transient", input: transientInput,
			held: []time.Duration{time.Second, FailHold - time.Second},
			free: []time.Duration{FailHold + time.Second, time.Hour}},
		{name: "unknown words are transient", input: `{"session_id":"sid-designer","error":"unknown"}`,
			held: []time.Duration{time.Second, FailHold - time.Second},
			free: []time.Duration{FailHold + time.Second}},
		{name: "account, by code and words", input: failedInput,
			held: []time.Duration{time.Second, FailHold + time.Second, time.Hour, 6*time.Hour + time.Minute, 72 * time.Hour}},
		{name: "account, by words alone", input: `{"session_id":"sid-designer","error":"unknown","last_assistant_message":"Credit balance is too low"}`,
			held: []time.Duration{FailHold + time.Second, 24 * time.Hour}},
		{name: "account, by the hook's error_details", input: `{"session_id":"sid-designer","error":"unknown","error_details":"invalid x-api-key"}`,
			held: []time.Duration{FailHold + time.Second, 24 * time.Hour}},
		{name: "a limit until it lifts", input: limitInput(at.Add(2 * time.Hour)),
			held: []time.Duration{time.Second, FailHold + time.Second, time.Hour, 2*time.Hour - time.Second},
			free: []time.Duration{2*time.Hour + time.Second, 3 * time.Hour}},
		{name: "a limit that lifts in a minute", input: limitInput(at.Add(time.Minute)),
			held: []time.Duration{time.Second, time.Minute - time.Second},
			free: []time.Duration{time.Minute + time.Second}},
		{name: "a limit that lifts far off waits at most LimitHoldMax", input: limitInput(at.Add(10 * time.Hour)),
			held: []time.Duration{FailHold + time.Second, LimitHoldMax - time.Second},
			free: []time.Duration{LimitHoldMax + time.Second, 9 * time.Hour}},
		{name: "a limit with no time waits FailHold", input: limitNoTimeInput,
			held: []time.Duration{time.Second, FailHold - time.Second},
			free: []time.Duration{FailHold + time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p, failedAt := heldFixture(t, tc.input)
			require.Equal(t, at, failedAt)

			for _, d := range tc.held {
				assert.True(t, heldAt(t, f, p, failedAt.Add(d)), "%v after the failure: held", d)
			}

			for _, d := range tc.free {
				assert.False(t, heldAt(t, f, p, failedAt.Add(d)), "%v after the failure: free", d)
			}
		})
	}
}

// An account problem is held until the role's next turn ends or it is restarted, not retried every few minutes: a turn
// the role goes through (the owner types to it; the next record is its own) frees the queue, so does a new session, and the
// owner's own message goes during it.
func TestAnAccountFailureHoldIsLiftedByTheNextTurnNotByTime(t *testing.T) {
	f, p, failedAt := heldFixture(t, failedInput)

	require.True(t, heldAt(t, f, p, failedAt.Add(48*time.Hour)))

	// The role goes through a turn since, say the owner fixed the login and typed to it: the record is no longer how the
	// session stands.
	f.status("designer", "idle", failedAt.Add(49*time.Hour))
	f.live("designer")
	assert.False(t, heldAt(t, f, p, failedAt.Add(49*time.Hour+time.Second)))

	// A start takes the record away.
	g, q, failedAt := heldFixture(t, failedInput)
	require.True(t, heldAt(t, g, q, failedAt.Add(time.Hour)))
	g.s.dropHookRecords("designer")
	assert.False(t, heldAt(t, g, q, failedAt.Add(time.Hour+time.Second)))
}

// The kind is in the role's view only when it is not transient, so that a transient error is what it was in --json and in
// zmina ls; a limit carries the time it lifts.
func TestViewSaysTheKindOfAFailedTurn(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name, input, kind string
		reset             time.Time
	}{
		{name: "transient", input: transientInput},
		{name: "account", input: failedInput, kind: "account"},
		{name: "a limit with its time", input: limitInput(at.Add(3 * time.Hour)), kind: "limit", reset: at.Add(3 * time.Hour)},
		{name: "a limit with none", input: limitNoTimeInput, kind: "limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("designer"))
			f.status("designer", "idle", *f.now)
			f.failed(t, "designer", tc.input, *f.now)

			for _, v := range f.s.Views() {
				if v.Role != "designer" {
					continue
				}

				assert.NotEmpty(t, v.TurnError)
				assert.Equal(t, tc.kind, v.TurnErrorKind)
				assert.True(t, tc.reset.Equal(v.TurnErrorReset), "%v", v.TurnErrorReset)

				raw, err := json.Marshal(v)
				require.NoError(t, err)

				if tc.kind == "" {
					assert.NotContains(t, string(raw), "turn_error_kind")
					assert.NotContains(t, string(raw), "turn_error_reset")
				} else {
					assert.Contains(t, string(raw), `"turn_error_kind":"`+tc.kind+`"`)
				}
			}
		})
	}
}

// The dispatcher's line is the plain one for a transient error; a limit says so, with when it lifts; an account problem
// says the owner has to mend it and how long the role waits. Told once, from the hook or the transcript.
func TestAPIErrorNoteSaysTheKind(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name, input, transcript string
		want                    string
	}{
		{name: "transient", input: transientInput, want: "designer: its last turn ended in an API error: " + transientText},
		{name: "account", input: failedInput, want: "designer: its last turn ended in an API error that needs the owner (an " +
			"account, billing, login or permission problem, which retrying does not mend): " + failedText +
			"; its queued lines wait until its next turn ends or it is restarted"},
		{name: "a limit with its time", input: limitInput(at.Add(3 * time.Hour)),
			want: "designer: its last turn ended in an API error (usage limit, resets Sat 15:00 UTC): rate_limit: You've hit your limit · resets 3:00pm (UTC)"},
		{name: "a limit with none", input: limitNoTimeInput,
			want: "designer: its last turn ended in an API error (usage limit): rate_limit: API Error: 429 Too Many Requests"},
		{name: "the transcript's words, no hook", transcript: "You've hit your limit · resets 9pm",
			want: "designer: its last turn ended in an API error (usage limit, resets Sat 21:00 UTC): You've hit your limit · resets 9pm"},
		{name: "the transcript's words of an account problem, no hook", transcript: "Invalid API key · Please run /login",
			want: "designer: its last turn ended in an API error that needs the owner (an account, billing, login or permission " +
				"problem, which retrying does not mend): Invalid API key · Please run /login; its queued lines wait until its " +
				"next turn ends or it is restarted"},
		{name: "the transcript's words of a transient one, no hook", transcript: "API Error: Connection lost mid-response.",
			want: "designer: its last turn ended in an API error: API Error: Connection lost mid-response."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("designer"))
			f.status("designer", "idle", *f.now)

			if tc.input != "" {
				f.failed(t, "designer", tc.input, *f.now)
			}

			if tc.transcript != "" {
				f.rg.errors["sid-designer"] = tc.transcript
			}

			*f.now = f.now.Add(time.Minute)
			rs := f.s.State.Role("designer")

			assert.Equal(t, tc.want, f.s.apiErrorNote("designer", rs))
			assert.Empty(t, f.s.apiErrorNote("designer", rs), "once")
		})
	}
}

// The words of the failure-hold event: what it always said for a transient error, and for the others how long.
func TestHoldWords(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		f    TurnFailure
		want string
	}{
		{name: "transient", f: TurnFailure{At: at, Error: "server_error", Details: "API Error: 529 Overloaded"},
			want: "the lines queued for it wait 5m0s"},
		{name: "account", f: TurnFailure{At: at, Error: "billing_error"},
			want: "an account problem that needs the owner: the lines queued for it wait until its next turn ends or it is restarted"},
		{name: "a limit with its time", f: TurnFailure{At: at, Error: "rate_limit", Details: "resets 3pm (UTC)"},
			want: "a usage limit: the lines queued for it wait until Sat 15:00 UTC, at most 6h"},
		{name: "a limit with none", f: TurnFailure{At: at, Error: "rate_limit"},
			want: "a usage limit with no reset time given: the lines queued for it wait 5m0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, HoldWords(&tc.f))
		})
	}
}

// What the hook says is kept as it said it; error_details is kept apart and only the sorting reads it, so the words a view
// and a note show are what they were.
func TestRecordFailureKeepsErrorDetailsApart(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	got, err := RecordFailure(l, "designer", []byte(`{"session_id":"s","error":"unknown","last_assistant_message":"API Error",`+
		`"error_details":"invalid x-api-key"}`), at)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, "unknown: API Error", got.Text())
	assert.Equal(t, "invalid x-api-key", got.ErrorDetails)
	assert.Equal(t, KindAccount, got.Class().Kind)

	var read TurnFailure
	require.True(t, readRecord(l.HookFailure("designer"), &read))
	assert.Equal(t, *got, read)

	// A record written before the field existed reads, and is sorted by the words it has.
	old := TurnFailure{SessionID: "s", At: at, Error: "model_not_found"}
	assert.Equal(t, KindAccount, old.Class().Kind)
}
