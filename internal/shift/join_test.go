package shift

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/chat"
	"github.com/iamnikolie/zmina/internal/peer"
	"github.com/iamnikolie/zmina/internal/task"
)

// queueLines queues tracked lines for a role, as zmina send --when-idle does, and returns each as it waits (its ack tail
// at its end) with its id.
func queueLines(t *testing.T, f fix, role string, texts ...string) ([]string, []string) {
	t.Helper()

	var lines, ids []string

	for _, text := range texts {
		id, err := f.s.QueueTracked(role, text)
		require.NoError(t, err)

		rs := f.s.State.Role(role)
		lines = append(lines, rs.Inbox[len(rs.Inbox)-1])
		ids = append(ids, id)
	}

	return lines, ids
}

// tagged is a text as it goes on the wire to a role: behind the tag of its start.
func tagged(f fix, role, text string) string { return tagMark(f.s.State.Role(role).Tag) + " " + text }

// Lines that wait for a role on its own channel go as one numbered message, each with its own ack tail, recorded as one
// post whose parts are the lines.
func TestJoinedLinesGoAsOneNumberedPost(t *testing.T) {
	for _, n := range []int{2, 3} {
		t.Run(fmt.Sprintf("%d lines", n), func(t *testing.T) {
			f, p := nativeFixture(t, crewYAML)
			upNative(t, f, "designer")

			texts := []string{"[dev] look at T-1", "[ops] the build is red", "[dev] and then T-2"}[:n]
			lines, ids := queueLines(t, f, "designer", texts...)

			var want []string
			for i, l := range lines {
				want = append(want, fmt.Sprintf("%d. %s", i+1, l))
			}

			did := f.s.deliverInbox("designer", *f.now)

			require.Len(t, p.raw, 1, "one post")
			assert.Equal(t, tagged(f, "designer", strings.Join(want, "\n")), p.raw[0])
			assert.Equal(t, "posted to designer: "+fmt.Sprintf("%d lines joined: ", n)+strings.Join(want, " "), did)

			rs := f.s.State.Role("designer")
			assert.Empty(t, rs.Inbox)
			assert.Empty(t, rs.QueuedIDs, "each line's ids were taken, once")

			require.Len(t, rs.Posted, 1)
			rec := rs.Posted[0]
			assert.Equal(t, strings.Join(want, "\n"), rec.Line)
			assert.Equal(t, ids, rec.IDs)
			require.Len(t, rec.Parts, n)

			for i, part := range rec.Parts {
				assert.Equal(t, Part{Line: lines[i], IDs: []string{ids[i]}}, part)
				assert.Contains(t, lines[i], "(zmina "+ids[i]+")", "its ack tail is intact")
				assert.Equal(t, *f.now, f.s.State.Acks[ids[i]].First, "delivered")
			}

			// One event for the post, naming how many lines it joined; none for each line posted.
			assert.Equal(t, 1, f.eventCount("posted"))

			for _, e := range f.ev.list {
				if e.Event == "posted" {
					assert.Equal(t, fmt.Sprintf("%d lines joined: ", n)+strings.Join(want, "\n"), e.Detail)
				}
			}
		})
	}
}

// One waiting line goes exactly as it always went: whole, behind the tag, no number, no parts.
func TestOneWaitingLineIsPostedAsBefore(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	lines, ids := queueLines(t, f, "designer", "[dev] look at T-1")

	assert.Equal(t, "posted to designer: "+lines[0], f.s.deliverInbox("designer", *f.now))
	assert.Equal(t, []string{tagged(f, "designer", lines[0])}, p.raw)

	rec := f.s.State.Role("designer").Posted[0]
	assert.Equal(t, lines[0], rec.Line)
	assert.Equal(t, ids, rec.IDs)
	assert.Nil(t, rec.Parts)

	for _, e := range f.ev.list {
		if e.Event == "posted" {
			assert.Equal(t, lines[0], e.Detail)
		}
	}
}

