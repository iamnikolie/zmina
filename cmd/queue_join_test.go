package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

// A post that joined several of a role's lines is listed as the lines it joined, each with its own facts, and so is a
// joined post kept as failed; one line stays one row.
func TestQueueLsShowsAJoinedPostAsItsLines(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	now := time.Now()

	c, err := crew.Load(file)
	require.NoError(t, err)

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)

	const first, second, third = "[dispatcher] look at T-1", "[ops] the build is red", "[dispatcher] and then T-2"

	rs := l.Role("dev")
	rs.Posted = []shift.Posted{{
		Line: "1. " + first + "\n2. " + second, MsgID: "m-1", Session: "s-1", At: now.Add(-3 * time.Minute),
		Parts: []shift.Part{{Line: first}, {Line: second}},
	}}
	rs.Failed = []shift.Failed{{
		Line: "1. " + third + "\n2. " + second, Error: "tmux refused", At: now.Add(-time.Hour),
		Parts: []shift.Part{{Line: third}, {Line: first}},
	}}
	require.NoError(t, l.Save())

	writeCards(t, file, "dev",
		shift.Card{ID: shift.CardID("dev", first), From: "dispatcher", Written: cardAt(12, 14, 2),
			Accepted: &shift.CardAccepted{At: cardAt(12, 14, 3), How: shift.AcceptedSocket, Session: "s-1"}},
		shift.Card{ID: shift.CardID("dev", second), From: "ops", Written: cardAt(12, 14, 2),
			Accepted: &shift.CardAccepted{At: cardAt(12, 14, 3), How: shift.AcceptedSocket, Session: "s-1"}},
	)

	out, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)

	where := "to its inbox socket " + now.Add(-3*time.Minute).Local().Format("01-02 15:04") + ", 3m ago, not yet seen"

	var shown []string

	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "      ") || strings.Contains(line, "[dispatcher]") || strings.Contains(line, "[ops]") {
			shown = append(shown, line)
		}
	}

	assert.Equal(t, []string{
		"  posted   " + first + " (" + where + ", 1 of 2 in one post)",
		"      from dispatcher 11-12 14:02 · accepted 11-12 14:03 socket",
		"  posted   " + second + " (" + where + ", 2 of 2 in one post)",
		"      from ops 11-12 14:02 · accepted 11-12 14:03 socket",
		"    " + third + " (tmux refused, " + now.Add(-time.Hour).Local().Format("01-02 15:04") + ", 1h ago, 1 of 2 in one post)",
		"    " + first + " (tmux refused, " + now.Add(-time.Hour).Local().Format("01-02 15:04") + ", 1h ago, 2 of 2 in one post)",
	}, shown)
	assert.NotContains(t, out, "1. ", "the joined text is not listed as one line")
}
