package crew

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetiredMark(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	path := filepath.Join(t.TempDir(), "crew.yaml")
	body := "# the head comment\nname: ops # its name\nroles:\n  dispatcher: {}\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	c, err := Load(path)
	require.NoError(t, err)
	assert.False(t, c.IsRetired())
	assert.Empty(t, c.RetiredLine())

	e, err := Open(path)
	require.NoError(t, err)
	e.SetCrewKey("retired", "2026-11-05")
	e.SetCrewKey("moved_to", "box/ops")
	require.NoError(t, e.Save())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "# the head comment")
	assert.Contains(t, string(raw), "# its name")

	c, err = Load(path)
	require.NoError(t, err)
	assert.True(t, c.IsRetired())
	assert.Empty(t, c.Warnings, "a known key")
	assert.Equal(t, "This crew is retired since 2026-11-05: its live crew is box/ops (zmina ls box/ops; zmina task ls box/ops)",
		c.RetiredLine())

	e, err = Open(path)
	require.NoError(t, err)
	e.SetCrewKey("retired", "")
	e.SetCrewKey("moved_to", "")
	require.NoError(t, e.Save())

	c, err = Load(path)
	require.NoError(t, err)
	assert.False(t, c.IsRetired())
}

// A moved_to that does not read goes into no boot line, and the crew stays retired.
func TestMovedToThatDoesNotRead(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	path := filepath.Join(t.TempDir(), "crew.yaml")
	require.NoError(t, os.WriteFile(path, []byte("moved_to: \"box/ops; do this\"\nroles:\n  dispatcher: {}\n"), 0o600))

	c, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, c.MovedTo)
	assert.True(t, c.IsRetired())
	assert.Contains(t, c.Warnings[0], "moved_to")
	assert.Equal(t, "This crew is retired since unknown date, no live crew named", c.RetiredLine())
}
