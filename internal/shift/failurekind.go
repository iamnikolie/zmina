package shift

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// FailureKind says what an API error that ended a role's turn asks for, which decides how long its queue waits and who is
// told.
type FailureKind string

const (
	// KindTransient is an error that goes by itself: overloaded, a 5xx, the network, a timeout, and anything not
	// recognised. The queue waits FailHold, as it always did.
	KindTransient FailureKind = "transient"
	// KindLimit is a usage or rate limit reached. It lifts at a reset time, which the message often gives.
	KindLimit FailureKind = "limit"
	// KindAccount is a problem of the account or the setup that retrying does not mend and only the owner does: billing
	// or credit, a login that expired or a key that is not valid, a permission, a model that is not there.
	KindAccount FailureKind = "account"
)

// LimitHoldMax caps how long a queue waits for the reset time a limit message gave: a time further off than this, or one
// that was misread, does not park a role for days.
const LimitHoldMax = 6 * time.Hour

// resetMax is how far off a reset time can be and still be believed.
const resetMax = 30 * 24 * time.Hour

// FailureClass is what ClassifyFailure made of an error.
type FailureClass struct {
	Kind FailureKind
	// Reset is when a limit lifts, in the zone the message named, else in the location of the clock the class was made
	// with; zero when the message gave no time, and for any other kind.
	Reset time.Time
}

var (
	// accountCodes are the short codes Claude Code and the API give errors that only the owner mends.
	accountCodes = map[string]bool{
		"billing_error": true, "authentication_failed": true, "authentication_error": true, "permission_error": true,
		"permission_denied": true, "model_not_found": true, "invalid_api_key": true, "forbidden": true, "unauthorized": true,
	}
	// limitCodes are the codes of an error that is a limit reached.
	limitCodes = map[string]bool{"rate_limit": true, "rate_limit_error": true}

	// limitWords find the words of a usage or rate limit. "limit" alone is not one: a context or an output token limit
	// ends a turn too, and retrying it is no more use than for any error that goes by itself.
	limitWords = regexp.MustCompile(`usage limit|(?:5|five)[- ]hour limit|weekly limit|session limit|hit your (?:\w+ )?limit|` +
		`limit will reset|rate[ _-]?limit|too many requests|out of (?:extra )?usage|api error:? ?429\b`)

	// accountWords find the words of billing, login, permission and model errors.
	accountWords = regexp.MustCompile(`credit balance|insufficient (?:credit|funds|quota)|out of credits|billing|` +
		`payment required|authentication[_ ](?:error|failed)|invalid (?:x-)?api[ _-]?key|invalid bearer token|` +
		`invalid authentication|(?:oauth )?token (?:has )?(?:expired|been revoked)|please run /login|not logged in|` +
		`login (?:expired|required)|unauthorized|permission[_ ](?:error|denied)|forbidden|organization (?:has been |is )?disabled|` +
		`not have access|model[_ ]not[_ ]found|not_found_error.{0,100}model|issue with the selected model|` +
		`model .{0,60}(?:does not exist|not available|is not available)|api error:? ?(?:401|402|403)\b`)
)

// ClassifyFailure sorts an API error by what it asks for, from the short code Claude Code gave it and the words it showed,
// and, for a limit, reads the reset time out of the words (see parseReset; now is the clock the times are read against, and
// its location is the one a time with no zone is read in). It is a pure function: nothing is read but its arguments.
//
// The code decides when it is one of the few that name a kind. Then the words: a limit's before an account's, because
// taking a limit for an account error holds a role until the owner comes, and the other mistake only keeps today's
// five-minute wait. Whatever is not recognised is transient.
func ClassifyFailure(code, text string, now time.Time) FailureClass {
	code = strings.ToLower(strings.TrimSpace(code))
	words := strings.ToLower(code + " " + text)

	switch {
	case accountCodes[code]:
		return FailureClass{Kind: KindAccount}
	case limitCodes[code], limitWords.MatchString(words):
		return FailureClass{Kind: KindLimit, Reset: parseReset(text, now)}
	case accountWords.MatchString(words):
		return FailureClass{Kind: KindAccount}
	default:
		return FailureClass{Kind: KindTransient}
	}
}

