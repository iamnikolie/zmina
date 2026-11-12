package shift

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/chat"
	"github.com/iamnikolie/zmina/internal/inbox"
	"github.com/iamnikolie/zmina/internal/peer"
)

// bellYAML is a crew with a role on its own channel (designer) and one typed into (scribe), both with a boot line, which
// is what tells a role what a doorbell is.
const bellYAML = `namespace: /ns
claude: claude
roles:
  dispatcher:
    boot: read d and boot
  designer:
    boot: read the brief
  scribe:
    boot: read the rules
    delivery: tmux
`

// bellOf is the text of the doorbell a body of lines is rung as, restated here from the design: how many lines, who and what
// where known, the command.
func bellOf(role, body string, n int, who string) string {
	id := inbox.IDOf("bell " + role + "\x00" + body)
	lines := fmt.Sprintf("%d lines wait", n)

	if n == 1 {
		lines = "1 line waits"
	}

	return "[zmina] " + lines + " for you" + who + ": zmina inbox show " + id
}

// bodyOf is the text stored for a doorbell, the way the delivery stores it: the lines numbered, or the one line.
func bodyOf(lines ...string) string {
	if len(lines) == 1 {
		return lines[0]
	}

	return numbered(lines)
}

// queueFrom queues lines for a role as the role dev wrote them about T-1.
func queueFrom(t *testing.T, f fix, role string, lines ...string) {
	t.Helper()

	for _, l := range lines {
		require.NoError(t, f.s.QueueFrom(role, l, Facts{From: "dev", Task: "T-1"}))
	}
}

// fill is a text of exactly n bytes starting with head.
func fill(head string, n int) string { return head + strings.Repeat("x", n-len(head)) }

// What goes to a role on its own channel is whole up to bellInline bytes, as stage 3 made it, and a bell past it: the
// joined run as numbered lines, one line as it is.
func TestBellThresholdOnTheRoleChannel(t *testing.T) {
	t.Run("a joined run at the bound is a numbered post", func(t *testing.T) {
		f, p := nativeFixture(t, bellYAML)
		upNative(t, f, "designer")

		a, b := fill("a", 600), fill("b", bellInline-len("1. ")-600-len("\n2. "))
		queueFrom(t, f, "designer", a, b)

		f.s.deliverInbox("designer", *f.now)

		require.Equal(t, []string{tagged(f, "designer", "1. "+a+"\n2. "+b)}, p.raw)
		assert.Len(t, p.raw[0], len(tagMark(f.s.State.Role("designer").Tag))+1+bellInline)
		assert.Empty(t, f.s.State.Role("designer").Posted[0].IDs)
		assert.NoDirExists(t, f.s.Crew.Layout().Inbox(), "nothing stored")
	})

	t.Run("a joined run one byte over is a bell", func(t *testing.T) {
		f, p := nativeFixture(t, bellYAML)
		upNative(t, f, "designer")

		a, b := fill("a", 600), fill("b", bellInline+1-len("1. ")-600-len("\n2. "))
		queueFrom(t, f, "designer", a, b)

		did := f.s.deliverInbox("designer", *f.now)

		want := bellOf("designer", numbered([]string{a, b}), 2, " (dev on T-1)")
		require.Equal(t, []string{tagged(f, "designer", want)}, p.raw)
		assert.Equal(t, "posted to designer: bell for 2 lines: "+want, did)
		assert.Less(t, len(want), 200)
	})

	t.Run("the bell is the record, its parts the lines, the ack ids inside the body", func(t *testing.T) {
		f, p := nativeFixture(t, bellYAML)
		upNative(t, f, "designer")

		lines, ids := queueLines(t, f, "designer", fill("a", 700), fill("b", 700))
		require.Greater(t, len(numbered(lines)), bellInline)

		did := f.s.deliverInbox("designer", *f.now)

		want := bellOf("designer", numbered(lines), 2, " (owner)")
		require.Equal(t, []string{tagged(f, "designer", want)}, p.raw)
		assert.Equal(t, "posted to designer: bell for 2 lines: "+want, did)

		rs := f.s.State.Role("designer")
		assert.Empty(t, rs.Inbox)
		assert.Empty(t, rs.QueuedIDs, "each line's ids were taken, once")
		require.Len(t, rs.Posted, 1)

		rec := rs.Posted[0]
		assert.Equal(t, want, rec.Line)
		assert.Equal(t, ids, rec.IDs)
		assert.Equal(t, []Part{{Line: lines[0], IDs: ids[:1]}, {Line: lines[1], IDs: ids[1:]}}, rec.Parts)
		assert.Equal(t, 1, f.eventCount("posted"))

		for i, id := range ids {
			assert.Equal(t, *f.now, f.s.State.Acks[id].First, "delivered: said again by its ack as ever")
			assert.NotContains(t, want, id)
			assert.Contains(t, lines[i], "(zmina "+id+")")
		}

		for _, e := range f.ev.list {
			if e.Event == "posted" {
				assert.Equal(t, "bell for 2 lines: "+want, e.Detail)
			}
		}

		// The body is stored once, under the id the bell names, with the ack tails, and knows its role.
		got, err := f.s.inboxStore().Get(bellIDOf(t, want))
		require.NoError(t, err)
		assert.Equal(t, numbered(lines), got.Body)
		assert.Equal(t, "designer", got.Role)

		stored, err := f.s.inboxStore().List()
		require.NoError(t, err)
		assert.Len(t, stored, 1)

		for _, id := range ids {
			assert.Contains(t, got.Body, "(zmina "+id+")")
		}
	})

	t.Run("one line at the bound goes whole, one byte over as a bell", func(t *testing.T) {
		f, p := nativeFixture(t, bellYAML)
		upNative(t, f, "designer")

		whole := fill("a", bellInline)
		queueFrom(t, f, "designer", whole)
		f.s.deliverInbox("designer", *f.now)
		require.Equal(t, []string{tagged(f, "designer", whole)}, p.raw)
		assert.Nil(t, f.s.State.Role("designer").Posted[0].Parts, "no parts: as before")

		p.showAll()
		f.s.deliverInbox("designer", *f.now)

		big := fill("a", bellInline+1)
		queueFrom(t, f, "designer", big)
		f.s.deliverInbox("designer", *f.now)

		want := bellOf("designer", big, 1, " (dev on T-1)")
		require.Len(t, p.raw, 2)
		assert.Equal(t, tagged(f, "designer", want), p.raw[1])

		rec := f.s.State.Role("designer").Posted[0]
		assert.Equal(t, want, rec.Line)
		assert.Equal(t, []Part{{Line: big}}, rec.Parts, "a part for the one line")
	})
}

// bellIDOf is the body id a bell's text names, the test failing when it names none.
func bellIDOf(t *testing.T, text string) string {
	t.Helper()

	id := bellID(text)
	require.NotEmpty(t, id, text)

	return id
}

