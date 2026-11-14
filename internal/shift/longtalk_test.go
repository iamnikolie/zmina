package shift

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/asks"
)

// A role whose conversation is long and that no restart will shorten is told of to the owner, once a conversation,
// answered or not. One whose restart is still to come is not.
func TestLongConversationIsToldToTheOwnerOnce(t *testing.T) {
	f := restarting(t, "", 300_000, time.Minute)
	store := &asks.Store{Dir: f.s.Crew.Layout().Asks()}
	f.s.Asks = store

	r, err := f.s.Role("designer")
	require.NoError(t, err)

	assert.Empty(t, f.s.longTalk("designer", r, *f.now), "its restart is still to come")
	assert.Empty(t, store.Pending())

	// The restart of this conversation was called off: nothing shortens it now.
	f.s.State.Role("designer").RestartGaveUp = "sid-designer"

	line := f.s.longTalk("designer", r, *f.now)
	assert.Contains(t, line, "designer: a long conversation (300k): told the owner (A-1)")

	open := store.Pending()
	require.Len(t, open, 1)
	assert.Equal(t, "designer: a long conversation, 300k", open[0].Title)
	assert.Contains(t, open[0].Text, "zmina restart designer --fresh")

	assert.Empty(t, f.s.longTalk("designer", r, *f.now), "said once")

	_, err = store.Close("A-1", asks.Ignored, "", *f.now)
	require.NoError(t, err)
	assert.Empty(t, f.s.longTalk("designer", r, *f.now), "and not again once he set it aside")

	// A shorter conversation, and the dispatcher's, are nobody's question.
	f.rg.context["sid-designer"] = 100_000
	f.s.State.Role("designer").RestartGaveUp = ""
	assert.Empty(t, f.s.longTalk("designer", r, *f.now))

	d, err := f.s.Role("dispatcher")
	require.NoError(t, err)
	assert.Empty(t, f.s.longTalk("dispatcher", d, *f.now))
}