// What goes alone: a line of these kinds ends the run before it, and is posted by itself when it is the first.
func TestJoinStopsAtALineThatGoesAlone(t *testing.T) {
	const packet = `[zmina T-1] Read handoff packet "/state/handoff/h-1.json"; before work run: zmina task accept T-1 --handoff h-1 --role designer`

	kinds := []neverJoined{
		{name: "a slash command", line: "/compact", typed: true},
		{name: "the rename", line: "/rename zm-designer", typed: true, set: func(_ fix, rs *RoleState) { rs.QueuedRename = "/rename zm-designer" }},
		{name: "the rename, whatever its words", line: "rename words", set: func(_ fix, rs *RoleState) { rs.QueuedRename = "rename words" }},
		{name: "the owner's own message", line: chat.Prefix("web:1") + "can you look", typed: true, set: func(_ fix, rs *RoleState) {
			rs.ChatReceipts = append(rs.ChatReceipts, chat.Receipt{ID: "1", Line: chat.Prefix("web:1") + "can you look"})
		}},
		{name: "the boot line", line: "read d and boot", set: func(_ fix, rs *RoleState) { rs.QueuedBoot = "read d and boot" }},
		{name: "a line only its conversation may read", line: "[zmina release] park what you hold and say so"},
		{name: "a task packet", line: packet},
		{name: "a line stored whole in the inbox", line: "the head of a long line… (+31000 chars, full: zmina inbox show in-0123456789ab) (zmina S-99)"},
	}

	for _, k := range kinds {
		t.Run(k.name+" in the middle", func(t *testing.T) {
			f, p := nativeFixture(t, crewYAML)
			upNative(t, f, "designer")

			k.set0(f)
			lines, _ := queueLines(t, f, "designer", "one", "two")
			rs := f.s.State.Role("designer")
			rs.Inbox = append(rs.Inbox, k.line)
			more, _ := queueLines(t, f, "designer", "three")

			assert.Equal(t, lines, f.s.joinRun("designer"), "the run ends before it")

			assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
			require.Len(t, p.raw, 1)
			assert.Equal(t, tagged(f, "designer", "1. "+lines[0]+"\n2. "+lines[1]), p.raw[0])
			assert.Equal(t, []string{k.line, more[0]}, rs.Inbox, "it and what follows wait")
		})

		t.Run(k.name+" first", func(t *testing.T) {
			f, p := nativeFixture(t, crewYAML)
			upNative(t, f, "designer")

			k.set0(f)
			rs := f.s.State.Role("designer")
			rs.Inbox = append(rs.Inbox, k.line)
			lines, _ := queueLines(t, f, "designer", "one", "two")

			assert.Empty(t, f.s.joinRun("designer"), "nothing is joined with it")

			f.s.deliverInbox("designer", *f.now)

			if k.typed {
				assert.Empty(t, p.raw, "typed, not posted")
				assert.Equal(t, []string{k.line}, f.tm.typed["designer"])
				assert.Equal(t, lines, rs.Inbox)

				return
			}

			assert.Equal(t, []string{tagged(f, "designer", k.line)}, p.raw, "alone, whole, no number")
			assert.Equal(t, lines, rs.Inbox)
			assert.Nil(t, rs.Posted[0].Parts)
		})
	}
}

// neverJoined is a kind of line that is never joined: its words, and the state it needs before it is queued.
type neverJoined struct {
	name string
	line string
	set  func(f fix, rs *RoleState)
	// typed is a line the role types, never posts.
	typed bool
}

// set0 makes the state the kind of line needs.
func (k neverJoined) set0(f fix) {
	if k.set != nil {
		k.set(f, f.s.State.Role("designer"))
	}
}

// The boot line of a state older than QueuedBoot is known by how it opens, as the queue knows it: never joined.
func TestJoinNeverJoinsABootKnownByItsOpening(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML+"  scribe:\n    boot: read the rules\n")
	upNative(t, f, "scribe")

	boot := "read the rules and wait"
	rs := f.s.State.Role("scribe")
	require.Empty(t, rs.QueuedBoot)
	rs.Inbox = []string{"one", boot, "two"}

	assert.Equal(t, []string{"one"}, f.s.joinRun("scribe"))

	rs.Inbox = []string{boot, "one", "two"}
	assert.Empty(t, f.s.joinRun("scribe"))
}

