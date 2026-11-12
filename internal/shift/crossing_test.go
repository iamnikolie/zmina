package shift

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/chat"
	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/inbox"
	"github.com/iamnikolie/zmina/internal/peer"
)

// crossYAML is a crew whose clock is UTC, with a role that reads on its own channel (dev) and one that wrote to it (ops).
// The doorbell is off, so that a line goes whole unless a test turns it on.
const crossYAML = `namespace: /ns
claude: claude
timezone: UTC
doorbell: off
roles:
  dispatcher:
    boot: read d and boot
  dev:
    boot: read the brief
  ops:
`

// at is a moment of the day the tests run on (the crew's clock is UTC).
func at(hour, minute int) time.Time { return time.Date(2026, 10, 24, hour, minute, 0, 0, time.UTC) }

// putCards writes a role's card file as zmina leaves it, and forgets what the shift had read of it. What the shift holds of
// other cards is written first, so that a line queued before is not lost with the cache.
func putCards(t *testing.T, f fix, role string, cards ...Card) {
	t.Helper()

	f.s.FlushCards()

	l := f.s.Crew.Layout()
	require.NoError(t, writeCardFile(l, l.DeliveryCards(role), cards))

	f.s.cards = nil
}

// devLine is the card of a line dev wrote to ops about a task, as ops' card file keeps it: the line's words are only its id.
func devLine(words, task string, written time.Time, mod ...func(*Card)) Card {
	c := Card{ID: CardID("ops", words), From: "dev", Task: task, Head: words, Written: written}

	for _, m := range mod {
		m(&c)
	}

	return c
}

// opsWrote queues for dev, at 14:02, a line ops wrote about a task (none when task is empty), and moves the clock to the
// minute the line is delivered in, 14:06.
func opsWrote(t *testing.T, f fix, line, task string) {
	t.Helper()

	*f.now = at(14, 2)
	require.NoError(t, f.s.QueueFrom("dev", line, Facts{From: "ops", Task: task}))

	*f.now = at(14, 6)
}

// scene is a dev that reads over its own channel, and a line ops wrote it at 14:02 about T-1 whose words say nothing of it.
func scene(t *testing.T, body string) (fix, *fakePeer, string) {
	t.Helper()

	f, p := nativeFixture(t, body)
	upNative(t, f, "dev")

	const line = "please review the change"
	opsWrote(t, f, line, "T-1")

	return f, p, line
}

// The line is delivered with the mark when dev wrote ops a newer line on the task: the mark stands before its words, and
// the queue, the record of the post, the log's line and the ids stay the line as it was written.
func TestCrossingMarksALineWrittenBeforeTheReadersLastLine(t *testing.T) {
	f, p, line := scene(t, crossYAML)
	c := devLine("how far are you", "T-1", at(14, 5))
	putCards(t, f, "ops", c)

	did := f.s.deliverInbox("dev", *f.now)

	want := "[written 14:02, before your last line on T-1 (14:05)] " + line
	assert.Equal(t, []string{tagged(f, "dev", want)}, p.raw)
	assert.Equal(t, "posted to dev: "+line, did)

	rs := f.s.State.Role("dev")
	assert.Empty(t, rs.Inbox)
	require.Len(t, rs.Posted, 1)
	assert.Equal(t, line, rs.Posted[0].Line, "the record is the line as it was queued")
	assert.Nil(t, rs.Posted[0].Parts)

	for _, e := range f.ev.list {
		if e.Event == "posted" {
			assert.Equal(t, want, e.Detail, "the log says what went out")
		}
	}

	card := cardOn(t, f, "dev", line)
	require.NotNil(t, card.Crossed)
	assert.Equal(t, CardCrossed{With: c.ID, Kind: CrossNewer, At: *f.now}, *card.Crossed)
	require.NotNil(t, card.Accepted)
	assert.Equal(t, "ops", card.From)
	assert.Equal(t, "T-1", card.Task)

	// It is seen by the message's id, whatever stands before its words.
	*f.now = f.now.Add(time.Minute)
	p.showAll()
	f.s.followPosted("dev", *f.now)
	f.s.FlushCards()

	assert.Empty(t, f.s.State.Role("dev").Posted)

	card = cardOn(t, f, "dev", line)
	require.NotNil(t, card.Seen)
	assert.Equal(t, SeenTranscript, card.Seen.By)
	require.NotNil(t, card.Crossed, "what was seen of the line did not take the mark off its card")
}

// The line is also marked when dev's last line to ops was written before it, and ops had not read it by then.
func TestCrossingMarksALineWrittenBeforeItsWriterHadReadTheReadersLine(t *testing.T) {
	f, p, line := scene(t, crossYAML)
	c := devLine("how far are you", "T-1", at(13, 50))
	putCards(t, f, "ops", c)

	f.s.deliverInbox("dev", *f.now)

	want := "[written 14:02, before ops had read your line of 13:50 on T-1] " + line
	assert.Equal(t, []string{tagged(f, "dev", want)}, p.raw)

	card := cardOn(t, f, "dev", line)
	require.NotNil(t, card.Crossed)
	assert.Equal(t, CardCrossed{With: c.ID, Kind: CrossUnread, At: *f.now}, *card.Crossed)
	assert.Equal(t, line, f.s.State.Role("dev").Posted[0].Line)
}

// Only the newest of dev's lines to ops on the task counts, and a line of another task is not one of them.
func TestCrossingLooksAtTheNewestLineOnTheTask(t *testing.T) {
	f, p, line := scene(t, crossYAML)
	putCards(t, f, "ops",
		devLine("first", "T-1", at(13, 40), func(c *Card) { c.Seen = &CardSeen{At: at(13, 41), By: SeenTranscript} }),
		devLine("second", "T-1", at(13, 50)),
		devLine("about the other task", "T-2", at(13, 59)),
	)

	f.s.deliverInbox("dev", *f.now)

	assert.Equal(t, []string{tagged(f, "dev", "[written 14:02, before ops had read your line of 13:50 on T-1] "+line)}, p.raw,
		"the first line was read, the newest was not, and the other task's is none of them")

	assert.Equal(t, CardID("ops", "second"), cardOn(t, f, "dev", line).Crossed.With)
}

