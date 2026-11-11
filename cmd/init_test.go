package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// init writes the three recommended context values into the starter file; the file parses, passes the strict check
// and says ok in doctor's context area. An existing crew file is not rewritten.
func TestInitStarterCarriesRecommendedContextValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ZMINA_ROLE", "")

	dir := t.TempDir()
	path := filepath.Join(dir, "crew.yaml")
	agents := filepath.Join(dir, "agents")
	require.NoError(t, os.MkdirAll(filepath.Join(agents, "designer"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(agents, "designer", "AGENT.md"), []byte("x"), 0o600))

	out, _, err := zm(t, path, "init", "--agents", agents)
	require.NoError(t, err)
	assert.Contains(t, out, "wrote "+path)

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(body), "context_max: 250k ")
	assert.Contains(t, string(body), "restart_at: 180k ")
	assert.Contains(t, string(body), "restart_done: 60k ")

	c, err := crew.LoadStrict(path)
	require.NoError(t, err)
	assert.Empty(t, c.Warnings)
	assert.Equal(t, []string{"dispatcher", "designer"}, c.Names())

	assert.Equal(t, []string{"ok Claude roles at or below the recommended values: context_max 250k, " +
		"restart_at 180k, restart_done 60k"}, contextLines(t, path))

	_, _, err = zm(t, path, "init", "--agents", agents)
	require.Error(t, err, "an existing crew file is not rewritten")

	again, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(body), string(again))
}
