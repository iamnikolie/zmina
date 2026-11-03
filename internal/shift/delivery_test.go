package shift

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnconfirmedSubmissionIsNotBlindlyRetyped(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	f.tm.onType = nil // Registry missed the accepted turn; the input is already empty.
	*f.now = f.now.Add(3 * time.Minute)
	line := "[dispatcher] Review the change"
	require.NoError(t, f.s.Queue("designer", line))
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	for i := 0; i < 5; i++ {
		*f.now = f.now.Add(2 * time.Minute)
		f.s.landed("designer", *f.now)
		f.s.deliverInbox("designer", *f.now)
	}
	assert.Equal(t, []string{line}, f.tm.typed["designer"])
	require.Len(t, f.s.State.Role("designer").Failed, 1)
	assert.Contains(t, f.s.State.Role("designer").Failed[0].Error, "unconfirmed")
	require.NotEmpty(t, notesOf(f, "designer"))
	assert.Contains(t, notesOf(f, "designer")[0], "delivery failed")
}

func TestQueuePreviewDoesNotConsumeOrDuplicateDelivery(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "busy", *f.now)
	*f.now = f.now.Add(3 * time.Minute)
	line := "[dispatcher] Review the change"
	require.NoError(t, f.s.Queue("designer", line))
	for i := 0; i < 2; i++ {
		require.Len(t, f.s.Queues(), 1)
		assert.Equal(t, []string{line}, f.s.Queues()[0].Inbox)
		assert.Empty(t, f.s.deliverInbox("designer", *f.now))
	}
	f.status("designer", "idle", *f.now)
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	for i := 0; i < 3; i++ {
		*f.now = f.now.Add(2 * time.Minute)
		assert.Empty(t, f.s.deliverInbox("designer", *f.now))
	}
	assert.Equal(t, []string{line}, f.tm.typed["designer"])
	assert.Empty(t, f.s.Queues())
}

func TestOwnRemainingDraftRetriesEnterThenClears(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	line := "Review the change"
	f.tm.drafts["designer"] = line
	f.s.State.Role("designer").Sent = &Sent{Line: line, At: *f.now}
	for i := 0; i < typeTries; i++ {
		*f.now = f.now.Add(2 * time.Minute)
		assert.Equal(t, i == typeTries-1, f.s.landed("designer", *f.now))
	}
	assert.Empty(t, f.tm.typed["designer"], "only Enter retries, never a second paste")
	assert.Equal(t, []string{"Enter", "Enter", "C-e", "C-u"}, f.tm.keys["designer"])
	assert.Empty(t, f.tm.drafts["designer"])
	require.Len(t, f.s.State.Role("designer").Failed, 1)
	assert.Equal(t, "typed, never taken", f.s.State.Role("designer").Failed[0].Error)
}
