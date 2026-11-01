// Package alarm keeps a crew's alarms: a reminder for the owner, a line for a role at a time, a role asleep until
// it wakes, a parked item that comes back. One JSON file per alarm under alarms/ next to the crew file, written
// alone like tasks and the parked list. The ticker rings what is due; an alarm that came due while the machine slept
// or the ticker was down rings once on the first beat after.
package alarm

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	clockLayout = "15:04"
	dateLayout  = "2006-01-02"
	week        = 7
	// dayAbbrev is how much of a weekday's name names it: thu, thursday.
	dayAbbrev = 3
	// DefaultDayStart is when a day named without a time rings.
	DefaultDayStart = "09:00"
)

// ErrBadWhen is a time zmina cannot read.
var ErrBadWhen = errors.New("cannot read the time")

// Clock is how the words of a time are read: in Loc (nil: this machine's), a day named without a time at DayStart
// (HH:MM; empty: DefaultDayStart).
type Clock struct {
	Loc      *time.Location
	DayStart string
}

// When reads text as a moment after now:
//
//	morning              the start of the next working day (today's, while it is still ahead)
//	today | tomorrow     that day; a weekday (mon … sun) its next one; a date 2006-01-02
//	… 09:00              any of the above at that time instead of the day's start
//	09:00                today at that time, tomorrow once it has passed
//	+3h | +90m | +2d     that long from now (also +1h30m)
func When(text string, now time.Time, c Clock) (time.Time, error) {
	if c.Loc == nil {
		c.Loc = time.Local
	}

	if c.DayStart == "" {
		c.DayStart = DefaultDayStart
	}

	now = now.In(c.Loc)
	words := strings.Fields(strings.ToLower(strings.TrimSpace(text)))

	switch {
	case len(words) == 1 && strings.HasPrefix(words[0], "+"):
		return after(words[0], now)
	case len(words) == 1 && isClock(words[0]):
		at, _ := atClock(now, words[0], c.Loc)
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}

		return at, nil
	case len(words) == 0 || len(words) > 2:
		return time.Time{}, fmt.Errorf("%w %q: morning, today, tomorrow, a weekday, a date (2006-01-02), any of them "+
			"followed by 09:00, 09:00 alone, or +3h / +90m / +2d", ErrBadWhen, text)
	}

	day, err := dayOf(words[0], now, c)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w %q", err, text)
	}

	clock := c.DayStart
	if len(words) == 2 {
		clock = words[1]
	}

	at, err := atClock(day, clock, c.Loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w %q", err, text)
	}

	if !at.After(now) {
		return time.Time{}, fmt.Errorf("%w %q: %s has passed", ErrBadWhen, text, at.Format(dateLayout+" "+clockLayout))
	}

	return at, nil
}

// dayOf is the day a word names.
func dayOf(word string, now time.Time, c Clock) (time.Time, error) {
	switch word {
	case "today":
		return now, nil
	case "tomorrow":
		return now.AddDate(0, 0, 1), nil
	case "morning":
		return morning(now, c), nil
	}

	if wd, ok := weekday(word); ok {
		n := (int(wd) - int(now.Weekday()) + week) % week
		if n == 0 {
			n = week
		}

		return now.AddDate(0, 0, n), nil
	}

	day, err := time.ParseInLocation(dateLayout, word, now.Location())
	if err != nil {
		return time.Time{}, ErrBadWhen
	}

	return day, nil
}

func weekday(word string) (time.Weekday, bool) {
	if len(word) < dayAbbrev {
		return 0, false
	}

	for d := time.Sunday; d <= time.Saturday; d++ {
		if strings.HasPrefix(strings.ToLower(d.String()), word) {
			return d, true
		}
	}

	return 0, false
}

// morning is the next working day whose start is still ahead: today's, before it begins on a weekday.
func morning(now time.Time, c Clock) time.Time {
	day := now

	for {
		at, err := atClock(day, c.DayStart, c.Loc)
		if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday && (err != nil || at.After(now)) {
			return day
		}

		day = day.AddDate(0, 0, 1)
	}
}

// atClock is clock (HH:MM, or H:MM) on day's date in loc.
func atClock(day time.Time, clock string, loc *time.Location) (time.Time, error) {
	c, err := time.Parse(clockLayout, clock)
	if err != nil {
		if c, err = time.Parse("3:04", clock); err != nil {
			return time.Time{}, ErrBadWhen
		}
	}

	return time.Date(day.Year(), day.Month(), day.Day(), c.Hour(), c.Minute(), 0, 0, loc), nil
}

func isClock(word string) bool {
	_, err := atClock(time.Now(), word, time.UTC)

	return err == nil
}

// after is now plus +Nm, +Nh, +Nd, or a Go duration (+1h30m).
func after(word string, now time.Time) (time.Time, error) {
	span := word[1:]

	if n, err := strconv.Atoi(strings.TrimSuffix(span, "d")); err == nil && strings.HasSuffix(span, "d") {
		if n <= 0 {
			return time.Time{}, fmt.Errorf("%w %q", ErrBadWhen, word)
		}

		return now.AddDate(0, 0, n), nil
	}

	d, err := time.ParseDuration(span)
	if err != nil || d < time.Minute {
		return time.Time{}, fmt.Errorf("%w %q: +90m, +3h, +2d", ErrBadWhen, word)
	}

	return now.Add(d), nil
}