// A role typed into gets a bell when more than one line that may be read together waits, or when one line is past
// bellTyped characters (not bytes); below that it is typed as before. The bell takes the run stage 3 would join and leaves
// what follows it queued.
func TestBellThresholdOnTypedDelivery(t *testing.T) {
	t.Run("one line of bellTyped characters is typed whole", func(t *testing.T) {
		f := typedFixture(t, bellYAML, "scribe")

		line := strings.Repeat("é", bellTyped)
		require.Greater(t, len(line), bellTyped, "more bytes than characters")
		queueFrom(t, f, "scribe", line)

		f.s.deliverInbox("scribe", *f.now)

		assert.Equal(t, []string{line}, f.tm.typed["scribe"])
		assert.Nil(t, f.s.State.Role("scribe").Sent.Parts)
		assert.NoDirExists(t, f.s.Crew.Layout().Inbox())
	})

	t.Run("one line a character over is a bell", func(t *testing.T) {
		f := typedFixture(t, bellYAML, "scribe")

		line := strings.Repeat("é", bellTyped+1)
		queueFrom(t, f, "scribe", line)

		did := f.s.deliverInbox("scribe", *f.now)

		want := bellOf("scribe", line, 1, " (dev on T-1)")
		assert.Equal(t, []string{want}, f.tm.typed["scribe"])
		assert.Equal(t, "typed into scribe: bell for 1 line: "+want, did)
		assert.Equal(t, 1, utf8.RuneCountInString(line)-bellTyped)

		sent := f.s.State.Role("scribe").Sent
		require.NotNil(t, sent)
		assert.Equal(t, want, sent.Line)
		assert.Equal(t, []Part{{Line: line}}, sent.Parts)
	})

	t.Run("two short lines are a bell, and what is not joinable stays queued behind", func(t *testing.T) {
		f := typedFixture(t, bellYAML, "scribe")

		queueFrom(t, f, "scribe", "one", "two", "/compact", "three")

		did := f.s.deliverInbox("scribe", *f.now)

		want := bellOf("scribe", numbered([]string{"one", "two"}), 2, " (dev on T-1)")
		assert.Equal(t, []string{want}, f.tm.typed["scribe"], "one line typed: the bell")
		assert.Equal(t, "typed into scribe: bell for 2 lines: "+want, did)

		rs := f.s.State.Role("scribe")
		assert.Equal(t, []string{"/compact", "three"}, rs.Inbox, "the run ended before the slash command")
		assert.Equal(t, []Part{{Line: "one"}, {Line: "two"}}, rs.Sent.Parts)

		body, err := f.s.inboxStore().Get(bellIDOf(t, want))
		require.NoError(t, err)
		assert.Equal(t, "1. one\n2. two", body.Body)

		for _, e := range f.ev.list {
			if e.Event == "typed" {
				assert.Equal(t, "bell for 2 lines: "+want, e.Detail)
			}
		}

		// The bell landed (the role took it): the next pass closes it, and types what follows as it always did.
		f.s.deliverInbox("scribe", *f.now)
		assert.Equal(t, []string{want, "/compact"}, f.tm.typed["scribe"])
		assert.Equal(t, []string{"three"}, rs.Inbox)

		for _, l := range []string{"one", "two"} {
			c := cardOn(t, f, "scribe", l)
			require.NotNil(t, c.Closed)
			assert.Equal(t, ClosedLanded, c.Closed.How, "the typed record's lifecycle as before")
			assert.Nil(t, c.Seen, "a bell that landed is not its lines seen")
			require.NotNil(t, c.Bell)
			assert.False(t, c.Bell.Shown.IsZero(), "the bell was shown")
		}
	})

	t.Run("one short line is typed as before", func(t *testing.T) {
		f := typedFixture(t, bellYAML, "scribe")

		queueFrom(t, f, "scribe", "one", "/compact")
		f.s.deliverInbox("scribe", *f.now)

		assert.Equal(t, []string{"one"}, f.tm.typed["scribe"])
		assert.Nil(t, f.s.State.Role("scribe").Sent.Parts)
	})
}

// neverBell is a kind of line that is never a bell, whatever its size: the kinds join never joins (neverJoined), each padded
// past both thresholds.
type neverBell struct {
	name  string
	line  string
	set   func(rs *RoleState)
	typed bool
}

func neverBells() []neverBell {
	const packet = `[zmina T-1] Read handoff packet "/state/handoff/h-1.json"; before work run: zmina task accept T-1 --handoff h-1 --role %s`

	pad := strings.Repeat("x", bellInline+100)

	return []neverBell{
		{name: "a slash command", line: "/compact " + pad, typed: true},
		{name: "the rename", line: "/rename zm-" + pad, typed: true, set: func(rs *RoleState) { rs.QueuedRename = "/rename zm-" + pad }},
		{name: "the owner's own message", line: chat.Prefix("web:1") + pad, typed: true, set: func(rs *RoleState) {
			rs.ChatReceipts = append(rs.ChatReceipts, chat.Receipt{ID: "1", Line: chat.Prefix("web:1") + pad})
		}},
		{name: "the boot line", line: "read d and boot " + pad, set: func(rs *RoleState) { rs.QueuedBoot = "read d and boot " + pad }},
		{name: "a line only its conversation may read", line: "[zmina release] park what you hold " + pad},
		{name: "a task packet", line: packet + " " + pad},
		{name: "a line stored whole in the inbox", line: pad + " (+31000 chars, full: zmina inbox show in-0123456789ab) (zmina S-99)"},
	}
}

// Slash commands, the rename, the owner's own message, the boot line, session-bound lines, packets and lines in locator
// form are never a bell: each goes as it did, whole, however big.
func TestBellIsNeverRungForWhatJoinNeverJoins(t *testing.T) {
	for _, k := range neverBells() {
		t.Run(k.name+" on the role channel", func(t *testing.T) {
			f, p := nativeFixture(t, bellYAML)
			upNative(t, f, "designer")

			rs := f.s.State.Role("designer")
			if k.set != nil {
				k.set(rs)
			}

			line := strings.Replace(k.line, "--role %s", "--role designer", 1)
			rs.Inbox = append(rs.Inbox, line)
			queueFrom(t, f, "designer", "one", "two")

			f.s.deliverInbox("designer", *f.now)

			if k.typed {
				assert.Empty(t, p.raw, "typed, not posted")
				assert.Equal(t, []string{line}, f.tm.typed["designer"])
			} else {
				assert.Equal(t, []string{tagged(f, "designer", line)}, p.raw, "whole, as before")
			}

			assert.NoDirExists(t, f.s.Crew.Layout().Inbox(), "no body stored")
			assert.Equal(t, []string{"one", "two"}, f.s.State.Role("designer").Inbox)
		})

		t.Run(k.name+" typed", func(t *testing.T) {
			f := typedFixture(t, bellYAML, "scribe")

			rs := f.s.State.Role("scribe")
			if k.set != nil {
				k.set(rs)
			}

			line := strings.Replace(k.line, "--role %s", "--role scribe", 1)
			rs.Inbox = append(rs.Inbox, line)
			queueFrom(t, f, "scribe", "one", "two")

			f.s.deliverInbox("scribe", *f.now)

			assert.Equal(t, []string{line}, f.tm.typed["scribe"], "typed whole, as before")
			assert.NoDirExists(t, f.s.Crew.Layout().Inbox(), "no body stored")
		})
	}
}

// A bell that was shown is not its lines seen: the card says the bell was shown, and the lines are seen only when their
// body was fetched, by the next pass that reads the role's fetches.
func TestFetchIsSeenAndTheBellShownIsNot(t *testing.T) {
	f, p := nativeFixture(t, bellYAML)
	upNative(t, f, "designer")

	lines, _ := queueLines(t, f, "designer", fill("a", 700), fill("b", 700))
	f.s.deliverInbox("designer", *f.now)
	id := bellIDOf(t, f.s.State.Role("designer").Posted[0].Line)

	for _, l := range lines {
		c := cardOn(t, f, "designer", l)
		require.NotNil(t, c.Accepted)
		require.NotNil(t, c.Bell)
		assert.Equal(t, id, c.Bell.Body)
		assert.True(t, c.Bell.Shown.IsZero(), "not shown yet")
	}

	// The conversation shows the bell.
	p.showAll()
	f.s.deliverInbox("designer", *f.now)

	assert.Empty(t, f.s.State.Role("designer").Posted)
	assert.Equal(t, 0, f.eventCount("seen"), "no line was seen")
	assert.Equal(t, 1, f.eventCount("bell-shown"))

	for _, l := range lines {
		c := cardOn(t, f, "designer", l)
		assert.Nil(t, c.Seen, "the bell shown is not the line seen")
		assert.Equal(t, *f.now, c.Bell.Shown)
		assert.Nil(t, c.Closed)
	}

	// The role runs the command: a line of its fetches, written without any lock. The next pass sees it.
	at := f.now.Add(time.Minute)
	require.NoError(t, RecordFetch(f.s.Crew.Layout(), "designer", id, "designer", at))
	*f.now = at.Add(time.Second)
	f.s.deliverInbox("designer", *f.now)

	for _, l := range lines {
		c := cardOn(t, f, "designer", l)
		require.NotNil(t, c.Seen)
		assert.Equal(t, SeenFetch, c.Seen.By)
		assert.True(t, c.Seen.At.Equal(at))
	}

	assert.Equal(t, 1, f.eventCount("fetched"))

	// Reading the file again changes nothing, and says nothing again.
	f.s.deliverInbox("designer", *f.now)
	assert.Equal(t, 1, f.eventCount("fetched"))
}

