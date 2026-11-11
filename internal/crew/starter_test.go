package crew

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The starter file carries the recommended values, parses, passes the strict check and says nothing in Warnings.
func TestStarterCarriesRecommendedValues(t *testing.T) {
	agents := t.TempDir()
	for _, r := range []string{"designer", "dispatcher"} {
		require.NoError(t, os.MkdirAll(filepath.Join(agents, r), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(agents, r, "AGENT.md"), []byte("x"), 0o600))
	}

	body := strings.Replace(Starter(agents), "dir: "+defaultDir, "dir: "+t.TempDir(), 1)
	path := write(t, body)

	c, err := LoadStrict(path)
	require.NoError(t, err)
	assert.Empty(t, c.Warnings)

	assert.Equal(t, ContextLimits{ContextMax: RecommendedContextMax, RestartAt: RecommendedRestartAt,
		RestartDone: RecommendedRestartDone}, c.CrewContext())

	dev := c.Roles["designer"]
	assert.Equal(t, RecommendedContextMax, dev.ContextLimit())
	assert.Equal(t, RecommendedRestartAt, dev.RestartContext())
	assert.Equal(t, RecommendedRestartDone, dev.RestartDoneAt())
}
