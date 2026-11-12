package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/shift"
)

// budgetDayFixture is a crew with a daily budget on dev, whose one reply cost $0.08 at the given UTC time, with the
// machine's own zone UTC. tail is the crew file's own timezone key, or empty.
func budgetDayFixture(t *testing.T, tail string, replied time.Time) *app {
	t.Helper()

	old := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = old })

	crewFile := testCrew(t, "  dev:\n    budget: 0.05\n"+tail)
	dir := filepath.Dir(crewFile)
	proj := filepath.Join(dir, "ns", "projects", "-work")
	require.NoError(t, os.MkdirAll(proj, 0o700))

	name := strings.TrimPrefix(strings.SplitN(string(mustRead(t, crewFile)), "\n", 2)[0], "name: ")
	body := `{"type":"agent-name","agentName":"` + name + `-dev"}` + "\n" + `{"type":"assistant","timestamp":"` +
		replied.UTC().Format(time.RFC3339) + `","cwd":"/work","message":{"id":"m1","model":"claude-opus-5-5",` +
		`"usage":{"input_tokens":4,"cache_creation_input_tokens":0,"cache_read_input_tokens":200000,` +
		`"output_tokens":2000}}}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(proj, "s1.jsonl"), []byte(body), 0o600))

	return &app{out: io.Discard, err: io.Discard, crew: crewFile}
}

func budgetNotes(t *testing.T, a *app) []string {
	t.Helper()

	st, err := shift.Read(filepath.Dir(a.crew))
	require.NoError(t, err)

	return st.Notes
}

// A role's budget is a day of the crew's clock: a reply at 05:00 in Tokyo on the 11th counts for the 11th there, though
// it is still the 10th in UTC (the machine's zone here), and the check at 09:30 in Tokyo, 00:30 UTC, finds the day's
// spend over the budget.
func TestBudgetDayIsTheCrewsDay(t *testing.T) {
	a := budgetDayFixture(t, "timezone: Asia/Tokyo\n", time.Date(2026, 3, 10, 20, 0, 0, 0, time.UTC))

	a.budgetTick(time.Date(2026, 3, 11, 0, 30, 0, 0, time.UTC))

	assert.Equal(t, []string{"budget: dev spent $0.080 today (API-equivalent), over its $0.050 a day"}, budgetNotes(t, a))
}

// The day turns at the crew's midnight, not the machine's. A reply at 23:00 in Tokyo on the 10th (14:00 UTC) counts at
// 23:30 there, but not at 00:30 on the 11th (15:30 UTC), when the crew's new day has begun though UTC's has not.
func TestBudgetDayTurnsAtTheCrewsMidnight(t *testing.T) {
	replied := time.Date(2026, 3, 10, 14, 0, 0, 0, time.UTC)

	a := budgetDayFixture(t, "timezone: Asia/Tokyo\n", replied)
	a.budgetTick(time.Date(2026, 3, 10, 14, 30, 0, 0, time.UTC))
	assert.Len(t, budgetNotes(t, a), 1, "the same day in Tokyo")

	b := budgetDayFixture(t, "timezone: Asia/Tokyo\n", replied)
	b.budgetTick(time.Date(2026, 3, 10, 15, 30, 0, 0, time.UTC))
	assert.Empty(t, budgetNotes(t, b), "the day turned at midnight in Tokyo: yesterday's spend is not today's")
}

// A crew whose file sets no timezone keeps the machine's day (UTC here): a reply at 20:00 on the 10th is not spend of
// the 11th, one at 00:10 on the 11th is.
func TestBudgetDayWithoutTimezoneIsTheMachinesDay(t *testing.T) {
	a := budgetDayFixture(t, "", time.Date(2026, 3, 10, 20, 0, 0, 0, time.UTC))

	a.budgetTick(time.Date(2026, 3, 11, 0, 30, 0, 0, time.UTC))
	assert.Empty(t, budgetNotes(t, a), "UTC midnight turned the day")

	b := budgetDayFixture(t, "", time.Date(2026, 3, 11, 0, 10, 0, 0, time.UTC))
	b.budgetTick(time.Date(2026, 3, 11, 0, 30, 0, 0, time.UTC))
	assert.Len(t, budgetNotes(t, b), 1, "a reply after UTC midnight is today's")
}
