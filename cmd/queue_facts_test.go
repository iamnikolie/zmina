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

// lastCard is the newest card dev has for a line, nil when it has none.
func lastCard(cards []shift.Card, line string) *shift.Card {
	of := shift.CardsOf(cards, "dev", line)
	if len(of) == 0 {
		return nil
	}

	return &of[len(of)-1]
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
	failedAt := now.Add(-time.Hour)
	rs.Failed = []shift.Failed{
		{Line: "[dispatcher] maybe there", Error: shift.UnclearTyping + " (tmux: timed out)", At: failedAt},
		{Line: "[dispatcher] refused", Error: "tmux refused", At: failedAt},
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
			k.Closed = &shift.CardClosed{At: failedAt, How: shift.ClosedFailed}
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
		"      from dev · closed failed " + failedAt.Local().Format("01-02 15:04"),
	}, shown)
}

// --json carries the same facts, the cards of the rows shown, apart from the lines.
func TestQueueLsJSONCarriesTheFacts(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	c, err := crew.Load(file)
	require.NoError(t, err)

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)
	rs := l.Role("dev")
	rs.Inbox = []string{"[dispatcher] waiting"}
	rs.Posted = []shift.Posted{{Line: "[dispatcher] posted", MsgID: "m-1", Session: "s-1", At: time.Now()}}
	require.NoError(t, l.Save())

	writeCards(t, file, "dev",
		shift.Card{ID: shift.CardID("dev", "[dispatcher] waiting"), From: "dispatcher", Task: "T-1", Written: cardAt(12, 14, 2)},
		shift.Card{ID: shift.CardID("dev", "[dispatcher] posted"), From: "owner", Written: cardAt(12, 14, 2),
			Accepted: &shift.CardAccepted{At: cardAt(12, 14, 3), How: shift.AcceptedSocket, Session: "s-1"}},
		// Of an earlier delivery of the same words, seen: no row's.
		shift.Card{ID: shift.CardID("dev", "[dispatcher] posted"), From: "zmina", Written: cardAt(11, 9, 0),
			Seen: &shift.CardSeen{At: cardAt(11, 9, 1), By: shift.SeenTranscript}})

	out, _, err := zm(t, file, "queue", "ls", "--json")
	require.NoError(t, err)

	var qs []struct {
		Role  string `json:"role"`
		Facts []struct {
			ID       string `json:"id"`
			From     string `json:"from"`
			Task     string `json:"task"`
			Accepted *struct {
				How     string `json:"how"`
				Session string `json:"session"`
			} `json:"accepted"`
		} `json:"facts"`
	}

	require.NoError(t, json.Unmarshal([]byte(out), &qs))
	require.Len(t, qs, 1)
	require.Len(t, qs[0].Facts, 2, "the cards of the rows, not the finished one of an earlier delivery")
	assert.Equal(t, shift.CardID("dev", "[dispatcher] waiting"), qs[0].Facts[0].ID)
	assert.Equal(t, "dispatcher", qs[0].Facts[0].From)
	assert.Equal(t, "T-1", qs[0].Facts[0].Task)
	assert.Nil(t, qs[0].Facts[0].Accepted)
	assert.Equal(t, "owner", qs[0].Facts[1].From)
	require.NotNil(t, qs[0].Facts[1].Accepted)
	assert.Equal(t, "socket", qs[0].Facts[1].Accepted.How)
	assert.Equal(t, "s-1", qs[0].Facts[1].Accepted.Session)
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
	assert.Contains(t, out, "delivery cards (1 cards or set-aside files past their keep or over the cap)")
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

	refused := lastCard(cards, "refused")
	require.NotNil(t, refused)
	assert.Nil(t, refused.Closed, "in the queue again")
	require.NotNil(t, refused.Back)
	assert.Equal(t, "queue retry", refused.Back.Why)
	assert.Equal(t, "dispatcher", refused.From)

	_, _, err = zm(t, file, "queue", "drop", "dev", "--all")
	require.NoError(t, err)

	refused = lastCard(shift.ReadCards(c.Layout(), "dev"), "refused")
	require.NotNil(t, refused)
	require.NotNil(t, refused.Closed)
	assert.Equal(t, shift.ClosedDropped, refused.Closed.How)

	maybe := lastCard(shift.ReadCards(c.Layout(), "dev"), "maybe there")
	require.NotNil(t, maybe)
	require.NotNil(t, maybe.Attention, "the look it needed is still on the card")
	require.NotNil(t, maybe.Closed)
	assert.Equal(t, shift.ClosedDropped, maybe.Closed.How)
}