// A line dev wrote that ops had read before it wrote is no crossing: whatever fact says so, at or before the moment it was
// written; a read after it, or no read, is.
func TestCrossingNeedsNoMarkWhenTheWriterHadReadTheLine(t *testing.T) {
	const unread = "[written 14:02, before ops had read your line of 13:50 on T-1] "

	for _, k := range []struct {
		name string
		mod  func(*Card)
		mark string
	}{
		{"never read", func(*Card) {}, unread},
		{"seen in the transcript before", func(c *Card) { c.Seen = &CardSeen{At: at(13, 55), By: SeenTranscript} }, ""},
		{"taken by a bridge before", func(c *Card) { c.Seen = &CardSeen{At: at(14, 1), By: SeenTaken} }, ""},
		{"its doorbell fetched before", func(c *Card) {
			c.Bell = &CardBell{Body: "in-0123456789ab", Shown: at(13, 51)}
			c.Seen = &CardSeen{At: at(13, 59), By: SeenFetch}
		}, ""},
		{"seen the moment it was written", func(c *Card) { c.Seen = &CardSeen{At: at(14, 2), By: SeenTranscript} }, ""},
		{"typed and landed before", func(c *Card) { c.Closed = &CardClosed{At: at(13, 52), How: ClosedLanded} }, ""},
		{"acked before", func(c *Card) { c.Closed = &CardClosed{At: at(13, 58), How: ClosedAcked} }, ""},
		{"seen after it was written", func(c *Card) { c.Seen = &CardSeen{At: at(14, 3), By: SeenTranscript} }, unread},
		{"typed and landed after it was written", func(c *Card) { c.Closed = &CardClosed{At: at(14, 4), How: ClosedLanded} }, unread},
		{"failed: it never reached", func(c *Card) { c.Closed = &CardClosed{At: at(13, 52), How: ClosedFailed} }, unread},
		{"a typed doorbell landed: its lines are not read until fetched", func(c *Card) {
			c.Bell = &CardBell{Body: "in-0123456789ab"}
			c.Closed = &CardClosed{At: at(13, 52), How: ClosedLanded}
		}, unread},
	} {
		t.Run(k.name, func(t *testing.T) {
			f, p, line := scene(t, crossYAML)
			putCards(t, f, "ops", devLine("how far are you", "T-1", at(13, 50), k.mod))

			f.s.deliverInbox("dev", *f.now)

			assert.Equal(t, []string{tagged(f, "dev", k.mark+line)}, p.raw)
			assert.Equal(t, k.mark != "", cardOn(t, f, "dev", line).Crossed != nil)
		})
	}
}

// What never crosses: each case changes one thing of a line that is marked, and the line goes as it was written.
func TestCrossingNeverMarksWhatIsNotAStructuralLineOnATaskOfBoth(t *testing.T) {
	// The control: the scene's own line is marked.
	f, p, _ := scene(t, crossYAML)
	putCards(t, f, "ops", devLine("how far are you", "T-1", at(13, 50)))
	f.s.deliverInbox("dev", *f.now)
	require.True(t, strings.HasPrefix(p.raw[0], tagMark(f.s.State.Role("dev").Tag)+" [written 14:02, before ops had read"), p.raw[0])

	cases := []struct {
		name  string
		from  string // who wrote the line; "ops" unless said
		task  string // the task of the line; T-1 unless said
		cards map[string][]Card
	}{
		{name: "no task on the line", task: "-", cards: map[string][]Card{
			"ops": {devLine("how far are you", "T-1", at(13, 50))}}},
		{name: "no line of dev's to ops at all", cards: map[string][]Card{}},
		{name: "ops' file has lines of others to it", cards: map[string][]Card{
			"ops": {{ID: CardID("ops", "other"), From: "owner", Task: "T-1", Written: at(13, 50)},
				{ID: CardID("ops", "zmina"), From: "zmina", Task: "T-1", Written: at(13, 50)}}}},
		{name: "dev wrote ops about another task", cards: map[string][]Card{
			"ops": {devLine("how far are you", "T-2", at(13, 50))}}},
		{name: "dev wrote someone else about the task", cards: map[string][]Card{
			"dispatcher": {{ID: CardID("dispatcher", "how far are you"), From: "dev", Task: "T-1", Written: at(13, 50)}}}},
		{name: "a card of dev's with no written time", cards: map[string][]Card{
			"ops": {devLine("how far are you", "T-1", time.Time{})}}},
		{name: "the line is zmina's", from: "zmina", cards: map[string][]Card{
			"zmina": {devLine("how far are you", "T-1", at(13, 50))}, "ops": {devLine("how far are you", "T-1", at(13, 50))}}},
		{name: "the line is the owner's", from: "owner", cards: map[string][]Card{
			"owner": {devLine("how far are you", "T-1", at(13, 50))}, "ops": {devLine("how far are you", "T-1", at(13, 50))}}},
	}

	for _, k := range cases {
		t.Run(k.name, func(t *testing.T) {
			f, p := nativeFixture(t, crossYAML)
			upNative(t, f, "dev")

			from, task := k.from, k.task
			if from == "" {
				from = "ops"
			}

			switch task {
			case "":
				task = "T-1"
			case "-":
				task = ""
			}

			*f.now = at(14, 2)
			require.NoError(t, f.s.QueueFrom("dev", "please review the change", Facts{From: from, Task: task}))
			*f.now = at(14, 6)

			for role, cards := range k.cards {
				putCards(t, f, role, cards...)
			}

			f.s.deliverInbox("dev", *f.now)

			assert.Equal(t, []string{tagged(f, "dev", "please review the change")}, p.raw)
			assert.Nil(t, cardOn(t, f, "dev", "please review the change").Crossed)
		})
	}
}

