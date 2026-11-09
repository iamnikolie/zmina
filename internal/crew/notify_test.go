package crew

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNotifyRules(t *testing.T) {
	dir := t.TempDir()
	load := func(body string) (*Crew, error) {
		return Load(write(t, "dir: "+dir+"\n"+body+"roles:\n  dispatcher:\n"))
	}

	c, err := load("")
	require.NoError(t, err)
	assert.False(t, c.NotifiesOn(NotifyReminder), "no command: nothing")

	c, err = load("notify: [push, --title, zmina]\n")
	require.NoError(t, err)
	assert.True(t, c.NotifiesOn(NotifyReminder))
	assert.True(t, c.NotifiesOn(NotifyBlocking), "reminders and blocking items by default")
	assert.False(t, c.NotifiesOn(NotifyAsk))
	assert.Equal(t, 24*time.Hour, c.NotifyAgainD())

	c, err = load("notify: push\nnotify_on: [ask, reminder, ask]\nnotify_again: \"off\"\n")
	require.NoError(t, err)
	assert.True(t, c.NotifiesOn(NotifyAsk))
	assert.False(t, c.NotifiesOn(NotifyBlocking))
	assert.Zero(t, c.NotifyAgainD())

	c, err = load("notify: push\nnotify_again: 6h\n")
	require.NoError(t, err)
	assert.Equal(t, 6*time.Hour, c.NotifyAgainD())

	for body, want := range map[string]string{
		"notify_on: [stuck]\n":       `notify_on "stuck": one of reminder, blocking, ask`,
		"quiet_hours: \"23:00\"\n":   `quiet_hours "23:00": two times of day such as 23:00-08:00, or off`,
		"quiet_hours: 08:00-08:00\n": `quiet_hours "08:00-08:00"`,
		"quiet_hours: 25:00-08:00\n": `quiet_hours "25:00-08:00"`,
		"notify_again: 10m\n":        `notify_again "10m": a span of an hour at least`,
		"notify_again: sometimes\n":  `notify_again "sometimes"`,
	} {
		_, err := load(body)
		require.ErrorContains(t, err, want, body)
	}
}

// Quiet hours are read on the crew's clock; a stretch may run past midnight, and its end is the first time after it.
func TestQuietHours(t *testing.T) {
	dir := t.TempDir()
	warsaw, err := time.LoadLocation("Europe/Warsaw")
	require.NoError(t, err)

	c, err := Load(write(t, "dir: "+dir+"\ntimezone: Europe/Warsaw\nquiet_hours: 23:00-08:00\nroles:\n  dispatcher:\n"))
	require.NoError(t, err)

	at := func(day, hh, mm int) time.Time { return time.Date(2026, 11, day, hh, mm, 0, 0, warsaw) }

	for _, tc := range []struct {
		t     time.Time
		quiet bool
		until time.Time
	}{
		{at(9, 22, 59), false, time.Time{}},
		{at(9, 23, 0), true, at(10, 8, 0)},
		{at(10, 3, 0), true, at(10, 8, 0)},
		{at(10, 7, 59), true, at(10, 8, 0)},
		{at(10, 8, 0), false, time.Time{}},
		{at(10, 12, 0).UTC(), false, time.Time{}},
		{at(10, 2, 0).UTC(), true, at(10, 8, 0)},
	} {
		quiet, until := c.QuietAt(tc.t)
		assert.Equal(t, tc.quiet, quiet, tc.t.String())
		assert.True(t, until.Equal(tc.until), "%s: until %s", tc.t, until)
	}

	c, err = Load(write(t, "dir: "+dir+"\ntimezone: Europe/Warsaw\nquiet_hours: 12:00-13:30\nroles:\n  dispatcher:\n"))
	require.NoError(t, err)
	quiet, until := c.QuietAt(at(10, 13, 0))
	assert.True(t, quiet)
	assert.True(t, until.Equal(at(10, 13, 30)))
	quiet, _ = c.QuietAt(at(10, 13, 30))
	assert.False(t, quiet)

	c, err = Load(write(t, "dir: "+dir+"\nroles:\n  dispatcher:\n"))
	require.NoError(t, err)
	quiet, _ = c.QuietAt(at(10, 3, 0))
	assert.False(t, quiet, "none set")
}
