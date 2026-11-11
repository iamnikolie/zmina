package limits

import (
	"encoding/json"
	"maps"
	"os"
	"strings"
)

// OwnStatusLine is the status line Claude Code would draw without zmina: the statusLine setting of the first settings
// file, listed highest precedence first (a project's local settings, a project's, the owner's own), that sets a
// command. A copy of the setting, so its other fields (padding) go with it; nil when no file sets one. A file that is
// missing, does not parse, or sets a status line that is no command is passed over, as Claude Code would.
func OwnStatusLine(files ...string) map[string]any {
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var settings struct {
			StatusLine map[string]any `json:"statusLine"`
		}

		if json.Unmarshal(raw, &settings) != nil {
			continue
		}

		line := settings.StatusLine
		if cmd, ok := line["command"].(string); ok && line["type"] == "command" && strings.TrimSpace(cmd) != "" {
			return maps.Clone(line)
		}
	}

	return nil
}
