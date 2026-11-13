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
// transcript; the codes are the ones the StopFailure hook gives. Anything not recognised is transient, and so is whatever
// is not the strict pair an account error is.
func TestClassifyFailure(t *testing.T) {
	soon := failureNow.Add(2 * time.Hour)

	for _, tc := range []struct {
		name, code, text string
		kind             FailureKind
		reset            time.Time
	}{
		// Transient: it goes by itself, or nothing is known of it, or it is not what it looks like.
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
		{name: "prompt too long", code: "invalid_request", kind: KindTransient,
			text: `API Error: 400 {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 213000 tokens > 200000 maximum"}}`},
		{name: "context length", kind: KindTransient,
			text: `API Error: 400 {"type":"error","error":{"type":"invalid_request_error","message":"input length and max_tokens exceed context limit: 198000 + 8192 > 200000"}}`},
		{name: "content filter", kind: KindTransient, text: "API Error: Output blocked by content filtering policy"},
		{name: "context limit is no usage limit", text: "Context limit reached · /compact or /clear to continue", kind: KindTransient},
		{name: "unknown code, no words", code: "unknown", kind: KindTransient},
		{name: "nothing", kind: KindTransient},
		{name: "words that merely mention authentication", text: "API Error: 500 the authentication service is restarting", kind: KindTransient},
		{name: "a name that is no code", code: "Something Else", text: "oops", kind: KindTransient},
		{name: "a bare 429 is no limit", text: "API Error: 429 Too Many Requests", kind: KindTransient},
		{name: "a bare 401 is no login", text: "API Error: 401 Unauthorized", kind: KindTransient},
		{name: "a bare 403 is no permission", text: "API Error: 403 Forbidden", kind: KindTransient},
		{name: "403 with an html body", kind: KindTransient,
			text: `API Error: 403 <html><head><title>403 Forbidden</title></head><body><h1>403 Forbidden</h1></body></html>`},
		{name: "a gateway quoting a 403", text: "API Error: 502 Bad Gateway: upstream returned 403 Forbidden", kind: KindTransient},
		{name: "a proxy page", text: "API Error: 401 the proxy requires authentication", kind: KindTransient},
		{name: "503 saying the model is not available", kind: KindTransient,
			text: `API Error: 503 {"type":"error","error":{"type":"server_error","message":"The model is temporarily not available, please retry"}}`},
		{name: "500 saying billing", text: "API Error: 500 billing service unavailable, try again", kind: KindTransient},
		{name: "a 400 quoting permission denied", kind: KindTransient,
			text: `API Error: 400 {"type":"error","error":{"type":"invalid_request_error","message":"tool result: permission denied: /etc/shadow"}}`},
		{name: "a 400 quoting an invalid key", kind: KindTransient,
			text: `API Error: 400 {"type":"error","error":{"type":"invalid_request_error","message":"the file mentions invalid x-api-key"}}`,
		},
		{name: "a 5xx type under a 401", kind: KindTransient,
			text: `API Error: 401 {"type":"error","error":{"type":"api_error","message":"authentication_error while contacting the auth service"}}`},
		{name: "a 404 that names no model", kind: KindTransient,
			text: `API Error: 404 {"type":"error","error":{"type":"not_found_error","message":"file not found"}}`},
		{name: "a 403 with another type", kind: KindTransient,
			text: `API Error: 403 {"type":"error","error":{"type":"invalid_request_error","message":"forbidden by policy"}}`},
		{name: "the words alone, no status or type", text: "your billing is forbidden and unauthorized", kind: KindTransient},
		{name: "the words of a model that is not available", text: "The model claude-x is not available for your account", kind: KindTransient},
		{name: "the words of no access", text: "you do not have access to this feature", kind: KindTransient},
		{name: "an organization disabled, no pair", text: "This organization has been disabled.", kind: KindTransient},

		// A limit, with the time it lifts when the message says it.
		{name: "rate limit by code", code: "rate_limit", kind: KindLimit,
			text: `API Error: 429 {"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your ` +
				`organization's rate limit of 30,000 input tokens per minute"}}`},
		{name: "rate limit by type", text: `API Error: 429 {"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, kind: KindLimit},
		{name: "hit your limit", text: "You've hit your limit · resets 3pm (America/New_York)", kind: KindLimit,
			reset: time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)},
		{name: "session limit", text: "You've hit your session limit · resets 11:30pm (UTC)", kind: KindLimit,
			reset: time.Date(2026, 11, 13, 23, 30, 0, 0, time.UTC)},
		{name: "weekly limit with a date", text: "Weekly limit reached ∙ resets Nov 16, 10am", kind: KindLimit,
			reset: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)},
		{name: "limit will reset at", text: "Claude usage limit reached. Your limit will reset at 5pm (America/Los_Angeles).", kind: KindLimit,
			reset: time.Date(2026, 11, 14, 1, 0, 0, 0, time.UTC)},
		{name: "a zone abbreviation after the time", text: "5-hour limit reached ∙ resets 11pm PST", kind: KindLimit,
			reset: time.Date(2026, 11, 14, 7, 0, 0, 0, time.UTC)},
		{name: "epoch after a bar", text: fmt.Sprintf("Claude AI usage limit reached|%d", soon.Unix()), kind: KindLimit, reset: soon},
		{name: "relative", text: "Usage limit reached, try again in 2 hours 30 minutes", kind: KindLimit,
			reset: failureNow.Add(150 * time.Minute)},
		{name: "relative, short", text: "rate limit exceeded; retry in 45s", kind: KindLimit, reset: failureNow.Add(45 * time.Second)},
		{name: "out of extra usage", text: "You're out of extra usage · resets 9pm (UTC)", kind: KindLimit,
			reset: time.Date(2026, 11, 13, 21, 0, 0, 0, time.UTC)},
		{name: "a limit with no time", text: "You've hit your limit", kind: KindLimit},
		{name: "a clock time that has passed is no time", text: "5-hour limit reached ∙ resets 2am (UTC)", kind: KindLimit},
		{name: "a time already past", text: "Claude AI usage limit reached|1700000000", kind: KindLimit},
		{name: "a time too far off", text: "Weekly limit reached ∙ resets Dec 30, 10am (UTC)", kind: KindLimit},
		{name: "a zone that does not exist", text: "usage limit reached · resets 3pm (Mars/Olympus)", kind: KindLimit},
		{name: "an abbreviation that is no zone", text: "usage limit reached · resets 3pm XYZ", kind: KindLimit},
		{name: "no time of day", text: "usage limit reached · resets 3", kind: KindLimit},
		{name: "a limit that a 5xx makes transient", text: "API Error: 529 usage limit reached, overloaded", kind: KindTransient},

		// An account problem: a pair that belongs together, or words that say it outright, or the code Claude Code gives it.
		{name: "credit balance by code", code: "billing_error", kind: KindAccount,
			text: "Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."},
		{name: "credit balance, no code", text: "Credit balance is too low", kind: KindAccount},
		{name: "credit balance in the API's 400", kind: KindAccount,
			text: `API Error: 400 {"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to ` +
				`access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}}`},
		{name: "402", text: "API Error: 402 payment required", kind: KindAccount},
		{name: "billing_error type", text: `API Error: 400 {"type":"error","error":{"type":"billing_error","message":"x"}}`, kind: KindAccount},
		{name: "login by code", code: "authentication_failed", text: "Invalid API key · Please run /login", kind: KindAccount},
		{name: "401 with authentication_error", kind: KindAccount,
			text: `API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"Invalid bearer token"}}`},
		{name: "oauth token expired", text: `API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired. ` +
			`Please obtain a new token or refresh your existing token."}}`, kind: KindAccount},
		{name: "oauth token expired, as the words alone", text: "OAuth token has expired", kind: KindAccount},
		{name: "invalid key", text: `API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			kind: KindAccount},
		{name: "invalid key, no status", text: "Invalid API key", kind: KindAccount},
		{name: "expired key", text: "Your API key has expired", kind: KindAccount},
		{name: "not logged in", text: "Not logged in · Please run /login", kind: KindAccount},
		{name: "403 with permission_error", kind: KindAccount,
			text: `API Error: 403 {"type":"error","error":{"type":"permission_error","message":"Your API key does not have ` +
				`permission to use the specified resource."}}`},
		{name: "model not found by code", code: "model_not_found", kind: KindAccount,
			text: "There's an issue with the selected model (claude-x). It may not exist or you may not have access to it. " +
				"Run /model to pick a different model."},
		{name: "model not found by Claude Code's words", kind: KindAccount,
			text: "There's an issue with the selected model (claude-x). It may not exist or you may not have access to it."},
		{name: "404 naming a model", kind: KindAccount,
			text: `API Error: 404 {"type":"error","error":{"type":"not_found_error","message":"model: claude-x"}}`},
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

// What decides when the words disagree: a 5xx or a server type wins over every word; a limit's code or type over an
// account's words; an account's code over a limit's words.
func TestClassifyFailureOrder(t *testing.T) {
	assert.Equal(t, KindTransient, ClassifyFailure("billing_error", "API Error: 500 {\"type\":\"api_error\"}", failureNow).Kind)
	assert.Equal(t, KindTransient, ClassifyFailure("rate_limit", "API Error: 529 Overloaded", failureNow).Kind)
	assert.Equal(t, KindLimit, ClassifyFailure("", "You've hit your limit. Your credit balance is fine.", failureNow).Kind)
	assert.Equal(t, KindLimit, ClassifyFailure("rate_limit", "Invalid API key", failureNow).Kind)
	assert.Equal(t, KindAccount, ClassifyFailure("billing_error", "usage limit", failureNow).Kind)
	assert.Equal(t, KindTransient, ClassifyFailure("server_error", "Overloaded", failureNow).Kind)
}

// A time with no zone is read where the clock is, and one with a zone named is the same instant wherever the clock is:
// nothing depends on the zone of the host. A clock time with no date that has passed is no time; a date that is none is no
// time; a unit that is not a whole word of the list is no span.
func TestParseResetReadsZonesFromTheTextAndTheClock(t *testing.T) {
	east := time.FixedZone("east", 3*3600)
	nowEast := time.Date(2026, 11, 13, 10, 0, 0, 0, east)

	for _, tc := range []struct {
		name, text string
		now        time.Time
		want       time.Time
	}{
		{name: "no zone, the clock's", text: "resets 3pm", now: nowEast, want: time.Date(2026, 11, 13, 15, 0, 0, 0, east)},
		{name: "no zone, passed: no time, not tomorrow's", text: "resets 9am", now: nowEast},
		{name: "a zone named, the same instant", text: "resets 3pm (UTC)", now: nowEast, want: time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)},
		{name: "the same on a clock in UTC", text: "resets 3pm (UTC)", now: failureNow, want: time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)},
		{name: "an abbreviation after the time", text: "resets 3pm UTC", now: nowEast, want: time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)},
		{name: "an abbreviation in brackets", text: "resets 3pm (PST)", now: failureNow,
			want: time.Date(2026, 11, 13, 15, 0, 0, 0, time.FixedZone("PST", -8*3600))},
		{name: "an abbreviation with an offset after it", text: "resets 3pm UTC+2", now: failureNow},
		{name: "an unknown abbreviation", text: "resets 3pm ZZZ", now: failureNow},
		{name: "a word after the time is no zone", text: "resets 3pm and you can go on", now: failureNow,
			want: time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC)},
		{name: "the reader's own zone is no zone", text: "resets 3pm (Local)", now: failureNow},
		{name: "24 hour clock", text: "limit will reset at 15:04 (UTC)", now: failureNow, want: time.Date(2026, 11, 13, 15, 4, 0, 0, time.UTC)},
		{name: "midnight has passed", text: "resets 12am (UTC)", now: failureNow},
		{name: "noon, still ahead", text: "resets 12:30pm (UTC)", now: failureNow, want: time.Date(2026, 11, 13, 12, 30, 0, 0, time.UTC)},
		{name: "noon, passed", text: "resets 12pm (UTC)", now: failureNow},
		{name: "a date with at", text: "resets Nov 15 at 8:30am (UTC)", now: failureNow, want: time.Date(2026, 11, 15, 8, 30, 0, 0, time.UTC)},
		{name: "an ordinal day", text: "resets Nov 15th, 8:30am (UTC)", now: failureNow, want: time.Date(2026, 11, 15, 8, 30, 0, 0, time.UTC)},
		{name: "a date next year", text: "resets Jan 2, 9am (UTC)", now: time.Date(2026, 12, 20, 12, 0, 0, 0, time.UTC),
			want: time.Date(2027, 1, 2, 9, 0, 0, 0, time.UTC)},
		{name: "a date today, passed: next year is over a month off", text: "resets Nov 13, 9am (UTC)", now: failureNow},
		{name: "a date that is none", text: "resets Feb 31, 9am (UTC)", now: failureNow},
		{name: "30 February is none too", text: "resets Feb 30, 9am (UTC)", now: time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)},
		{name: "a day span", text: "resets in 1 day 2 hours", now: failureNow, want: failureNow.Add(26 * time.Hour)},
		{name: "units spelled out", text: "try again in 1 hr 5 mins and 10 secs", now: failureNow, want: failureNow.Add(65*time.Minute + 10*time.Second)},
		{name: "milliseconds are not minutes", text: "retry in 500ms", now: failureNow, want: failureNow.Add(500 * time.Millisecond)},
		{name: "ms spelled out", text: "retry in 1500 milliseconds", now: failureNow, want: failureNow.Add(1500 * time.Millisecond)},
		{name: "months are no span", text: "try again in 2 months", now: failureNow},
		{name: "weeks are a span, too far off", text: "try again in 2 weeks", now: failureNow, want: failureNow.Add(14 * 24 * time.Hour)},
		{name: "five weeks is too far off", text: "try again in 5 weeks", now: failureNow},
		{name: "a unit that is a longer word", text: "try again in 5 minutia", now: failureNow},
		{name: "a unit that starts a longer word", text: "try again in 2 monthly cycles", now: failureNow},
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

// The zones a notice may name, in brackets or after the time, are the one table: the abbreviations here and the IANA names.
func TestZoneOf(t *testing.T) {
	for name, offset := range map[string]int{
		"UTC": 0, "utc": 0, "GMT": 0, "PST": -8 * 3600, "PDT": -7 * 3600, "est": -5 * 3600, "CEST": 2 * 3600,
		"America/New_York": -5 * 3600, "Asia/Kolkata": 19800,
	} {
		loc := zoneOf(name)
		if loc == nil {
			t.Errorf("%s: no zone", name)

			continue
		}

		_, got := time.Date(2026, 1, 15, 12, 0, 0, 0, loc).Zone()
		assert.Equal(t, offset, got, name)
	}

	for _, name := range []string{"", " ", "Local", "local", "XYZ", "Mars/Olympus", "UTC+2"} {
		assert.Nil(t, zoneOf(name), name)
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