// The owner's own message is never joined, whichever runtime the role has: a Codex bridge takes it as any line, and it is
// still his own words.
func TestJoinNeverJoinsTheOwnersMessageToACodexRole(t *testing.T) {
	f, dir := codexFixture(t)
	bridge(t, f, dir, "thread-1")

	own := chat.Prefix("web:1") + "can you look"
	rs := f.s.State.Role("dev")
	rs.ChatReceipts = append(rs.ChatReceipts, chat.Receipt{ID: "1", Line: own})

	require.NoError(t, f.s.Queue("dev", "one"))
	rs.Inbox = append(rs.Inbox, own)
	require.NoError(t, f.s.Queue("dev", "two"))

	assert.Equal(t, []string{"one"}, f.s.joinRun("dev"), "the run ends before it")

	rs.Inbox = []string{own, "one", "two"}
	assert.Empty(t, f.s.joinRun("dev"), "and it goes alone when it is first")
	assert.NotEmpty(t, f.s.deliverInbox("dev", *f.now))
	assert.Equal(t, []string{"one", "two"}, rs.Inbox)
	assert.Nil(t, rs.Posted[0].Parts)
}

// A line that is the same words as one already in the run ends it: each line has a card and ids of its own, told apart
// by their words.
func TestJoinStopsAtAnotherLineOfTheSameWords(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	require.NoError(t, f.s.Queue("designer", "ping"))
	require.NoError(t, f.s.Queue("designer", "pong"))
	require.NoError(t, f.s.Queue("designer", "ping"))

	assert.Equal(t, []string{"ping", "pong"}, f.s.joinRun("designer"))
}

// The message as a whole is within the size bound a post to the dispatcher has; the run ends before the line that would
// pass it. A line over the bound by itself goes alone, whole, as it always went. (The doorbell is off here: it would send
// what is this big as a bell, which bell_test.go looks at.)
func TestJoinEndsAtTheSizeBound(t *testing.T) {
	t.Run("the next line would pass it", func(t *testing.T) {
		f, p := nativeFixture(t, "doorbell: off\n"+crewYAML)
		upNative(t, f, "designer")

		a, b, c := strings.Repeat("a", 5000), strings.Repeat("b", 5000), strings.Repeat("c", 5000)
		require.NoError(t, f.s.Queue("designer", a))
		require.NoError(t, f.s.Queue("designer", b))
		require.NoError(t, f.s.Queue("designer", c))

		assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
		require.Len(t, p.posted["sid-designer"], 1)
		assert.Equal(t, "1. "+a+"\n2. "+b, p.posted["sid-designer"][0])
		assert.LessOrEqual(t, len(p.posted["sid-designer"][0]), postMax)
		assert.Equal(t, []string{c}, f.s.State.Role("designer").Inbox)
	})

	t.Run("exactly the bound fits", func(t *testing.T) {
		f, p := nativeFixture(t, "doorbell: off\n"+crewYAML)
		upNative(t, f, "designer")

		first := strings.Repeat("a", postMax-len("1. ")-len("\n2. ")-len("b"))
		require.NoError(t, f.s.Queue("designer", first))
		require.NoError(t, f.s.Queue("designer", "b"))
		require.NoError(t, f.s.Queue("designer", "c"))

		assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
		require.Len(t, p.posted["sid-designer"], 1)
		assert.Equal(t, postMax, len(p.posted["sid-designer"][0]))
		assert.Equal(t, []string{"c"}, f.s.State.Role("designer").Inbox)
	})

	t.Run("a line over the bound goes alone", func(t *testing.T) {
		f, p := nativeFixture(t, "doorbell: off\n"+crewYAML)
		upNative(t, f, "designer")

		big := strings.Repeat("x", postMax+10)
		require.NoError(t, f.s.Queue("designer", big))
		require.NoError(t, f.s.Queue("designer", "small"))

		assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
		assert.Equal(t, []string{big}, p.posted["sid-designer"], "whole, no number, as before")
		assert.Equal(t, []string{"small"}, f.s.State.Role("designer").Inbox)
	})
}

