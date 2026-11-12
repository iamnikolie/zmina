package shift

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A role's schedule is read on the crew file's timezone, not on the clock the beat happens to carry: with a crew in
// Tokyo, "22:00" is 22:00 there, a day later than the same wall time in UTC when the beat's clock is still on the
// day before.
func TestScheduleIsReadOnTheCrewsTimezone(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)

	f := fixtureWith(t, "timezone: Asia/Tokyo\n"+crewYAML+`  nightly:
    schedule: "0 22 * * *"
    prompt: run the harvest
`)

	// 21:58 UTC on the 24th is already 06:58 on the 25th in Tokyo.
	*f.now = time.Date(2026, 10, 24, 21, 58, 0, 0, time.UTC)
	f.s.Tick()

	next := f.s.State.Role("nightly").NextRun
	assert.True(t, next.Equal(time.Date(2026, 10, 25, 22, 0, 0, 0, tokyo)), "22:00 on the 25th in Tokyo, got %s", next)

	// 22:00 UTC on the 24th passes: the crew's 22:00 is 15 hours away.
	*f.now = time.Date(2026, 10, 24, 22, 5, 0, 0, time.UTC)
	f.s.Tick()
	assert.Empty(t, f.tk.list, "not at 22:00 on the host's or the beat's clock")
	assert.False(t, f.tm.Has("nightly"))

	// 22:00 in Tokyo on the 25th is 13:00 UTC.
	*f.now = time.Date(2026, 10, 25, 12, 59, 0, 0, time.UTC)
	f.s.Tick()
	assert.Empty(t, f.tk.list)

	*f.now = time.Date(2026, 10, 25, 13, 1, 0, 0, time.UTC)
	f.s.Tick()
	require.Len(t, f.tk.list, 1)
	assert.Equal(t, "schedule", f.tk.list[0].By)
	assert.True(t, f.tm.Has("nightly"), "the scheduled run starts the role")

	// The next one is the day after, on the same Tokyo wall time.
	next = f.s.State.Role("nightly").NextRun
	assert.True(t, next.Equal(time.Date(2026, 10, 26, 22, 0, 0, 0, tokyo)), "22:00 on the 26th in Tokyo, got %s", next)
}

// A weekday schedule crosses a day boundary the same way: Saturday morning in Tokyo is still Friday night in UTC, and
// the crew's Monday comes nine hours before Monday's 09:00 UTC.
func TestWeekdayScheduleFollowsTheCrewsDays(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)

	f := fixtureWith(t, "timezone: Asia/Tokyo\n"+crewYAML+`  weekly:
    schedule: "0 9 * * 1-5"
    prompt: run
`)

	// Friday 23:30 UTC is Saturday 08:30 in Tokyo.
	*f.now = time.Date(2026, 10, 23, 23, 30, 0, 0, time.UTC)
	f.s.Tick()

	next := f.s.State.Role("weekly").NextRun
	assert.True(t, next.Equal(time.Date(2026, 10, 26, 9, 0, 0, 0, tokyo)), "Monday 09:00 in Tokyo, got %s", next)
	assert.Equal(t, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), next.UTC(), "that is midnight UTC")
}

// A crew whose file sets no timezone keeps the clock it was given: the schedule is read on it as before.
func TestScheduleWithoutTimezoneKeepsTheBeatsClock(t *testing.T) {
	f := fixtureWith(t, crewYAML+`  nightly:
    schedule: "0 22 * * *"
    prompt: run
`)

	*f.now = time.Date(2026, 10, 24, 21, 58, 0, 0, time.UTC)
	f.s.Tick()
	assert.Equal(t, time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC), f.s.State.Role("nightly").NextRun)

	assert.Equal(t, *f.now, f.s.CrewClock(*f.now))
	assert.Same(t, time.UTC, f.s.CrewClock(*f.now).Location())
}
