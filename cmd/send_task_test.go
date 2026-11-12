package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

// cardWith is the newest card of a role whose head holds the words, nil when there is none.
func cardWith(cards []shift.Card, words string) *shift.Card {
	for i := len(cards) - 1; i >= 0; i-- {
		if strings.Contains(cards[i].Head, words) {
			return &cards[i]
		}
	}

	return nil
}

// zmina send --task says what a line is about, by structure: the card of the line keeps the task and the role that ran the
// command as the sender. The task has to exist, with the flag or without --when-idle, and a number names a task as it does
// in every other command.
func TestSendTaskIsKeptOnTheCardAndMustExist(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, file, "task", "new", "dev", "Review the change")
	require.NoError(t, err)

	c, err := crew.Load(file)
	require.NoError(t, err)

	t.Setenv("ZMINA_ROLE", "ops")

	// A task that is not there: nothing is queued, and the line has no card.
	for _, args := range [][]string{
		{"send", "dev", "--when-idle", "--task", "T-9999", "look at the build"},
		{"send", "dev", "--task", "T-9999", "look at the build"},
		{"send", "dev", "--task", "nonsense", "look at the build"},
	} {
		_, _, err = zm(t, file, args...)
		require.ErrorContains(t, err, "no task", args)
	}

	assert.Nil(t, cardWith(shift.ReadCards(c.Layout(), "dev"), "look at the build"))

	// Queued with a task that exists: the card has the task, and ops as the sender.
	_, _, err = zm(t, file, "send", "dev", "--when-idle", "--task", "T-1", "look at the build")
	require.NoError(t, err)

	card := cardWith(shift.ReadCards(c.Layout(), "dev"), "look at the build")
	require.NotNil(t, card)
	assert.Equal(t, "ops", card.From)
	assert.Equal(t, "T-1", card.Task)

	// A bare number names the task, and what is kept is its id.
	_, _, err = zm(t, file, "send", "dev", "--when-idle", "--task", "1", "and the tests")
	require.NoError(t, err)

	card = cardWith(shift.ReadCards(c.Layout(), "dev"), "and the tests")
	require.NotNil(t, card)
	assert.Equal(t, "T-1", card.Task)

	// Without a task there is none: it is never read from the words.
	_, _, err = zm(t, file, "send", "dev", "--when-idle", "about T-1 in words only")
	require.NoError(t, err)

	card = cardWith(shift.ReadCards(c.Layout(), "dev"), "about T-1 in words only")
	require.NotNil(t, card)
	assert.Empty(t, card.Task)

	// Without --when-idle the line goes at once or not at all: a role that is down is told so, after the task was checked.
	_, _, err = zm(t, file, "send", "dev", "--task", "T-1", "now, please")
	require.ErrorContains(t, err, "not up")

	// The flag is described where the command is.
	out, _, err := zm(t, file, "send", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "--task string")
	assert.Contains(t, out, "[written")
}

// queue ls says on the facts line of a line that was delivered with a mark that it crossed, and how; --json carries it.
func TestQueueLsShowsACrossedLine(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	c, err := crew.Load(file)
	require.NoError(t, err)

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)
	l.Role("dev").Posted = []shift.Posted{
		{Line: "[ops] first", MsgID: "m-1", Session: "s-1", At: time.Now().Add(-3 * time.Minute)},
		{Line: "[ops] second", MsgID: "m-2", Session: "s-1", At: time.Now().Add(-2 * time.Minute)},
	}
	require.NoError(t, l.Save())

	writeCards(t, file, "dev",
		shift.Card{ID: shift.CardID("dev", "[ops] first"), From: "ops", Task: "T-1", Written: cardAt(12, 14, 2),
			Accepted: &shift.CardAccepted{At: cardAt(12, 14, 6), How: shift.AcceptedSocket, Session: "s-1"},
			Crossed:  &shift.CardCrossed{With: "in-0123456789ab", Kind: shift.CrossNewer, At: cardAt(12, 14, 6)}},
		shift.Card{ID: shift.CardID("dev", "[ops] second"), From: "ops", Task: "T-1", Written: cardAt(12, 14, 2),
			Accepted: &shift.CardAccepted{At: cardAt(12, 14, 6), How: shift.AcceptedSocket, Session: "s-1"}},
	)

	out, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)

	var facts []string

	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "      ") {
			facts = append(facts, line)
		}
	}

	assert.Equal(t, []string{
		"      from ops 11-12 14:02 · on T-1 · accepted 11-12 14:06 socket · crossed (newer)",
		"      from ops 11-12 14:02 · on T-1 · accepted 11-12 14:06 socket",
	}, facts)

	out, _, err = zm(t, file, "queue", "ls", "--json")
	require.NoError(t, err)

	var qs []struct {
		Facts []struct {
			Crossed *struct {
				With string `json:"with"`
				Kind string `json:"kind"`
			} `json:"crossed"`
		} `json:"facts"`
	}

	require.NoError(t, json.Unmarshal([]byte(out), &qs))
	require.Len(t, qs, 1)
	require.Len(t, qs[0].Facts, 2)
	require.NotNil(t, qs[0].Facts[0].Crossed)
	assert.Equal(t, "newer", qs[0].Facts[0].Crossed.Kind)
	assert.Equal(t, "in-0123456789ab", qs[0].Facts[0].Crossed.With)
	assert.Nil(t, qs[0].Facts[1].Crossed)
}
