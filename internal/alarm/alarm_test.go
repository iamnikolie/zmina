package alarm

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWhen(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Warsaw")
	require.NoError(t, err)

	c := Clock{Loc: loc, DayStart: "09:00"}
	// Friday 2026-10-30 14:20 in Warsaw.
	now := time.Date(2026, 10, 30, 14, 20, 0, 0, loc)
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, loc) }

	for text, want := range map[string]time.Time{
		"+2m":             now.Add(2 * time.Minute),
		"+90m":            now.Add(90 * time.Minute),
		"+3h":             now.Add(3 * time.Hour),
		"+1h30m":          now.Add(90 * time.Minute),
		"+2d":             time.Date(2026, 11, 1, 14, 20, 0, 0, loc),
		"17:00":           at(30, 17, 0),
		"9:30":            time.Date(2026, 10, 31, 9, 30, 0, 0, loc),
		"today 18:00":     at(30, 18, 0),
		"tomorrow":        at(31, 9, 0),
		"Tomorrow 11:15":  at(31, 11, 15),
		"morning":         time.Date(2026, 11, 2, 9, 0, 0, 0, loc), // Friday afternoon: Monday's start
		"thu":             time.Date(2026, 11, 5, 9, 0, 0, 0, loc),
		"friday 10:00":    time.Date(2026, 11, 6, 10, 0, 0, 0, loc), // today's name is next week's
		"2026-11-08":      time.Date(2026, 11, 8, 9, 0, 0, 0, loc),
		"2026-11-08 7:45": time.Date(2026, 11, 8, 7, 45, 0, 0, loc),
	} {
		got, err := When(text, now, c)
		require.NoError(t, err, text)
		assert.True(t, want.Equal(got), "%s: want %s, got %s", text, want, got)
	}

	early := time.Date(2026, 11, 2, 7, 0, 0, 0, loc) // Monday 07:00
	got, err := When("morning", early, c)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 11, 2, 9, 0, 0, 0, loc), got, "today's start while it is ahead")

	for _, bad := range []string{"", "soon", "+0m", "+30s", "+x", "today 14:00", "2026-10-01", "tomorrow at 9", "mo",
		"tomorrow 25:00"} {
		_, err := When(bad, now, c)
		assert.ErrorIs(t, err, ErrBadWhen, bad)
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	a, err := s.Add(Alarm{Kind: Remind, At: now.Add(time.Hour), Text: "check the site"}, now)
	require.NoError(t, err)
	assert.Equal(t, "R-1", a.ID)
	assert.Equal(t, Set, a.State)

	b, err := s.Add(Alarm{Kind: Sleep, Role: "dev", At: now.Add(time.Minute), Text: "wake"}, now)
	require.NoError(t, err)

	// A damaged file still takes its number.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "R-3.json"), []byte("{"), 0o600))
	c, err := s.Add(Alarm{Kind: Remind, At: now.Add(-time.Minute), Text: "missed"}, now)
	require.NoError(t, err)
	assert.Equal(t, "R-4", c.ID)

	_, problems, err := s.Scan()
	require.NoError(t, err)
	assert.Len(t, problems, 1)

	assert.Equal(t, []string{"R-4", "R-2", "R-1"}, ids(s.Pending()), "soonest first")
	assert.Equal(t, []string{"R-4"}, ids(s.Due(now)))

	_, err = s.Move("2", now.Add(-time.Second), "+1m", now)
	require.NoError(t, err)
	assert.Equal(t, []string{"R-4", "R-2"}, ids(s.Due(now)))

	off, err := s.TakeOff("r-2", now)
	require.NoError(t, err)
	assert.Equal(t, Off, off.State)
	_, err = s.TakeOff(b.ID, now)
	require.Error(t, err)

	got, err := s.Get("R-1")
	require.NoError(t, err)
	assert.Equal(t, "you", got.For())
}

// Files written before an alarm could owe its part outside the lock have none of those fields: they read, a rung one
// stays rung, and what a new file says comes back whole.
func TestAlarmFilesFromBeforeTheEffectFieldsStillRead(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	for name, body := range map[string]string{
		"R-1": `{"id": "R-1", "kind": "remind", "at": "2026-11-01T09:00:00Z", "text": "set one", "state": "set",
  "created": "2026-11-01T08:00:00Z", "updated": "2026-11-01T08:00:00Z"}`,
		"R-2": `{"id": "R-2", "kind": "remind", "at": "2026-11-01T09:00:00Z", "text": "rung one", "state": "rang",
  "rang": "2026-11-01T09:00:30Z", "ask": "A-1", "created": "2026-11-01T08:00:00Z", "updated": "2026-11-01T09:00:30Z"}`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o600))
	}

	all, problems, err := s.Scan()
	require.NoError(t, err)
	require.Empty(t, problems)
	require.Len(t, all, 2)

	assert.True(t, all[0].Pending())
	assert.Empty(t, all[0].Owed)
	assert.Equal(t, Rang, all[1].State)
	assert.False(t, all[1].Pending(), "rung by the old build: rung")
	assert.Equal(t, []string{"R-1"}, ids(s.Pending()))

	owing := all[0]
	owing.Owed, owing.Tries, owing.Tried, owing.TryErr = "set one", 2, now, "notify: exit status 1"
	require.NoError(t, s.Put(owing))

	got, err := s.Get("R-1")
	require.NoError(t, err)
	assert.Equal(t, owing, got)
	assert.True(t, got.Pending(), "owing its part: still to ring")
}

// An alarm that could not be delivered is listed on its own, is not pending, is not moved, and is cleared by off.
func TestFailedAlarmIsNotPendingAndIsClearedByOff(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	a, err := s.Add(Alarm{Kind: Remind, At: now.Add(-time.Hour), Text: "call the bank"}, now)
	require.NoError(t, err)

	live, err := s.Add(Alarm{Kind: Remind, At: now.Add(time.Hour), Text: "later one"}, now)
	require.NoError(t, err)

	a.State, a.Tries, a.TryErr = Failed, 6, "notify: exit status 1"
	require.NoError(t, s.Put(a))

	assert.Equal(t, []string{live.ID}, ids(s.Pending()))
	assert.Empty(t, s.Due(now), "a failed alarm does not ring again")
	assert.Equal(t, []string{a.ID}, ids(s.Failed()))

	_, err = s.Move(a.ID, now.Add(time.Hour), "+1h", now)
	require.ErrorContains(t, err, "R-1 already failed")

	off, err := s.TakeOff(a.ID, now)
	require.NoError(t, err)
	assert.Equal(t, Off, off.State)
	assert.Empty(t, s.Failed())
}

func ids(as []Alarm) []string {
	out := []string{}
	for _, a := range as {
		out = append(out, a.ID)
	}

	return out
}
