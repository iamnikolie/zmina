package shift

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eventCount is how many events of this name the fake log holds.
func (f fix) eventCount(event string) int {
	n := 0

	for _, e := range f.ev.list {
		if e.Event == event {
			n++
		}
	}

	return n
}

// postedTo posts a tracked line to an up native role and returns its text and id.
func postedTo(t *testing.T, f fix, role, text string) (string, string) {
	t.Helper()

	id, err := f.s.QueueTracked(role, text)
	require.NoError(t, err)

	rs := f.s.State.Role(role)
	require.Len(t, rs.Inbox, 1)
	line := rs.Inbox[0]

	require.NotEmpty(t, f.s.deliverInbox(role, *f.now))
	require.Len(t, rs.Posted, 1)
	require.Empty(t, rs.Inbox)

	return line, id
}

// A line kept as failed that its role then acked does not stay failed: it is not listed, and the delivery note it
// raised is withdrawn.
func TestAckClearsAFailedLine(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.s.State.Role("designer").Inbound = true
	id, err := f.s.QueueTracked("designer", "[dev] look at T-1")
	require.NoError(t, err)
	*f.now = f.now.Add(bootPause)
	f.status("designer", "idle", *f.now)

	f.tm.typeErr = errNotTyped("tmux refused")
	for range typeTries {
		f.s.Tick()
		*f.now = f.now.Add(time.Minute)
	}

	rs := f.s.State.Role("designer")
	require.Len(t, rs.Failed, 1)
	require.Equal(t, []string{id}, rs.Failed[0].IDs)
	require.Contains(t, strings.Join(f.s.State.Notes, "|"), "designer: delivery failed")

	acked, _ := f.s.AckLines([]string{id}, false, "designer", "designer")
	require.Equal(t, []string{id}, acked)

	assert.Empty(t, rs.Failed)
	assert.True(t, f.ev.has("designer", "acked-cleared"))

	for _, q := range f.s.Queues() {
		assert.NotEqual(t, "designer", q.Role, "nothing waits for the designer: queue ls shows nothing")
	}

	f.s.currentNotes()
	assert.NotContains(t, strings.Join(f.s.State.Notes, "|"), "designer: delivery failed", "no longer reported")

	n, _, err := f.s.Retry("designer", false)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, rs.Inbox, "nothing to retry")
}

// A failed line that carries several ids is cleared when the last of them is acked, not before.
func TestAckClearsAFailedLineWhenEveryIDIsAcked(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("dispatcher"))
	f.s.State.Role("dispatcher").Inbound = true
	f.status("dispatcher", "idle", *f.now)
	f.tm.onType = nil
	*f.now = f.now.Add(3 * time.Minute)

	_, err := f.s.Note("dev", "T-2 result ready", false)
	require.NoError(t, err)
	_, err = f.s.Note("ops", "T-3 result ready", false)
	require.NoError(t, err)
	require.NotEmpty(t, f.s.deliver(*f.now))

	rs := f.s.State.Role("dispatcher")
	require.NotNil(t, rs.Sent)
	first, second := rs.Sent.IDs[0], rs.Sent.IDs[1]

	for i := 0; i < 5 && len(rs.Failed) == 0; i++ {
		*f.now = f.now.Add(2 * time.Minute)
		f.s.landed("dispatcher", *f.now)
	}

	require.Len(t, rs.Failed, 1)

	f.s.AckLines([]string{first}, false, "dispatcher", "dispatcher")
	require.Len(t, rs.Failed, 1, "one id still waits")

	f.s.AckLines([]string{second}, false, "dispatcher", "dispatcher")
	assert.Empty(t, rs.Failed)
}

// A line acked while it is posted, or typed and not yet landed, leaves those records too: it must not turn into a
// failed one for being unseen once the wait is over.
func TestAckClearsAPostedAndATypedLine(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	_, id := postedTo(t, f, "designer", "[dev] look at T-1")

	f.s.AckLines([]string{id}, false, "designer", "designer")
	assert.Empty(t, f.s.State.Role("designer").Posted)

	*f.now = f.now.Add(postedWait)
	f.s.followPosted("designer", *f.now)
	assert.Empty(t, f.s.State.Role("designer").Failed, "acked, so not failed for being unseen")

	g := fixtureWith(t, crewYAML)
	require.NoError(t, g.s.Up("designer"))
	g.s.State.Role("designer").Inbound = true
	g.status("designer", "idle", *g.now)
	*g.now = g.now.Add(3 * time.Minute)

	typed, err := g.s.QueueTracked("designer", "[dev] look at T-2")
	require.NoError(t, err)
	require.NotEmpty(t, g.s.deliverInbox("designer", *g.now))
	require.NotNil(t, g.s.State.Role("designer").Sent)

	g.s.AckLines([]string{typed}, false, "designer", "designer")
	assert.Nil(t, g.s.State.Role("designer").Sent)
}

