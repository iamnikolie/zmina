package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

// due moves an alarm into the past, as if its time had come while nobody looked.
func due(t *testing.T, crewFile, id string) {
	t.Helper()

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	_, err = alarmStore(c).Move(id, time.Now().Add(-time.Minute), "", time.Now())
	require.NoError(t, err)
}

func TestRemindTheOwner(t *testing.T) {
	dir := t.TempDir()
	pushed := filepath.Join(dir, "pushed")
	script := filepath.Join(dir, "push")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s|%s\\n' \"$*\" \"$ZMINA_ALARM\" >> "+pushed+"\n"),
		0o700))

	crewFile := testCrew(t, "notify: ["+script+", --title, zmina]\ntimezone: Europe/Warsaw\n")

	note := filepath.Join(dir, "note.md")
	require.NoError(t, os.WriteFile(note, []byte("Decide on the one-pager.\n"), 0o600))

	out, _, err := zm(t, crewFile, "remind", "+2m", "the", "site", "is", "on", "hold", "--note", note)
	require.NoError(t, err)
	assert.Regexp(t, `^R-1 · you · \w{3} \d+ \w{3} \d\d:\d\d \(in [12]m\)\n$`, out)

	_, _, err = zm(t, crewFile, "remind", "soon", "x")
	require.ErrorContains(t, err, "cannot read the time")

	_, _, err = zm(t, crewFile, "remind", "+1m", "x", "--role", "nobody")
	require.Error(t, err)

	out, _, err = zm(t, crewFile, "remind", "ls")
	require.NoError(t, err)
	assert.Regexp(t, `R-1\s+.*\(in [12]m\)\s+you\s+remind\s+the site is on hold`, out)

	_, _, err = zm(t, crewFile, "tick")
	require.NoError(t, err)

	out, _, err = zm(t, crewFile, "todo", "ls")
	require.NoError(t, err)
	assert.Equal(t, "nothing waits on you\n", out, "not yet")

	due(t, crewFile, "R-1")

	out, _, err = zm(t, crewFile, "tick")
	require.NoError(t, err)
	assert.Contains(t, out, "R-1: reminded you: the site is on hold → A-1")
	assert.Contains(t, out, "R-1: notified")
	assert.Equal(t, "--title zmina the site is on hold|R-1\n", string(mustRead(t, pushed)))

	out, _, err = zm(t, crewFile, "todo", "ls")
	require.NoError(t, err)
	assert.Regexp(t, `A-1\s+check\s+\S+\s+remind\s+the site is on hold\n\nDecide on the one-pager.`, out)

	_, _, err = zm(t, crewFile, "tick")
	require.NoError(t, err)
	assert.Equal(t, "--title zmina the site is on hold|R-1\n", string(mustRead(t, pushed)), "rings once")

	out, _, err = zm(t, crewFile, "remind", "ls", "--all")
	require.NoError(t, err)
	assert.Regexp(t, `R-1\s+.*ago\)\s+you\s+remind\s+rang → A-1\s+the site is on hold`, out)

	out, _, err = zm(t, crewFile, "remind", "ls")
	require.NoError(t, err)
	assert.Equal(t, "no alarms set\n", out)
}

func TestRemindMoveOff(t *testing.T) {
	crewFile := testCrew(t, "")

	_, _, err := zm(t, crewFile, "remind", "tomorrow", "standup")
	require.NoError(t, err)

	out, _, err := zm(t, crewFile, "remind", "move", "1", "tomorrow", "11:30")
	require.NoError(t, err)
	assert.Contains(t, out, "R-1 · you · ")
	assert.Contains(t, out, "11:30")

	out, _, err = zm(t, crewFile, "remind", "off", "r-1")
	require.NoError(t, err)
	assert.Equal(t, "R-1 · off\n", out)

	_, _, err = zm(t, crewFile, "remind", "move", "1", "+1h")
	require.ErrorContains(t, err, "R-1 already off")
}

func TestRoleAlarmAndSleep(t *testing.T) {
	crewFile := testCrew(t, "  designer:\n")

	out, _, err := zm(t, crewFile, "remind", "+5m", "look", "at", "the", "review", "--role", "designer")
	require.NoError(t, err)
	assert.Contains(t, out, "R-1 · designer · ")

	due(t, crewFile, "R-1")

	out, _, err = zm(t, crewFile, "tick")
	require.NoError(t, err)
	assert.Contains(t, out, "R-1: designer told, started")

	out, _, err = zm(t, crewFile, "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "[zmina alarm] look at the review")

	_, _, err = zm(t, crewFile, "sleep", "designer")
	require.ErrorContains(t, err, "--until")

	_, _, err = zm(t, crewFile, "sleep", "dispatcher", "--until", "+1h")
	require.ErrorContains(t, err, "never sleeps")

	// The fake claude draws its input line.
	require.Eventually(t, func() bool {
		out, _, _ := zm(t, crewFile, "peek", "designer")

		return strings.Contains(out, "❯")
	}, 5*time.Second, 100*time.Millisecond)

	out, _, err = zm(t, crewFile, "sleep", "designer", "--until", "+1h")
	require.NoError(t, err)
	assert.Regexp(t, `^designer asleep until .* \(in (59m|1h)\) · R-2\n$`, out)

	out, _, err = zm(t, crewFile, "ls")
	require.NoError(t, err)
	assert.Regexp(t, `designer\s+\S+\s+down \(asleep until \w{3} \d\d:\d\d\)`, out)

	due(t, crewFile, "R-2")

	out, _, err = zm(t, crewFile, "tick")
	require.NoError(t, err)
	assert.Contains(t, out, "R-2: designer told, started")

	out, _, err = zm(t, crewFile, "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "[zmina alarm] you were put to sleep")

	out, _, err = zm(t, crewFile, "ls")
	require.NoError(t, err)
	assert.NotContains(t, out, "asleep")
}

func TestLaterUntilComesBack(t *testing.T) {
	crewFile := testCrew(t, "")

	out, _, err := zm(t, crewFile, "later", "add", "the", "one-pager", "--until", "+3h")
	require.NoError(t, err)
	assert.Regexp(t, `^L-1 · back .* · R-1\n$`, out)

	due(t, crewFile, "R-1")

	out, _, err = zm(t, crewFile, "tick")
	require.NoError(t, err)
	assert.Contains(t, out, "R-1: L-1 is back on your todo")

	out, _, err = zm(t, crewFile, "todo", "ls", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"kind": "later"`)

	out, _, err = zm(t, crewFile, "todo", "park", "L-1")
	require.NoError(t, err)
	assert.Equal(t, "L-1 · parked again\n", out)

	out, _, err = zm(t, crewFile, "todo", "ls")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "nothing waits on you"))

	out, _, err = zm(t, crewFile, "later", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "L-1")
}