// A line that goes alone and is never read with others is never marked, whatever it was written about: a slash command,
// the rename, the boot, the owner's own message, a task packet, a line only its conversation reads, a line stored whole.
func TestCrossingNeverMarksALineThatIsNotJoinable(t *testing.T) {
	f, _ := nativeFixture(t, crossYAML)
	upNative(t, f, "dev")

	const packet = `[zmina T-1] Read handoff packet "/state/handoff/h-1.json"; before work run: zmina task accept T-1 --handoff h-1 --role dev`

	rs := f.s.State.Role("dev")
	rs.QueuedRename = "/rename zm-dev"
	rs.QueuedBoot = "read the brief and go"
	owner := chat.Prefix("web:1") + "can you look"
	rs.ChatReceipts = append(rs.ChatReceipts, chat.Receipt{ID: "1", Line: owner})

	putCards(t, f, "ops", devLine("how far are you", "T-1", at(13, 50)))

	// The control: a plain line written at 14:02 about T-1.
	require.NotNil(t, f.s.crossing("dev", "please review the change", "ops", "T-1", at(14, 2)))

	for name, line := range map[string]string{
		"a slash command":       "/compact",
		"the rename":            "/rename zm-dev",
		"the boot":              "read the brief and go",
		"the owner's own":       owner,
		"a task packet":         packet,
		"a release request":     "[zmina release] park what you hold and say so",
		"a recovery pointer":    "[zmina recovery] read the pack",
		"a line stored whole":   "the head of a long line… (+31000 chars, full: zmina inbox show in-0123456789ab) (zmina S-99)",
		"a restart request":     "[zmina] restart: write your note",
		"a restart ahead":       "[zmina] restart ahead: wrap up",
		"the idle release line": "[zmina] idle for a while: you are released and go down",
	} {
		assert.Nil(t, f.s.crossing("dev", line, "ops", "T-1", at(14, 2)), name)
	}

	// Through a delivery: a task packet at the head of the queue is posted as it is.
	*f.now = at(14, 2)
	f.s.enqueue("dev", packet, Facts{From: "ops", Task: "T-1"})
	*f.now = at(14, 6)

	p := f.s.Peer.(*fakePeer)
	f.s.deliverInbox("dev", *f.now)
	assert.Equal(t, []string{tagged(f, "dev", packet)}, p.raw)
}

// Lines joined in one post carry their marks after their number, each with its own; the ids, the parts and the card of each
// line stay what they were, and the size of the post counts what goes out.
func TestCrossingMarkInAJoinedPostStandsAfterTheNumber(t *testing.T) {
	f, p := nativeFixture(t, crossYAML)
	upNative(t, f, "dev")

	putCards(t, f, "ops", devLine("how far are you", "T-1", at(14, 5)))

	*f.now = at(14, 2)

	var (
		lines []string
		ids   []string
	)

	for _, q := range []struct{ text, task string }{
		{"first, about the task", "T-1"},
		{"second, about nothing", ""},
		{"third, about the task", "T-1"},
	} {
		id, err := f.s.QueueTrackedFrom("dev", q.text, Facts{From: "ops", Task: q.task})
		require.NoError(t, err)

		rs := f.s.State.Role("dev")
		lines = append(lines, rs.Inbox[len(rs.Inbox)-1])
		ids = append(ids, id)
	}

	*f.now = at(14, 6)

	did := f.s.deliverInbox("dev", *f.now)

	const mark = "[written 14:02, before your last line on T-1 (14:05)] "

	want := "1. " + mark + lines[0] + "\n2. " + lines[1] + "\n3. " + mark + lines[2]
	require.Equal(t, []string{tagged(f, "dev", want)}, p.raw)
	assert.Equal(t, "posted to dev: 3 lines joined: "+strings.ReplaceAll(want, "\n", " "), did)

	rs := f.s.State.Role("dev")
	assert.Empty(t, rs.Inbox)
	assert.Empty(t, rs.QueuedIDs, "each line's ids were taken, once")

	require.Len(t, rs.Posted, 1)
	rec := rs.Posted[0]
	assert.Equal(t, want, rec.Line, "the post is the text that went out")
	assert.Equal(t, ids, rec.IDs)
	assert.Equal(t, []Part{{Line: lines[0], IDs: ids[:1]}, {Line: lines[1], IDs: ids[1:2]}, {Line: lines[2], IDs: ids[2:]}}, rec.Parts,
		"the parts are the lines as they were written")

	for i, id := range ids {
		assert.Equal(t, *f.now, f.s.State.Acks[id].First, "delivered: said again by its ack as ever")
		assert.Equal(t, strings.TrimSuffix(strings.TrimPrefix(lines[i], ""), " (zmina "+id+")"), f.s.State.Acks[id].Text,
			"an ack keeps the words, never the mark")
	}

	assert.Equal(t, 1, f.eventCount("posted"))

	for i, k := range []string{CrossNewer, "", CrossNewer} {
		card := cardOn(t, f, "dev", lines[i])
		if k == "" {
			assert.Nil(t, card.Crossed, lines[i])

			continue
		}

		require.NotNil(t, card.Crossed, lines[i])
		assert.Equal(t, k, card.Crossed.Kind)
		require.NotNil(t, card.Accepted)
	}

	// Each line of the post is seen by the message's id, and the post is settled.
	*f.now = f.now.Add(time.Minute)
	p.showAll()
	f.s.followPosted("dev", *f.now)
	f.s.FlushCards()

	assert.Empty(t, f.s.State.Role("dev").Posted)

	for _, l := range lines {
		require.NotNil(t, cardOn(t, f, "dev", l).Seen, l)
	}
}

