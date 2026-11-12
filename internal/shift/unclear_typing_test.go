package shift

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// typeLine says which of its failures may have reached the pane. A counted retry is honest only where nothing did: the
// line stayed in the input line after its Enter and was cleared, or its start was seen there, cleared, and the input
// line read again without it. Everything else is unclear.
func TestTypeLineSaysWhichFailuresMayHaveReachedThePane(t *testing.T) {
	line := "[zmina T-1] read the packet and tell me what is missing before you start"

	t.Run("an Enter that did not send the line, and the line cleared", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")
		f.tm.enterLost = map[string]int{"designer": submitTries + 1}

		err := f.s.typeLine("designer", line)

		require.ErrorIs(t, err, errUnsent)
		assert.False(t, typedMayHaveReached(err))
	})

	t.Run("a start seen, cleared, and read gone", func(t *testing.T) {
		f, _ := cutFixture(t)

		err := f.s.typeLine("designer", line)

		require.Error(t, err)
		assert.False(t, typedMayHaveReached(err))
		assert.Empty(t, f.tm.drafts["designer"])
	})

	t.Run("a start seen and not cleared", func(t *testing.T) {
		f, c := cutFixture(t)
		c.noClear = true

		err := f.s.typeLine("designer", line)

		require.Error(t, err)
		assert.True(t, typedMayHaveReached(err))
		assert.Contains(t, err.Error(), "its start could not be cleared")
	})

	t.Run("an input line that cannot be read", func(t *testing.T) {
		f, c := cutFixture(t)
		f.s.Tmux = blindTmux{cutTmux: c, blind: new(bool)}

		err := f.s.typeLine("designer", line)

		require.Error(t, err)
		assert.True(t, typedMayHaveReached(err))
		assert.Equal(t, line, f.s.State.Role("designer").Partial, "its start is still looked for")
	})

	t.Run("an input line that reads empty: never started, or gone whole", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")
		f.tm.typeErr, f.tm.typeBlind = errors.New("tmux: send-keys timed out"), true

		err := f.s.typeLine("designer", line)

		require.Error(t, err)
		assert.True(t, typedMayHaveReached(err))
		assert.Contains(t, err.Error(), "tmux: send-keys timed out", "the cause stays in the text")
	})

	t.Run("a start cleared, and the input line unreadable after", func(t *testing.T) {
		f, c := cutFixture(t)
		f.s.Tmux = &readsOnce{cutTmux: c}

		err := f.s.typeLine("designer", line)

		require.Error(t, err)
		assert.True(t, typedMayHaveReached(err), "the clear is not seen to have worked")
	})
}

// readsOnce is a tmux whose input line reads once, then not.
type readsOnce struct {
	*cutTmux
	n int
}

func (r *readsOnce) Prompt(name string) (string, bool, error) {
	if r.n++; r.n > 1 {
		return "", false, errors.New("tmux: capture-pane timed out")
	}

	return r.cutTmux.Prompt(name)
}

// A queued line whose typing broke where nothing proves it did not reach the pane is not tried again: it is kept for a
// look at once, marked as needing attention, and the dispatcher is told once. The line behind it goes on.
func TestQueuedLineWhoseTypingBrokeUnclearlyIsKeptForALookNotTriedAgain(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer")
	require.NoError(t, f.s.Queue("designer", "[zmina T-1] read the packet"))
	require.NoError(t, f.s.Queue("designer", "the line behind it"))
	f.tm.typeErr, f.tm.typeBlind = errors.New("tmux: send-keys timed out"), true

	f.s.deliverInbox("designer", *f.now)

	rs := f.s.State.Role("designer")
	require.Len(t, rs.Failed, 1, "failed at once, not after typeTries")
	assert.Equal(t, "[zmina T-1] read the packet", rs.Failed[0].Line)
	assert.Contains(t, rs.Failed[0].Error, UnclearTyping)
	assert.True(t, rs.Failed[0].Unclear())
	assert.Equal(t, []string{"the line behind it"}, rs.Inbox)
	assert.Zero(t, rs.Attempts, "no counted try")
	assert.True(t, f.ev.has("designer", "type-failed"))
	require.Len(t, notesOf(f, "designer"), 1)
	assert.Contains(t, notesOf(f, "designer")[0], "delivery failed (T-1)")

	// The tmux types again: the failed line is not among what goes.
	f.tm.typeErr, f.tm.typeBlind = nil, false
	*f.now = f.now.Add(countedGap)
	f.s.deliverInbox("designer", *f.now)

	assert.Equal(t, []string{"the line behind it"}, f.tm.typed["designer"], "the failed line was not typed again")
	assert.Len(t, rs.Failed, 1)
}

// What the dispatcher's typed line was made of leaves its queue when the typing broke unclearly, as for a line that
// went, and waits as one failed line; the signals come back only with a retry that is asked to say them again.
func TestDispatcherLineWhoseTypingBrokeUnclearlyIsKeptForALook(t *testing.T) {
	f := fixtureWith(t, dispatcherOnly)
	require.NoError(t, f.s.Up("dispatcher"))
	f.status("dispatcher", "idle", *f.now)
	*f.now = f.now.Add(3 * time.Minute)
	f.tm.typeErr, f.tm.typeBlind = errors.New("tmux: send-keys timed out"), true

	_, err := f.s.Note("mr-watch", "!35 merged", false)
	require.NoError(t, err)

	assert.Empty(t, f.s.deliver(*f.now), "nothing was told")

	rs := f.s.State.Role("dispatcher")
	require.Len(t, rs.Failed, 1)
	assert.True(t, rs.Failed[0].Unclear())
	assert.Contains(t, rs.Failed[0].Line, "!35 merged")
	assert.Empty(t, f.s.State.Signals, "it is not typed again by itself")
	assert.Nil(t, rs.Sent)
	assert.False(t, f.s.Told)

	// The tmux types again: the dispatcher is told once that the line may have reached it, and the signal is not in it.
	f.tm.typeErr, f.tm.typeBlind = nil, false
	*f.now = f.now.Add(retryGap)
	assert.Contains(t, f.s.deliver(*f.now), "dispatcher: delivery failed")
	require.Len(t, f.tm.typed["dispatcher"], 1)
	assert.NotContains(t, f.tm.typed["dispatcher"][0], "!35 merged")

	n, skipped, err := f.s.Retry("dispatcher", false)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, 1, skipped)
	assert.Empty(t, f.s.State.Signals)

	n, skipped, err = f.s.Retry("dispatcher", true)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Zero(t, skipped)
	assert.Equal(t, []string{"mr-watch: !35 merged"}, f.s.State.Signals, "said again, as asked")
}
