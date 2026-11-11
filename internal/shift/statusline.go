package shift

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/iamnikolie/zmina/internal/limits"
)

// tapStatusLine is the status line a role starts with: zmina _limits-tap, which records the plan limits Claude Code
// hands every status line (zmina limits reads them) and then runs the owner's own status line on the same input, so
// the pane draws what it always did. The owner's own is the one Claude Code would pick for the role: its project's
// local settings, its project's, then the namespace's, and it is only read, never edited; the role's session is the
// only one that gets the tap. The project is the directory the session starts in (dir: a task's worktree when the role
// is started with one), then the role's configured dir. With none configured the tap draws nothing of its own. base
// is the zmina executable and the crew, as every hook of the role names them.
func (s *Shift) tapStatusLine(name, base, dir string) map[string]any {
	var files []string

	role, _, _ := strings.Cut(name, "#")

	projects := []string{dir}
	if r, ok := s.Crew.Roles[role]; ok {
		projects = append(projects, r.Dir)
	}

	for i, p := range projects {
		if p == "" || slices.Contains(projects[:i], p) {
			continue
		}

		files = append(files, filepath.Join(p, ".claude", "settings.local.json"), filepath.Join(p, ".claude", "settings.json"))
	}

	files = append(files, filepath.Join(s.Crew.Namespace, "settings.json"))

	line := map[string]any{"type": "command"}
	command := base + " _limits-tap"

	if own := limits.OwnStatusLine(files...); own != nil {
		maps.Copy(line, own) // its other fields (padding) stay
		command += " -- " + shellWord(own["command"].(string))
	}

	line["command"] = command

	return line
}
