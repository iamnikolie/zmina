package cmd

import (
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/cost"
)

// usageZoneCrew is usageCrew with one reply at a time in its session, the machine's zone set to local for the test.
func usageZoneCrew(t *testing.T, local *time.Location, tail string, replied time.Time) string {
	t.Helper()

	old := time.Local
	time.Local = local
	t.Cleanup(func() { time.Local = old })

	crewFile, projects, session := usageCrew(t, tail)
	usageWrite(t, filepath.Join(projects, "-a", "s1.jsonl"),
		`{"type":"agent-name","agentName":"`+session+`"}`+"\n"+usageTurn("m1", replied))

	return crewFile
}

// The days of usage are the crew's: a reply at 05:00 in Tokyo on the 11th is the 11th though it is still the 10th
// where the machine is (UTC here), and one at 20:00 on the 10th in Los Angeles is the 10th though the machine's day
// is the 11th. A crew with no timezone keeps the machine's.
func TestUsageDayIsTheCrewsDay(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tail    string
		replied time.Time
		day     string
	}{
		{"crew ahead of the machine", "timezone: Asia/Tokyo\n", time.Date(2026, 3, 10, 20, 0, 0, 0, time.UTC), "2026-03-11"},
		{"crew behind the machine", "timezone: America/Los_Angeles\n", time.Date(2026, 3, 11, 3, 0, 0, 0, time.UTC),
			"2026-03-10"},
		{"no timezone, before midnight on the machine", "", time.Date(2026, 3, 10, 20, 0, 0, 0, time.UTC), "2026-03-10"},
		{"no timezone, after midnight on the machine", "", time.Date(2026, 3, 11, 3, 0, 0, 0, time.UTC), "2026-03-11"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crewFile := usageZoneCrew(t, time.UTC, tc.tail, tc.replied)

			out, _, err := zm(t, crewFile, "usage", "--json", "--by", "day", "--since", "3650d")
			require.NoError(t, err)

			var rep usageReport
			require.NoError(t, json.Unmarshal([]byte(out), &rep))
			require.Len(t, rep.Rows, 1)
			assert.Equal(t, tc.day, rep.Rows[0].Key)
		})
	}
}

// Each crew counts its own days, when usage reads several: the same instant is the 11th in one and the 10th in the
// other.
func TestUsageDaysOfSeveralCrewsAreEachOwn(t *testing.T) {
	replied := time.Date(2026, 3, 10, 20, 0, 0, 0, time.UTC)
	tokyo := usageZoneCrew(t, time.UTC, "timezone: Asia/Tokyo\n", replied)
	local := usageZoneCrew(t, time.UTC, "", replied)

	prices, err := cost.Load(pricesPath())
	require.NoError(t, err)

	cells, problems := usageCells([]uiCrew{{Key: "tokyo", Path: tokyo}, {Key: "local", Path: local}}, "", time.Time{}, prices)
	require.Empty(t, problems)

	days := map[string]string{}
	for _, c := range cells {
		days[c.Crew] = c.Day
	}

	assert.Equal(t, map[string]string{"tokyo": "2026-03-11", "local": "2026-03-10"}, days)
}

// The page's "today" is on the clock the cells' days are: the crew's. With the machine 14 hours ahead of UTC and the crew
// 12 behind, the two are never on one date, and a reply made now is today's.
func TestUICostTodayIsTheCrewsDay(t *testing.T) {
	crewFile := usageZoneCrew(t, time.FixedZone("far east", 14*3600), "timezone: Etc/GMT+12\n", time.Now())

	got := (&app{out: io.Discard, err: io.Discard, crew: crewFile}).readUICost()

	assert.InDelta(t, 0.080016, got.Today, 1e-9)
	assert.InDelta(t, 0.080016, got.Week, 1e-9)
}
