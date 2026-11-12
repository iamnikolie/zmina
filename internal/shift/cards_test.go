package shift

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/fsx"
)

// cardsOn is the cards a role has on disk: what a look reads.
func cardsOn(f fix, role string) []Card {
	return ReadCards(f.s.Crew.Layout(), role)
}

// cardOn is the one card of a line, the test failing when there is not exactly one.
func cardOn(t *testing.T, f fix, role, line string) Card {
	t.Helper()

	var found []Card

	for _, c := range cardsOn(f, role) {
		if c.ID == CardID(role, line) {
			found = append(found, c)
		}
	}

	require.Len(t, found, 1, "one card for %q", line)

	return found[0]
}

// A queued line has a card from the moment it is queued: who wrote it, the task it is about where that is structure, a
// head of the line and when it was written. The card file is delivery/<role>.json in the crew's directory.
func TestEnqueueWritesACardWithFromAndTask(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	line := "[dev] look at T-1 " + strings.Repeat("and then some more words ", 20)

	require.NoError(t, f.s.QueueFrom("designer", line, Facts{From: "dev", Task: "T-1"}))
	require.NoError(t, f.s.Queue("designer", "from the hand that ran the command"))
	f.s.FlushCards()

	path := f.s.Crew.Layout().DeliveryCards("designer")
	assert.Equal(t, filepath.Join(f.s.Crew.Layout().Dir, "delivery", "designer.json"), path)
	assert.FileExists(t, path)

	c := cardOn(t, f, "designer", line)
	assert.Equal(t, "dev", c.From)
	assert.Equal(t, "T-1", c.Task)
	assert.Equal(t, *f.now, c.Written)
	assert.LessOrEqual(t, len(c.Head), cardHeadMax)
	assert.True(t, strings.HasPrefix(c.Head, "[dev] look at T-1 and then some"), c.Head)
	assert.Nil(t, c.Accepted)
	assert.Nil(t, c.Closed)

	// Who ran the command is the sender of a line its caller says nothing of; no task is read from its words.
	own := cardOn(t, f, "designer", "from the hand that ran the command")
	assert.Equal(t, "owner", own.From)
	assert.Empty(t, own.Task)

	// The facts are a file of their own: not a field of the state.
	raw, err := os.ReadFile(f.s.Crew.Layout().DeliveryCards("designer"))
	require.NoError(t, err)
	assert.NotContains(t, string(stateJSON(t, f.s)), "written")
	assert.Contains(t, string(raw), `"written"`)
}

// A card is written once a pass, when something changed: queueing twice before a flush is one write, and a pass that
// changes nothing writes nothing.
func TestCardsAreWrittenOncePerFlushAndOnlyWhenChanged(t *testing.T) {
	f := fixtureWith(t, crewYAML)

	var writes []string

	fsx.BeforeCommit = func(_, path string) { writes = append(writes, filepath.Base(path)) }
	t.Cleanup(func() { fsx.BeforeCommit = nil })

	require.NoError(t, f.s.Queue("designer", "one"))
	require.NoError(t, f.s.Queue("designer", "two"))
	assert.Empty(t, writes, "kept in memory until the pass ends")

	f.s.FlushCards()
	assert.Equal(t, []string{"designer.json"}, writes, "one write for both lines")
	assert.Len(t, cardsOn(f, "designer"), 2)

	f.s.FlushCards()
	assert.Len(t, writes, 1, "nothing changed: nothing written")

	f.s.cardAccept("designer", "one", AcceptedTyped, "", *f.now)
	f.s.cardAccept("designer", "two", AcceptedTyped, "", *f.now)
	f.s.FlushCards()
	assert.Len(t, writes, 2, "two facts, one write")
}