// Lines of the same words have a card each, and queue ls puts under each row the card that is its own: the typed row the
// one that was accepted, the waiting row the other, never the first's facts twice; and a waiting line with no card of its
// own shows nothing, not an older line's.
func TestQueueLsShowsEachIdenticalRowItsOwnFacts(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	now := time.Now()

	c, err := crew.Load(file)
	require.NoError(t, err)

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)

	rs := l.Role("dev")
	rs.Sent = &shift.Sent{Line: "same words", At: now.Add(-time.Minute)}
	rs.Inbox = []string{"same words", "same words"}
	require.NoError(t, l.Save())

	id := shift.CardID("dev", "same words")

	writeCards(t, file, "dev",
		shift.Card{ID: id, From: "dispatcher", Written: cardAt(12, 14, 0),
			Accepted: &shift.CardAccepted{At: cardAt(12, 14, 1), How: shift.AcceptedTyped}},
		shift.Card{ID: id, From: "owner", Written: cardAt(12, 14, 5)},
		// Of an earlier delivery, finished: no row's.
		shift.Card{ID: id, From: "zmina", Written: cardAt(11, 9, 0), Seen: &shift.CardSeen{At: cardAt(11, 9, 1), By: shift.SeenTranscript}})

	out, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)

	var shown []string

	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "      ") || strings.Contains(line, "same words") {
			shown = append(shown, line)
		}
	}

	assert.Equal(t, []string{
		"  inbox    same words",
		"      from owner 11-12 14:05",
		"  inbox    same words",
		"  typed    same words (typed into its prompt " + now.Add(-time.Minute).Local().Format("01-02 15:04") + ", 1m ago, not landed yet: it may have it)",
		"      from dispatcher 11-12 14:00 · accepted 11-12 14:01 typed",
	}, shown, "the second waiting row has no card of its own")
}

// The pass also removes the card files moved aside as corrupt once they are over a week old, and the card file of a role
// that is no longer in the crew once every card in it is past its keep; a copy of a role in the crew (dev#2) is in it.
func TestRetentionRemovesAsideCardFilesAndFilesOfRolesGone(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	now := time.Now()

	c, err := crew.Load(file)
	require.NoError(t, err)

	old := now.Add(-9 * 24 * time.Hour)
	closed := shift.Card{ID: "in-000000000001", Closed: &shift.CardClosed{At: old, How: shift.ClosedLanded}}

	writeCards(t, file, "ghost", closed)
	writeCards(t, file, "dev#2", shift.Card{ID: "in-000000000002", Written: old})
	writeCards(t, file, "dev", shift.Card{ID: "in-000000000003", Written: now})

	dir := c.Layout().Delivery()
	aside := filepath.Join(dir, "dev.json.broken-"+old.Format("20060102-150405"))
	fresh := filepath.Join(dir, "dev.json.broken-"+now.Format("20060102-150405"))
	require.NoError(t, os.WriteFile(aside, []byte("{"), 0o600))
	require.NoError(t, os.WriteFile(fresh, []byte("{"), 0o600))

	out, _, err := zm(t, file, "retention", "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "delivery cards (2 cards or set-aside files past their keep or over the cap)")
	assert.FileExists(t, aside, "a dry run removes nothing")

	_, _, err = zm(t, file, "retention")
	require.NoError(t, err)

	assert.NoFileExists(t, aside)
	assert.FileExists(t, fresh)
	assert.NoFileExists(t, c.Layout().DeliveryCards("ghost"), "a role gone, every card past its keep")
	assert.FileExists(t, c.Layout().DeliveryCards("dev#2"), "a copy of a role in the crew")
	assert.FileExists(t, c.Layout().DeliveryCards("dev"))
}