// A fetch line older than the bell's acceptance belongs to an earlier bell of the same words: it marks nothing.
func TestAFetchOfAnEarlierBellOfTheSameWordsSeesNothing(t *testing.T) {
	f, p := nativeFixture(t, bellYAML)
	upNative(t, f, "designer")

	early := f.now.Add(-time.Hour)
	body := bodyOf(fill("a", 700), fill("b", 700))
	require.NoError(t, RecordFetch(f.s.Crew.Layout(), "designer", inbox.IDOf("bell designer\x00"+body), "designer", early))

	queueFrom(t, f, "designer", fill("a", 700), fill("b", 700))
	f.s.deliverInbox("designer", *f.now)
	require.Len(t, p.raw, 1)
	p.showAll()
	f.s.deliverInbox("designer", *f.now)

	for _, c := range cardsOn(f, "designer") {
		assert.Nil(t, c.Seen)
	}

	assert.Equal(t, 0, f.eventCount("fetched"))
}

// A typed bell that landed closes its lines' cards as landed, as any typed line, and the fetch is still noted on them.
func TestFetchOfATypedBellIsNotedOnTheLandedCards(t *testing.T) {
	f := typedFixture(t, bellYAML, "scribe")

	queueFrom(t, f, "scribe", "one", "two")
	f.s.deliverInbox("scribe", *f.now)
	id := bellIDOf(t, f.s.State.Role("scribe").Sent.Line)
	f.s.closeTakenTyped("scribe")
	require.Nil(t, f.s.State.Role("scribe").Sent, "landed")

	at := f.now.Add(time.Minute)
	require.NoError(t, RecordFetch(f.s.Crew.Layout(), "scribe", id, "scribe", at))
	*f.now = at
	f.s.deliverInbox("scribe", *f.now)

	for _, l := range []string{"one", "two"} {
		c := cardOn(t, f, "scribe", l)
		require.NotNil(t, c.Seen)
		assert.Equal(t, SeenFetch, c.Seen.By)
		require.NotNil(t, c.Closed)
		assert.Equal(t, ClosedLanded, c.Closed.How)
	}
}

// A bell shown and not fetched is never rung again by itself. After postedWait each line's card needs attention and the
// dispatcher is told once.
func TestUnfetchedBellNeedsAttentionAndTheDispatcherHearsOnce(t *testing.T) {
	f, p := nativeFixture(t, bellYAML)
	upNative(t, f, "designer")
	upNative(t, f, "dispatcher")

	// Lines nobody acks, so that nothing says them again.
	lines := []string{fill("a", 700), fill("b", 700)}
	queueFrom(t, f, "designer", lines...)
	f.s.deliverInbox("designer", *f.now)
	id := bellIDOf(t, f.s.State.Role("designer").Posted[0].Line)
	p.showAll()
	f.s.deliverInbox("designer", *f.now)
	shown := *f.now

	// Not yet.
	*f.now = shown.Add(postedWait - time.Second)
	assert.Empty(t, f.s.bellWatch(*f.now))

	*f.now = shown.Add(postedWait)
	notes := f.s.bellWatch(*f.now)
	f.s.FlushCards()
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "designer: a bell for 2 lines was shown 10m ago and the role has not fetched them")
	assert.Contains(t, notes[0], "zmina queue ls")
	assert.NotContains(t, notes[0], "inbox show", "the command that prints the lines is the role's to run, not the dispatcher's")
	assert.NotContains(t, notes[0], id)
	assert.Equal(t, 1, f.eventCount("bell-unfetched"))
	f.s.addNotes(notes)

	for _, l := range lines {
		c := cardOn(t, f, "designer", l)
		require.NotNil(t, c.Attention)
		assert.Equal(t, bellUnfetched, c.Attention.Why)
	}

	// Nor again, however long: not a second note, and no second ring.
	*f.now = shown.Add(2 * postedWait)
	assert.Empty(t, f.s.bellWatch(*f.now))
	f.s.Tick()
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	f.s.deliverInbox("designer", *f.now)

	assert.Len(t, p.raw, 1+len(p.posted["sid-dispatcher"]), "no bell but the first was rung to the role")
	assert.Len(t, p.posted["sid-designer"], 1, "a bell shown is not rung again by itself")
	assert.Equal(t, 1, strings.Count(strings.Join(p.posted["sid-dispatcher"], "\n"), "a bell for 2 lines was shown"),
		"the dispatcher has one note for the bell")

	// The fetch ends it: the attention goes with the seen.
	require.NoError(t, RecordFetch(f.s.Crew.Layout(), "designer", id, "designer", *f.now))
	f.s.bellWatch(*f.now)
	f.s.FlushCards()

	for _, l := range lines {
		c := cardOn(t, f, "designer", l)
		require.NotNil(t, c.Seen)
		assert.Nil(t, c.Attention)
	}
}

// A line acked meanwhile is not one that waits for a fetch: its ack closes its card, and no note is made for it.
func TestAckedLineOfABellIsNotWatched(t *testing.T) {
	f, p := nativeFixture(t, bellYAML)
	upNative(t, f, "designer")

	lines, ids := queueLines(t, f, "designer", fill("a", 700), fill("b", 700))
	f.s.deliverInbox("designer", *f.now)
	p.showAll()
	f.s.deliverInbox("designer", *f.now)

	acked, _ := f.s.AckLines(ids, false, "designer", "designer")
	require.Equal(t, ids, acked)
	f.s.FlushCards()

	for _, l := range lines {
		c := cardOn(t, f, "designer", l)
		require.NotNil(t, c.Closed)
		assert.Equal(t, ClosedAcked, c.Closed.How)
	}

	*f.now = f.now.Add(postedWait)
	assert.Empty(t, f.s.bellWatch(*f.now))
	assert.Empty(t, f.s.BellWaits("designer", *f.now))
}

// A bell unfetched for as long as a card is kept is let go, and no longer listed.
func TestUnfetchedBellIsLetGoAfterTheKeep(t *testing.T) {
	f, p := nativeFixture(t, bellYAML)
	upNative(t, f, "designer")

	lines, _ := queueLines(t, f, "designer", fill("a", 700), fill("b", 700))
	f.s.deliverInbox("designer", *f.now)
	p.showAll()
	f.s.deliverInbox("designer", *f.now)

	*f.now = f.now.Add(cardKeep + time.Minute)
	assert.Empty(t, f.s.BellWaits("designer", *f.now))
	f.s.bellWatch(*f.now)
	f.s.FlushCards()

	for _, l := range lines {
		c := cardOn(t, f, "designer", l)
		require.NotNil(t, c.Closed)
		assert.Equal(t, ClosedDropped, c.Closed.How)
	}
}

// The role's brief lists the bells it was shown and has not fetched, each with its command; a fetched one is not listed.
func TestPendingBriefListsTheUnfetchedBell(t *testing.T) {
	f, p := nativeFixture(t, bellYAML)
	upNative(t, f, "designer")

	// Posted and not shown yet: the role has not met the bell.
	queueLines(t, f, "designer", fill("a", 700), fill("b", 700))
	f.s.deliverInbox("designer", *f.now)
	id := bellIDOf(t, f.s.State.Role("designer").Posted[0].Line)
	f.s.FlushCards()
	assert.NotContains(t, f.s.PendingBrief("designer", false), "inbox show")

	p.showAll()
	f.s.deliverInbox("designer", *f.now)

	brief := f.s.PendingBrief("designer", false)
	assert.Contains(t, brief, "zmina inbox show "+id+" (2 lines from owner, shown ")
	assert.Contains(t, brief, "ack each by its own id")
	assert.NotContains(t, f.s.PendingBrief("dispatcher", false), "inbox show", "never the dispatcher")

	require.NoError(t, RecordFetch(f.s.Crew.Layout(), "designer", id, "designer", *f.now))
	assert.NotContains(t, f.s.PendingBrief("designer", false), "inbox show", "fetched, even before a pass read it")
}

