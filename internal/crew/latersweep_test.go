package crew

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// later_sweep is off unless the crew file names it; its defaults are a week, two weeks and twelve items.
func TestLaterSweepKey(t *testing.T) {
	const roles = "namespace: /ns\nroles:\n  dispatcher:\n  dev:\n"

	c, err := Load(write(t, roles))
	require.NoError(t, err)
	assert.False(t, c.LaterSweepOn(), "off unless the file says so")
	assert.Nil(t, c.LaterSweep)

	c, err = Load(write(t, "later_sweep: {role: dev}\n"+roles))
	require.NoError(t, err)
	require.True(t, c.LaterSweepOn())
	assert.Equal(t, "dev", c.LaterSweep.Role)
	assert.Equal(t, 7*24*time.Hour, c.LaterSweep.EverySpan())
	assert.Equal(t, 14*24*time.Hour, c.LaterSweep.OlderSpan())
	assert.Equal(t, 12, c.LaterSweep.MaxItems())
	assert.Empty(t, c.Warnings)

	c, err = Load(write(t, "later_sweep:\n  role: dispatcher\n  every: 3d\n  older: 21d\n  max: 5\n"+roles))
	require.NoError(t, err)
	assert.Equal(t, "dispatcher", c.LaterSweep.Role)
	assert.Equal(t, 72*time.Hour, c.LaterSweep.EverySpan())
	assert.Equal(t, 21*24*time.Hour, c.LaterSweep.OlderSpan())
	assert.Equal(t, 5, c.LaterSweep.MaxItems())

	c, err = Load(write(t, "later_sweep: {role: dev, every: 36h, older: 90s}\n"+roles))
	require.NoError(t, err, "a span is a Go duration or days, as everywhere in the file")
	assert.Equal(t, 36*time.Hour, c.LaterSweep.EverySpan())
	assert.Equal(t, 90*time.Second, c.LaterSweep.OlderSpan())
}

// The role is read as the check reads it: a name with spaces round it is the name, so the ticker never asks for a role the
// check let through under another spelling.
func TestLaterSweepRoleIsTrimmedWhenTheCrewIsLoaded(t *testing.T) {
	const roles = "namespace: /ns\nroles:\n  dispatcher:\n  dev:\n"

	c, err := Load(write(t, "later_sweep: {role: \" dev \"}\n"+roles))
	require.NoError(t, err)
	assert.Equal(t, "dev", c.LaterSweep.Role)

	_, err = Load(write(t, "later_sweep: {role: \"  \"}\n"+roles))
	require.ErrorContains(t, err, "later_sweep: role is a role of this crew")
}

// max: 0 is how an absent max reads, and is the default: the message for a number out of range says so.
func TestLaterSweepMaxZeroIsTheDefault(t *testing.T) {
	const roles = "namespace: /ns\nroles:\n  dispatcher:\n  dev:\n"

	c, err := Load(write(t, "later_sweep: {role: dev, max: 0}\n"+roles))
	require.NoError(t, err)
	assert.Equal(t, DefaultLaterSweepMax, c.LaterSweep.MaxItems())

	_, err = Load(write(t, "later_sweep: {role: dev, max: 26}\n"+roles))
	require.ErrorContains(t, err, "later_sweep: max is a number of items from 1 to 25 (0 or no key: 12), not 26")
}

func TestLaterSweepKeyIsCheckedWhenTheCrewIsLoaded(t *testing.T) {
	const roles = "namespace: /ns\nroles:\n  dispatcher:\n  dev:\n"

	for body, want := range map[string]string{
		"later_sweep: {}\n":                           "later_sweep: role is a role of this crew",
		"later_sweep: {every: 7d}\n":                  "later_sweep: role is a role of this crew",
		"later_sweep: {role: ghost}\n":                `later_sweep: role "ghost" is not one of the roles`,
		"later_sweep: {role: dev#2}\n":                `later_sweep: role "dev#2" is not one of the roles`,
		"later_sweep: {role: dev, every: soon}\n":     `later_sweep: every "soon"`,
		"later_sweep: {role: dev, every: 0d}\n":       `later_sweep: every "0d"`,
		"later_sweep: {role: dev, every: -2h}\n":      `later_sweep: every "-2h"`,
		"later_sweep: {role: dev, older: 2weeks}\n":   `later_sweep: older "2weeks"`,
		"later_sweep: {role: dev, older: 0}\n":        `later_sweep: older "0"`,
		"later_sweep: {role: dev, max: -1}\n":         "later_sweep: max is a number of items from 1 to 25 (0 or no key: 12), not -1",
		"later_sweep: {role: dev, max: 26}\n":         "later_sweep: max is a number of items from 1 to 25",
		"later_sweep: dev\n":                          "cannot unmarshal",
		"later_sweep: {role: dev, every: [1, 2]}\n":   "cannot unmarshal",
		"later_sweep: {role: dev, max: twelve}\n":     "cannot unmarshal",
		"later_sweep: {role: dev, older: 7d, x: 1}\n": "",
	} {
		c, err := Load(write(t, body+roles))
		if want == "" {
			// An unknown key inside the block is a warning with its line, as in the rest of the file.
			require.NoError(t, err, body)
			assert.NotEmpty(t, c.Warnings, body)

			continue
		}

		require.ErrorContains(t, err, want, body)
	}
}
