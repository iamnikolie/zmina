package shift

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The daily sweep is due when none has begun, or a day after the last one began; a stamp dated ahead of now (the
// clock was set back) does not hold it for days.
func TestWorktreeGCDue(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

	for name, tc := range map[string]struct {
		at  time.Time
		due bool
	}{
		"none yet":         {time.Time{}, true},
		"a minute ago":     {now.Add(-time.Minute), false},
		"just under a day": {now.Add(-WorktreeGCEvery + time.Second), false},
		"a day":            {now.Add(-WorktreeGCEvery), true},
		"two days":         {now.Add(-2 * WorktreeGCEvery), true},
		"dated ahead":      {now.Add(3 * 24 * time.Hour), true},
	} {
		st := NewState()
		st.WorktreeGCAt = tc.at
		assert.Equal(t, tc.due, st.WorktreeGCDue(now), name)
	}
}

// The stamp is saved with the state and a state written before it existed reads without it.
func TestWorktreeGCAtIsSaved(t *testing.T) {
	dir := t.TempDir()

	l, err := Lock(dir)
	require.NoError(t, err)
	assert.True(t, l.WorktreeGCAt.IsZero(), "a state from before the field has none")

	at := time.Date(2026, 11, 11, 4, 0, 0, 0, time.UTC)
	l.WorktreeGCAt = at
	require.NoError(t, l.Save())

	st, err := Read(dir)
	require.NoError(t, err)
	assert.True(t, at.Equal(st.WorktreeGCAt))
}
