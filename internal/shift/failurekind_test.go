package shift

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// failureNow is the clock the classification tests read reset times against: a fixed moment in UTC, never the host's zone.
var failureNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// Errors as Claude Code and the API word them, by the kind each asks for. The words are the shapes seen in a role's pane and
// transcript; the codes are the ones the StopFailure hook gives. Anything not recognised is transient.
func TestClassifyFailure(t *testing.T) {
	soon := failureNow.Add(2 * time.Hour)

	for _, tc := range []struct {
		name, code, text string
		kind             FailureKind
		reset            time.Time
	}{
		// Transient: it goes by itself, or nothing is known of it.
		{name: "server error", code: "server_error", kind: KindTransient,
			text: `API Error: 500 {"type":"error","error":{"type":"api_error","message":"Internal server error"}}`},
		{name: "overloaded", code: "server_error", kind: KindTransient,
			text: `API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
		{name: "bad gateway", text: "API Error: 502 Bad Gateway", kind: KindTransient},
		{name: "unavailable", text: "API Error: 503 Service Unavailable", kind: KindTransient},
		{name: "connection lost", text: "API Error: Connection lost mid-response.", kind: KindTransient},
		{name: "timeout", text: "API Error: Request timed out.", kind: KindTransient},
		{name: "network", text: "API Error: Unable to connect to API (ECONNRESET)", kind: KindTransient},
		{name: "output tokens", code: "max_output_tokens", kind: KindTransient,
			text: "Claude's response exceeded the 32000 output token maximum. To configure this behavior, set the " +
				"CLAUDE_CODE_MAX_OUTPUT_TOKENS environment variable."},
		{name: "prompt too long", code: "invalid_request", text: "API Error: 400 prompt is too long: 213000 tokens > 200000 maximum", kind: KindTransient},
		{name: "context limit is no usage limit", text: "Context limit reached · /compact or /clear to continue", kind: KindTransient},
		{name: "unknown code, no words", code: "unknown", kind: KindTransient},
		{name: "nothing", kind: KindTransient},
		{name: "words that merely mention authentication", text: "API Error: 500 the authentication service is restarting", kind: KindTransient},
		{name: "a name that is no code", code: "Something Else", text: "oops", kind: KindTransient},

		// A limit, with the time it lifts when the message says it.
		{name: "rate limit by code", code: "rate_limit", kind: KindLimit,
			text: `API Error: 429 {"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your ` +
				`organization's rate limit of 30,000 input tokens per minute"}}`},
		{name: "rate limit by words", text: "API Error: 429 Too Many Requests", kind: KindLimit},
		{name: "hit your limit", text: "You've hit your limit · resets 3pm (America/New_York)", kind: KindLimit,
			reset: time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)},
		{name: "session limit", text: "You've hit your session limit · resets 11:30pm (UTC)", kind: KindLimit,
			reset: time.Date(2026, 11, 13, 23, 30, 0, 0, time.UTC)},
		{name: "five hour limit, next day", text: "5-hour limit reached ∙ resets 2am", kind: KindLimit,
			reset: time.Date(2026, 11, 14, 2, 0, 0, 0, time.UTC)},
		{name: "weekly limit with a date", text: "Weekly limit reached ∙ resets Nov 16, 10am", kind: KindLimit,
			reset: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)},
		{name: "limit will reset at", text: "Claude usage limit reached. Your limit will reset at 5pm (America/Los_Angeles).", kind: KindLimit,
			reset: time.Date(2026, 11, 14, 1, 0, 0, 0, time.UTC)},
		{name: "epoch after a bar", text: fmt.Sprintf("Claude AI usage limit reached|%d", soon.Unix()), kind: KindLimit, reset: soon},
		{name: "relative", text: "Usage limit reached, try again in 2 hours 30 minutes", kind: KindLimit,
			reset: failureNow.Add(150 * time.Minute)},
		{name: "relative, short", text: "rate limit exceeded; retry in 45s", kind: KindLimit, reset: failureNow.Add(45 * time.Second)},
		{name: "out of extra usage", text: "You're out of extra usage · resets 9pm", kind: KindLimit,
			reset: time.Date(2026, 11, 13, 21, 0, 0, 0, time.UTC)},
		{name: "a limit with no time", text: "You've hit your limit", kind: KindLimit},
		{name: "a time already past", text: "Claude AI usage limit reached|1700000000", kind: KindLimit},
		{name: "a time too far off", text: "Weekly limit reached ∙ resets Dec 30, 10am", kind: KindLimit},
		{name: "a zone that does not exist", text: "usage limit reached · resets 3pm (Mars/Olympus)", kind: KindLimit},
		{name: "no time of day", text: "usage limit reached · resets 3", kind: KindLimit},

		// An account problem: billing, login, permission, a model that is not there.
		{name: "credit balance", code: "billing_error", kind: KindAccount,
			text: "Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."},
		{name: "credit balance, no code", text: "Credit balance is too low", kind: KindAccount},
		{name: "login", code: "authentication_failed", text: "Invalid API key · Please run /login", kind: KindAccount},
		{name: "token expired", text: `API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired. ` +
			`Please obtain a new token or refresh your existing token."}}`, kind: KindAccount},
		{name: "invalid key", text: `API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			kind: KindAccount},
		{name: "not logged in", text: "Not logged in · Please run /login", kind: KindAccount},
		{name: "permission", text: `API Error: 403 {"type":"error","error":{"type":"permission_error","message":"Your API key does not have ` +
			`permission to use the specified resource."}}`, kind: KindAccount},
		{name: "organization disabled", text: "This organization has been disabled.", kind: KindAccount},
		{name: "model not found by code", code: "model_not_found", kind: KindAccount,
			text: "There's an issue with the selected model (claude-x). It may not exist or you may not have access to it. " +
				"Run /model to pick a different model."},
		{name: "model not found by words", text: "There's an issue with the selected model (claude-x). It may not exist or you may not " +
			"have access to it.", kind: KindAccount},
		{name: "model missing from the api", text: `API Error: 404 {"type":"error","error":{"type":"not_found_error","message":"model: claude-x"}}`,
			kind: KindAccount},
		{name: "model not available", text: "The model claude-x is not available for your account", kind: KindAccount},
		{name: "code in capitals and spaces", code: " Billing_Error ", kind: KindAccount},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyFailure(tc.code, tc.text, failureNow)

			assert.Equal(t, tc.kind, got.Kind)

			if tc.reset.IsZero() {
				assert.True(t, got.Reset.IsZero(), "no reset time: %v", got.Reset)
			} else {
				assert.True(t, tc.reset.Equal(got.Reset), "reset %v, want %v", got.Reset, tc.reset)
			}
		})
	}
}

