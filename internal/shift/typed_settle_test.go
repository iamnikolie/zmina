package shift

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// busyAfterTyping is the role a moment after a line was typed into it: its status says busy, and its transcript, which
// does not show the line yet, still ends in the turn before it.
func busyAfterTyping(f fix, role string) {
	f.status(role, "busy", f.now.Add(100*time.Millisecond))
	f.rg.endedAt["sid-"+role] = f.now.Add(-5 * time.Minute)
}

// A line typed into a role that took it is not followed by the next while the transcript still ends in the turn
// before it: the role is busy with the first, and the next would be typed into a session that is working. It goes once
// the transcript shows a turn after the line, or typedSettle has passed.
func TestTypedLineWaitsForTheLineBeforeItToShow(t *testing.T) {
	setup := func(t *testing.T) (fix, time.Time) {
		f := typedFixture(t, crewYAML, "designer")
		require.NoError(t, f.s.Queue("designer", "one"))
		require.NoError(t, f.s.Queue("designer", "two"))

		typedAt := *f.now

		_, err := f.s.DeliverPass()
		require.NoError(t, err)
		require.Equal(t, []string{"one"}, f.tm.typed["designer"])
		require.Equal(t, typedAt, f.s.State.Role("designer").TypedAt)

		busyAfterTyping(f, "designer")

		return f, typedAt
	}

	t.Run("by the pass", func(t *testing.T) {
		f, typedAt := setup(t)

		for range 5 {
			*f.now = f.now.Add(200 * time.Millisecond)

			_, err := f.s.DeliverPass()
			require.NoError(t, err)
		}

		assert.Equal(t, []string{"one"}, f.tm.typed["designer"], "the second waits")
		assert.Equal(t, []string{"two"}, f.s.State.Role("designer").Inbox)

		*f.now = typedAt.Add(typedSettle - time.Millisecond)

		_, err := f.s.DeliverPass()
		require.NoError(t, err)
		assert.Equal(t, []string{"one"}, f.tm.typed["designer"])

		*f.now = typedAt.Add(typedSettle)

		_, err = f.s.DeliverPass()
		require.NoError(t, err)
		assert.Equal(t, []string{"one", "two"}, f.tm.typed["designer"], "the transcript has had its time")
	})

	t.Run("by the beat", func(t *testing.T) {
		f, typedAt := setup(t)

		*f.now = typedAt.Add(time.Second)
		assert.Empty(t, f.s.deliverInbox("designer", *f.now))
		assert.Equal(t, []string{"one"}, f.tm.typed["designer"])
	})

	t.Run("a turn after the line shows", func(t *testing.T) {
		f, typedAt := setup(t)

		// A background shell keeps the status busy after the turn the line began is over: the transcript says so.
		f.rg.endedAt["sid-designer"] = typedAt.Add(2 * time.Second)
		*f.now = typedAt.Add(3 * time.Second)

		_, err := f.s.DeliverPass()
		require.NoError(t, err)
		assert.Equal(t, []string{"one", "two"}, f.tm.typed["designer"])
	})

	t.Run("a beat a minute later", func(t *testing.T) {
		f, typedAt := setup(t)

		*f.now = typedAt.Add(time.Minute)
		assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now), "no slower than before for the settle")
		assert.Equal(t, []string{"one", "two"}, f.tm.typed["designer"])
	})
}

// The dispatcher's typed line is held the same way.
func TestTypedLineToTheDispatcherWaitsForTheLineBeforeItToShow(t *testing.T) {
	f := typedFixture(t, crewYAML, "dispatcher")

	_, err := f.s.Note("dev", "T-1 needs a decision", false)
	require.NoError(t, err)

	typedAt := *f.now

	_, err = f.s.DeliverPass()
	require.NoError(t, err)
	require.Len(t, f.tm.typed["dispatcher"], 1)

	busyAfterTyping(f, "dispatcher")

	_, err = f.s.Note("dev", "T-2 needs a decision", false)
	require.NoError(t, err)

	*f.now = typedAt.Add(time.Second)

	_, err = f.s.DeliverPass()
	require.NoError(t, err)
	assert.Len(t, f.tm.typed["dispatcher"], 1, "the second signal waits")

	*f.now = typedAt.Add(typedSettle)

	_, err = f.s.DeliverPass()
	require.NoError(t, err)
	assert.Len(t, f.tm.typed["dispatcher"], 2)
}
