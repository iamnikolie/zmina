package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

// writeCards puts a role's card file where zmina keeps it, as an earlier pass wrote it.
func writeCards(t *testing.T, file, role string, cards ...shift.Card) {
	t.Helper()

	c, err := crew.Load(file)
	require.NoError(t, err)

	raw, err := json.Marshal(map[string]any{"version": 1, "cards": cards})
	require.NoError(t, err)

	path := c.Layout().DeliveryCards(role)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, raw, 0o600))
}

// cardAt is a moment of the day as queue ls shows it in the machine's zone.
func cardAt(day, hour, minute int) time.Time {
	return time.Date(2026, 11, day, hour, minute, 0, 0, time.Local)
}

// A command that queues a line leaves its card, written with the state under the lock it holds, and queue ls shows who
// wrote the line and when.
func TestSendWhenIdleWritesACardAndQueueLsShowsIt(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, file, "send", "dev", "--when-idle", "look at the build")
	require.NoError(t, err)

	c, err := crew.Load(file)
	require.NoError(t, err)

	cards := shift.ReadCards(c.Layout(), "dev")
	require.Len(t, cards, 1)
	assert.Equal(t, "owner", cards[0].From, "the hand at a shell is the owner")
	assert.False(t, cards[0].Written.IsZero())
	assert.Contains(t, cards[0].Head, "look at the build")

	out, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^  inbox +look at the build.*\n {6}from owner \d\d-\d\d \d\d:\d\d$`, out)
}

// queue ls puts what was seen of each waiting, in-flight and failed line in one indented line under it: who wrote it
// and when, when it was accepted and how, when it was seen, when it was given back or needed a look. A line with no card
// has nothing under it.
func TestQueueLsShowsWhatWasSeenOfEachLine(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	now := time.Now()

	c, err := crew.Load(file)
	require.NoError(t, err)

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)

	rs := l.Role("dev")
	rs.Inbox = []string{"[dispatcher] waiting", "[dispatcher] no card"}
	rs.Sent = &shift.Sent{Line: "[dispatcher] typed", At: now.Add(-2 * time.Minute)}
	rs.Posted = []shift.Posted{{Line: "[dispatcher] posted", MsgID: "m-1", Session: "s-1", At: now.Add(-3 * time.Minute)}}
	rs.Failed = []shift.Failed{
		{Line: "[dispatcher] maybe there", Error: shift.UnclearTyping + " (tmux: timed out)", At: now.Add(-time.Hour)},
		{Line: "[dispatcher] refused", Error: "tmux refused", At: now.Add(-time.Hour)},
	}
	require.NoError(t, l.Save())

	card := func(line string, mod func(*shift.Card)) shift.Card {
		k := shift.Card{ID: shift.CardID("dev", line), Head: line}
		mod(&k)

		return k
	}

	writeCards(t, file, "dev",
		card("[dispatcher] waiting", func(k *shift.Card) {
			k.From, k.Task, k.Written = "dispatcher", "T-1", cardAt(12, 14, 2)
			k.Back = &shift.CardBack{At: cardAt(12, 14, 9), Why: "queue retry", N: 2}
		}),
		card("[dispatcher] typed", func(k *shift.Card) {
			k.From, k.Written = "owner", cardAt(12, 14, 2)
			k.Accepted = &shift.CardAccepted{At: cardAt(12, 14, 3), How: shift.AcceptedTyped, Session: "s-1"}
		}),
		card("[dispatcher] posted", func(k *shift.Card) {
			k.Written = cardAt(12, 14, 2)
			k.Accepted = &shift.CardAccepted{At: cardAt(12, 14, 3), How: shift.AcceptedSocket, Session: "s-1"}
		}),
		card("[dispatcher] maybe there", func(k *shift.Card) {
			k.From, k.Written = "zmina", cardAt(12, 13, 0)
			k.Attention = &shift.CardAttention{At: cardAt(12, 13, 5), Why: shift.UnclearTyping}
		}),
		card("[dispatcher] refused", func(k *shift.Card) {
			k.From = "dev"
			k.Closed = &shift.CardClosed{At: cardAt(12, 13, 5), How: shift.ClosedFailed}
		}),
	)

	out, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)

	lines := strings.Split(out, "\n")

	// The words of each line, then under it the facts; a line with no card has none, and a line with nothing to say none.
	var shown []string

	for _, l := range lines {
		if strings.HasPrefix(l, "      ") || strings.Contains(l, "[dispatcher]") {
			shown = append(shown, l)
		}
	}

	assert.Equal(t, []string{
		"  inbox    [dispatcher] waiting",
		"      from dispatcher 11-12 14:02 · on T-1 · given back 11-12 14:09 (2 times)",
		"  inbox    [dispatcher] no card",
		"  typed    [dispatcher] typed (typed into its prompt " + now.Add(-2*time.Minute).Local().Format("01-02 15:04") + ", 2m ago, not landed yet: it may have it)",
		"      from owner 11-12 14:02 · accepted 11-12 14:03 typed",
		"  posted   [dispatcher] posted (to its inbox socket " + now.Add(-3*time.Minute).Local().Format("01-02 15:04") + ", 3m ago, not yet seen)",
		"      written 11-12 14:02 · accepted 11-12 14:03 socket",
		"    [dispatcher] maybe there (" + shift.UnclearTyping + " (tmux: timed out), " + now.Add(-time.Hour).Local().Format("01-02 15:04") + ", 1h ago)",
		"      from zmina 11-12 13:00 · attention 11-12 13:05",
		"    [dispatcher] refused (tmux refused, " + now.Add(-time.Hour).Local().Format("01-02 15:04") + ", 1h ago)",
		"      from dev",
	}, shown)
}

// --json carries the same facts, the cards of the lines shown, apart from the lines.
func TestQueueLsJSONCarriesTheFacts(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	c, err := crew.Load(file)
	require.NoError(t, err)

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)
	l.Role("dev").Inbox = []string{"[dispatcher] waiting"}
	require.NoError(t, l.Save())

	writeCards(t, file, "dev", shift.Card{ID: shift.CardID("dev", "[dispatcher] waiting"), From: "dispatcher", Task: "T-1",
		Written:  cardAt(12, 14, 2),
		Accepted: &shift.CardAccepted{At: cardAt(12, 14, 3), How: shift.AcceptedSocket, Session: "s-1"},
		Seen:     &shift.CardSeen{At: cardAt(12, 14, 4), By: shift.SeenTranscript}})

	out, _, err := zm(t, file, "queue", "ls", "--json")
	require.NoError(t, err)

	var qs []struct {
		Role  string `json:"role"`
		Facts []struct {
			ID       string `json:"id"`
			From     string `json:"from"`
			Task     string `json:"task"`
			Accepted struct {
				How     string `json:"how"`
				Session string `json:"session"`
			} `json:"accepted"`
			Seen struct {
				By string `json:"by"`
			} `json:"seen"`
		} `json:"facts"`
	}

	require.NoError(t, json.Unmarshal([]byte(out), &qs))
	require.Len(t, qs, 1)
	require.Len(t, qs[0].Facts, 1)
	assert.Equal(t, shift.CardID("dev", "[dispatcher] waiting"), qs[0].Facts[0].ID)
	assert.Equal(t, "dispatcher", qs[0].Facts[0].From)
	assert.Equal(t, "T-1", qs[0].Facts[0].Task)
	assert.Equal(t, "socket", qs[0].Facts[0].Accepted.How)
	assert.Equal(t, "s-1", qs[0].Facts[0].Accepted.Session)
	assert.Equal(t, "transcript", qs[0].Facts[0].Seen.By)
}

// A look never writes: queue ls leaves the card file as it was, even a corrupt one, and still lists the lines.
func TestQueueLsNeverTouchesTheCardFile(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	c, err := crew.Load(file)
	require.NoError(t, err)

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)
	l.Role("dev").Inbox = []string{"waiting"}
	require.NoError(t, l.Save())

	path := c.Layout().DeliveryCards("dev")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"cards":[{"id":`), 0o600))

	out, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "inbox    waiting")

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, `{"version":1,"cards":[{"id":`, string(raw))

	matches, err := filepath.Glob(path + ".broken-*")
	require.NoError(t, err)
	assert.Empty(t, matches, "nothing moved aside by a look")
}