// A post carries at most joinMax lines; the rest go in the next, once the first showed.
func TestJoinEndsAtTheCountCap(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	var want []string

	for i := range joinMax + 2 {
		line := fmt.Sprintf("line %d", i)
		require.NoError(t, f.s.Queue("designer", line))
		want = append(want, line)
	}

	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	require.Len(t, p.posted["sid-designer"], 1)
	assert.Equal(t, joinMax, strings.Count(p.posted["sid-designer"][0], "\n")+1)
	assert.True(t, strings.HasSuffix(p.posted["sid-designer"][0], fmt.Sprintf("%d. %s", joinMax, want[joinMax-1])))
	assert.Equal(t, want[joinMax:], f.s.State.Role("designer").Inbox)

	p.showAll()
	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	assert.Equal(t, "1. line 8\n2. line 9", p.posted["sid-designer"][1], "the numbers start again in each post")
}

// A joined post shown in the conversation closes every line's card as seen, and the record.
func TestJoinedPostShownIsSeenLineByLine(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	lines, _ := queueLines(t, f, "designer", "[dev] look at T-1", "[ops] the build is red", "[dev] and then T-2")
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	for _, l := range lines {
		c := cardOn(t, f, "designer", l)
		require.NotNil(t, c.Accepted, l)
		assert.Equal(t, AcceptedSocket, c.Accepted.How)
		assert.Equal(t, "sid-designer", c.Accepted.Session)
		assert.Nil(t, c.Seen, "posted is not seen")
	}

	*f.now = f.now.Add(time.Minute)
	p.showAll()
	f.s.deliverInbox("designer", *f.now)

	assert.Empty(t, f.s.State.Role("designer").Posted)

	for _, l := range lines {
		c := cardOn(t, f, "designer", l)
		require.NotNil(t, c.Seen, l)
		assert.Equal(t, SeenTranscript, c.Seen.By)
		assert.Equal(t, *f.now, c.Seen.At)
	}

	seen := 0

	for _, e := range f.ev.list {
		if e.Event == "seen" {
			assert.Contains(t, lines, e.Detail, "one for each line, its own words")
			seen++
		}
	}

	assert.Equal(t, len(lines), seen)
}

// Every part of a joined post that is a task line has its delivery marked, as a line posted alone has.
func TestJoinedPostMarksEachTaskLineSubmitted(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	first := task.Task{ID: "T-1", Role: "designer", State: task.Open, Delivery: &task.Delivery{ID: "h-1", Role: "designer", Line: "do the first thing"}}
	second := task.Task{ID: "T-2", Role: "designer", State: task.Open, Delivery: &task.Delivery{ID: "h-2", Role: "designer", Line: "do the second thing"}}
	f.tk.list = []task.Task{first, second}

	require.NoError(t, f.s.Queue("designer", "do the first thing"))
	require.NoError(t, f.s.Queue("designer", "do the second thing"))
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	require.Len(t, f.s.State.Role("designer").Posted, 1, "one post for both")

	for _, id := range []string{"T-1", "T-2"} {
		got, err := f.tk.Get(id)
		require.NoError(t, err)
		assert.Equal(t, *f.now, got.Delivery.SentAt, id)
		assert.Equal(t, "sid-designer", got.Delivery.Session, id)
	}
}

// A joined post the conversation never showed is kept as one failed record that keeps its parts; queue retry gives the
// lines back as separate lines, in order, each with its ids.
func TestJoinedPostNeverShownFailsWithItsParts(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	lines, ids := queueLines(t, f, "designer", "[dev] look at T-1", "[ops] the build is red", "[dev] and then T-2")
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	text := f.s.State.Role("designer").Posted[0].Line

	*f.now = f.now.Add(postedWait)
	f.s.deliverInbox("designer", *f.now)

	rs := f.s.State.Role("designer")
	require.Len(t, rs.Failed, 1)
	assert.Empty(t, rs.Posted)

	failed := rs.Failed[0]
	assert.Equal(t, postedNever, failed.Error)
	assert.Equal(t, text, failed.Line)
	assert.Equal(t, ids, failed.IDs)
	require.Len(t, failed.Parts, 3)

	for i, part := range failed.Parts {
		assert.Equal(t, Part{Line: lines[i], IDs: []string{ids[i]}}, part)
		c := cardOn(t, f, "designer", lines[i])
		require.NotNil(t, c.Attention, "may have reached the role: a look first")
	}

	n, left, err := f.s.Retry("designer", true)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Zero(t, left)

	assert.Empty(t, rs.Failed)
	assert.Equal(t, lines, rs.Inbox, "the lines back, in order, one by one")
	f.s.FlushCards()

	for i, l := range lines {
		assert.Equal(t, []string{ids[i]}, rs.QueuedIDs[l], "each with its ids")

		c := cardOn(t, f, "designer", l)
		require.NotNil(t, c.Back, "its card says it went back")
		assert.Equal(t, "queue retry", c.Back.Why)
	}
}

