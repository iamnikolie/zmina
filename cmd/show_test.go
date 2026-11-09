package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShowReadsEveryFamily: zmina show reads any id zmina prints, in any case, as what it is in words and the command
// to act; zmina log names what a task line is about.
func TestShowReadsEveryFamily(t *testing.T) {
	file := testCrew(t, "  dev:\n")

	_, _, err := zm(t, file, "task", "new", "dev", "fix the login")
	require.NoError(t, err)
	_, _, err = zm(t, file, "later", "add", "rotate the deploy key", "--note", "all repos")
	require.NoError(t, err)
	_, _, err = zm(t, file, "todo", "add", "Ship the login fix to production today?", "--title", "ship the login fix",
		"--rec", "yes")
	require.NoError(t, err)
	_, _, err = zm(t, file, "remind", "tomorrow 09:00", "renew the certificate")
	require.NoError(t, err)

	t.Setenv("ZMINA_ROLE", "dev")
	_, _, err = zm(t, file, "lesson", "add", "Run make lint before a commit", "--why", "CI failed twice",
		"--applies", "every")
	require.NoError(t, err)
	t.Setenv("ZMINA_ROLE", "")

	out, _, err := zm(t, file, "show", "t-1", "L1", "A-1", "r-1", "ls-1", "S-5")
	require.NoError(t, err)

	for _, want := range []string{
		"T-1 · fix the login\n  task · with dev · 0m\n",
		"  act: zmina task show T-1\n",
		"L-1 · rotate the deploy key\n  parked item · waits to be taken back",
		"  note: all repos\n  act: zmina later take L-1 <role>\n",
		"A-1 · ship the login fix\n  decision for the owner · waits on the owner",
		"  asks: Ship the login fix to production today?\n  recommends: yes\n  act: zmina todo yes A-1 (its recommendation), or zmina todo answer A-1",
		"R-1 · renew the certificate\n  remind · for you · rings ",
		"  act: zmina remind off R-1\n",
		"LS-1 · Run make lint before a commit\n  lesson · new · ",
		"  why: CI failed twice\n  act: zmina lesson show LS-1\n",
		"S-5 · waits for no ack and is not in the log\n",
	} {
		assert.Contains(t, out, want)
	}

	out, _, err = zm(t, file, "show", "T-9")
	require.NoError(t, err)
	assert.Equal(t, "T-9 · not found in this crew's ledger\n", out)

	_, _, err = zm(t, file, "show", "14")
	require.ErrorContains(t, err, "no id zmina knows: T-n (task)")

	out, _, err = zm(t, file, "log", "--task", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "task T-1 (fix the login)  by ")
}
