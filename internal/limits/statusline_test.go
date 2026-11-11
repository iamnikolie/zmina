package limits

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func settingsFile(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return path
}

func TestOwnStatusLineTakesTheFirstFileThatSetsACommand(t *testing.T) {
	local := settingsFile(t, `{"statusLine":{"type":"command","command":"/p/local.sh","padding":2}}`)
	project := settingsFile(t, `{"statusLine":{"type":"command","command":"/p/project.sh"}}`)
	user := settingsFile(t, `{"theme":"dark","statusLine":{"type":"command","command":"~/bin/line.sh --short"}}`)
	missing := filepath.Join(t.TempDir(), "nothing.json")

	assert.Equal(t, map[string]any{"type": "command", "command": "/p/local.sh", "padding": 2.0}, OwnStatusLine(local, project, user))
	assert.Equal(t, map[string]any{"type": "command", "command": "/p/project.sh"}, OwnStatusLine(missing, project, user))
	assert.Equal(t, map[string]any{"type": "command", "command": "~/bin/line.sh --short"}, OwnStatusLine(missing, user))
}

func TestOwnStatusLinePassesOverWhatClaudeCodeWouldNotRun(t *testing.T) {
	user := settingsFile(t, `{"statusLine":{"type":"command","command":"/u/line.sh"}}`)

	for name, body := range map[string]string{
		"no status line":              `{"theme":"dark"}`,
		"not JSON":                    `{"statusLine":`,
		"a string":                    `{"statusLine":"/p/line.sh"}`,
		"another type":                `{"statusLine":{"type":"static","command":"/p/line.sh"}}`,
		"no type":                     `{"statusLine":{"command":"/p/line.sh"}}`,
		"an empty command":            `{"statusLine":{"type":"command","command":"  "}}`,
		"a command that is no string": `{"statusLine":{"type":"command","command":["a"]}}`,
		"null":                        `{"statusLine":null}`,
		"empty":                       ``,
	} {
		assert.Equal(t, map[string]any{"type": "command", "command": "/u/line.sh"}, OwnStatusLine(settingsFile(t, body), user), name)
	}

	assert.Nil(t, OwnStatusLine(), "no files")
	assert.Nil(t, OwnStatusLine(settingsFile(t, `{}`)), "no status line anywhere")
}