// The text of a post with marks is what the bound is measured on: two lines that fit the message with none do not fit it with
// the mark of the second, and the second waits for the next pass.
func TestCrossingMarkCountsAgainstTheSizeOfAJoinedPost(t *testing.T) {
	a := fill("a", 6000)
	b := fill("b", postMax-len("1. ")-len(a)-len("\n2. "))

	run := func(t *testing.T, body string) (fix, *fakePeer) {
		f, p := nativeFixture(t, body)
		upNative(t, f, "dev")
		putCards(t, f, "ops", devLine("how far are you", "T-1", at(14, 5)))

		*f.now = at(14, 2)
		require.NoError(t, f.s.QueueFrom("dev", a, Facts{From: "ops"}))
		require.NoError(t, f.s.QueueFrom("dev", b, Facts{From: "ops", Task: "T-1"}))
		*f.now = at(14, 6)

		return f, p
	}

	t.Run("without marks the two go together, filling the message", func(t *testing.T) {
		f, p := run(t, "crossing: off\n"+crossYAML)

		assert.Equal(t, []string{a, b}, f.s.joinRun("dev"))

		f.s.deliverInbox("dev", *f.now)
		require.Len(t, p.raw, 1)
		assert.Len(t, p.raw[0], len(tagMark(f.s.State.Role("dev").Tag))+1+postMax)
	})

	t.Run("with its mark the second does not fit", func(t *testing.T) {
		f, p := run(t, crossYAML)

		assert.Equal(t, []string{a}, f.s.joinRun("dev"))

		f.s.deliverInbox("dev", *f.now)
		assert.Equal(t, []string{tagged(f, "dev", a)}, p.raw, "the first alone, as a run of one")
		assert.Equal(t, []string{b}, f.s.State.Role("dev").Inbox)

		// The second goes by itself on the next pass, behind its mark and recorded as it was queued.
		*f.now = f.now.Add(time.Minute)
		f.s.State.Role("dev").Posted = nil
		f.s.deliverInbox("dev", *f.now)
		require.Len(t, p.raw, 2)
		assert.Equal(t, tagged(f, "dev", "[written 14:02, before your last line on T-1 (14:05)] "+b), p.raw[1])
		assert.Equal(t, b, f.s.State.Role("dev").Posted[0].Line)
	})
}

// A mark counts against the doorbell's threshold too: the lines that fill the message to its bound with none are a bell
// with one.
func TestCrossingMarkCountsAgainstTheDoorbellThreshold(t *testing.T) {
	a, b := fill("a", 600), fill("b", bellInline-len("1. ")-600-len("\n2. "))

	run := func(t *testing.T, body string) (fix, *fakePeer) {
		f, p := nativeFixture(t, body)
		upNative(t, f, "dev")
		putCards(t, f, "ops", devLine("how far are you", "T-1", at(14, 5)))

		*f.now = at(14, 2)
		require.NoError(t, f.s.QueueFrom("dev", a, Facts{From: "ops"}))
		require.NoError(t, f.s.QueueFrom("dev", b, Facts{From: "ops", Task: "T-1"}))
		*f.now = at(14, 6)

		return f, p
	}

	f, p := run(t, "crossing: off\n"+strings.Replace(crossYAML, "doorbell: off\n", "", 1))
	f.s.deliverInbox("dev", *f.now)
	assert.Equal(t, []string{tagged(f, "dev", "1. "+a+"\n2. "+b)}, p.raw, "at the bound: a numbered post")

	f, p = run(t, strings.Replace(crossYAML, "doorbell: off\n", "", 1))
	f.s.deliverInbox("dev", *f.now)
	require.Len(t, p.raw, 1)
	assert.True(t, IsBell(tagRe.ReplaceAllString(p.raw[0], "")), "with the mark it is over: %s", p.raw[0])
}

// A bell keeps the marks inside its stored text with their lines, says how many lines crossed, and keeps the lines as they
// were written in the record and on the cards.
func TestCrossingMarkInABellBodyAndItsCount(t *testing.T) {
	bellCrew := strings.Replace(crossYAML, "doorbell: off\n", "", 1)

	for _, k := range []struct {
		name    string
		tasks   [2]string
		crossed string
	}{
		{"both lines crossed", [2]string{"T-1", "T-1"}, " (ops on T-1 · 2 crossed)"},
		{"one line crossed", [2]string{"T-1", ""}, " (ops on T-1 · 1 crossed)"},
	} {
		t.Run(k.name, func(t *testing.T) {
			f, p := nativeFixture(t, bellCrew)
			upNative(t, f, "dev")
			putCards(t, f, "ops", devLine("how far are you", "T-1", at(14, 5)))

			*f.now = at(14, 2)

			var lines, ids []string

			for i, text := range []string{fill("a", 700), fill("b", 700)} {
				id, err := f.s.QueueTrackedFrom("dev", text, Facts{From: "ops", Task: k.tasks[i]})
				require.NoError(t, err)

				rs := f.s.State.Role("dev")
				lines = append(lines, rs.Inbox[len(rs.Inbox)-1])
				ids = append(ids, id)
			}

			*f.now = at(14, 6)

			const mark = "[written 14:02, before your last line on T-1 (14:05)] "

			var marks []*crossMark

			for _, task := range k.tasks {
				if task == "" {
					marks = append(marks, nil)

					continue
				}

				marks = append(marks, &crossMark{text: mark})
			}

			body := numberedMarked(lines, marks)
			wantBell := "[zmina] 2 lines wait for you" + k.crossed + ": zmina inbox show " + inbox.IDOf("bell dev\x00"+body)

			did := f.s.deliverInbox("dev", *f.now)

			require.Equal(t, []string{tagged(f, "dev", wantBell)}, p.raw)
			assert.Equal(t, "posted to dev: bell for 2 lines: "+wantBell, did)
			assert.True(t, IsBell(wantBell), "it still reads as a bell")

			stored, err := f.s.inboxStore().Get(bellIDOf(t, wantBell))
			require.NoError(t, err)
			assert.Equal(t, body, stored.Body)
			assert.Contains(t, stored.Body, "1. "+mark+lines[0], "the mark is inside the stored body, with its line")

			rs := f.s.State.Role("dev")
			require.Len(t, rs.Posted, 1)
			assert.Equal(t, wantBell, rs.Posted[0].Line)
			assert.Equal(t, []Part{{Line: lines[0], IDs: ids[:1]}, {Line: lines[1], IDs: ids[1:]}}, rs.Posted[0].Parts)

			for i, task := range k.tasks {
				card := cardOn(t, f, "dev", lines[i])
				require.NotNil(t, card.Bell)
				assert.Equal(t, task != "", card.Crossed != nil, lines[i])
			}
		})
	}
}