// A post that never started is tried again as a line's is; then the first line alone is kept as failed, as when it goes
// alone, and the line after it waits with tries of its own.
func TestJoinedPostThatNeverStartedFailsItsFirstLine(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	lines, ids := queueLines(t, f, "designer", "[dev] look at T-1", "[ops] the build is red")
	p.err = fmt.Errorf("%w: dial: no such file", peer.ErrUnavailable)

	for range typeTries - 1 {
		assert.Empty(t, f.s.deliverInbox("designer", *f.now), "tried again")
		assert.Equal(t, lines, f.s.State.Role("designer").Inbox)
		*f.now = f.now.Add(time.Minute)
	}

	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	rs := f.s.State.Role("designer")
	assert.Equal(t, lines[1:], rs.Inbox, "the line after it still waits")
	assert.Zero(t, rs.Attempts, "with tries of its own")
	assert.Empty(t, rs.Posted)
	require.Len(t, rs.Failed, 1)
	assert.Equal(t, lines[0], rs.Failed[0].Line)
	assert.Equal(t, ids[:1], rs.Failed[0].IDs)
	assert.Empty(t, rs.Failed[0].Parts)
	assert.False(t, rs.Failed[0].Unclear(), "nothing reached it: a retry is safe")
	require.NotEmpty(t, notesOf(f, "designer"))

	c := cardOn(t, f, "designer", lines[0])
	require.NotNil(t, c.Closed)
	assert.Equal(t, ClosedFailed, c.Closed.How)

	n, _, err := f.s.Retry("designer", false)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, lines, rs.Inbox)
}

// A write that broke after it started is followed as one post: the session may have all of it, so none is posted again.
func TestJoinedPostWhoseWriteBrokeIsFollowedWhole(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	lines, ids := queueLines(t, f, "designer", "[dev] look at T-1", "[ops] the build is red")
	p.err = &peer.Committed{Err: fmt.Errorf("broken pipe")}

	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	rs := f.s.State.Role("designer")
	assert.Empty(t, rs.Inbox)
	require.Len(t, rs.Posted, 1, "followed until it shows")
	assert.Equal(t, ids, rs.Posted[0].IDs)
	require.Len(t, rs.Posted[0].Parts, 2)
	assert.True(t, f.ev.has("designer", "post-broken"))

	p.err = nil
	require.NoError(t, f.s.Queue("designer", "later"))
	assert.Empty(t, f.s.deliverInbox("designer", *f.now), "waits for the broken one")
	assert.Len(t, p.raw, 1)

	p.showAll()
	f.s.deliverInbox("designer", *f.now)
	require.Len(t, rs.Posted, 1, "the joined post is done, the line after it is posted")
	assert.Equal(t, "later", rs.Posted[0].Line)

	for _, l := range lines {
		require.NotNil(t, cardOn(t, f, "designer", l).Seen)
	}
}

// A joined post whose conversation ended before it showed goes back to the head of the queue, line by line in order,
// ahead of what was queued since.
func TestJoinedPostToAnEndedConversationGoesBackInOrder(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	lines, ids := queueLines(t, f, "designer", "[dev] look at T-1", "[ops] the build is red", "[dev] and then T-2")
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	later, _ := queueLines(t, f, "designer", "[dev] much later")

	f.rg.byPID[f.tm.up["designer"]].SessionID = "sid-designer-2"
	f.s.followPosted("designer", *f.now)

	rs := f.s.State.Role("designer")
	assert.Empty(t, rs.Posted)
	assert.Equal(t, append(lines, later...), rs.Inbox)
	f.s.FlushCards()

	for i, l := range lines {
		assert.Equal(t, []string{ids[i]}, rs.QueuedIDs[l])
		require.NotNil(t, cardOn(t, f, "designer", l).Back)
	}

	assert.Equal(t, 1, f.eventCount("requeued"))
}