// A limit's words win over an account's, so that a limit is never mistaken for a problem only the owner mends; a code that
// names a kind wins over the words.
func TestClassifyFailureOrder(t *testing.T) {
	assert.Equal(t, KindLimit, ClassifyFailure("", "You've hit your limit. Manage billing in settings.", failureNow).Kind)
	assert.Equal(t, KindAccount, ClassifyFailure("billing_error", "usage limit", failureNow).Kind)
	assert.Equal(t, KindLimit, ClassifyFailure("rate_limit", "Invalid API key", failureNow).Kind)
	assert.Equal(t, KindTransient, ClassifyFailure("server_error", "Overloaded", failureNow).Kind)
}

// A time with no zone is read where the clock is, and one with a zone named is the same instant wherever the clock is:
// nothing depends on the zone of the host.
func TestParseResetReadsZonesFromTheTextAndTheClock(t *testing.T) {
	east := time.FixedZone("east", 3*3600)
	nowEast := time.Date(2026, 11, 13, 10, 0, 0, 0, east)

	for _, tc := range []struct {
		name, text string
		now        time.Time
		want       time.Time
	}{
		{name: "no zone, the clock's", text: "resets 3pm", now: nowEast, want: time.Date(2026, 11, 13, 15, 0, 0, 0, east)},
		{name: "no zone, the day after", text: "resets 9am", now: nowEast, want: time.Date(2026, 11, 14, 9, 0, 0, 0, east)},
		{name: "a zone named, the same instant", text: "resets 3pm (UTC)", now: nowEast, want: time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)},
		{name: "the same on a clock in UTC", text: "resets 3pm (UTC)", now: failureNow, want: time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)},
		{name: "24 hour clock", text: "limit will reset at 15:04 (UTC)", now: failureNow, want: time.Date(2026, 11, 13, 15, 4, 0, 0, time.UTC)},
		{name: "midnight", text: "resets 12am (UTC)", now: failureNow, want: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)},
		{name: "noon, still ahead", text: "resets 12:30pm (UTC)", now: failureNow, want: time.Date(2026, 11, 13, 12, 30, 0, 0, time.UTC)},
		{name: "noon, passed", text: "resets 12pm (UTC)", now: failureNow, want: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)},
		{name: "a date with at", text: "resets Nov 15 at 8:30am (UTC)", now: failureNow, want: time.Date(2026, 11, 15, 8, 30, 0, 0, time.UTC)},
		{name: "an ordinal day", text: "resets Nov 15th, 8:30am (UTC)", now: failureNow, want: time.Date(2026, 11, 15, 8, 30, 0, 0, time.UTC)},
		{name: "a date next year", text: "resets Jan 2, 9am (UTC)", now: time.Date(2026, 12, 20, 12, 0, 0, 0, time.UTC),
			want: time.Date(2027, 1, 2, 9, 0, 0, 0, time.UTC)},
		{name: "a day span", text: "resets in 1 day 2 hours", now: failureNow, want: failureNow.Add(26 * time.Hour)},
		{name: "nothing", text: "You've hit your limit", now: failureNow},
		{name: "an hour out of range", text: "resets 13pm (UTC)", now: failureNow},
		{name: "a minute out of range", text: "resets 3:75pm (UTC)", now: failureNow},
		{name: "an epoch of the past", text: "limit|1700000000", now: failureNow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseReset(tc.text, tc.now)

			if tc.want.IsZero() {
				assert.True(t, got.IsZero(), got)

				return
			}

			assert.True(t, tc.want.Equal(got), "got %v, want %v", got, tc.want)
			assert.Equal(t, tc.want.Location().String(), got.Location().String(), "in the zone the text named, else the clock's")
		})
	}
}

func TestFailureClassWords(t *testing.T) {
	assert.Empty(t, FailureClass{Kind: KindTransient}.Words())
	assert.Empty(t, FailureClass{}.Words())
	assert.Equal(t, "usage limit", FailureClass{Kind: KindLimit}.Words())
	assert.Equal(t, "usage limit, resets Fri 15:00 UTC",
		FailureClass{Kind: KindLimit, Reset: time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)}.Words())
	assert.Equal(t, "account problem, needs the owner", FailureClass{Kind: KindAccount}.Words())
}