// Only a role whose current start was told what a bell is is rung one.
func TestOnlyARoleThatWasToldIsRungABell(t *testing.T) {
	big := []string{fill("a", 700), fill("b", 700)}

	t.Run("no mark", func(t *testing.T) {
		f, p := nativeFixture(t, bellYAML)
		upNative(t, f, "designer")
		require.NoError(t, os.Remove(f.s.Crew.Layout().DeliveryBell("designer")))

		queueFrom(t, f, "designer", big...)
		f.s.deliverInbox("designer", *f.now)

		assert.Equal(t, []string{tagged(f, "designer", "1. "+big[0]+"\n2. "+big[1])}, p.raw, "the joined post of stage 3")
		assert.NoDirExists(t, f.s.Crew.Layout().Inbox())
	})

	t.Run("a mark of another start", func(t *testing.T) {
		f, p := nativeFixture(t, bellYAML)
		upNative(t, f, "designer")

		// An older zmina restarted the role and wrote no mark: the state has the new start, the file the one before.
		rs := f.s.State.Role("designer")
		rs.Started = rs.Started.Add(-time.Hour)

		queueFrom(t, f, "designer", big...)
		f.s.deliverInbox("designer", *f.now)

		assert.Equal(t, []string{tagged(f, "designer", "1. "+big[0]+"\n2. "+big[1])}, p.raw)
	})

	t.Run("typed, no mark", func(t *testing.T) {
		f := typedFixture(t, bellYAML, "scribe")
		require.NoError(t, os.Remove(f.s.Crew.Layout().DeliveryBell("scribe")))

		queueFrom(t, f, "scribe", "one", "two")
		f.s.deliverInbox("scribe", *f.now)

		assert.Equal(t, []string{"one"}, f.tm.typed["scribe"], "a line a pass, as before")
	})

	t.Run("a role started without a boot line", func(t *testing.T) {
		f := typedFixture(t, strings.Replace(bellYAML, "    boot: read the rules\n", "", 1), "scribe")

		assert.NoFileExists(t, f.s.Crew.Layout().DeliveryBell("scribe"), "nothing told it, so no mark")

		queueFrom(t, f, "scribe", "one", "two")
		f.s.deliverInbox("scribe", *f.now)
		assert.Equal(t, []string{"one"}, f.tm.typed["scribe"])
	})
}

// The boot line of every role but the dispatcher carries the sentence, and the start that carried it leaves the mark, which
// the next start replaces.
func TestBootCarriesTheDoorbellSentenceAndTheStartLeavesAMark(t *testing.T) {
	f, _ := nativeFixture(t, bellYAML)

	for _, name := range []string{"designer", "scribe"} {
		r, err := f.s.Role(name)
		require.NoError(t, err)
		assert.Contains(t, f.s.BootText(r), BellRule, name)
	}

	d, err := f.s.Role("dispatcher")
	require.NoError(t, err)
	assert.NotContains(t, f.s.BootText(d), BellRule, "the dispatcher is never rung a bell")

	assert.Contains(t, BellRule, "zmina inbox show")
	assert.Contains(t, BellRule, "ack each by its own id")

	upNative(t, f, "designer")
	upNative(t, f, "dispatcher")

	l := f.s.Crew.Layout()
	assert.NoFileExists(t, l.DeliveryBell("dispatcher"))

	var first bellMark
	require.True(t, readRecord(l.DeliveryBell("designer"), &first))
	assert.True(t, first.Started.Equal(f.s.State.Role("designer").Started))
	assert.True(t, f.s.bellKnown("designer"))

	// Put down and started again, later: the mark is of the new start.
	require.NoError(t, f.s.Tmux.Kill("designer"))
	*f.now = f.now.Add(time.Hour)
	require.NoError(t, f.s.Up("designer"))

	var second bellMark
	require.True(t, readRecord(l.DeliveryBell("designer"), &second))
	assert.True(t, second.Started.After(first.Started))
	assert.True(t, f.s.bellKnown("designer"))
}

// doorbell: off turns it all off: no sentence in the boot line, no mark, and no bell even from a mark an earlier start left.
func TestDoorbellOffRingsNothing(t *testing.T) {
	big := []string{fill("a", 700), fill("b", 700)}

	t.Run("a crew that says off", func(t *testing.T) {
		f, p := nativeFixture(t, "doorbell: off\n"+bellYAML)
		upNative(t, f, "designer")

		r, err := f.s.Role("designer")
		require.NoError(t, err)
		assert.NotContains(t, f.s.BootText(r), BellRule)
		assert.NoFileExists(t, f.s.Crew.Layout().DeliveryBell("designer"))

		queueFrom(t, f, "designer", big...)
		f.s.deliverInbox("designer", *f.now)

		assert.Equal(t, []string{tagged(f, "designer", "1. "+big[0]+"\n2. "+big[1])}, p.raw)
	})

	t.Run("turned off after the start", func(t *testing.T) {
		f, p := nativeFixture(t, bellYAML)
		upNative(t, f, "designer")
		require.True(t, f.s.bellKnown("designer"))

		off := false
		f.s.Crew.Doorbell = &off

		queueFrom(t, f, "designer", big...)
		f.s.deliverInbox("designer", *f.now)

		assert.Equal(t, []string{tagged(f, "designer", "1. "+big[0]+"\n2. "+big[1])}, p.raw)
	})
}

// A bell that was never shown is kept as one failed record that keeps its lines; queue retry gives them back as separate
// lines, in order, each with its ids, and the bell mark is not on them.
func TestFailedBellRetriesItsLinesSeparately(t *testing.T) {
	f, _ := nativeFixture(t, bellYAML)
	upNative(t, f, "designer")

	lines, ids := queueLines(t, f, "designer", fill("a", 700), fill("b", 700), fill("c", 100))
	f.s.deliverInbox("designer", *f.now)

	rs := f.s.State.Role("designer")
	text := rs.Posted[0].Line
	require.True(t, IsBell(text))

	*f.now = f.now.Add(postedWait)
	f.s.deliverInbox("designer", *f.now)

	require.Len(t, rs.Failed, 1)
	failed := rs.Failed[0]
	assert.Equal(t, postedNever, failed.Error)
	assert.Equal(t, text, failed.Line, "the failed record's text is the bell's")
	assert.Equal(t, ids, failed.IDs)
	assert.True(t, failed.Unclear())
	require.Len(t, failed.Parts, 3)

	for i, p := range failed.Parts {
		assert.Equal(t, Part{Line: lines[i], IDs: ids[i : i+1]}, p)
		require.NotNil(t, cardOn(t, f, "designer", lines[i]).Attention, "a look first")
	}

	n, left, err := f.s.Retry("designer", true)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Zero(t, left)

	assert.Equal(t, lines, rs.Inbox, "separate lines, in order")
	f.s.FlushCards()

	for i, l := range lines {
		assert.Equal(t, []string{ids[i]}, rs.QueuedIDs[l])

		c := cardOn(t, f, "designer", l)
		require.NotNil(t, c.Back)
		assert.Nil(t, c.Bell, "the bell it went in did not reach the role")
		assert.Nil(t, c.Accepted)
	}
}

