package crew

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const reconcileRoles = "namespace: /ns\nroles:\n  dispatcher:\n  dev:\n"

// reconcile is off unless the crew file names it; on turns every part on.
func TestReconcileKey(t *testing.T) {
	c, err := Load(write(t, reconcileRoles))
	require.NoError(t, err)
	assert.Equal(t, Reconcile{}, c.Reconcile)
	assert.False(t, c.Reconcile.On(), "off unless the file says so")
	assert.Equal(t, DefaultReconcileTries, c.Reconcile.MaxTries())
	assert.Equal(t, OrphansOff, c.Reconcile.OrphanMode())

	on := Reconcile{Restart: true, Assign: true, Tries: 3, Orphans: OrphansDown}

	for body, want := range map[string]Reconcile{
		"reconcile: on\n":    on,
		"reconcile: true\n":  on,
		"reconcile: yes\n":   on,
		"reconcile: off\n":   {},
		"reconcile: false\n": {},
		"reconcile: {}\n":    {Tries: 3, Orphans: OrphansOff},
		// A block names the parts it wants: the rest is off, and tries is 3.
		"reconcile: {assign: true}\n":                                         {Assign: true, Tries: 3, Orphans: OrphansOff},
		"reconcile: {restart: on, tries: 5}\n":                                {Restart: true, Tries: 5, Orphans: OrphansOff},
		"reconcile: {orphans: report}\n":                                      {Tries: 3, Orphans: OrphansReport},
		"reconcile:\n  restart: true\n  tries: 1\n":                           {Restart: true, Tries: 1, Orphans: OrphansOff},
		"reconcile: {restart: true, assign: true, tries: 3, orphans: down}\n": on,
		"reconcile: {assign: false, orphans: off}\n":                          {Tries: 3, Orphans: OrphansOff},
		"reconcile: {tries: \"2\", assign: yes}\n":                            {Assign: true, Tries: 2, Orphans: OrphansOff},
	} {
		c, err := Load(write(t, body+reconcileRoles))
		require.NoError(t, err, body)
		assert.Equal(t, want, c.Reconcile, body)
		assert.Empty(t, c.Warnings, body)
	}
}

func TestReconcileParts(t *testing.T) {
	assert.True(t, Reconcile{Assign: true}.On())
	assert.True(t, Reconcile{Restart: true}.On())
	assert.True(t, Reconcile{Orphans: OrphansReport}.On())
	assert.False(t, Reconcile{Tries: 4, Orphans: OrphansOff}.On(), "tries alone switches nothing on")
	assert.Equal(t, 4, Reconcile{Tries: 4}.MaxTries())
	assert.Equal(t, MaxReconcileTries, Reconcile{Tries: 99}.MaxTries())
}

func TestReconcileRefusesWhatItCannotRead(t *testing.T) {
	for _, body := range []string{
		"reconcile: maybe\n",
		"reconcile: [on]\n",
		"reconcile: {tries: 0}\n",
		"reconcile: {tries: 6}\n",
		"reconcile: {tries: many}\n",
		"reconcile: {restart: sometimes}\n",
		"reconcile: {assign: [true]}\n",
		"reconcile: {orphans: kill}\n",
		"reconcile: {orphan: down}\n",
		"reconcile: {assign: true, assign: false}\n",
	} {
		_, err := Load(write(t, body+reconcileRoles))
		assert.ErrorContains(t, err, "reconcile", body)
	}
}

// A key of the block that is not one of the four names the four, so a typo is read off the error.
func TestReconcileNamesItsKeysInTheError(t *testing.T) {
	_, err := Load(write(t, "reconcile: {restrat: true}\n"+reconcileRoles))
	require.Error(t, err)
	assert.ErrorContains(t, err, `reconcile has no key "restrat" (restart, assign, tries, orphans)`)
}