// The ack of one part's id clears that part only: the post stays for the others, and a part acked before the post goes
// back stays out of it. When every part is acked, the record closes.
func TestAckOfOnePartOfAJoinedPost(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	lines, ids := queueLines(t, f, "designer", "[dev] look at T-1", "[ops] the build is red", "[dev] and then T-2")
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	rs := f.s.State.Role("designer")

	acked, _ := f.s.AckLines([]string{ids[1]}, false, "designer", "designer")
	require.Equal(t, []string{ids[1]}, acked)
	require.Len(t, rs.Posted, 1, "the others wait to be seen")
	assert.Len(t, rs.Posted[0].Parts, 3)
	assert.Contains(t, f.s.State.Acks, ids[0])
	assert.Contains(t, f.s.State.Acks, ids[2])

	// Its conversation ends: the acked line stays out, the rest go back in order.
	f.rg.byPID[f.tm.up["designer"]].SessionID = "sid-designer-2"
	f.s.followPosted("designer", *f.now)
	assert.Equal(t, []string{lines[0], lines[2]}, rs.Inbox)

	// All three acked: the record closes, and its cards.
	g, _ := nativeFixture(t, crewYAML)
	upNative(t, g, "designer")

	more, moreIDs := queueLines(t, g, "designer", "[dev] look at T-1", "[ops] the build is red")
	require.NotEmpty(t, g.s.deliverInbox("designer", *g.now))

	g.s.AckLines(moreIDs[:1], false, "designer", "designer")
	require.Len(t, g.s.State.Role("designer").Posted, 1)

	g.s.AckLines(moreIDs[1:], false, "designer", "designer")
	assert.Empty(t, g.s.State.Role("designer").Posted)
	g.s.FlushCards()

	for _, l := range more {
		c := cardOn(t, g, "designer", l)
		require.NotNil(t, c.Closed)
		assert.Equal(t, ClosedAcked, c.Closed.How)
	}
}

// A line that waits for an ack is not said again while it is in a joined post: renotify knows it by its id.
func TestRenotifyLeavesAJoinedPostAlone(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	_, ids := queueLines(t, f, "designer", "[dev] look at T-1", "[ops] the build is red")
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	for _, id := range ids {
		assert.True(t, f.s.inFlight(f.s.State.Acks[id]), id)
	}

	*f.now = f.now.Add(f.s.Crew.RenotifyD() + time.Minute)
	f.s.renotify(*f.now)

	assert.Empty(t, f.s.State.Role("designer").Inbox)
}

// A Codex role's lines join the same way, in the bridge's inbox, and are seen when the bridge takes the file.
func TestJoinedLinesToACodexRole(t *testing.T) {
	f, dir := codexFixture(t)
	bridge(t, f, dir, "thread-1")

	lines, _ := queueLines(t, f, "dev", "[dispatcher] look at T-1", "[ops] the build is red")
	require.NotEmpty(t, f.s.deliverInbox("dev", *f.now))

	files := waitingFiles(t, dir)
	require.Len(t, files, 1, "one file for both lines")

	raw, err := os.ReadFile(files[0])
	require.NoError(t, err)

	var got struct{ Text string }
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.True(t, strings.HasSuffix(got.Text, "1. "+lines[0]+"\n2. "+lines[1]), got.Text)

	rs := f.s.State.Role("dev")
	require.Len(t, rs.Posted, 1)
	require.Len(t, rs.Posted[0].Parts, 2)

	for _, l := range lines {
		c := cardOn(t, f, "dev", l)
		require.NotNil(t, c.Accepted)
		assert.Equal(t, AcceptedCodexInbox, c.Accepted.How)
		assert.Nil(t, c.Seen, "the file waits untaken")
	}

	take(t, dir, files[0])
	*f.now = f.now.Add(time.Minute)
	f.s.deliverInbox("dev", *f.now)

	assert.Empty(t, rs.Posted)

	for _, l := range lines {
		c := cardOn(t, f, "dev", l)
		require.NotNil(t, c.Seen)
		assert.Equal(t, SeenTaken, c.Seen.By)
	}
}