// A line that crossed and goes typed is typed behind its mark, and is the queue's line in everything after it: the record
// of what was typed, the card, the closing when it lands. The input line that holds the typed text is the line behind its mark.
func TestCrossingTypedLineLandsAndIsRecordedAsWritten(t *testing.T) {
	f := typedFixture(t, crossYAML, "dev")
	putCards(t, f, "ops", devLine("how far are you", "T-1", at(14, 5)))

	*f.now = at(14, 2)
	const line = "please review the change"
	require.NoError(t, f.s.QueueFrom("dev", line, Facts{From: "ops", Task: "T-1"}))

	*f.now = at(14, 6)
	f.status("dev", "idle", *f.now)

	did := f.s.deliverInbox("dev", *f.now)

	const typed = "[written 14:02, before your last line on T-1 (14:05)] " + line

	assert.Equal(t, []string{typed}, f.tm.typed["dev"])
	assert.Equal(t, "typed into dev: "+line, did)

	rs := f.s.State.Role("dev")
	require.NotNil(t, rs.Sent)
	assert.Equal(t, line, rs.Sent.Line, "what is kept of the typed line is the line")
	assert.Empty(t, rs.Inbox)

	card := cardOn(t, f, "dev", line)
	require.NotNil(t, card.Crossed)
	assert.Equal(t, CrossNewer, card.Crossed.Kind)
	require.NotNil(t, card.Accepted)
	assert.Equal(t, AcceptedTyped, card.Accepted.How)

	// The role's status moved after the line, yet its input line still holds the line behind its mark: it has not landed. The
	// check looks for what was typed, and a draft that opens with the mark is the line.
	f.tm.drafts["dev"] = typed
	*f.now = f.now.Add(time.Second)
	assert.False(t, f.s.landed("dev", *f.now), "its text is still in the input line")
	require.NotNil(t, f.s.State.Role("dev").Sent)

	// The input line shows only the start of the mark: still the line.
	f.tm.drafts["dev"] = typed[:30]
	assert.False(t, f.s.landed("dev", *f.now))

	// The input line empty: it landed, and its card is closed as the line's.
	delete(f.tm.drafts, "dev")
	assert.True(t, f.s.landed("dev", *f.now))
	assert.Nil(t, f.s.State.Role("dev").Sent)
	f.s.FlushCards()

	card = cardOn(t, f, "dev", line)
	require.NotNil(t, card.Closed)
	assert.Equal(t, ClosedLanded, card.Closed.How)
	require.NotNil(t, card.Crossed)
}

// A typed line that is never taken fails as the line it was queued as; a draft that is the owner's, which opens with no mark,
// is not taken for it.
func TestCrossingTypedLineThatIsNeverTakenFailsAsTheOriginalLine(t *testing.T) {
	f := typedFixture(t, crossYAML, "dev")
	putCards(t, f, "ops", devLine("how far are you", "T-1", at(14, 5)))

	*f.now = at(14, 2)
	const line = "please review the change"
	require.NoError(t, f.s.QueueFrom("dev", line, Facts{From: "ops", Task: "T-1"}))

	*f.now = at(14, 6)
	f.status("dev", "idle", *f.now)
	f.s.deliverInbox("dev", *f.now)

	const typed = "[written 14:02, before your last line on T-1 (14:05)] " + line

	require.Equal(t, []string{typed}, f.tm.typed["dev"])

	// The role takes no Enter: the typed text sits in its input line, behind its mark, for as long as zmina looks.
	f.tm.enterLost = map[string]int{"dev": 100}
	f.tm.drafts["dev"] = typed

	for range typeTries {
		*f.now = f.now.Add(confirmAfter)
		f.s.landed("dev", *f.now)
	}

	rs := f.s.State.Role("dev")
	assert.Nil(t, rs.Sent)
	require.Len(t, rs.Failed, 1)
	assert.Equal(t, line, rs.Failed[0].Line, "kept as the line was written")
	assert.Equal(t, "typed, never taken", rs.Failed[0].Error)
	assert.NotContains(t, f.tm.keys["dev"], "Enter\x00", "")
	assert.Contains(t, f.tm.keys["dev"], "C-u", "the typed text was cleared from the input line")

	// A retry brings the line back as it was written; it is marked again when it goes again.
	n, _, err := f.s.Retry("dev", false)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	assert.Equal(t, []string{line}, f.s.State.Role("dev").Inbox)

	f.s.FlushCards()
	assert.Nil(t, cardOn(t, f, "dev", line).Crossed, "the mark of an earlier delivery is not the new one's")
}

// A typed line whose typing fails is kept as the line it was queued as, in the queue while it is tried and on the failed list
// after; and one that may have reached the pane is kept for a look as that line.
func TestCrossingTypedLineKeepsTheOriginalWhereItsTypingFails(t *testing.T) {
	const line = "please review the change"

	prep := func(t *testing.T) fix {
		f := typedFixture(t, crossYAML, "dev")
		putCards(t, f, "ops", devLine("how far are you", "T-1", at(14, 5)))

		*f.now = at(14, 2)
		require.NoError(t, f.s.QueueFrom("dev", line, Facts{From: "ops", Task: "T-1"}))
		*f.now = at(14, 6)
		f.status("dev", "idle", *f.now)

		return f
	}

	const typed = "[written 14:02, before your last line on T-1 (14:05)] " + line

	t.Run("counted: nothing reached the pane", func(t *testing.T) {
		f := prep(t)
		f.tm.typeErr = errNotTyped("tmux: no server")

		f.s.deliverInbox("dev", *f.now)
		assert.Equal(t, []string{line}, f.s.State.Role("dev").Inbox, "still in the queue as written, one try counted")
		assert.Equal(t, 1, f.s.State.Role("dev").Attempts)

		for range typeTries - 1 {
			*f.now = f.now.Add(countedGap)
			f.s.deliverInbox("dev", *f.now)
		}

		rs := f.s.State.Role("dev")
		assert.Empty(t, rs.Inbox)
		require.Len(t, rs.Failed, 1)
		assert.Equal(t, line, rs.Failed[0].Line)
		assert.False(t, rs.Failed[0].Unclear())

		c := cardOn(t, f, "dev", line)
		require.NotNil(t, c.Closed)
		assert.Equal(t, ClosedFailed, c.Closed.How)
		assert.Nil(t, c.Crossed, "it never went: no mark was delivered")

		n, _, err := f.s.Retry("dev", false)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		assert.Equal(t, []string{line}, f.s.State.Role("dev").Inbox, "retried as written")
	})

	t.Run("unclear: it may have reached the pane", func(t *testing.T) {
		f := prep(t)
		f.tm.typeErr = errors.New("tmux: send-keys timed out")

		f.s.deliverInbox("dev", *f.now)

		rs := f.s.State.Role("dev")
		assert.Empty(t, rs.Inbox)
		require.Len(t, rs.Failed, 1)
		assert.Equal(t, line, rs.Failed[0].Line)
		assert.True(t, rs.Failed[0].Unclear())
		assert.Equal(t, typed, rs.Partial, "what is looked for in the input line is what was typed")

		c := cardOn(t, f, "dev", line)
		require.NotNil(t, c.Attention)
	})

	t.Run("the typed start is cleared from the input line", func(t *testing.T) {
		f := prep(t)
		f.tm.typeErr = errors.New("tmux: send-keys timed out")
		f.tm.typeLeaves = true

		f.s.deliverInbox("dev", *f.now)

		assert.Empty(t, f.tm.drafts["dev"], "what was typed, behind its mark, was taken out again")
		assert.Contains(t, f.tm.keys["dev"], "C-u")
		assert.Equal(t, []string{line}, f.s.State.Role("dev").Inbox)
	})
}