// The dispatcher's posted line acked before it was seen closes as a seen one does: the tell's stage is recorded.
func TestAckOfAPostedDispatcherLineRecordsItsStage(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	_, id, _, err := f.s.NoteSentAs("remote box/ops", tellText, *f.now)
	require.NoError(t, err)
	f.s.BindMessage(TellPrefix+"e-1", id)
	require.NotEmpty(t, f.s.deliver(*f.now))
	require.Len(t, f.s.State.Role("dispatcher").Posted, 1)

	f.s.AckLines([]string{id}, false, "dispatcher", "dispatcher")

	assert.Empty(t, f.s.State.Role("dispatcher").Posted)
	assert.Contains(t, f.s.State.MessageStages[TellPrefix+"e-1"], "submitted")
	assert.Contains(t, f.s.State.MessageStages[TellPrefix+"e-1"], "read")
}

// An ack of one signal of a post that also carries a note (a part with no id) closes that signal and nothing else: the
// note is still in flight, and when the dispatcher's conversation ends before the post shows, it goes back.
func TestAckOfOneSignalLeavesTheNoteOfItsPost(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	_, err := f.s.Note("dev", "T-2 result ready", false)
	require.NoError(t, err)
	f.s.AddNotes([]string{"released designer after 31m idle"})
	require.NotEmpty(t, f.s.deliver(*f.now))

	rs := f.s.State.Role("dispatcher")
	require.Len(t, rs.Posted, 1)

	id := ackIDOf(t, f.s, "dev: T-2 result ready")
	f.s.AckLines([]string{id}, false, "dispatcher", "dispatcher")
	require.Len(t, rs.Posted, 1, "the note is still in flight")

	f.s.State.Stopped = *f.now // nothing starts a new dispatcher
	delete(f.tm.up, "dispatcher")

	for range 3 {
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
	}

	assert.Empty(t, rs.Posted)
	assert.Contains(t, f.s.State.Notes, "released designer after 31m idle", "the note went back")
	assert.Empty(t, f.s.State.Signals, "the acked signal did not")
}

// The same for a post kept as failed: acking its signal leaves its note, which a retry gives back.
func TestAckOfOneSignalLeavesTheNoteOfAFailedPost(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	_, err := f.s.Note("dev", "T-2 result ready", false)
	require.NoError(t, err)
	f.s.AddNotes([]string{"released designer after 31m idle"})
	require.NotEmpty(t, f.s.deliver(*f.now))

	id := ackIDOf(t, f.s, "dev: T-2 result ready")
	*f.now = f.now.Add(postedWait)
	f.s.followPosted("dispatcher", *f.now)

	rs := f.s.State.Role("dispatcher")
	require.Len(t, rs.Failed, 1)

	f.s.AckLines([]string{id}, false, "dispatcher", "dispatcher")
	require.Len(t, rs.Failed, 1, "its note is not acked")

	f.s.State.Notes = nil
	_, _, err = f.s.Retry("dispatcher", true)
	require.NoError(t, err)
	assert.Equal(t, []string{"released designer after 31m idle"}, f.s.State.Notes)
	assert.Empty(t, f.s.State.Signals)
}

// A line acked while it still sits in the role's input line has not landed: its record stays for landed to finish,
// which presses Enter again and then clears the input, and the acked line is not kept as failed. Cleared by the ack, it
// would leave a draft nothing presses Enter on and nothing clears, which blocks every later typed line.
func TestAckLeavesATypedLineThatSitsInTheInputLine(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", f.now.Add(-time.Hour))
	*f.now = f.now.Add(3 * time.Minute)

	line := "[dev] look at T-1 (zmina S-1)"
	f.s.State.AckSeq = 1
	f.s.State.Acks = map[string]*Ack{"S-1": {ID: "S-1", Role: "designer", Text: "[dev] look at T-1", First: *f.now, Last: *f.now}}

	rs := f.s.State.Role("designer")
	rs.Sent = &Sent{Line: line, At: *f.now, IDs: []string{"S-1"}}
	f.tm.drafts["designer"] = line // typed, Enter not taken

	acked, _ := f.s.AckLines([]string{"S-1"}, false, "", "hand")
	require.Equal(t, []string{"S-1"}, acked)
	require.NotNil(t, rs.Sent, "not landed: the line is still in the input line")
	assert.Equal(t, []string{"S-1"}, rs.Sent.AckedIDs, "its ack is recorded on it, for landed")

	for range typeTries + 1 {
		*f.now = f.now.Add(confirmAfter + time.Second)
		f.s.landed("designer", *f.now)
	}

	assert.Nil(t, rs.Sent)
	assert.Empty(t, f.tm.drafts["designer"], "the input line is cleared")
	assert.Empty(t, rs.Failed, "its reader has it: nothing to inspect")
	assert.NotContains(t, strings.Join(f.s.State.Notes, "|"), "delivery failed")
}