// A line typed into a role's input line is accepted as typed, at the moment it was typed; once the role's status moves
// after it, it has landed, and a typed line shows no seen: its card is closed as landed.
func TestTypedLineIsAcceptedThenLandedClosesItsCard(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer")
	require.NoError(t, f.s.Queue("designer", "[dev] please look"))

	f.s.deliverInbox("designer", *f.now)

	c := cardOn(t, f, "designer", "[dev] please look")
	require.NotNil(t, c.Accepted)
	assert.Equal(t, AcceptedTyped, c.Accepted.How)
	assert.Equal(t, *f.now, c.Accepted.At)
	assert.Equal(t, "sid-designer", c.Accepted.Session)
	assert.Nil(t, c.Seen)
	assert.Nil(t, c.Closed, "typed and not landed yet: still in play")

	*f.now = f.now.Add(time.Second)
	assert.True(t, f.s.landed("designer", *f.now))
	f.s.FlushCards()

	c = cardOn(t, f, "designer", "[dev] please look")
	require.NotNil(t, c.Closed)
	assert.Equal(t, ClosedLanded, c.Closed.How)
	assert.Equal(t, *f.now, c.Closed.At)
	assert.Nil(t, c.Seen, "a typed line shows no seen")
}

// A line posted to a role's inbox socket is accepted as socket, with the conversation it went to, and seen in the
// transcript once the conversation shows it.
func TestPostedLineIsAcceptedThenSeenInTheTranscript(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	require.NoError(t, f.s.Queue("designer", "[dev] look at T-1"))

	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	c := cardOn(t, f, "designer", "[dev] look at T-1")
	require.NotNil(t, c.Accepted)
	assert.Equal(t, AcceptedSocket, c.Accepted.How)
	assert.Equal(t, "sid-designer", c.Accepted.Session)
	assert.Nil(t, c.Seen, "posted is not seen")

	*f.now = f.now.Add(time.Minute)
	p.showAll()
	f.s.followPosted("designer", *f.now)
	f.s.FlushCards()

	c = cardOn(t, f, "designer", "[dev] look at T-1")
	require.NotNil(t, c.Seen)
	assert.Equal(t, SeenTranscript, c.Seen.By)
	assert.Equal(t, *f.now, c.Seen.At)
	assert.Equal(t, AcceptedSocket, c.Accepted.How, "accepted stays")
}

// A Codex bridge's inbox: the file renamed in is the acceptance, the taken mark is the seen.
func TestCodexLineIsAcceptedInItsInboxAndSeenWhenTaken(t *testing.T) {
	f, dir := codexFixture(t)
	bridge(t, f, dir, "thread-1")
	require.NoError(t, f.s.Queue("dev", "[dispatcher] look at T-1"))

	require.NotEmpty(t, f.s.deliverInbox("dev", *f.now))

	c := cardOn(t, f, "dev", "[dispatcher] look at T-1")
	require.NotNil(t, c.Accepted)
	assert.Equal(t, AcceptedCodexInbox, c.Accepted.How)
	assert.Nil(t, c.Seen, "the file waits untaken")

	files := waitingFiles(t, dir)
	require.Len(t, files, 1)
	take(t, dir, files[0])

	*f.now = f.now.Add(time.Minute)
	f.s.deliverInbox("dev", *f.now)

	c = cardOn(t, f, "dev", "[dispatcher] look at T-1")
	require.NotNil(t, c.Seen)
	assert.Equal(t, SeenTaken, c.Seen.By)
}

// A typing that broke where nothing says it did not reach the pane is not tried again: its card says it needs attention.
func TestUnclearTypingIsAttentionOnTheCard(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer")
	require.NoError(t, f.s.Queue("designer", "[zmina T-1] read the packet"))
	f.tm.typeErr = errors.New("tmux: send-keys timed out")

	f.s.deliverInbox("designer", *f.now)

	c := cardOn(t, f, "designer", "[zmina T-1] read the packet")
	require.NotNil(t, c.Attention)
	assert.True(t, strings.HasPrefix(c.Attention.Why, UnclearTyping), c.Attention.Why)
	assert.Equal(t, *f.now, c.Attention.At)
	assert.Nil(t, c.Closed, "kept for a look, not closed")
	assert.Nil(t, c.Accepted, "nothing says the role took it")
}