// A typed bell whose typing broke where it may have reached the pane keeps all its lines for a look, as one record whose
// text is the bell's: none is typed again by itself, and a retry gives them back as separate lines.
func TestTypedBellThatMayHaveReachedItsPaneKeepsItsLines(t *testing.T) {
	f := typedFixture(t, bellYAML, "scribe")
	f.tm.typeErr = fmt.Errorf("tmux: send-keys timed out")

	queueFrom(t, f, "scribe", "one", "two", "three")

	did := f.s.deliverInbox("scribe", *f.now)
	assert.Contains(t, did, "a queued bell may have reached its pane")

	rs := f.s.State.Role("scribe")
	assert.Empty(t, rs.Inbox)
	require.Len(t, rs.Failed, 1)
	assert.True(t, rs.Failed[0].Unclear())
	assert.True(t, IsBell(rs.Failed[0].Line))
	assert.Equal(t, []Part{{Line: "one"}, {Line: "two"}, {Line: "three"}}, rs.Failed[0].Parts)

	f.tm.typeErr = nil
	n, _, err := f.s.Retry("scribe", true)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{"one", "two", "three"}, rs.Inbox)
}

// A typed bell whose typing never started is the first line's failure alone, counted, as a line's is; the lines after
// it wait.
func TestTypedBellThatNeverStartedFailsItsFirstLineAlone(t *testing.T) {
	f := typedFixture(t, bellYAML, "scribe")
	f.tm.typeErr = errNotTyped("tmux: no answer")

	queueFrom(t, f, "scribe", "one", "two")

	for range typeTries {
		f.s.deliverInbox("scribe", *f.now)
		*f.now = f.now.Add(time.Minute)
	}

	rs := f.s.State.Role("scribe")
	require.Len(t, rs.Failed, 1)
	assert.Equal(t, "one", rs.Failed[0].Line)
	assert.False(t, rs.Failed[0].Unclear())
	assert.Equal(t, []string{"two"}, rs.Inbox)
}

