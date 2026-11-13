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
	// KindTransient is an error that goes by itself: overloaded, a 5xx, a gateway or proxy page, the network, a timeout, a
	// request the API refused for its content, and anything not recognised. The queue waits FailHold.
	KindTransient FailureKind = "transient"
	// KindLimit is a usage or rate limit reached. It lifts at a reset time, which the notice often gives.
	KindLimit FailureKind = "limit"
	// KindAccount is a problem of the account or the setup that retrying does not mend and only the owner does: billing
	// or credit, a login that expired or a key that is not valid, a permission, a model that is not there. It is told only
	// by a status and an error type that belong together, or by words that say it outright.
	KindAccount FailureKind = "account"
)

const (
	// AccountHold is how long the queue of a role waits after an account error: a slow probe. It is long because nothing
	// but the owner mends it, and it is not for ever because nothing here can tell that the owner has: past it one line
	// goes as it would for any other error, and a turn that fails the same way is a record of its own and holds again.
	AccountHold = 30 * time.Minute
	// LimitHoldMax caps how long a queue waits for the reset time a limit message gave: a time further off than this, or one
	// that was misread, does not park a role for hours on end.
	LimitHoldMax = 6 * time.Hour
)

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
	// limitCodes are the codes of an error that is a limit reached.
	limitCodes = map[string]bool{"rate_limit": true, "rate_limit_error": true}

	// limitWords find the words of a usage or rate limit. "limit" alone is not one: a context or an output token limit
	// ends a turn too, and a bare 429 or "too many requests" says no more than any error that goes by itself.
	limitWords = regexp.MustCompile(`usage limit|(?:5|five)[- ]hour limit|weekly limit|session limit|hit your (?:\w+ )?limit|` +
		`limit will reset|rate[ _-]?limit|out of (?:extra )?usage`)

	// statusRe finds the HTTP status an error line starts with: "API Error: 529 ...".
	statusRe = regexp.MustCompile(`(?:api error:?|status(?: code)?:?|http)\s*(\d{3})\b`)

	// typeRe finds the error types the API names, in a JSON body or as a word.
	typeRe = regexp.MustCompile(`\b(authentication_error|permission_error|not_found_error|billing_error|rate_limit_error|` +
		`overloaded_error|api_error|server_error|invalid_request_error)\b`)

	// pageRe finds a page where an error should be: an HTML body, or a gateway or proxy answering for the API. Whatever
	// status it carries says nothing of the account.
	pageRe = regexp.MustCompile(`<\s*(?:!doctype|html|head|body)\b|bad gateway|gateway time-?out|\bproxy\b|cloudflare|nginx`)

	// creditRe and loginRe are the words that say an account error outright, whatever the status.
	creditRe = regexp.MustCompile(`credit balance is too low`)
	loginRe  = regexp.MustCompile(`invalid (?:x-)?api[ _-]?key|(?:api key|oauth token|token) (?:has )?(?:expired|been revoked)|` +
		`expired api key|please run /login|issue with the selected model`)

	// modelRe is an error body that names a model: the 404 of a model that is not there.
	modelRe = regexp.MustCompile(`not_found_error.{0,120}\bmodel\b`)
)

// accountCodes are the short codes Claude Code gives errors that only the owner mends.
var accountCodes = map[string]bool{"billing_error": true, "authentication_failed": true, "model_not_found": true}

