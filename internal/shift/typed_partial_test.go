package shift

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cutTmux is a tmux whose typing breaks half way: the first cut characters of the line reach the role's input line,
// then the call fails, as a send-keys that times out between two pieces. A line shorter than cut goes whole.
type cutTmux struct {
	*fakeTmux
	cut int
	// noClear makes the keys that clear the input line do nothing.
	noClear bool
}

func (c *cutTmux) Type(n, text string) error {
	r := []rune(text)
	if len(r) > c.cut {
		r = r[:c.cut]
	}

	c.drafts[n] = string(r)

	return errors.New("tmux: send-keys timed out")
}

func (c *cutTmux) TypeLines(n, text string) error { return c.Type(n, text) }

func (c *cutTmux) Keys(n string, keys ...string) error {
	if c.noClear {
		return errors.New("tmux: no server")
	}

	return c.fakeTmux.Keys(n, keys...)
}

// cutFixture is a crew with a designer up, idle and booted, whose tmux breaks half way through a typed line.
func cutFixture(t *testing.T) (fix, *cutTmux) {
	t.Helper()

	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	*f.now = f.now.Add(bootPause)
	f.status("designer", "idle", *f.now)

	c := &cutTmux{fakeTmux: f.tm, cut: 30}
	f.s.Tmux = c

	return f, c
}

// A line whose typing broke half way does not leave its start in the role's input line: it is cleared, the try counts,
// and the line stays queued for the next try, which types it whole into an empty input line.
func TestTypedLineThatBreaksHalfWayIsClearedAndTriedAgain(t *testing.T) {
	f, _ := cutFixture(t)
	line := "[zmina T-1] read the packet and tell me what is missing before you start"
	require.NoError(t, f.s.Queue("designer", line))

	f.s.deliverInbox("designer", *f.now)

	rs := f.s.State.Role("designer")
	assert.Empty(t, f.tm.drafts["designer"], "nothing of the line is left in the input line")
	assert.Equal(t, []string{"C-e", "C-u"}, f.tm.keys["designer"])
	assert.Equal(t, 1, rs.Attempts)
	assert.Equal(t, []string{line}, rs.Inbox, "still queued")
	assert.Empty(t, rs.Failed)
	assert.True(t, f.ev.has("designer", "unsent-cleared"))
	assert.True(t, f.ev.has("designer", "type-failed"))

	// The tmux comes back: the next counted try finds an empty input line and types the line whole.
	f.s.Tmux = f.tm
	*f.now = f.now.Add(countedGap)
	f.s.deliverInbox("designer", *f.now)

	assert.Equal(t, []string{line}, f.tm.typed["designer"])
	assert.Empty(t, rs.Inbox)
	assert.NotNil(t, rs.Sent)
}

// A line that keeps failing half way reaches the failed list after typeTries tries: its leftovers never hold the queue,
// so the tries are not stopped by the draft guard, and the line behind it is typed next.
func TestTypedLineThatBreaksHalfWayEveryTimeMovesOn(t *testing.T) {
	f, _ := cutFixture(t)
	require.NoError(t, f.s.Queue("designer", "[zmina T-1] a line the tmux cannot type to its end"))
	require.NoError(t, f.s.Queue("designer", "the line behind it"))

	for range typeTries {
		f.s.deliverInbox("designer", *f.now)
		*f.now = f.now.Add(countedGap)
	}

	rs := f.s.State.Role("designer")
	require.Len(t, rs.Failed, 1)
	assert.Equal(t, "[zmina T-1] a line the tmux cannot type to its end", rs.Failed[0].Line)
	assert.Equal(t, []string{"the line behind it"}, rs.Inbox)
	assert.Empty(t, f.tm.drafts["designer"])

	f.s.Tmux = f.tm
	f.s.deliverInbox("designer", *f.now)
	assert.Equal(t, []string{"the line behind it"}, f.tm.typed["designer"])
}

// The same for the line typed into the dispatcher: what its typing left is cleared, and the signals stay.
func TestDispatcherLineThatBreaksHalfWayIsCleared(t *testing.T) {
	f := fixtureWith(t, dispatcherOnly)
	require.NoError(t, f.s.Up("dispatcher"))
	f.status("dispatcher", "idle", *f.now)
	*f.now = f.now.Add(3 * time.Minute)
	f.s.Tmux = &cutTmux{fakeTmux: f.tm, cut: 12}

	_, err := f.s.Note("mr-watch", "!35 merged", false)
	require.NoError(t, err)
	f.s.Tick()

	assert.Empty(t, f.tm.drafts["dispatcher"], "nothing of the line is left in the input line")
	assert.Equal(t, []string{"C-e", "C-u"}, f.tm.keys["dispatcher"])
	assert.Equal(t, []string{"mr-watch: !35 merged"}, f.s.State.Signals, "still waits")
	assert.Nil(t, f.s.State.Role("dispatcher").Sent)
}

