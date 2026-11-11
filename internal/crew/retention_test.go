package crew

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retention is on at 30 days unless the crew file says otherwise: a span of 30 days or more, or off. Anything
// shorter or not a span is refused naming what it takes.
func TestRetentionKey(t *testing.T) {
	const roles = "namespace: /ns\nroles:\n  dispatcher:\n"

	for body, want := range map[string]struct {
		on   bool
		span time.Duration
	}{
		"":                        {true, 30 * 24 * time.Hour},
		"retention: 30d\n":        {true, 30 * 24 * time.Hour},
		"retention: 45d\n":        {true, 45 * 24 * time.Hour},
		"retention: 720h\n":       {true, 30 * 24 * time.Hour},
		"retention: 90d\n":        {true, 90 * 24 * time.Hour},
		"retention: off\n":        {false, 30 * 24 * time.Hour},
		"retention: \"off\"\n":    {false, 30 * 24 * time.Hour},
		"retention: \"\"\n":       {true, 30 * 24 * time.Hour},
		"retention: \"  60d \"\n": {true, 60 * 24 * time.Hour},
	} {
		c, err := Load(write(t, body+roles))
		require.NoError(t, err, body)
		assert.Equal(t, want.on, c.RetentionOn(), body)
		assert.Equal(t, want.span, c.RetentionSpan(), body)
		assert.Empty(t, c.Warnings, body)
	}

	for _, bad := range []string{"29d", "7d", "1h", "0", "30", "weekly", "on", "true", "Off"} {
		_, err := Load(write(t, "retention: "+bad+"\n"+roles))
		require.ErrorContains(t, err, "retention", bad)
		require.ErrorContains(t, err, "30 days at least", bad)
	}
}

// The starter names retention commented out; uncommented, the file still loads and the key has its value.
func TestStarterMentionsRetention(t *testing.T) {
	body := strings.Replace(Starter(t.TempDir()), "dir: "+defaultDir, "dir: "+t.TempDir(), 1)
	require.Contains(t, body, "\n# retention: 30d ")

	c, err := LoadStrict(write(t, body))
	require.NoError(t, err)
	assert.True(t, c.RetentionOn(), "on by default, with the comment as it is")

	c, err = LoadStrict(write(t, strings.Replace(body, "\n# retention: 30d", "\nretention: off", 1)))
	require.NoError(t, err)
	assert.False(t, c.RetentionOn())
}
