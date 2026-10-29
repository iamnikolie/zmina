package cron

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}

	return t
}

func TestNext(t *testing.T) {
	cases := []struct{ spec, from, want string }{
		{"0 22 * * *", "2026-10-24 21:58", "2026-10-24 22:00"},
		{"0 22 * * *", "2026-10-24 22:00", "2026-10-25 22:00"},
		{"*/15 * * * *", "2026-10-24 10:07", "2026-10-24 10:15"},
		{"30 9-18/3 * * *", "2026-10-24 13:00", "2026-10-24 15:30"},
		{"0 9 * * 1-5", "2026-10-24 10:00", "2026-10-26 09:00"}, // Saturday -> Monday
		{"0 9 * * 7", "2026-10-24 10:00", "2026-10-25 09:00"},   // 7 is Sunday
		{"0 0 1 * *", "2026-10-24 10:00", "2026-11-01 00:00"},
		{"0 0 31 2 *", "2026-10-24 10:00", ""},                 // never
		{"0 8 13 * 5", "2026-10-24 10:00", "2026-10-30 08:00"}, // either day field
	}

	for _, c := range cases {
		s, err := Parse(c.spec)
		require.NoError(t, err, c.spec)

		got := s.Next(at(c.from))
		if c.want == "" {
			assert.True(t, got.IsZero(), c.spec)

			continue
		}

		assert.Equal(t, at(c.want), got, c.spec)
	}
}

func TestParseRefuses(t *testing.T) {
	for _, spec := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "*/0 * * * *", "5-1 * * * *", "a * * * *"} {
		_, err := Parse(spec)
		assert.Error(t, err, spec)
	}
}

func TestDaylightSaving(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		t.Skip("no zoneinfo")
	}

	daily, _ := Parse("30 2 * * *")
	hourly, _ := Parse("0 * * * *")
	quarter, _ := Parse("*/15 * * * *")

	at := func(utc string) time.Time {
		v, err := time.Parse("2006-01-02 15:04", utc)
		require.NoError(t, err)

		return v.In(loc)
	}

	// Spring 2027-03-28: 02:00 CET jumps to 03:00 CEST; 02:30 does not exist that day.
	assert.Equal(t, at("2027-03-29 00:30"), daily.Next(at("2027-03-27 02:00")), "a time that does not exist is skipped")
	assert.Equal(t, at("2027-03-28 01:00"), quarter.Next(at("2027-03-28 00:50")), "01:50 CET -> 03:00 CEST")

	// Autumn 2026-10-25: 03:00 CEST goes back to 02:00 CET; 02:00-02:59 comes twice.
	first := at("2026-10-25 00:30") // 02:30 CEST
	assert.Equal(t, at("2026-10-26 01:30"), daily.Next(first), "02:30 fires once, not again at 02:30 CET")
	assert.Equal(t, at("2026-10-25 02:00"), hourly.Next(at("2026-10-25 00:00")), "02:00 CEST -> 03:00 CET")

	// Every case returns: the search never runs backwards.
	for _, spec := range []string{"* * * * *", "30 2 * * *", "0 2 * * *", "59 1 * * *", "0 3 * * 0"} {
		s, _ := Parse(spec)
		for _, from := range []string{"2027-03-27 23:59", "2027-03-28 00:59", "2026-10-25 00:59", "2026-10-25 01:30"} {
			n := s.Next(at(from))
			assert.True(t, n.After(at(from)), "%s from %s", spec, from)
		}
	}
}