// ClassifyFailure sorts an API error by what it asks for, from the short code Claude Code gave it and the words it showed,
// and, for a limit, reads the reset time out of the words (see parseReset; now is the clock the times are read against, and
// its location is the one a time with no zone is read in). It is a pure function: nothing is read but its arguments.
//
// It is strict about what the owner has to mend. Whatever carries a 5xx status or a server, overloaded or api error type is
// transient, whatever else it says; so is a page (HTML, a gateway or proxy) and a status with no error type that belongs
// to it. An account error is a pair that belongs together (401 with authentication_error, 402 or billing, 403 with
// permission_error, 404 with a not_found_error naming a model), or words that say it outright (a credit balance too low, an
// API key or a token that is invalid or expired, "please run /login"; the API's own 400 for a request it did not like, an
// invalid_request_error, may quote such words and is not one, as it is none for a credit balance, which that 400 is), or one
// of the codes Claude Code gives for it. Bare words (billing, forbidden, unauthorized) and a status alone are not. A limit is
// its code, its type or its words; a code that names a kind decides before the words. Whatever is not recognised is
// transient.
func ClassifyFailure(code, text string, now time.Time) FailureClass {
	code = strings.ToLower(strings.TrimSpace(code))
	words := strings.ToLower(code + " " + text)

	status := 0
	if m := statusRe.FindStringSubmatch(words); m != nil {
		status, _ = strconv.Atoi(m[1])
	}

	types := map[string]bool{}
	for _, m := range typeRe.FindAllString(words, -1) {
		types[m] = true
	}

	switch {
	case status >= 500 && status <= 599, types["server_error"], types["overloaded_error"], types["api_error"], code == "server_error",
		pageRe.MatchString(words):
		return FailureClass{Kind: KindTransient}
	case accountCodes[code]:
		return FailureClass{Kind: KindAccount}
	case limitCodes[code], types["rate_limit_error"], limitWords.MatchString(words):
		return FailureClass{Kind: KindLimit, Reset: parseReset(text, now)}
	case status == 402, types["billing_error"], creditRe.MatchString(words),
		status == 401 && types["authentication_error"], status == 403 && types["permission_error"],
		status == 404 && modelRe.MatchString(words),
		!types["invalid_request_error"] && loginRe.MatchString(words):
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
	resetSpan = regexp.MustCompile(`(?i)(\d+)\s*([a-z]+)`)

	// resetAt is a reset at a clock time, with the day when it is not today and the zone when the notice names one, in
	// brackets or as an abbreviation right after the time: "resets 3pm (Europe/Berlin)", "resets 2:30am", "limit will reset
	// at 15:04 UTC", "resets Oct 9, 10am". Its groups: month, day, hour, minute, am or pm, zone in brackets, zone
	// abbreviation, an offset after it (which makes the abbreviation no zone known here).
	resetAt = regexp.MustCompile(`(?i)\bresets?\s+(?:at\s+|on\s+)?(?:(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+` +
		`(\d{1,2})(?:[a-z]{2})?(?:,|\s+at)?\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?` +
		`(?:\s*\(([^)]+)\)|\s+((?-i:[A-Z]{2,5}))\b(\s?[+-]\d)?)?`)
)

// spanUnits are the units of a span, whole words only: "ms" or "months" is never read as minutes.
var spanUnits = map[string]time.Duration{
	"ms": time.Millisecond, "msec": time.Millisecond, "msecs": time.Millisecond, "millisecond": time.Millisecond,
	"milliseconds": time.Millisecond,
	"s":            time.Second, "sec": time.Second, "secs": time.Second, "second": time.Second, "seconds": time.Second,
	"m": time.Minute, "min": time.Minute, "mins": time.Minute, "minute": time.Minute, "minutes": time.Minute,
	"h": time.Hour, "hr": time.Hour, "hrs": time.Hour, "hour": time.Hour, "hours": time.Hour,
	"d": 24 * time.Hour, "day": 24 * time.Hour, "days": 24 * time.Hour,
	"w": 7 * 24 * time.Hour, "wk": 7 * 24 * time.Hour, "wks": 7 * 24 * time.Hour, "week": 7 * 24 * time.Hour,
	"weeks": 7 * 24 * time.Hour,
}

// zoneAbbr are the zone abbreviations a notice may carry, as seconds east of UTC; they and the IANA names are the zones
// there are (zoneOf). An abbreviation that is ambiguous in the world is read as the one a US-English notice means.
var zoneAbbr = map[string]int{
	"UTC": 0, "GMT": 0, "Z": 0,
	"EST": -5 * 3600, "EDT": -4 * 3600, "CST": -6 * 3600, "CDT": -5 * 3600,
	"MST": -7 * 3600, "MDT": -6 * 3600, "PST": -8 * 3600, "PDT": -7 * 3600,
	"CET": 3600, "CEST": 2 * 3600,
}

// zoneOf is the zone a notice names, in brackets or as an abbreviation: an abbreviation of zoneAbbr or an IANA name. Nil
// for any other, so that a time in a zone not known here is no time, never a guess; "Local" is the reader's, not the
// notice's, and is no zone either.
func zoneOf(name string) *time.Location {
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, "local") {
		return nil
	}

	if off, ok := zoneAbbr[strings.ToUpper(name)]; ok {
		if off == 0 {
			return time.UTC
		}

		return time.FixedZone(strings.ToUpper(name), off)
	}

	l, err := time.LoadLocation(name)
	if err != nil {
		return nil
	}

	return l
}

// parseReset is the moment a limit lifts, as the message says it, or the zero time when it does not say, says it in a form
// not known here, or says one that cannot be right (not after now, more than a month off, a date that is no date, a zone not
// known). A time with a zone named is read in that zone and returned in it; a time with none is read in the location of
// now, which is where the one who sees the notice is, and returned there. A clock time with no date that has passed already
// is no time: it is not read as tomorrow's.
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

// readSpan is now and the span: "2 hours 30 minutes". The zero time for none, and for a unit that is not one of spanUnits.
func readSpan(s string, now time.Time) time.Time {
	var d time.Duration

	for _, m := range resetSpan.FindAllStringSubmatch(s, -1) {
		n, err := strconv.Atoi(m[1])
		unit, ok := spanUnits[strings.ToLower(m[2])]

		if err != nil || n > 100000 || !ok {
			return time.Time{}
		}

		d += time.Duration(n) * unit
	}

	if d <= 0 {
		return time.Time{}
	}

	return now.Add(d)
}

// readClock is the time of a resetAt match (see resetAt for its groups).
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

	if zone := m[6] + m[7]; strings.TrimSpace(zone) != "" {
		if m[8] != "" {
			return time.Time{}
		}

		if loc = zoneOf(zone); loc == nil {
			return time.Time{}
		}
	}

	local := now.In(loc)

	if m[1] == "" {
		// Today's; one that has passed is no time, and not tomorrow's.
		at := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
		if at.Hour() != hour || at.Minute() != minute {
			return time.Time{}
		}

		return at
	}

	months := []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
	month := time.Month(indexOf(months, strings.ToLower(m[1])) + 1)
	day, _ := strconv.Atoi(m[2])

	// A date with its day: this year's, or next year's when it has passed (a notice in December that says Jan 2).
	for year := local.Year(); year <= local.Year()+1; year++ {
		at := time.Date(year, month, day, hour, minute, 0, 0, loc)
		if at.Month() != month || at.Day() != day || at.Hour() != hour || at.Minute() != minute {
			// Feb 31, or a minute that does not exist in the zone: no such time.
			return time.Time{}
		}

		if at.After(now) {
			return at
		}
	}

	return time.Time{}
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}

	return -1
}
