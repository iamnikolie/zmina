package shift

import (
	"errors"
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

	f.tm.typeErr = errors.New("tmux refused")
	for range typeTries {
		f.s.Tick()
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

	n, err := f.s.Retry("designer")
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
	g.tm.onType = nil
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
