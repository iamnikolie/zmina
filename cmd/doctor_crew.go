package cmd

import (
	"slices"
	"strings"

	"github.com/iamnikolie/zmina/internal/crew"
)

// checkModels warns about roles that name no model: they run on the default of the Claude Code or Codex that starts
// them, so a change of that default reprices them without the crew file saying so. A warning, never a failure.
func checkModels(c *crew.Crew, add func(name, level, format string, args ...any)) {
	var open []string

	for _, name := range c.Names() {
		if !pinsModel(c.Roles[name]) {
			open = append(open, name)
		}
	}

	if len(open) == 0 {
		add("models", "ok", "every role names its model")

		return
	}

	noun := "roles"
	if len(open) == 1 {
		noun = "role"
	}

	add("models", "warn", "%d %s with no model: (%s) run on whatever default their CLI has today, so a change of it "+
		"reprices them unseen; zmina seat set <role> model=<name> pins one", len(open), noun, clipList(open))
}

// pinsModel reports whether a role's model is named by the crew file: model:, a --model among its args (Claude), or a
// profile (Codex), which carries the model of its own.
func pinsModel(r *crew.Role) bool {
	if r.Model != "" {
		return true
	}

	if r.IsCodex() {
		return r.Profile != ""
	}

	return slices.ContainsFunc(r.Args, func(a string) bool { return a == "--model" || strings.HasPrefix(a, "--model=") })
}