// A line that failed with nothing reaching the role is closed as failed, and one dropped by hand as dropped; a posted
// line its reader acked is acked.
func TestACardIsClosedWhereItsLineLeaves(t *testing.T) {
	t.Run("failed after its tries", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")
		require.NoError(t, f.s.Queue("designer", "one"))
		f.tm.typeErr = errNotTyped("tmux: no server")

		for range typeTries {
			f.s.deliverInbox("designer", *f.now)
			*f.now = f.now.Add(countedGap)
		}

		c := cardOn(t, f, "designer", "one")
		require.NotNil(t, c.Closed)
		assert.Equal(t, ClosedFailed, c.Closed.How)
		assert.Nil(t, c.Attention)
	})

	t.Run("dropped", func(t *testing.T) {
		f := fixtureWith(t, crewYAML)
		require.NoError(t, f.s.Queue("designer", "one"))

		_, err := f.s.Drop("designer", true)
		require.NoError(t, err)
		f.s.FlushCards()

		c := cardOn(t, f, "designer", "one")
		require.NotNil(t, c.Closed)
		assert.Equal(t, ClosedDropped, c.Closed.How)
	})

	t.Run("a failed line, dropped or acked later", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")
		require.NoError(t, f.s.Queue("designer", "one"))
		f.tm.typeErr = errNotTyped("tmux: no server")

		for range typeTries {
			f.s.deliverInbox("designer", *f.now)
			*f.now = f.now.Add(countedGap)
		}

		require.Equal(t, ClosedFailed, cardOn(t, f, "designer", "one").Closed.How)

		_, err := f.s.Drop("designer", false)
		require.NoError(t, err)
		f.s.FlushCards()

		c := cardOn(t, f, "designer", "one")
		assert.Equal(t, ClosedDropped, c.Closed.How, "the end of its failed record is the line's")
	})

	t.Run("acked", func(t *testing.T) {
		f, _ := nativeFixture(t, crewYAML)
		upNative(t, f, "designer")

		line, id := postedTo(t, f, "designer", "[dev] look at T-1")

		_, warn := f.s.AckLines([]string{id}, false, "designer", "designer")
		require.Empty(t, warn)
		f.s.FlushCards()

		c := cardOn(t, f, "designer", line)
		require.NotNil(t, c.Closed)
		assert.Equal(t, ClosedAcked, c.Closed.How)
	})

	t.Run("aged out", func(t *testing.T) {
		f := fixtureWith(t, crewYAML)
		require.NoError(t, f.s.Up("designer"))
		rs := f.s.State.Role("designer")
		require.NoError(t, f.s.Queue("designer", "one"))

		// It sat on the failed list as one that needs a look, a week and more.
		rs.Inbox = nil
		f.s.addFailed("designer", Failed{Line: "one", Error: UnclearEnded, At: *f.now})
		require.Len(t, rs.Failed, 1)

		*f.now = f.now.Add(failedMaxAge + time.Hour)
		f.s.sweepDelivery(*f.now, map[string]bool{})
		f.s.FlushCards()

		assert.Empty(t, rs.Failed)

		c := cardOn(t, f, "designer", "one")
		require.NotNil(t, c.Attention)
		require.NotNil(t, c.Closed)
		assert.Equal(t, ClosedDropped, c.Closed.How)
		assert.Contains(t, c.Closed.Why, "aged out")
	})
}

// A line given back to the queue keeps the card it had and says it was given back; it does not get a second card. The
// new delivery has its own accepted.
func TestGivenBackLineKeepsItsCard(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	line, _ := postedTo(t, f, "designer", "[dev] look at T-1")
	before := cardOn(t, f, "designer", line)
	require.NotNil(t, before.Accepted)

	require.NoError(t, f.s.Down("designer", DownOptions{}))

	for range 3 {
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
	}

	require.Equal(t, []string{line}, f.s.State.Role("designer").Inbox, "given back to its queue")

	c := cardOn(t, f, "designer", line)
	assert.Len(t, cardsOn(f, "designer"), 1, "no second card")
	assert.Equal(t, before.Written, c.Written, "the card it had")
	require.NotNil(t, c.Back)
	assert.Equal(t, 1, c.Back.N)
	assert.Contains(t, c.Back.Why, "conversation ended")
	assert.Nil(t, c.Accepted, "the new delivery has its own")

	// Started again, it goes again: the same card, accepted anew, given back once.
	require.NoError(t, f.s.Up("designer"))
	restarted(f, "designer", "sid-designer-2")

	for range 3 {
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
	}

	assert.Len(t, cardsOn(f, "designer"), 1)

	c = cardOn(t, f, "designer", line)
	require.NotNil(t, c.Accepted)
	assert.Equal(t, "sid-designer-2", c.Accepted.Session)
	assert.Equal(t, 1, c.Back.N)
}