// A post that never started is tried again as a line's is, and the body it stored is the same entry each try.
func TestBellPostThatNeverStartedStoresItsBodyOnce(t *testing.T) {
	f, p := nativeFixture(t, bellYAML)
	upNative(t, f, "designer")

	queueFrom(t, f, "designer", fill("a", 700), fill("b", 700))
	p.err = fmt.Errorf("%w: dial: no such file", peer.ErrUnavailable)

	for range 2 {
		assert.Empty(t, f.s.deliverInbox("designer", *f.now))
		*f.now = f.now.Add(time.Minute)
	}

	entries, err := f.s.inboxStore().List()
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the same text under the same id")

	p.err = nil
	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	entries, err = f.s.inboxStore().List()
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

// A body that cannot be stored is a bell that cannot be rung: the lines go as they are.
func TestBellFallsBackToTheLinesWhenItsBodyCannotBeStored(t *testing.T) {
	f, p := nativeFixture(t, bellYAML)
	upNative(t, f, "designer")

	require.NoError(t, os.WriteFile(f.s.Crew.Layout().Inbox(), []byte("a file where the directory belongs"), 0o600))

	big := []string{fill("a", 700), fill("b", 700)}
	queueFrom(t, f, "designer", big...)
	f.s.deliverInbox("designer", *f.now)

	assert.Equal(t, []string{tagged(f, "designer", "1. "+big[0]+"\n2. "+big[1])}, p.raw)
	assert.Equal(t, 1, f.eventCount("bell-failed"))
}

// The bell is short, whatever the lists: three names and then how many more, left out where unknown.
func TestBellTextIsBoundedAndLeavesOutWhatIsUnknown(t *testing.T) {
	id := "in-0123456789ab"

	assert.Equal(t, "[zmina] 1 line waits for you: zmina inbox show "+id, bellText(1, nil, nil, id))
	assert.Equal(t, "[zmina] 2 lines wait for you (dev): zmina inbox show "+id, bellText(2, []string{"dev"}, nil, id))
	assert.Equal(t, "[zmina] 2 lines wait for you (on T-1): zmina inbox show "+id, bellText(2, nil, []string{"T-1"}, id))
	assert.Equal(t, "[zmina] 8 lines wait for you (dev, ops, owner +2 on T-1, T-2, T-3 +1): zmina inbox show "+id,
		bellText(8, []string{"dev", "ops", "owner", "zmina", "qa"}, []string{"T-1", "T-2", "T-3", "T-4"}, id))

	long := strings.Repeat("n", 90)
	text := bellText(8, []string{long, long, long, long}, []string{"T-" + long, "T-2", "T-3", "T-4"}, id)
	assert.Less(t, len(text), 200)
	assert.Equal(t, id, bellID(text), "and it still reads as a bell")

	for _, not := range []string{"", "1. one", "[zmina] 3 lines wait for you", "[zmina] hello: zmina inbox show " + id,
		"[zmina] 2 lines wait for you: zmina inbox show in-xyz"} {
		assert.Empty(t, bellID(not), not)
	}
}

// A role's fetches are never rewritten, for the role may append at any moment: the file goes whole, once nothing was written
// to it for as long as a card is kept, and until then stays as it is. A role gone from the crew loses its mark.
func TestRetentionRemovesAnOldFetchFileWholeAndNeverRewritesAFreshOne(t *testing.T) {
	f, _ := nativeFixture(t, bellYAML)
	l := f.s.Crew.Layout()
	now := *f.now

	id := "in-0123456789ab"
	require.NoError(t, RecordFetch(l, "designer", id, "designer", now.Add(-cardKeep-time.Hour)))
	require.NoError(t, RecordFetch(l, "designer", id, "designer", now.Add(-time.Hour)))
	require.NoError(t, RecordFetch(l, "old", id, "old", now.Add(-cardKeep-time.Hour)))
	require.NoError(t, RecordFetch(l, "old", id, "old", now.Add(-cardKeep-2*time.Hour)))
	require.NoError(t, os.WriteFile(l.DeliveryBell("gone"), []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(l.DeliveryBell("designer"), []byte("{}\n"), 0o600))

	// The file of designer was written to a moment ago, the file of old long ago.
	require.NoError(t, os.Chtimes(l.DeliveryFetched("designer"), now.Add(-time.Hour), now.Add(-time.Hour)))
	require.NoError(t, os.Chtimes(l.DeliveryFetched("old"), now.Add(-cardKeep-time.Hour), now.Add(-cardKeep-time.Hour)))

	known := func(role string) bool { return role == "designer" }

	before, err := os.ReadFile(l.DeliveryFetched("designer"))
	require.NoError(t, err)

	n, err := PruneCards(l, now, known, true)
	require.NoError(t, err)
	assert.Equal(t, 3, n, "the two lines of the old file and the mark of a role gone")
	assert.FileExists(t, l.DeliveryFetched("old"), "a dry run moves nothing")

	n, err = PruneCards(l, now, known, false)
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	assert.NoFileExists(t, l.DeliveryFetched("old"), "not touched for as long as a card is kept: gone whole")
	assert.NoFileExists(t, l.DeliveryBell("gone"))
	assert.FileExists(t, l.DeliveryBell("designer"))

	after, err := os.ReadFile(l.DeliveryFetched("designer"))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "a file written to within the keep is not rewritten, its old line with it")
}

// A fetch is one line appended without a lock, and a line that does not read is left out, not fatal.
func TestFetchesAreAppendedAndReadLineByLine(t *testing.T) {
	f, _ := nativeFixture(t, bellYAML)
	l := f.s.Crew.Layout()
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	require.NoError(t, RecordFetch(l, "designer", "in-0123456789ab", "designer", at))
	require.NoError(t, RecordFetch(l, "designer", "in-ba9876543210", "hand", at.Add(time.Minute)))

	f2, err := os.OpenFile(l.DeliveryFetched("designer"), os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f2.WriteString("not json\n{\"id\":\"nope\",\"at\":\"2026-10-24T12:00:00Z\"}\n")
	require.NoError(t, err)
	require.NoError(t, f2.Close())

	got := readFetches(l.DeliveryFetched("designer"))
	require.Len(t, got, 2)
	assert.Equal(t, "in-0123456789ab", got[0].ID)
	assert.Equal(t, "hand", got[1].By)

	assert.Error(t, RecordFetch(l, "../x", "in-0123456789ab", "x", at), "a role is a file name, never a path")
}

// A Codex role's boot line carries the sentence too, so it is rung a bell through its bridge's inbox; the bridge taking the
// file shows the bell, not the lines, which are seen by their fetch.
func TestBellToACodexRole(t *testing.T) {
	f, dir := codexFixture(t)

	r, err := f.s.Role("dev")
	require.NoError(t, err)
	assert.Contains(t, f.s.BootText(r), BellRule)
	require.True(t, f.s.bellKnown("dev"), "its start carried the sentence and left the mark")

	bridge(t, f, dir, "thread-1")

	lines := []string{fill("a", 700), fill("b", 700)}
	queueFrom(t, f, "dev", lines...)
	require.NotEmpty(t, f.s.deliverInbox("dev", *f.now))

	want := bellOf("dev", numbered(lines), 2, " (dev on T-1)")

	files := waitingFiles(t, dir)
	require.Len(t, files, 1)

	raw, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Contains(t, string(raw), want)
	assert.NotContains(t, string(raw), lines[0], "the lines wait in the inbox, not in the file")

	take(t, dir, files[0])
	*f.now = f.now.Add(time.Minute)
	f.s.deliverInbox("dev", *f.now)

	rs := f.s.State.Role("dev")
	assert.Empty(t, rs.Posted)

	for _, l := range lines {
		c := cardOn(t, f, "dev", l)
		assert.Nil(t, c.Seen, "taken is the bell taken, not the line seen")
		require.NotNil(t, c.Bell)
		assert.False(t, c.Bell.Shown.IsZero())
		assert.Equal(t, AcceptedCodexInbox, c.Accepted.How)
	}

	require.NoError(t, RecordFetch(f.s.Crew.Layout(), "dev", bellIDOf(t, want), "dev", f.now.Add(time.Minute)))
	*f.now = f.now.Add(2 * time.Minute)
	f.s.deliverInbox("dev", *f.now)

	for _, l := range lines {
		c := cardOn(t, f, "dev", l)
		require.NotNil(t, c.Seen)
		assert.Equal(t, SeenFetch, c.Seen.By)
	}
}

// A later delivery of the same words is noted on its own card, never on the card of an earlier bell that was shown and
// waits for its fetch.
func TestALaterDeliveryOfTheSameWordsLeavesAShownBellsCardAlone(t *testing.T) {
	f, p := nativeFixture(t, bellYAML)
	upNative(t, f, "designer")

	again := fill("a", 700)
	queueFrom(t, f, "designer", again, fill("b", 700))
	f.s.deliverInbox("designer", *f.now)
	p.showAll()
	f.s.deliverInbox("designer", *f.now)

	// The same words once more, alone and small enough to go whole.
	queueFrom(t, f, "designer", again)
	f.s.deliverInbox("designer", *f.now)
	require.Len(t, p.posted["sid-designer"], 2)
	require.Equal(t, again, p.posted["sid-designer"][1], "whole, not a bell")

	p.showAll()
	f.s.deliverInbox("designer", *f.now)

	var bell, plain []Card

	for _, c := range cardsOn(f, "designer") {
		if c.ID != CardID("designer", again) {
			continue
		}

		if c.Bell != nil {
			bell = append(bell, c)
		} else {
			plain = append(plain, c)
		}
	}

	require.Len(t, bell, 1)
	require.Len(t, plain, 1)
	assert.Nil(t, bell[0].Seen, "the bell's line is not seen by what was shown of another delivery")
	require.NotNil(t, plain[0].Seen)
	assert.Equal(t, SeenTranscript, plain[0].Seen.By)
}

// The dispatcher is never rung a bell, whatever its queue holds: its own path posts its own lines as it always did.
func TestTheDispatcherIsNeverRungABell(t *testing.T) {
	f, p := nativeFixture(t, bellYAML)
	upNative(t, f, "dispatcher")

	assert.False(t, f.s.bellsOn("dispatcher"))
	assert.Empty(t, f.s.bellFor("dispatcher", []string{fill("a", 700), fill("b", 700)}, false, *f.now))

	big := []string{fill("a", 700), fill("b", 700)}
	queueFrom(t, f, "dispatcher", big...)
	f.s.deliverInbox("dispatcher", *f.now)

	require.NotEmpty(t, p.posted["sid-dispatcher"])

	for _, text := range p.posted["sid-dispatcher"] {
		assert.False(t, IsBell(text), text)
	}

	assert.NoDirExists(t, f.s.Crew.Layout().Inbox())
}

// --- the review's fixes ---

// typedBellFixture is a role told of acks that is typed into all the same (its socket is down), up and idle, whose typed
// lines never show as landed.
func typedBellFixture(t *testing.T, role string) fix {
	t.Helper()

	f := fixtureWith(t, bellYAML)
	require.NoError(t, f.s.Up(role))
	f.s.State.Role(role).Inbound = true // told of acks, typed all the same
	f.status(role, "idle", *f.now)
	f.tm.onType = nil // the status never changes: the line never shows as landed
	*f.now = f.now.Add(3 * time.Minute)

	return f
}

// A typed bell of a session that ended before it landed is settled line by line: the line an ack says again is dropped, the
// line nothing says again is kept for a look, as one record whose text is the bell's; judged as a whole, the one tracked
// line would take the other with it, unseen.
func TestTypedBellOfAnEndedSessionKeepsTheLinesNoAckSaysAgain(t *testing.T) {
	f := typedBellFixture(t, "designer")

	id, err := f.s.QueueTracked("designer", "[dev] look at T-1")
	require.NoError(t, err)

	rs := f.s.State.Role("designer")
	tracked := rs.Inbox[0]

	const plain = "[ops] the build is red"

	require.NoError(t, f.s.QueueFrom("designer", plain, Facts{From: "ops"}))
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	require.NotNil(t, rs.Sent)
	require.True(t, IsBell(rs.Sent.Line))
	text := rs.Sent.Line

	f.tm.die("designer")
	f.s.Tick()
	f.s.Tick()

	assert.Nil(t, rs.Sent)
	assert.Empty(t, rs.Inbox, "not typed again")

	require.Len(t, rs.Failed, 1, "the line no ack says again is not lost")
	failed := rs.Failed[0]
	assert.True(t, strings.HasPrefix(failed.Error, UnclearEnded))
	assert.True(t, failed.Unclear())
	assert.Equal(t, text, failed.Line, "the record's text is the bell's")
	assert.Equal(t, []Part{{Line: plain}}, failed.Parts)

	require.NotNil(t, f.s.State.Acks[id], "the tracked line's ack stays, and says it again")
	assert.True(t, f.ev.has("designer", "dropped"))

	require.NotNil(t, cardOn(t, f, "designer", plain).Attention, "needs a look")

	trackedCard := cardOn(t, f, "designer", tracked)
	require.NotNil(t, trackedCard.Closed)
	assert.Equal(t, ClosedDropped, trackedCard.Closed.How)

	// A retry gives the line back, alone.
	n, _, err := f.s.Retry("designer", true)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{plain}, rs.Inbox)
}

// A typed bell all of whose lines an ack says again is dropped, as one line is; one with none is kept whole.
func TestTypedBellOfAnEndedSessionWhoseLinesAreAllTrackedOrAllNot(t *testing.T) {
	t.Run("all tracked", func(t *testing.T) {
		f := typedBellFixture(t, "designer")

		_, err := f.s.QueueTracked("designer", "[dev] one")
		require.NoError(t, err)
		_, err = f.s.QueueTracked("designer", "[dev] two")
		require.NoError(t, err)
		require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

		f.tm.die("designer")
		f.s.Tick()
		f.s.Tick()

		rs := f.s.State.Role("designer")
		assert.Nil(t, rs.Sent)
		assert.Empty(t, rs.Failed, "each is said again by its ack")
		assert.Len(t, f.s.State.Acks, 2)
	})

	t.Run("none tracked", func(t *testing.T) {
		f := typedBellFixture(t, "designer")

		queueFrom(t, f, "designer", "one", "two")
		require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

		f.tm.die("designer")
		f.s.Tick()
		f.s.Tick()

		rs := f.s.State.Role("designer")
		require.Len(t, rs.Failed, 1)
		assert.Equal(t, []Part{{Line: "one"}, {Line: "two"}}, rs.Failed[0].Parts)
	})
}

// The dispatcher's typed line is made of signals and notes, never of its own queued lines (those go alone). One signal an ack
// says again used to drop the notes beside it, which nothing says again; each part is settled by its own kind now.
func TestDispatcherTypedLineOfAnEndedSessionKeepsItsNotes(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("dispatcher"))
	f.s.State.Role("dispatcher").Inbound = true
	f.status("dispatcher", "idle", *f.now)
	f.tm.onType = nil
	*f.now = f.now.Add(3 * time.Minute)

	_, err := f.s.Note("dev", "T-5 result ready", false)
	require.NoError(t, err)

	const note = "dev: delivery failed: tmux refused; inspect zmina queue ls"

	f.s.addNotes([]string{note})
	require.NotEmpty(t, f.s.deliver(*f.now))

	rs := f.s.State.Role("dispatcher")
	require.NotNil(t, rs.Sent)
	require.Len(t, rs.Sent.Parts, 2, "one signal with its id, and the notes")
	require.Len(t, rs.Sent.IDs, 1)

	f.tm.die("dispatcher")
	f.s.settleEnded("dispatcher", *f.now)

	assert.Nil(t, rs.Sent)
	require.Len(t, rs.Failed, 1, "the note is not lost")
	assert.Equal(t, []Part{{Notes: []string{note}}}, rs.Failed[0].Parts)
	assert.Empty(t, rs.Failed[0].IDs, "no id: it is what no ack says again")
	assert.Len(t, f.s.State.Acks, 1, "the signal's ack stays, and says it again")

	// A retry puts the notes back where they waited.
	_, _, err = f.s.Retry("dispatcher", true)
	require.NoError(t, err)
	assert.Contains(t, f.s.State.Notes, note)
}