// A typed line is cleared without a failed mark only when its ack was recorded on the record (clearInFlight). A line
// whose id merely is not among the acks any more is no such line: the ack may have gone by eviction (acksMax) or by
// another path, and nobody read the line. It fails as an unconfirmed line does, for inspection.
func TestTypedLineWhoseAckWasEvictedStillFails(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.s.State.Role("designer").Inbound = true
	f.status("designer", "idle", *f.now)
	f.tm.onType = nil // the status never changes: the line never shows as landed
	*f.now = f.now.Add(3 * time.Minute)

	id, err := f.s.QueueTracked("designer", "[dev] look at T-1")
	require.NoError(t, err)
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	rs := f.s.State.Role("designer")
	require.NotNil(t, rs.Sent)

	for range acksMax {
		f.s.newAck("designer", "a later line")
	}

	require.Nil(t, f.s.State.Acks[id], "evicted by the newer ones")

	for range typeTries + 1 {
		*f.now = f.now.Add(confirmAfter + time.Second)
		f.s.landed("designer", *f.now)
	}

	assert.Nil(t, rs.Sent)
	require.Len(t, rs.Failed, 1, "kept as failed: nobody acked it")
	assert.Contains(t, rs.Failed[0].Line, "look at T-1")
	assert.Zero(t, f.eventCount("acked-cleared"))
}

// A Codex line acked while its file still waits in the bridge's inbox is withdrawn with its record: the next bridge
// would run a line its reader already has.
func TestAckWithdrawsACodexLineWaitingInItsFile(t *testing.T) {
	f, dir := codexFixture(t)
	bridge(t, f, dir, "thread-1")

	id, err := f.s.QueueTracked("dev", "[dispatcher] look at T-1")
	require.NoError(t, err)
	require.NotEmpty(t, f.s.deliverInbox("dev", *f.now))
	require.Len(t, waitingFiles(t, dir), 1)

	f.s.AckLines([]string{id}, false, "dev", "dev")

	assert.Empty(t, f.s.State.Role("dev").Posted)
	assert.Empty(t, waitingFiles(t, dir), "no bridge will run it")
}

// A line the bridge took before the ack is not touched: the taken mark is its record.
func TestAckOfACodexLineTheBridgeTookLeavesItsMark(t *testing.T) {
	f, dir := codexFixture(t)
	bridge(t, f, dir, "thread-1")

	id, err := f.s.QueueTracked("dev", "[dispatcher] look at T-1")
	require.NoError(t, err)
	require.NotEmpty(t, f.s.deliverInbox("dev", *f.now))

	files := waitingFiles(t, dir)
	require.Len(t, files, 1)
	take(t, dir, files[0])

	f.s.AckLines([]string{id}, false, "dev", "dev")

	assert.Empty(t, f.s.State.Role("dev").Posted)

	marks, err := os.ReadDir(filepath.Join(dir, "taken"))
	require.NoError(t, err)
	assert.Len(t, marks, 1)
}

// A typed line that carries two ids is acked when both were acked: an id that merely is not among the acks (evicted)
// is not an ack. With one acked and its sibling gone, nobody has read the other, and the line that never landed fails
// as an unconfirmed one does; with both acked, landed closes it without a failed mark.
func TestTypedLineIsAckedOnlyWhenEveryIDWasAcked(t *testing.T) {
	for _, both := range []bool{false, true} {
		f := fixtureWith(t, crewYAML)
		require.NoError(t, f.s.Up("designer"))
		f.status("designer", "idle", *f.now)
		*f.now = f.now.Add(3 * time.Minute)

		line := "[dev] look at T-1"
		rs := f.s.State.Role("designer")
		rs.Sent = &Sent{Line: line, At: *f.now, IDs: []string{"S-1", "S-2"}}
		f.s.State.Acks = map[string]*Ack{"S-2": {ID: "S-2", Role: "designer", Text: line, First: *f.now, Last: *f.now}}

		if both {
			f.s.State.Acks["S-1"] = &Ack{ID: "S-1", Role: "designer", Text: line, First: *f.now, Last: *f.now}
		} // else S-1 was evicted: it is in no ack

		f.tm.drafts["designer"] = line // typed, Enter not taken

		ids := []string{"S-2"}
		if both {
			ids = []string{"S-1", "S-2"}
		}

		acked, _ := f.s.AckLines(ids, false, "designer", "designer")
		require.Equal(t, ids, acked)
		require.NotNil(t, rs.Sent, "not landed: the line is still in the input line")
		assert.ElementsMatch(t, ids, rs.Sent.AckedIDs, "the ids that were acked, by name")

		for range typeTries + 1 {
			*f.now = f.now.Add(confirmAfter + time.Second)
			f.s.landed("designer", *f.now)
		}

		assert.Nil(t, rs.Sent)

		if both {
			assert.Empty(t, rs.Failed, "both acked: its readers have it")
			assert.Equal(t, 1, f.eventCount("acked-cleared"))

			continue
		}

		require.Len(t, rs.Failed, 1, "S-1 was never acked: its absence says nothing")
		assert.Zero(t, f.eventCount("acked-cleared"))
	}
}