// A posted line given back to its queue (its conversation ended), and one that never showed and was retried, are the line as
// it was written: the mark is made again when it goes again, from the cards of that time.
func TestCrossingGivenBackAndRetriedLinesAreTheOriginalLine(t *testing.T) {
	t.Run("given back", func(t *testing.T) {
		f, p, line := scene(t, crossYAML)
		putCards(t, f, "ops", devLine("how far are you", "T-1", at(13, 50)))

		f.s.deliverInbox("dev", *f.now)
		require.Len(t, p.raw, 1)
		assert.Contains(t, p.raw[0], "[written 14:02, before ops had read")
		require.NotNil(t, cardOn(t, f, "dev", line).Crossed)

		require.NoError(t, f.s.Down("dev", DownOptions{}))

		for range 3 {
			*f.now = f.now.Add(time.Minute)
			f.s.Tick()
		}

		require.Equal(t, []string{line}, f.s.State.Role("dev").Inbox, "back at the head as it was written")

		c := cardOn(t, f, "dev", line)
		require.NotNil(t, c.Back)
		assert.Nil(t, c.Crossed, "the mark was of the delivery that did not reach the role")

		// Dev wrote ops again meanwhile: the new delivery is marked from the cards of its own time.
		putCards(t, f, "ops", devLine("how far are you", "T-1", at(14, 8)))

		require.NoError(t, f.s.Up("dev"))
		restarted(f, "dev", "sid-dev-2")

		for range 3 {
			*f.now = f.now.Add(time.Minute)
			f.s.Tick()
		}

		require.Len(t, p.raw, 2)
		assert.Equal(t, tagged(f, "dev", "[written 14:02, before your last line on T-1 (14:08)] "+line), p.raw[1])

		c = cardOn(t, f, "dev", line)
		require.NotNil(t, c.Crossed)
		assert.Equal(t, CrossNewer, c.Crossed.Kind)
		assert.Equal(t, 1, c.Back.N)
	})

	t.Run("never shown, kept as failed, retried", func(t *testing.T) {
		f, _, line := scene(t, crossYAML)
		putCards(t, f, "ops", devLine("how far are you", "T-1", at(13, 50)))

		require.NotEmpty(t, f.s.deliverInbox("dev", *f.now))

		*f.now = f.now.Add(postedWait)
		f.s.deliverInbox("dev", *f.now)

		rs := f.s.State.Role("dev")
		require.Len(t, rs.Failed, 1)
		assert.Equal(t, line, rs.Failed[0].Line, "kept as it was written")
		assert.Equal(t, postedNever, rs.Failed[0].Error)

		n, _, err := f.s.Retry("dev", true)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		assert.Equal(t, []string{line}, f.s.State.Role("dev").Inbox)
	})
}

// A crew with crossing: off delivers every line as it was written, and does not tell its roles of the flag's use.
func TestCrossingOffDeliversEveryLineAsWritten(t *testing.T) {
	f, p, line := scene(t, "crossing: off\n"+crossYAML)
	putCards(t, f, "ops", devLine("how far are you", "T-1", at(14, 5)))

	f.s.deliverInbox("dev", *f.now)

	assert.Equal(t, []string{tagged(f, "dev", line)}, p.raw)
	assert.Nil(t, cardOn(t, f, "dev", line).Crossed)

	r := f.s.Crew.Roles["dev"]
	assert.NotContains(t, f.s.BootText(r), CrossingRule)
	assert.Contains(t, f.s.BootText(r), WaitRule, "the other rules are told as ever")

	on, _, _ := scene(t, crossYAML)
	assert.Contains(t, on.s.BootText(on.s.Crew.Roles["dev"]), CrossingRule)
}