// The daily retention pass prunes the cards of a role nothing has written to lately: a card finished more than a week ago
// goes, one in play stays; a dry run lists it and writes nothing.
func TestRetentionPrunesTheDeliveryCards(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	now := time.Now()

	writeCards(t, file, "dev",
		shift.Card{ID: "in-000000000001", Closed: &shift.CardClosed{At: now.Add(-8 * 24 * time.Hour), How: shift.ClosedLanded}},
		shift.Card{ID: "in-000000000002", Written: now.Add(-time.Hour)})

	c, err := crew.Load(file)
	require.NoError(t, err)

	out, _, err := zm(t, file, "retention", "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "delivery cards (1 past their keep or over the cap)")
	assert.Len(t, shift.ReadCards(c.Layout(), "dev"), 2, "a dry run writes nothing")

	out, _, err = zm(t, file, "retention")
	require.NoError(t, err)
	assert.Contains(t, out, "delivery cards 1")

	cards := shift.ReadCards(c.Layout(), "dev")
	require.Len(t, cards, 1)
	assert.Equal(t, "in-000000000002", cards[0].ID)
}

// What a command that changes a queue does to the cards is written under the lock it holds, with the state: queue retry
// gives a failed line back (its card is kept and says so), queue drop lets one go (its card is closed).
func TestQueueRetryAndDropWriteTheCardsWithTheState(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	now := time.Now()

	c, err := crew.Load(file)
	require.NoError(t, err)

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)
	l.Role("dev").Failed = []shift.Failed{
		{Line: "refused", Error: "tmux refused", At: now},
		{Line: "maybe there", Error: shift.UnclearEnded, At: now},
	}
	require.NoError(t, l.Save())

	writeCards(t, file, "dev",
		shift.Card{ID: shift.CardID("dev", "refused"), From: "dispatcher", Written: cardAt(12, 14, 2),
			Closed: &shift.CardClosed{At: cardAt(12, 14, 5), How: shift.ClosedFailed, Why: "tmux refused"}},
		shift.Card{ID: shift.CardID("dev", "maybe there"), Written: cardAt(12, 14, 2),
			Attention: &shift.CardAttention{At: cardAt(12, 14, 5), Why: shift.UnclearEnded}})

	_, _, err = zm(t, file, "queue", "retry", "dev")
	require.NoError(t, err)

	cards := shift.ReadCards(c.Layout(), "dev")
	require.Len(t, cards, 2, "the cards it had, no new ones")

	refused := shift.CardOf(cards, "dev", "refused")
	require.NotNil(t, refused)
	assert.Nil(t, refused.Closed, "in the queue again")
	require.NotNil(t, refused.Back)
	assert.Equal(t, "queue retry", refused.Back.Why)
	assert.Equal(t, "dispatcher", refused.From)

	_, _, err = zm(t, file, "queue", "drop", "dev", "--all")
	require.NoError(t, err)

	refused = shift.CardOf(shift.ReadCards(c.Layout(), "dev"), "dev", "refused")
	require.NotNil(t, refused)
	require.NotNil(t, refused.Closed)
	assert.Equal(t, shift.ClosedDropped, refused.Closed.How)

	maybe := shift.CardOf(shift.ReadCards(c.Layout(), "dev"), "dev", "maybe there")
	require.NotNil(t, maybe)
	require.NotNil(t, maybe.Attention, "the look it needed is still on the card")
	require.NotNil(t, maybe.Closed)
	assert.Equal(t, shift.ClosedDropped, maybe.Closed.How)
}