// A held head holds the join: a failed turn holds the queue of a role as it holds one line, and what waits goes as one
// post once the hold is over. An owner's message that is exempt goes alone, typed, and nothing is joined with it.
func TestJoinKeepsTheHeadsHolds(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	start := *f.now
	f.status("designer", "idle", start)
	f.rg.endedAt["sid-designer"] = start.Add(time.Second)
	f.s.State.Role("designer").Booted = start

	own := chat.Prefix("web:1") + "can you look"
	rs := f.s.State.Role("designer")
	rs.ChatReceipts = append(rs.ChatReceipts, chat.Receipt{ID: "1", Line: own})
	lines, _ := queueLines(t, f, "designer", "[dev] look at T-1", "[ops] the build is red")

	failedAt := start.Add(time.Minute)
	*f.now = failedAt
	f.status("designer", "idle", failedAt)
	f.live("designer")
	f.failed(t, "designer", failedInput, failedAt)

	*f.now = failedAt.Add(time.Minute)
	assert.Empty(t, f.s.deliverInbox("designer", *f.now), "held")
	assert.Empty(t, p.raw)

	rs.Inbox = append([]string{own}, rs.Inbox...)
	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now), "the owner's message is exempt")
	assert.Equal(t, []string{own}, f.tm.typed["designer"], "typed, alone")
	assert.Empty(t, p.raw)
	assert.Equal(t, lines, rs.Inbox)

	rs.Inbox, rs.Sent = lines, nil
	*f.now = failedAt.Add(FailHold + time.Second)
	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	require.Len(t, p.posted["sid-designer"], 1)
	assert.Equal(t, "1. "+lines[0]+"\n2. "+lines[1], p.posted["sid-designer"][0])
}

// A failed try of the first line holds the join for the gap a line's own failure holds it.
func TestJoinWaitsForTheGapAfterAFailedTry(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	queueLines(t, f, "designer", "[dev] look at T-1", "[ops] the build is red")
	rs := f.s.State.Role("designer")
	rs.Attempts, rs.TriedAt = 1, *f.now

	assert.Empty(t, f.s.deliverInbox("designer", *f.now))
	assert.Empty(t, p.raw)

	*f.now = f.now.Add(countedGap)
	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	assert.Len(t, p.raw, 1)
}

// Typed delivery is one line a pass, as it was.
func TestTypedDeliveryStaysOneLinePerPass(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer")

	require.NoError(t, f.s.Queue("designer", "one"))
	require.NoError(t, f.s.Queue("designer", "two"))

	assert.Equal(t, "typed into designer: one", f.s.deliverInbox("designer", *f.now))
	assert.Equal(t, []string{"one"}, f.tm.typed["designer"])
	assert.Equal(t, []string{"two"}, f.s.State.Role("designer").Inbox)
	assert.Nil(t, f.s.State.Role("designer").Sent.Parts)
}

// The dispatcher's own queued lines are joined by its post (postDispatcher), which this change leaves as it was: the
// queue's own pass posts one line of its queue.
func TestJoinLeavesTheDispatchersQueuePassAlone(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	require.NoError(t, f.s.Queue("dispatcher", "one"))
	require.NoError(t, f.s.Queue("dispatcher", "two"))

	assert.Equal(t, "posted to dispatcher: one", f.s.deliverInbox("dispatcher", *f.now))
	assert.Equal(t, []string{tagged(f, "dispatcher", "one")}, p.raw)
	assert.Nil(t, f.s.State.Role("dispatcher").Posted[0].Parts)
}

// queue ls shows a joined post as its lines, each with its own facts; the dispatcher's post stays one row.
func TestQueueViewShowsAJoinedPostAsItsParts(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	lines, _ := queueLines(t, f, "designer", "[dev] look at T-1", "[ops] the build is red")
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	var view *QueueView

	for _, q := range f.s.Queues() {
		if q.Role == "designer" {
			view = &q
		}
	}

	require.NotNil(t, view)
	require.Len(t, view.Posted, 1)
	require.True(t, view.Joined(view.Posted[0].Parts))

	for j, l := range lines {
		c := view.PostedPartFacts(0, j)
		require.NotNil(t, c, l)
		assert.Equal(t, CardID("designer", l), c.ID)
		require.NotNil(t, c.Accepted)
	}

	assert.Nil(t, view.PostedPartFacts(0, 2))
	assert.Len(t, view.Facts, 2, "each line's card is among the facts")
}