// The cards of the line's writer are read, never written, and a file that is missing or does not read is no mark and no
// error: the line goes as it was written, and nothing is moved aside or told.
func TestCrossingWithoutTheWritersCardsGoesUnmarkedAndTouchesNothing(t *testing.T) {
	good := devLine("how far are you", "T-1", at(14, 5))

	for _, k := range []struct {
		name string
		put  func(t *testing.T, f fix, path string)
	}{
		{"no file", func(*testing.T, fix, string) {}},
		{"corrupt", func(t *testing.T, _ fix, path string) {
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"cards":[{"id":`), 0o600))
		}},
		{"a directory in its place", func(t *testing.T, _ fix, path string) { require.NoError(t, os.MkdirAll(path, 0o700)) }},
		{"from a newer zmina", func(t *testing.T, _ fix, path string) {
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte(`{"version":99,"cards":[]}`), 0o600))
		}},
	} {
		t.Run(k.name, func(t *testing.T) {
			f, p, line := scene(t, crossYAML)
			path := f.s.Crew.Layout().DeliveryCards("ops")
			k.put(t, f, path)

			var before []byte
			if raw, err := os.ReadFile(path); err == nil {
				before = raw
			}

			did := f.s.deliverInbox("dev", *f.now)

			assert.Equal(t, "posted to dev: "+line, did)
			assert.Equal(t, []string{tagged(f, "dev", line)}, p.raw)
			assert.Nil(t, cardOn(t, f, "dev", line).Crossed)
			assert.Zero(t, f.eventCount("cards-failed"), "told of nothing")

			aside, err := filepath.Glob(path + ".broken-*")
			require.NoError(t, err)
			assert.Empty(t, aside, "not moved aside: it is not this delivery's file")

			if before != nil {
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, before, after, "not written")
			}
		})
	}

	// The control: with the file there the same line is marked, and the file is as it was.
	f, p, _ := scene(t, crossYAML)
	putCards(t, f, "ops", good)
	path := f.s.Crew.Layout().DeliveryCards("ops")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	f.s.deliverInbox("dev", *f.now)
	assert.Contains(t, p.raw[0], "[written 14:02, before your last line on T-1 (14:05)] ")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// A time is on the crew's clock, and a day is said when it is not today.
func TestCrossingTimesAreOnTheCrewClockWithTheDayWhenNotToday(t *testing.T) {
	const line = "please review the change"

	// mark is the mark of a line written at wrote, with dev's last line to ops written at last, delivered at 14:06 today.
	mark := func(t *testing.T, body string, wrote, last time.Time) string {
		f, p := nativeFixture(t, body)
		upNative(t, f, "dev")

		*f.now = wrote
		require.NoError(t, f.s.QueueFrom("dev", line, Facts{From: "ops", Task: "T-1"}))
		*f.now = at(14, 6)

		putCards(t, f, "ops", devLine("how far are you", "T-1", last))

		f.s.deliverInbox("dev", *f.now)
		require.Len(t, p.raw, 1)

		return strings.TrimSuffix(tagRe.ReplaceAllString(p.raw[0], ""), line)
	}

	yesterday := func(h, m int) time.Time { return at(h, m).AddDate(0, 0, -1) }
	day := func(t time.Time) string { return t.Format("Mon 2 Jan 15:04") }

	assert.Equal(t, "[written 14:02, before your last line on T-1 (14:05)] ", mark(t, crossYAML, at(14, 2), at(14, 5)),
		"today: the hour and the minute alone")

	assert.Equal(t, "[written "+day(yesterday(14, 2))+", before your last line on T-1 ("+day(yesterday(14, 5))+")] ",
		mark(t, crossYAML, yesterday(14, 2), yesterday(14, 5)), "both of yesterday")

	assert.Equal(t, "[written 14:02, before ops had read your line of "+day(yesterday(23, 50))+" on T-1] ",
		mark(t, crossYAML, at(14, 2), yesterday(23, 50)), "the reader's line of yesterday, the line of today")

	// On a crew clock that is not UTC the hour is that clock's: 14:02 UTC is 16:02 at Warsaw on this day.
	assert.Equal(t, "[written 16:02, before your last line on T-1 (16:05)] ",
		mark(t, "timezone: Europe/Warsaw\n"+strings.Replace(crossYAML, "timezone: UTC\n", "", 1), at(14, 2), at(14, 5)))
}

// An ack keeps the words of the line, and a copy zmina says again after its time is its own line from zmina, on no task:
// it is not marked, and it is the mark of its own delivery that a card would record.
func TestCrossingRenotifiedCopyIsNotMarked(t *testing.T) {
	f, p, _ := scene(t, crossYAML)
	putCards(t, f, "ops", devLine("how far are you", "T-1", at(14, 5)))

	rs := f.s.State.Role("dev")
	rs.Inbox = nil // the scene's untracked line is replaced by a tracked one

	*f.now = at(14, 2)
	id, err := f.s.QueueTrackedFrom("dev", "please review the change", Facts{From: "ops", Task: "T-1"})
	require.NoError(t, err)
	*f.now = at(14, 6)

	line := rs.Inbox[0]
	require.NotEmpty(t, f.s.deliverInbox("dev", *f.now))
	assert.Contains(t, p.raw[0], "[written 14:02, before your last line on T-1 (14:05)] "+line)

	p.showAll()
	f.s.deliverInbox("dev", *f.now) // settles the posted line

	*f.now = f.now.Add(30 * time.Minute)
	f.s.renotify(*f.now)

	again := f.s.State.Role("dev").Inbox
	require.Len(t, again, 1)
	assert.Contains(t, again[0], "still pending since")
	assert.NotContains(t, again[0], "[written", "the queue never held the mark")

	require.NotEmpty(t, f.s.deliverInbox("dev", *f.now))
	require.Len(t, p.raw, 2)
	assert.Equal(t, tagged(f, "dev", again[0]), p.raw[1], "said again as it was queued, from zmina, on no task")

	f.s.FlushCards()

	var crossed int

	for _, c := range cardsOn(f, "dev") {
		if c.Crossed != nil {
			crossed++

			assert.Equal(t, "ops", c.From, "the mark is on the line that crossed, not its copy")
		}
	}

	assert.Equal(t, 1, crossed)
	assert.Equal(t, "please review the change", f.s.State.Acks[id].Text)
}

// zmina send without --when-idle posts at once: the line carries the mark of the moment it is written, its card says the
// task, and a line typed where the post cannot go carries it too.
func TestCrossingDirectSendIsMarkedAndKeepsItsTask(t *testing.T) {
	f, p := nativeFixture(t, crossYAML)
	upNative(t, f, "dev")
	f.s.By = "ops"

	*f.now = at(14, 6)
	putCards(t, f, "ops", devLine("how far are you", "T-1", at(13, 50)))

	const line = "please review the change"
	require.NoError(t, f.s.SendFrom("dev", line, Facts{Task: "T-1"}))
	f.s.FlushCards()

	assert.Equal(t, []string{tagged(f, "dev", "[written 14:06, before ops had read your line of 13:50 on T-1] "+line)}, p.raw)
	assert.Equal(t, line, f.s.State.Role("dev").Posted[0].Line)

	card := cardOn(t, f, "dev", line)
	assert.Equal(t, "ops", card.From)
	assert.Equal(t, "T-1", card.Task)
	require.NotNil(t, card.Crossed)
	assert.Equal(t, CrossUnread, card.Crossed.Kind)
	require.NotNil(t, card.Accepted)

	// Without a task it is the line, as ever.
	require.NoError(t, f.s.Send("dev", "and the other thing"))
	assert.Equal(t, tagged(f, "dev", "and the other thing"), p.raw[1])

	// Typed where the post cannot go: behind its mark, with no record to keep it on.
	p.err = fmt.Errorf("%w: dial: no such file", peer.ErrUnavailable)
	f.s.State.Role("dev").Posted = nil
	require.NoError(t, f.s.SendFrom("dev", "once more, please", Facts{Task: "T-1"}))
	assert.Equal(t, []string{"[written 14:06, before ops had read your line of 13:50 on T-1] once more, please"}, f.tm.typed["dev"])
}

// A line to a Codex role goes into its bridge's inbox behind its mark, and is seen when the bridge takes it, by its id.
func TestCrossingMarksALineToACodexRoleAndItsTakenMarkStillSeesIt(t *testing.T) {
	f, _ := nativeFixture(t, strings.Replace(crewYAML, "claude: claude\n", "claude: claude\ntimezone: UTC\n", 1)+"  dev:\n    runtime: codex\n")
	f.s.CodexInbox = codex.Inbox{}

	r, err := f.s.Role("dev")
	require.NoError(t, err)

	dir := codex.InboxDir(filepath.Join(f.s.Crew.StateDir(), "codex"), r.Session)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "taken"), 0o700))
	require.NoError(t, f.s.Up("dev"))
	bridge(t, f, dir, "thread-1")

	const line = "please review the change"

	*f.now = at(14, 2)
	require.NoError(t, f.s.QueueFrom("dev", line, Facts{From: "designer", Task: "T-1"}))
	*f.now = at(14, 6)

	putCards(t, f, "designer", Card{ID: CardID("designer", "how far are you"), From: "dev", Task: "T-1", Written: at(14, 5)})

	require.NotEmpty(t, f.s.deliverInbox("dev", *f.now))

	files := waitingFiles(t, dir)
	require.Len(t, files, 1)

	raw, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Contains(t, string(raw), "[written 14:02, before your last line on T-1 (14:05)] "+line)

	c := cardOn(t, f, "dev", line)
	require.NotNil(t, c.Crossed)
	assert.Equal(t, AcceptedCodexInbox, c.Accepted.How)
	assert.Equal(t, line, f.s.State.Role("dev").Posted[0].Line)

	take(t, dir, files[0])

	*f.now = f.now.Add(time.Minute)
	f.s.deliverInbox("dev", *f.now)

	c = cardOn(t, f, "dev", line)
	require.NotNil(t, c.Seen)
	assert.Equal(t, SeenTaken, c.Seen.By)
	assert.Empty(t, f.s.State.Role("dev").Posted)
}

// A line queued for the dispatcher is delivered by the dispatcher's own post, which marks nothing, and it is marked on no
// path: a mark that came with the path a line happened to take would say nothing. The same cards mark the line of a role.
func TestCrossingTheDispatcherIsNeverMarked(t *testing.T) {
	const line = "[ops] how is it going"

	run := func(t *testing.T, to string, via func(f fix)) []string {
		f, p := nativeFixture(t, crossYAML)
		upNative(t, f, to)

		*f.now = at(14, 2)
		require.NoError(t, f.s.QueueFrom(to, line, Facts{From: "ops", Task: "T-1"}))
		*f.now = at(14, 6)

		// The reader wrote ops on the task after the line was written, whoever the reader is.
		putCards(t, f, "ops", Card{ID: CardID("ops", "how far are you"), From: to, Task: "T-1", Written: at(14, 5)})

		via(f)

		return p.raw
	}

	for name, via := range map[string]func(f fix){
		"its own post":        func(f fix) { f.s.deliver(*f.now) },
		"the role's delivery": func(f fix) { f.s.deliverInbox("dispatcher", *f.now) },
		"a delivery to it":    func(f fix) { f.s.DeliverTo("dispatcher") },
	} {
		raw := run(t, "dispatcher", via)
		require.Len(t, raw, 1, name)
		assert.True(t, strings.HasSuffix(raw[0], "] "+line), name+": "+raw[0])
		assert.NotContains(t, raw[0], "[written", name)
	}

	raw := run(t, "dev", func(f fix) { f.s.deliverInbox("dev", *f.now) })
	require.Len(t, raw, 1)
	assert.Contains(t, raw[0], "[written 14:02, before your last line on T-1 (14:05)] "+line, "the control: a role's line is marked")
}

// The mark is read from a draft by what opens it, and the line behind it is compared as the queue's line.
func TestHoldsSeesALineBehindItsMark(t *testing.T) {
	const line = "please review the change"

	for name, k := range map[string]struct {
		draft string
		want  bool
	}{
		"the line itself":                        {line, true},
		"its start":                              {line[:12], true},
		"behind a mark":                          {"[written 14:02, before your last line on T-1 (14:05)] " + line, true},
		"behind a mark, the line's start":        {"[written 14:02, before your last line on T-1 (14:05)] please rev", true},
		"behind a mark, a day said":              {"[written Fri 23 Oct 14:02, before ops had read your line of 13:50 on T-1] " + line, true},
		"the mark alone":                         {"[written 14:02, before your last line on T-1 (14:05)] ", true},
		"the mark cut by the screen":             {"[written 14:02, before ops had read yo", true},
		"behind a mark, another line":            {"[written 14:02, before your last line on T-1 (14:05)] something else", false},
		"another line":                           {"something else", false},
		"nothing":                                {"", false},
		"a draft that only mentions a mark":      {"look, [written 14:02] is how it reads", false},
		"the owner's draft that opens like none": {"[wrote] " + line, false},
	} {
		assert.Equal(t, k.want, holds(k.draft, line), name)
	}
}

// The boot says so once, in one sentence, with no other rule moved; the size of zmina's rules stays under its bound.
func TestCrossingRuleIsInTheBootText(t *testing.T) {
	f, _ := nativeFixture(t, crossYAML)

	// Every role that has a boot hears the rules; a role with none hears none of them, as ever.
	for _, name := range []string{"dev", "dispatcher"} {
		r := f.s.Crew.Roles[name]
		boot := f.s.BootText(r)
		assert.Equal(t, 1, strings.Count(boot, CrossingRule), name)
		assert.Contains(t, boot, "zmina send <role> --task T-n")
	}

	assert.Less(t, len(CrossingRule), 260, "one sentence")
	assert.True(t, strings.HasSuffix(RoleRules, CrossingRule))
	assert.True(t, strings.HasSuffix(CodexRoleRules, CrossingRule))
}