// shownBell rings a bell of two lines to designer on its channel, has the conversation show it, and returns the body id, the
// lines, and the time it was accepted.
func shownBell(t *testing.T, f fix, p *fakePeer) (string, []string) {
	t.Helper()

	lines := []string{fill("a", 700), fill("b", 700)}
	queueFrom(t, f, "designer", lines...)
	f.s.deliverInbox("designer", *f.now)

	id := bellIDOf(t, f.s.State.Role("designer").Posted[0].Line)
	p.showAll()
	f.s.deliverInbox("designer", *f.now)

	return id, lines
}

// A fetch counts only when the role ran it. One run by another role of the crew, the dispatcher included, or by the owner's
// shell, reads the lines for nobody; one by a caller that does not say who it is (a shared Codex role's tool) is the role's.
func TestAFetchCountsOnlyWhenTheRoleRanIt(t *testing.T) {
	for _, tc := range []struct {
		by     string
		counts bool
	}{
		{"designer", true},
		{"", true},
		{"scribe", false},
		{"dispatcher", false},
		{"hand", false},
		{"designer#2", false},
	} {
		t.Run("by "+tc.by, func(t *testing.T) {
			f, p := nativeFixture(t, bellYAML)
			upNative(t, f, "designer")

			id, lines := shownBell(t, f, p)

			*f.now = f.now.Add(time.Minute)
			require.NoError(t, RecordFetch(f.s.Crew.Layout(), "designer", id, tc.by, *f.now))

			// The brief reads the fetches too, before any pass puts them on the cards.
			assert.Equal(t, tc.counts, !strings.Contains(f.s.PendingBrief("designer", false), "inbox show "+id))
			assert.Equal(t, !tc.counts, len(f.s.BellWaits("designer", *f.now)) == 1)

			f.s.deliverInbox("designer", *f.now)

			for _, l := range lines {
				c := cardOn(t, f, "designer", l)
				assert.Equal(t, tc.counts, c.Seen != nil, "seen by the fetch")
			}

			assert.Equal(t, map[bool]int{true: 1, false: 0}[tc.counts], f.eventCount("fetched"))
		})
	}
}

// A fetch older than the bell's acceptance is the lines of an earlier bell of the same words: the brief does not take it
// for the role having read these.
func TestTheBriefIgnoresAFetchOlderThanTheBell(t *testing.T) {
	f, p := nativeFixture(t, bellYAML)
	upNative(t, f, "designer")

	body := bodyOf(fill("a", 700), fill("b", 700))
	id := inbox.IDOf("bell designer\x00" + body)
	require.NoError(t, RecordFetch(f.s.Crew.Layout(), "designer", id, "designer", f.now.Add(-time.Hour)))

	queueFrom(t, f, "designer", fill("a", 700), fill("b", 700))
	f.s.deliverInbox("designer", *f.now)
	p.showAll()
	f.s.deliverInbox("designer", *f.now)

	assert.Contains(t, f.s.PendingBrief("designer", false), "zmina inbox show "+id)
}

// The tail of a long file of fetches is read, the newest lines, and the line cut at the start of it is left out.
func TestFetchesAreReadFromTheTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "designer.fetched.jsonl")
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	var b strings.Builder

	old := fetchLine{ID: "in-aaaaaaaaaaaa", At: at, By: "designer"}
	raw, err := json.Marshal(old)
	require.NoError(t, err)

	for b.Len() < 2*bellFetchMax {
		b.Write(raw)
		b.WriteByte('\n')
	}

	last, err := json.Marshal(fetchLine{ID: "in-bbbbbbbbbbbb", At: at.Add(time.Hour), By: "designer"})
	require.NoError(t, err)
	b.Write(last)
	b.WriteByte('\n')

	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o600))

	got := readFetches(path)
	require.NotEmpty(t, got)
	assert.Equal(t, "in-bbbbbbbbbbbb", got[len(got)-1].ID, "the newest line is read")

	for _, f := range got {
		assert.True(t, inbox.ValidID(f.ID))
	}

	assert.Less(t, len(got), strings.Count(b.String(), "\n")*6/10, "the last bellFetchMax bytes of a file twice as big, not all of it")
}

