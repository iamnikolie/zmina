package shift

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The kind of a failed line is read from its text; no field of the state holds it.
func TestFailedLineIsUnclearByItsText(t *testing.T) {
	for text, want := range map[string]bool{
		UnclearTyping:                          true,
		UnclearTyping + " (tmux: timed out)":   true,
		UnclearEnded:                           true,
		UnclearUnconfirmed:                     true,
		postedNever:                            true,
		"tmux refused":                         false,
		"typed, never taken":                   false,
		errUnsent.Error():                      false,
		"typed, Enter did not send it":         false,
		"":                                     false,
		"typing broke; its start could not be": false,
	} {
		assert.Equal(t, want, Failed{Error: text}.Unclear(), text)
	}
}

// queue retry puts back what nothing reached and leaves what may have reached the role, saying how many it left; with
// unclear it puts back all.
func TestRetrySkipsLinesThatMayHaveReachedTheRole(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))

	rs := f.s.State.Role("designer")
	now := *f.now
	rs.Failed = []Failed{
		{Line: "refused", Error: "tmux refused", At: now},
		{Line: "may be there", Error: UnclearTyping + " (tmux: timed out)", At: now},
		{Line: "never shown", Error: postedNever, At: now},
	}

	n, skipped, err := f.s.Retry("designer", false)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 2, skipped)
	assert.Equal(t, []string{"refused"}, rs.Inbox)
	require.Len(t, rs.Failed, 2)
	assert.Equal(t, "may be there", rs.Failed[0].Line)

	n, skipped, err = f.s.Retry("designer", true)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Zero(t, skipped)
	assert.Equal(t, []string{"may be there", "never shown", "refused"}, rs.Inbox)
	assert.Empty(t, rs.Failed)
}

// queue ls tells the two kinds apart in its data, and a typed line still in flight is listed: both carry through
// --json, which is these views.
func TestQueuesCarryTheKindOfEachFailedLineAndTheLineInFlight(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))

	rs := f.s.State.Role("designer")
	rs.Failed = []Failed{
		{Line: "refused", Error: "tmux refused", At: *f.now},
		{Line: "may be there", Error: UnclearEnded, At: *f.now},
	}
	rs.Sent = &Sent{Line: "typed a moment ago", At: *f.now}

	qs := f.s.Queues()
	require.Len(t, qs, 1)
	require.Len(t, qs[0].Failed, 2)
	assert.False(t, qs[0].Failed[0].Unclear)
	assert.True(t, qs[0].Failed[1].Unclear)
	require.NotNil(t, qs[0].Sent)
	assert.Equal(t, "typed a moment ago", qs[0].Sent.Line)

	var view View

	for _, v := range f.s.Views() {
		if v.Role == "designer" {
			view = v
		}
	}

	assert.Equal(t, 2, view.Failed)
	assert.Equal(t, 1, view.Attention)

	// A role with only a line in flight is listed too.
	rs.Failed = nil
	require.Len(t, f.s.Queues(), 1)
}
