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

	t.Run("a failure before any key was sent", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")
		f.tm.typeErr = errNotTyped("tmux list-panes: no answer in 10s")

		err := f.s.typeLine("designer", line)

		require.Error(t, err)
		assert.False(t, typedMayHaveReached(err))
		assert.Contains(t, err.Error(), "no answer in 10s", "its own words stay")
		assert.Empty(t, f.s.State.Role("designer").Partial, "no start to look for")
		assert.Empty(t, f.tm.keys["designer"], "no key sent")
	})

	t.Run("a start seen in the input line, cleared, and read gone", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")
		f.tm.typeErr, f.tm.typeLeaves = errors.New("tmux: send-keys timed out"), true

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
		f.tm.typeErr = errors.New("tmux: send-keys timed out")

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
	f.tm.typeErr = errors.New("tmux: send-keys timed out")

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
	f.tm.typeErr = nil
	*f.now = f.now.Add(countedGap)
	f.s.deliverInbox("designer", *f.now)

	assert.Equal(t, []string{"the line behind it"}, f.tm.typed["designer"], "the failed line was not typed again")
	assert.Len(t, rs.Failed, 1)
}

// The dispatcher is not a role: whatever its typing breaks on, its signals and notes stay queued, the next try waits
// retryGap, and nothing says it failed, not even a note to the dispatcher about its own line. One blip, and then several,
// leave nothing in the failed list, no note piling up, and every signal still to come.
func TestDispatcherLineIsNeverKeptAsFailedWhenItsTypingBreaks(t *testing.T) {
	for name, fail := range map[string]func(f fix){
		"nothing was typed":            func(f fix) { f.tm.typeErr = errNotTyped("tmux list-panes: no answer in 10s") },
		"the pane shows nothing of it": func(f fix) { f.tm.typeErr = errors.New("tmux send-keys: no answer in 10s") },
		"its start is in the input line": func(f fix) {
			f.tm.typeErr, f.tm.typeLeaves = errors.New("tmux send-keys: no answer in 10s"), true
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, dispatcherOnly)
			require.NoError(t, f.s.Up("dispatcher"))
			f.status("dispatcher", "idle", *f.now)
			*f.now = f.now.Add(3 * time.Minute)

			_, err := f.s.Note("mr-watch", "!35 merged", false)
			require.NoError(t, err)
			f.s.addNotes([]string{"designer: looks stuck"})
			acks := len(f.s.State.Acks)
			calls := counted(f, nil)

			fail(f)

			rs := f.s.State.Role("dispatcher")

			// One try each retryGap, however many passes come between: nothing piles up.
			for try := 1; try <= 4; try++ {
				assert.Empty(t, f.s.deliver(*f.now))
				assert.Equal(t, try, calls.count("tmux Type"), "tried")
				assert.Empty(t, rs.Failed, "never kept as failed")
				assert.Equal(t, []string{"mr-watch: !35 merged"}, f.s.State.Signals, "still queued")
				assert.Equal(t, []string{"designer: looks stuck"}, f.s.State.Notes, "no note about its own line")
				assert.Equal(t, acks, len(f.s.State.Acks), "no id left behind by a line that did not go")
				assert.Nil(t, rs.Sent)

				*f.now = f.now.Add(5 * time.Second)
				assert.Empty(t, f.s.deliver(*f.now), "inside retryGap: not tried again")
				assert.Equal(t, try, calls.count("tmux Type"))

				*f.now = f.now.Add(retryGap)
			}

			// The tmux answers: one try after the gap, and the line goes with its signal and its note.
			f.tm.typeErr, f.tm.typeLeaves = nil, false
			delete(f.tm.drafts, "dispatcher")
			*f.now = f.now.Add(retryGap)

			line := f.s.deliver(*f.now)

			assert.Contains(t, line, "!35 merged")
			require.Len(t, f.tm.typed["dispatcher"], 1)
			assert.Contains(t, f.tm.typed["dispatcher"][0], "designer: looks stuck")
			assert.Empty(t, rs.Failed)
			assert.Empty(t, f.s.State.Signals)
		})
	}
}

// A tmux that did not get as far as a key (the commonest blip: the pane could not be asked for in time) leaves a line
// where main left it: tried again, counted, and failed as one nothing reached after typeTries tries.
func TestQueuedLineWhoseTypingNeverStartedIsTriedAgainThenFailedAsNothingReached(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer")
	require.NoError(t, f.s.Queue("designer", "[zmina T-1] read the packet"))
	f.tm.typeErr = errNotTyped("tmux list-panes: no answer in 10s")

	rs := f.s.State.Role("designer")

	for try := 1; try < typeTries; try++ {
		f.s.deliverInbox("designer", *f.now)

		assert.Equal(t, try, rs.Attempts, "a counted try")
		assert.Equal(t, []string{"[zmina T-1] read the packet"}, rs.Inbox, "still queued")
		assert.Empty(t, rs.Failed)
		assert.Empty(t, rs.Partial, "nothing was typed: no start to look for")
		assert.Empty(t, f.tm.keys["designer"], "no key into the pane")

		*f.now = f.now.Add(countedGap)
	}

	f.s.deliverInbox("designer", *f.now)

	require.Len(t, rs.Failed, 1)
	assert.False(t, rs.Failed[0].Unclear(), "nothing reached the role: a retry is safe")
	assert.Contains(t, rs.Failed[0].Error, "no answer in 10s")
	assert.Empty(t, rs.Inbox)

	n, skipped, err := f.s.Retry("designer", false)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Zero(t, skipped)
}

// A tracked role line whose typing broke unclearly counts as delivered: its ack runs from then and says it again after
// the crew's renotify, and that is the only way it is said again; a plain retry leaves the failed record alone.
func TestTrackedLineWhoseTypingBrokeUnclearlyIsSaidAgainByItsAck(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer")
	rs := f.s.State.Role("designer")
	rs.Inbound = true

	id, err := f.s.QueueTracked("designer", "[dev] look at T-1")
	require.NoError(t, err)

	f.tm.typeErr = errors.New("tmux: send-keys timed out")
	f.s.deliverInbox("designer", *f.now)

	require.Len(t, rs.Failed, 1)
	require.True(t, rs.Failed[0].Unclear())
	assert.Equal(t, []string{id}, rs.Failed[0].IDs)
	assert.Empty(t, rs.Inbox)

	ack := f.s.State.Acks[id]
	require.NotNil(t, ack)
	assert.Equal(t, *f.now, ack.Last, "delivered: its ack runs from now")
	assert.Equal(t, *f.now, ack.First)

	// Past the renotify interval it is said again, once, as a line of its own.
	f.tm.typeErr = nil
	*f.now = f.now.Add(f.s.Crew.RenotifyD() + time.Minute)
	f.status("designer", "idle", *f.now)
	f.s.renotify(*f.now)

	require.Len(t, rs.Inbox, 1)
	assert.Contains(t, rs.Inbox[0], "[dev] look at T-1")
	assert.Contains(t, rs.Inbox[0], "still pending since")
	assert.Equal(t, 1, f.s.State.Acks[id].Renotes)

	// A plain retry does not type it a second time from the failed list.
	n, skipped, err := f.s.Retry("designer", false)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, 1, skipped)
	assert.Len(t, rs.Inbox, 1)
	assert.Len(t, rs.Failed, 1)
}