// The text of a bell whose lines went back to the queue says so, and no fetch is noted for it: a failed bell's lines given
// back by queue retry, and a bell whose conversation ended before it showed.
func TestTheTextOfABellWhoseLinesWentBackIsSuperseded(t *testing.T) {
	supersededBody := func(t *testing.T, f fix, id string) inbox.Entry {
		t.Helper()

		e, err := f.s.inboxStore().Get(id)
		require.NoError(t, err)

		return e
	}

	t.Run("queue retry of a failed bell", func(t *testing.T) {
		f, _ := nativeFixture(t, bellYAML)
		upNative(t, f, "designer")

		lines := []string{fill("a", 700), fill("b", 700)}
		queueFrom(t, f, "designer", lines...)
		f.s.deliverInbox("designer", *f.now)

		rs := f.s.State.Role("designer")
		id := bellIDOf(t, rs.Posted[0].Line)

		*f.now = f.now.Add(postedWait)
		f.s.deliverInbox("designer", *f.now)
		require.Len(t, rs.Failed, 1)

		e := supersededBody(t, f, id)
		assert.Equal(t, numbered(lines), e.Body, "kept readable while it may have been read")
		assert.Equal(t, "designer", e.Role)

		_, _, err := f.s.Retry("designer", true)
		require.NoError(t, err)

		e = supersededBody(t, f, id)
		assert.Equal(t, BellSupersededText, e.Body)
		assert.Empty(t, e.Role, "reading it is no fetch")
		assert.NotContains(t, e.Body, lines[0])

		// The same lines rung again are their text again.
		f.s.deliverInbox("designer", *f.now)
		e = supersededBody(t, f, id)
		assert.Equal(t, numbered(lines), e.Body)
		assert.Equal(t, "designer", e.Role)
	})

	t.Run("a conversation that ended before the bell showed", func(t *testing.T) {
		f, _ := nativeFixture(t, bellYAML)
		upNative(t, f, "designer")

		lines := []string{fill("a", 700), fill("b", 700)}
		queueFrom(t, f, "designer", lines...)
		f.s.deliverInbox("designer", *f.now)
		id := bellIDOf(t, f.s.State.Role("designer").Posted[0].Line)

		f.rg.byPID[f.tm.up["designer"]].SessionID = "sid-designer-2"
		f.s.followPosted("designer", *f.now)

		assert.Equal(t, lines, f.s.State.Role("designer").Inbox, "back in the queue")
		assert.Equal(t, BellSupersededText, supersededBody(t, f, id).Body)
	})

	t.Run("a typed bell that may have reached the pane stays readable until a retry", func(t *testing.T) {
		f := typedFixture(t, bellYAML, "scribe")
		f.tm.typeErr = fmt.Errorf("tmux: send-keys timed out")

		queueFrom(t, f, "scribe", "one", "two")
		f.s.deliverInbox("scribe", *f.now)

		rs := f.s.State.Role("scribe")
		require.Len(t, rs.Failed, 1)
		id := bellIDOf(t, rs.Failed[0].Line)

		assert.Equal(t, "1. one\n2. two", supersededBody(t, f, id).Body)

		f.tm.typeErr = nil
		_, _, err := f.s.Retry("scribe", true)
		require.NoError(t, err)
		assert.Equal(t, BellSupersededText, supersededBody(t, f, id).Body)
	})
}

// A bell whose cards cannot be written makes no note: the attention on the cards is what says the dispatcher was told, and
// without it the note would be made again each beat.
func TestNoNoteForABellWhoseCardsCannotBeWritten(t *testing.T) {
	f, p := nativeFixture(t, bellYAML)
	upNative(t, f, "designer")

	shownBell(t, f, p)
	shown := *f.now

	path := f.s.Crew.Layout().DeliveryCards("designer")
	good, err := os.ReadFile(path)
	require.NoError(t, err)

	// The cards are loaded; then the file can no longer be written (a directory stands where it is).
	f.s.cards = nil
	require.NotNil(t, f.s.cardsOf("designer"))
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))

	*f.now = shown.Add(postedWait)
	assert.Empty(t, f.s.bellWatch(*f.now), "no durable guard, no note")
	assert.Equal(t, 1, f.eventCount("cards-failed"))
	assert.Zero(t, f.eventCount("bell-unfetched"))

	// The file is back (the cards are as they were): the one note is made, and once.
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, good, 0o600))
	f.s.cards = nil

	notes := f.s.bellWatch(*f.now)
	require.Len(t, notes, 1)
	assert.Empty(t, f.s.bellWatch(*f.now), "once")

	f.s.cards = nil
	assert.Empty(t, f.s.bellWatch(*f.now), "once, from the file too: the attention is on disk")
}

// After a clear the boot, which carries the sentence, waits at the tail of the queue: what is ahead of it goes as it always
// went, and the bells begin once the boot has left the queue.
func TestNoBellUntilTheQueuedBootHasLeft(t *testing.T) {
	f, p := nativeFixture(t, bellYAML)
	upNative(t, f, "designer")

	r, err := f.s.Role("designer")
	require.NoError(t, err)

	boot := f.s.bootText(r, false)
	rs := f.s.State.Role("designer")

	big := []string{fill("a", 700), fill("b", 700)}
	queueFrom(t, f, "designer", big...)
	f.s.enqueue("designer", boot, Facts{From: "zmina"})
	rs.QueuedBoot = boot

	assert.False(t, f.s.bellsOn("designer"))
	f.s.deliverInbox("designer", *f.now)

	require.Len(t, p.raw, 1)
	assert.Equal(t, tagged(f, "designer", "1. "+big[0]+"\n2. "+big[1]), p.raw[0], "as it went before the doorbell")
	assert.NoDirExists(t, f.s.Crew.Layout().Inbox())

	// The boot goes alone, and once it showed, the bells are back.
	p.showAll()
	f.s.deliverInbox("designer", *f.now)
	require.Len(t, p.raw, 2)
	assert.Equal(t, tagged(f, "designer", boot), p.raw[1])
	assert.Empty(t, rs.Inbox)
	assert.True(t, f.s.bellsOn("designer"), "QueuedBoot is still set, and the boot is not in the queue")

	p.showAll()
	f.s.deliverInbox("designer", *f.now)

	queueFrom(t, f, "designer", big...)
	f.s.deliverInbox("designer", *f.now)
	require.Len(t, p.raw, 3)
	assert.True(t, IsBell(strings.TrimPrefix(p.raw[2], tagMark(rs.Tag)+" ")))
}

// A text that is stored is stored again each time its bell is made, so that the keep starts again, even when an identical
// entry is there.
func TestBellRefreshesItsStoredTextEachTime(t *testing.T) {
	f, _ := nativeFixture(t, bellYAML)
	upNative(t, f, "designer")

	run := []string{fill("a", 700), fill("b", 700)}

	b := f.s.bellFor("designer", run, false, *f.now)
	require.NotNil(t, b)

	path := filepath.Join(f.s.Crew.Layout().Inbox(), b.id+".json")
	old := time.Now().Add(-29 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))

	again := f.s.bellFor("designer", run, false, *f.now)
	require.NotNil(t, again)
	assert.Equal(t, b.id, again.id)

	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), st.ModTime(), time.Minute, "kept again")

	entries, err := f.s.inboxStore().List()
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

// A bell that failed as never shown and was fetched all the same had its lines read: its failed record is closed, the cards
// are seen, and queue retry has nothing to say twice. A fetch by another role closes nothing.
func TestAFailedBellThatWasFetchedIsClosed(t *testing.T) {
	setup := func(t *testing.T) (fix, string, []string) {
		t.Helper()

		f, _ := nativeFixture(t, bellYAML)
		upNative(t, f, "designer")

		lines := []string{fill("a", 700), fill("b", 700)}
		queueFrom(t, f, "designer", lines...)
		f.s.deliverInbox("designer", *f.now)

		id := bellIDOf(t, f.s.State.Role("designer").Posted[0].Line)

		*f.now = f.now.Add(postedWait)
		f.s.deliverInbox("designer", *f.now)
		require.Len(t, f.s.State.Role("designer").Failed, 1)

		return f, id, lines
	}

	t.Run("by the role", func(t *testing.T) {
		f, id, lines := setup(t)

		*f.now = f.now.Add(time.Minute)
		require.NoError(t, RecordFetch(f.s.Crew.Layout(), "designer", id, "designer", *f.now))
		f.s.deliverInbox("designer", *f.now)

		rs := f.s.State.Role("designer")
		assert.Empty(t, rs.Failed, "its lines were read")

		for _, l := range lines {
			c := cardOn(t, f, "designer", l)
			require.NotNil(t, c.Seen)
			assert.Equal(t, SeenFetch, c.Seen.By)
		}

		var said string

		for _, e := range f.ev.list {
			if e.Event == "fetched" {
				said = e.Detail
			}
		}

		assert.Contains(t, said, "its failed record closed")

		n, left, err := f.s.Retry("designer", true)
		require.NoError(t, err)
		assert.Zero(t, n+left)
		assert.Empty(t, rs.Inbox, "not delivered again")
	})

	t.Run("by another role", func(t *testing.T) {
		f, id, _ := setup(t)

		*f.now = f.now.Add(time.Minute)
		require.NoError(t, RecordFetch(f.s.Crew.Layout(), "designer", id, "dispatcher", *f.now))
		f.s.deliverInbox("designer", *f.now)

		assert.Len(t, f.s.State.Role("designer").Failed, 1)
	})
}
