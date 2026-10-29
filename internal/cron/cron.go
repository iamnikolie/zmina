// Package cron reads the five-field schedules of the crew file (minute hour day-of-month month day-of-week) and finds
// their next time. Fields take *, numbers, lists (1,15), ranges (1-5) and steps (*/10, 9-17/2); day-of-week 0 and 7
// are Sunday. When both day fields are restricted, either one matching is enough, as in cron.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed five-field schedule.
type Schedule struct {
	minute, hour, dom, month, dow uint64
	domAny, dowAny                bool
}

type field struct {
	name     string
	min, max int
}

var fields = []field{{"minute", 0, 59}, {"hour", 0, 23}, {"day of month", 1, 31}, {"month", 1, 12}, {"day of week", 0, 7}}

// Parse reads a schedule such as "0 22 * * *" or "*/30 9-18 * * 1-5".
func Parse(spec string) (*Schedule, error) {
	parts := strings.Fields(spec)
	if len(parts) != len(fields) {
		return nil, fmt.Errorf("schedule %q: five fields — minute hour day-of-month month day-of-week", spec)
	}

	var bits [5]uint64

	for i, p := range parts {
		b, err := parseField(p, fields[i])
		if err != nil {
			return nil, fmt.Errorf("schedule %q: %w", spec, err)
		}

		bits[i] = b
	}

	s := &Schedule{minute: bits[0], hour: bits[1], dom: bits[2], month: bits[3], dow: bits[4],
		domAny: parts[2] == "*", dowAny: parts[4] == "*"}
	if s.dow&(1<<7) != 0 {
		s.dow |= 1
	}

	return s, nil
}

// Next is the first time after from that the schedule fires, in from's location; zero if none within five years.
// Across a daylight-saving change each wall-clock time fires once: a time that does not exist that day (the hour
// skipped in spring) does not fire, and in the hour repeated in autumn a time fires only the first time it comes.
// The search only ever moves forward, whatever the zone does.
func (s *Schedule) Next(from time.Time) time.Time {
	floor := wall(from)
	t := from.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)

	for t.Before(limit) {
		var next time.Time

		switch {
		case s.month&(1<<uint(t.Month())) == 0:
			next = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
		case !s.dayMatches(t):
			next = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
		case s.hour&(1<<uint(t.Hour())) == 0:
			next = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
		case s.minute&(1<<uint(t.Minute())) == 0:
			next = t.Add(time.Minute)
		case wall(t) <= floor:
			// The repeated hour: this wall-clock time came already.
			next = t.Add(time.Minute)
		default:
			return t
		}

		if !next.After(t) {
			next = t.Add(time.Minute)
		}

		t = next
	}

	return time.Time{}
}

// wall is a time as the clock on the wall shows it, ordered as text.
func wall(t time.Time) string { return t.Format("200601021504") }

func (s *Schedule) dayMatches(t time.Time) bool {
	dom := s.dom&(1<<uint(t.Day())) != 0
	dow := s.dow&(1<<uint(t.Weekday())) != 0

	switch {
	case s.domAny && s.dowAny:
		return true
	case s.domAny:
		return dow
	case s.dowAny:
		return dom
	default:
		return dom || dow
	}
}

func parseField(p string, f field) (uint64, error) {
	var bits uint64

	for part := range strings.SplitSeq(p, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1

		if hasStep {
			v, err := strconv.Atoi(stepStr)
			if err != nil || v <= 0 {
				return 0, fmt.Errorf("%s: bad step %q", f.name, stepStr)
			}

			step = v
		}

		lo, hi := f.min, f.max

		if rng != "*" {
			a, b, isRange := strings.Cut(rng, "-")

			var err error
			if lo, err = num(a, f); err != nil {
				return 0, err
			}

			hi = lo
			if isRange {
				if hi, err = num(b, f); err != nil {
					return 0, err
				}
			} else if hasStep {
				hi = f.max
			}

			if hi < lo {
				return 0, fmt.Errorf("%s: range %q runs backwards", f.name, rng)
			}
		}

		for v := lo; v <= hi; v += step {
			bits |= 1 << uint(v)
		}
	}

	return bits, nil
}

func num(s string, f field) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil || v < f.min || v > f.max {
		return 0, fmt.Errorf("%s: %q is not %d-%d", f.name, s, f.min, f.max)
	}

	return v, nil
}