// queue retry puts a failed line back as a give-back: the card it had is kept and says so, not a new one.
func TestRetriedLineKeepsItsCard(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer")
	require.NoError(t, f.s.Queue("designer", "one"))
	f.tm.typeErr = errNotTyped("tmux: no server")

	for range typeTries {
		f.s.deliverInbox("designer", *f.now)
		*f.now = f.now.Add(countedGap)
	}

	require.Len(t, f.s.State.Role("designer").Failed, 1)

	n, _, err := f.s.Retry("designer", false)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	f.s.FlushCards()

	assert.Len(t, cardsOn(f, "designer"), 1)

	c := cardOn(t, f, "designer", "one")
	assert.Nil(t, c.Closed, "in the queue again")
	require.NotNil(t, c.Back)
	assert.Equal(t, "queue retry", c.Back.Why)
}

// A post to the dispatcher of signals and notes gets one card for the composed line, not one for each signal; it is
// accepted as posted and seen when the conversation shows it.
func TestDispatcherLineHasOneCard(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	_, err := f.s.Note("dev", "T-5 result ready", false)
	require.NoError(t, err)
	_, err = f.s.Note("dev", "T-6 result ready", false)
	require.NoError(t, err)

	line := f.s.deliver(*f.now)
	require.NotEmpty(t, line)

	cards := cardsOn(f, "dispatcher")
	require.Len(t, cards, 1, "the composed line, not each signal")
	assert.Equal(t, CardID("dispatcher", line), cards[0].ID)
	assert.Equal(t, "zmina", cards[0].From)
	require.NotNil(t, cards[0].Accepted)
	assert.Equal(t, AcceptedSocket, cards[0].Accepted.How)

	*f.now = f.now.Add(time.Minute)
	p.showAll()
	f.s.followPosted("dispatcher", *f.now)
	f.s.FlushCards()

	cards = cardsOn(f, "dispatcher")
	require.Len(t, cards, 1)
	require.NotNil(t, cards[0].Seen)
	assert.Equal(t, SeenTranscript, cards[0].Seen.By)
}

// A queued line of the dispatcher's own is carded when it is queued, and goes in a post with the signals: its card is
// accepted and seen, and the composed line has its own.
func TestDispatcherOwnQueuedLineKeepsItsOwnCard(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	require.NoError(t, f.s.QueueFrom("dispatcher", "[owner] a word", Facts{From: "owner"}))

	_, err := f.s.Note("dev", "T-5 result ready", false)
	require.NoError(t, err)

	require.NotEmpty(t, f.s.deliver(*f.now))
	*f.now = f.now.Add(time.Minute)
	p.showAll()
	f.s.followPosted("dispatcher", *f.now)
	f.s.FlushCards()

	own := cardOn(t, f, "dispatcher", "[owner] a word")
	assert.Equal(t, "owner", own.From)
	require.NotNil(t, own.Accepted)
	require.NotNil(t, own.Seen)
	assert.Len(t, cardsOn(f, "dispatcher"), 2, "its own line and the composed one")
}