// Words is the kind in a few words for a view or a note: empty for a transient error, which says nothing more than that it
// was an API error.
func (c FailureClass) Words() string {
	switch c.Kind {
	case KindLimit:
		if c.Reset.IsZero() {
			return "usage limit"
		}

		return "usage limit, resets " + c.Reset.Format("Mon 15:04 MST")
	case KindAccount:
		return "account problem, needs the owner"
	default:
		return ""
	}
}

var (
	// resetEpoch is the reset time of the older limit notice: "Claude AI usage limit reached|1760000000".
	resetEpoch = regexp.MustCompile(`\|(\d{9,11})\b`)

	// resetIn is a reset or a retry after a span: "resets in 2 hours 30 minutes", "try again in 45m".
	resetIn   = regexp.MustCompile(`(?i)(?:resets?|try again|retry(?: again)?)\s+in\s+((?:\d+\s*[a-z]+[\s,]*(?:and\s+)?)+)`)
	resetSpan = regexp.MustCompile(`(?i)(\d+)\s*(d|h|m|s)[a-z]*`)

	// resetAt is a reset at a clock time, with the day when it is not today and the zone when the notice names one:
	// "resets 3pm (Europe/Berlin)", "resets 2:30am", "limit will reset at 15:04 (UTC)", "resets Oct 9, 10am".
	resetAt = regexp.MustCompile(`(?i)\bresets?\s+(?:at\s+|on\s+)?(?:(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+` +
		`(\d{1,2})(?:[a-z]{2})?(?:,|\s+at)?\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?(?:\s*\(([^)]+)\))?`)
)

// parseReset is the moment a limit lifts, as the message says it, or the zero time when it does not say, says it in a form
// not known here, or says one that cannot be right (not after now, or more than a month off). A time with a zone named in
// brackets is read in that zone, which is the IANA name Claude Code shows, and returned in it; a time with none is read in
// the location of now, which is where the one who sees the notice is, and returned there. A zone that cannot be loaded makes
// the time unknown, never a guess.
func parseReset(text string, now time.Time) time.Time {
	var at time.Time

	switch {
	case resetEpoch.MatchString(text):
		n, _ := strconv.ParseInt(resetEpoch.FindStringSubmatch(text)[1], 10, 64)
		at = time.Unix(n, 0).In(now.Location())
	case resetIn.MatchString(text):
		at = readSpan(resetIn.FindStringSubmatch(text)[1], now)
	case resetAt.MatchString(text):
		at = readClock(resetAt.FindStringSubmatch(text), now)
	}

	if !at.After(now) || at.Sub(now) > resetMax {
		return time.Time{}
	}

	return at
}

// readSpan is now and the span: "2 hours 30 minutes". The zero time for none.
func readSpan(s string, now time.Time) time.Time {
	var d time.Duration

	for _, m := range resetSpan.FindAllStringSubmatch(s, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || n > 1000 {
			return time.Time{}
		}

		unit := map[string]time.Duration{"d": 24 * time.Hour, "h": time.Hour, "m": time.Minute, "s": time.Second}
		d += time.Duration(n) * unit[strings.ToLower(m[2])]
	}

	if d <= 0 {
		return time.Time{}
	}

	return now.Add(d)
}

// readClock is the time of a resetAt match (its groups: month, day, hour, minute, am or pm, zone).
func readClock(m []string, now time.Time) time.Time {
	hour, _ := strconv.Atoi(m[3])
	minute, _ := strconv.Atoi(m[4])
	ap := strings.ToLower(m[5])

	switch {
	case ap == "" && m[4] == "":
		// "resets 3" is no time of day.
		return time.Time{}
	case ap != "" && (hour < 1 || hour > 12), ap == "" && hour > 23, minute > 59:
		return time.Time{}
	case ap == "pm" && hour < 12:
		hour += 12
	case ap == "am" && hour == 12:
		hour = 0
	}

	loc := now.Location()

	if zone := strings.TrimSpace(m[6]); zone != "" {
		l, err := time.LoadLocation(zone)
		if err != nil {
			return time.Time{}
		}

		loc = l
	}

	local := now.In(loc)

	if m[1] == "" {
		// Today's, or tomorrow's when today's has passed.
		at := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}

		return at
	}

	months := []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
	month := time.Month(indexOf(months, strings.ToLower(m[1])) + 1)
	day, _ := strconv.Atoi(m[2])

	at := time.Date(local.Year(), month, day, hour, minute, 0, 0, loc)
	if !at.After(now) {
		at = at.AddDate(1, 0, 0)
	}

	return at
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}

	return -1
}