// Only what zmina typed is cleared: a draft that is not the start of the line, whatever the typing did, is the
// owner's, and it stays.
func TestPartialClearLeavesTheOwnersDraft(t *testing.T) {
	f, _ := cutFixture(t)

	// The owner's words reach the input line while the line is being typed: the draft is theirs.
	f.s.Tmux = ownerTmux{f.tm}

	err := f.s.typeLine("designer", "[zmina T-1] read the packet")
	require.Error(t, err)

	assert.Equal(t, "half a thought of the owner", f.tm.drafts["designer"], "the draft stays")
	assert.Empty(t, f.tm.keys["designer"], "no key was sent")
}

// ownerTmux is a tmux that fails to type while the owner's draft sits in the input line.
type ownerTmux struct{ *fakeTmux }

func (o ownerTmux) Type(n, _ string) error {
	o.drafts[n] = "half a thought of the owner"

	return errors.New("tmux: send-keys timed out")
}

func (o ownerTmux) TypeLines(n, text string) error { return o.Type(n, text) }

// A start that cannot be cleared is not a failure to try again: the line may be sent by any Enter that reaches the input
// line, so it is kept as one that needs attention, with the error saying why the input line still holds it.
func TestPartialClearThatFailsKeepsTheLineForALook(t *testing.T) {
	f, c := cutFixture(t)
	c.noClear = true
	line := "[zmina T-1] read the packet and tell me what is missing before you start"
	require.NoError(t, f.s.Queue("designer", line))

	f.s.deliverInbox("designer", *f.now)

	rs := f.s.State.Role("designer")
	assert.Empty(t, rs.Inbox, "not typed again")
	require.Len(t, rs.Failed, 1)
	assert.True(t, rs.Failed[0].Unclear())
	assert.Contains(t, rs.Failed[0].Error, "could not be cleared from the input line")
	assert.NotEmpty(t, f.tm.drafts["designer"], "the clear did not take")
	require.NotEmpty(t, f.s.State.Notes)
	assert.Contains(t, f.s.State.Notes[0], "could not be cleared from the input line")
}

// blindTmux is a tmux that breaks half way through a line and cannot be read from then on, as one whose server
// stopped answering: the start is in the input line, and nothing can be told of it yet.
type blindTmux struct {
	*cutTmux
	blind *bool
}

func (b blindTmux) Type(n, text string) error {
	*b.blind = true

	return b.cutTmux.Type(n, text)
}

func (b blindTmux) Prompt(n string) (string, bool, error) {
	if *b.blind {
		return "", false, errors.New("tmux: capture-pane timed out")
	}

	return b.cutTmux.Prompt(n)
}

// A start that could not even be looked for is remembered, and the line it was is kept for a look, not typed again: the
// delivery that comes once the tmux answers clears the start before it reads the input line for a draft, so it does not
// hold the queue as the owner's, and goes on with the line behind it.
func TestPartialThatCouldNotBeReadIsClearedByTheNextDelivery(t *testing.T) {
	f, c := cutFixture(t)
	line := "[zmina T-1] read the packet and tell me what is missing before you start"
	require.NoError(t, f.s.Queue("designer", line))
	require.NoError(t, f.s.Queue("designer", "the line behind it"))

	f.s.Tmux = blindTmux{cutTmux: c, blind: new(bool)}
	f.s.deliverInbox("designer", *f.now)

	rs := f.s.State.Role("designer")
	assert.Equal(t, line, rs.Partial, "remembered")
	assert.Empty(t, f.tm.keys["designer"], "no key into an input line that cannot be read")
	assert.NotEmpty(t, f.tm.drafts["designer"], "the start is still there")
	require.Len(t, rs.Failed, 1, "its end is not known: kept for a look, not tried again")
	assert.Equal(t, line, rs.Failed[0].Line)
	assert.True(t, rs.Failed[0].Unclear())

	// The tmux answers again: the start goes, and the line behind it is typed whole.
	f.s.Tmux = f.tm
	*f.now = f.now.Add(countedGap)
	f.s.deliverInbox("designer", *f.now)

	assert.Equal(t, []string{"C-e", "C-u"}, f.tm.keys["designer"])
	assert.Empty(t, rs.Partial, "the mark goes with a line typed whole")
	assert.Equal(t, []string{"the line behind it"}, f.tm.typed["designer"])
	assert.Len(t, rs.Failed, 1)
}

// The mark does not outlive the owner's own words: a draft that is not the start of the line stays, and the mark goes.
func TestLeftoverMarkLeavesTheOwnersDraft(t *testing.T) {
	f, _ := cutFixture(t)
	f.s.Tmux = f.tm
	f.s.State.Role("designer").Partial = "[zmina T-1] read the packet"
	f.tm.drafts["designer"] = "half a thought of the owner"

	f.s.clearLeftover("designer")

	assert.Equal(t, "half a thought of the owner", f.tm.drafts["designer"])
	assert.Empty(t, f.tm.keys["designer"])
	assert.Empty(t, f.s.State.Role("designer").Partial)
}