// A card file that cannot be used never stops a delivery: the line goes, and the failure is told once as an event. A
// corrupt file is moved aside the way state.json is and started anew.
func TestCardFileThatCannotBeUsedNeverBlocksDelivery(t *testing.T) {
	t.Run("unreadable", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")
		path := f.s.Crew.Layout().DeliveryCards("designer")
		require.NoError(t, os.MkdirAll(path, 0o700), "a directory where the file belongs")

		require.NoError(t, f.s.Queue("designer", "one"))
		require.NoError(t, f.s.Queue("designer", "two"))

		assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
		assert.Equal(t, []string{"one"}, f.tm.typed["designer"], "delivered all the same")
		assert.Equal(t, 1, f.eventCount("cards-failed"))

		*f.now = f.now.Add(time.Minute)
		f.s.landed("designer", *f.now)
		f.s.deliverInbox("designer", *f.now)
		assert.Equal(t, []string{"one", "two"}, f.tm.typed["designer"])
		assert.Equal(t, 1, f.eventCount("cards-failed"), "told once")
	})

	t.Run("not writable", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")
		require.NoError(t, os.WriteFile(f.s.Crew.Layout().Delivery(), []byte("a file where the directory belongs"), 0o600))

		require.NoError(t, f.s.Queue("designer", "one"))
		assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
		assert.Equal(t, []string{"one"}, f.tm.typed["designer"])
		assert.Equal(t, 1, f.eventCount("cards-failed"))
	})

	t.Run("corrupt", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")
		path := f.s.Crew.Layout().DeliveryCards("designer")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"cards":[{"id":`), 0o600))

		require.NoError(t, f.s.Queue("designer", "one"))
		assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
		assert.Equal(t, []string{"one"}, f.tm.typed["designer"])

		aside, err := filepath.Glob(path + ".broken-*")
		require.NoError(t, err)
		require.Len(t, aside, 1, "moved aside, as state.json is")
		assert.Equal(t, path+".broken-"+f.now.Format("20060102-150405"), aside[0])

		assert.Len(t, cardsOn(f, "designer"), 1, "started anew")
		assert.Equal(t, 1, f.eventCount("cards-failed"))
	})

	t.Run("from a newer zmina", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")
		path := f.s.Crew.Layout().DeliveryCards("designer")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))

		newer := []byte(`{"version":99,"cards":[]}`)
		require.NoError(t, os.WriteFile(path, newer, 0o600))

		require.NoError(t, f.s.Queue("designer", "one"))
		assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, newer, got, "left as the newer zmina wrote it")
	})
}

// A card finished more than a week ago goes the next time the file is written; and the file never holds more than the
// cap, the finished cards going first.
func TestCardsArePrunedByAgeAndCap(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	t.Run("by age", func(t *testing.T) {
		cards := []Card{
			{ID: "old closed", Closed: &CardClosed{At: now.Add(-cardKeep - time.Hour), How: ClosedLanded}},
			{ID: "old seen", Seen: &CardSeen{At: now.Add(-cardKeep - time.Hour), By: SeenTranscript}},
			{ID: "recent closed", Closed: &CardClosed{At: now.Add(-cardKeep + time.Hour), How: ClosedLanded}},
			{ID: "old but waiting", Written: now.Add(-30 * 24 * time.Hour)},
		}

		got := pruneCards(cards, now)

		var ids []string
		for _, c := range got {
			ids = append(ids, c.ID)
		}

		assert.Equal(t, []string{"recent closed", "old but waiting"}, ids)
	})

	t.Run("by cap", func(t *testing.T) {
		var cards []Card

		for i := range cardsMax + 40 {
			c := Card{ID: "c" + string(rune('a'+i%26)), Head: string(rune('0' + i%10))}
			if i%2 == 0 {
				c.Closed = &CardClosed{At: now, How: ClosedLanded}
			}

			cards = append(cards, c)
		}

		waiting := 0

		for _, c := range cards {
			if c.Closed == nil {
				waiting++
			}
		}

		got := pruneCards(cards, now)
		assert.Len(t, got, cardsMax)

		kept := 0

		for _, c := range got {
			if c.Closed == nil {
				kept++
			}
		}

		assert.Equal(t, waiting, kept, "the cards still in play stay while finished ones go")
	})

	t.Run("a write prunes", func(t *testing.T) {
		f := fixtureWith(t, crewYAML)
		path := f.s.Crew.Layout().DeliveryCards("designer")
		require.NoError(t, writeCardFile(f.s.Crew.Layout(), path, []Card{
			{ID: "gone", Closed: &CardClosed{At: f.now.Add(-8 * 24 * time.Hour), How: ClosedLanded}},
		}))

		require.NoError(t, f.s.Queue("designer", "one"))
		f.s.FlushCards()

		cards := cardsOn(f, "designer")
		require.Len(t, cards, 1)
		assert.Equal(t, CardID("designer", "one"), cards[0].ID)
	})

	t.Run("the retention pass", func(t *testing.T) {
		f := fixtureWith(t, crewYAML)
		l := f.s.Crew.Layout()
		require.NoError(t, writeCardFile(l, l.DeliveryCards("designer"), []Card{
			{ID: "gone", Closed: &CardClosed{At: f.now.Add(-8 * 24 * time.Hour), How: ClosedLanded}},
			{ID: "kept", Written: *f.now},
		}))

		n, err := PruneCards(l, *f.now, true)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		assert.Len(t, cardsOn(f, "designer"), 2, "a dry run writes nothing")

		n, err = PruneCards(l, *f.now, false)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		assert.Equal(t, "kept", cardsOn(f, "designer")[0].ID)
	})
}

// The cards are never read to decide a delivery: the same queue is delivered the same way with the cards gone, and the
// shift's clock, not the wall's, stamps them.
func TestDeliveryIsTheSameWithoutCards(t *testing.T) {
	run := func(t *testing.T, wipe bool) []string {
		f := typedFixture(t, crewYAML, "designer")
		require.NoError(t, f.s.Queue("designer", "one"))
		require.NoError(t, f.s.Queue("designer", "two"))

		if wipe {
			require.NoError(t, os.RemoveAll(f.s.Crew.Layout().Delivery()))
			f.s.cards = nil
		}

		for range 2 {
			f.s.deliverInbox("designer", *f.now)
			*f.now = f.now.Add(time.Minute)
			f.s.landed("designer", *f.now)
		}

		return f.tm.typed["designer"]
	}

	assert.Equal(t, run(t, false), run(t, true))
}

// Every line that goes into a role's queue goes through enqueue, so that each has its card: the source is walked, and a
// line appended to a queue anywhere else fails here.
func TestInboxIsOnlyAppendedByEnqueue(t *testing.T) {
	appends := regexp.MustCompile(`\.Inbox\s*=\s*(append\(|slices\.(Insert|Concat)\()`)

	// The guard sees what it is for.
	for _, bad := range []string{
		"rs.Inbox = append(rs.Inbox, line)",
		"s.State.Role(name).Inbox = append([]string{line}, rs.Inbox...)",
		"rs.Inbox = slices.Concat(lines, rs.Inbox)",
		"rs.Inbox = slices.Insert(rs.Inbox, 0, line)",
	} {
		assert.True(t, appends.MatchString(bad), bad)
	}

	root := filepath.Join("..", "..")

	var offenders []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}

		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			filepath.Base(path) == "enqueue.go" && filepath.Base(filepath.Dir(path)) == "shift" {
			return nil
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		for i, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "//") && appends.MatchString(line) {
				offenders = append(offenders, filepath.ToSlash(path)+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}

		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, offenders, "queue a line with Shift.enqueue (enqueueHead, giveBackHead), so that it has its card")
}

// The one-shot pass and the courier's pass (DeliverPass, DeliverRoles) write the cards of what they delivered, as the
// beat does: they hold the same lock and nothing after them would.
func TestDeliveryPassesWriteTheirCards(t *testing.T) {
	t.Run("DeliverPass", func(t *testing.T) {
		f, _ := nativeFixture(t, crewYAML)
		upNative(t, f, "designer")
		require.NoError(t, f.s.QueueFrom("designer", "[dev] look at T-1", Facts{From: "dev", Task: "T-1"}))

		changed, err := f.s.DeliverPass()
		require.NoError(t, err)
		require.True(t, changed)

		c := cardOn(t, f, "designer", "[dev] look at T-1")
		assert.Equal(t, "dev", c.From)
		require.NotNil(t, c.Accepted, "written by the pass itself, without a flush of the caller's")
		assert.Equal(t, AcceptedSocket, c.Accepted.How)
	})

	t.Run("DeliverRoles", func(t *testing.T) {
		f, _ := nativeFixture(t, crewYAML)
		upNative(t, f, "designer")
		require.NoError(t, f.s.Queue("designer", "[dev] look at T-2"))

		_, err := f.s.DeliverRoles([]string{"designer"})
		require.NoError(t, err)

		c := cardOn(t, f, "designer", "[dev] look at T-2")
		require.NotNil(t, c.Accepted)
	})

	t.Run("a copy of the shift, as the courier makes for each pass", func(t *testing.T) {
		f, _ := nativeFixture(t, crewYAML)
		upNative(t, f, "designer")
		require.NoError(t, f.s.Queue("designer", "[dev] look at T-3"))
		f.s.FlushCards()
		f.s.cards = nil // the courier's own shift never queues or delivers: it only makes copies

		pass := *f.s
		_, err := pass.DeliverRoles([]string{"designer"})
		require.NoError(t, err)
		assert.Nil(t, f.s.cards, "what a pass loaded is the pass's own")

		c := cardOn(t, f, "designer", "[dev] look at T-3")
		require.NotNil(t, c.Accepted)
	})
}
