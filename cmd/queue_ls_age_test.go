package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

// queue ls says how old a posted line and a failed line are, so a line that sat for days does not read as one of
// this minute.
func TestQueueLsShowsTheAgeOfPostedAndFailedLines(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	c, err := crew.Load(file)
	require.NoError(t, err)

	now := time.Now()

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)

	rs := l.Role("dev")
	rs.Posted = []shift.Posted{{Line: "[dispatcher] look at T-1", MsgID: "m-1", Session: "s-1", At: now.Add(-50 * time.Hour)}}
	rs.Failed = []shift.Failed{{Line: "[dispatcher] and T-2", Error: "tmux refused", At: now.Add(-3 * time.Hour)}}
	require.NoError(t, l.Save())

	out, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.Regexp(t, `posted +\[dispatcher\] look at T-1 \(to its inbox socket \d\d-\d\d \d\d:\d\d, 2d ago, not yet seen\)`, out)
	assert.Regexp(t, `failed +\[dispatcher\] and T-2 \(tmux refused, \d\d-\d\d \d\d:\d\d, 3h ago\)`, out)
}
