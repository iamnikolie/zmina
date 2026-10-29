package crew

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// starterNamespace is the caller's own namespace when it runs inside one, written with ~ for the home directory.
func starterNamespace() string {
	ns := os.Getenv("CLAUDE_CONFIG_DIR")
	if ns == "" {
		return defaultNamespace
	}

	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(ns, home+"/") {
		return "~" + strings.TrimPrefix(ns, home)
	}

	return ns
}

// Starter writes a crew file with the dispatcher and every role that has an AGENT.md under agents.
func Starter(agents string) string {
	var roles []string

	entries, _ := os.ReadDir(ExpandHome(agents))
	for _, e := range entries {
		if !e.IsDir() || e.Name() == defaultDispatcher || !roleRe.MatchString(e.Name()) {
			continue
		}

		if _, err := os.Stat(filepath.Join(ExpandHome(agents), e.Name(), "AGENT.md")); err == nil {
			roles = append(roles, e.Name())
		}
	}

	sort.Strings(roles)

	var b strings.Builder

	fmt.Fprintf(&b, `# zmina crew — the role sessions that work the shift. 'zmina skill' has the reference.
namespace: %s   # CLAUDE_CONFIG_DIR of every role
dir: %s         # default working directory
agents: %s      # <role>/AGENT.md is a role's default boot document
dispatcher: dispatcher                # never sleeps, routes tasks, hears the ticker
stuck_minutes: %d                     # busy longer than this -> the dispatcher hears of it

roles:
  dispatcher:
    about: routes tasks to roles, starts and stops them, watches their health
`, starterNamespace(), defaultDir, agents, defaultStuckMinutes)

	for _, r := range roles {
		fmt.Fprintf(&b, "  %s:\n    keep: manual   # manual | always\n", r)
	}

	b.WriteString(`  # dev-api:                         # a developer seat without an AGENT.md
  #   session: zm-dev-api
  #   dir: ~/work/api
  #   boot: read AGENTS.md, then wait for a task from zm-dispatcher
  #   about: the api service, backend
  #   model: opus
  #   args: [--chrome]
`)

	return b.String()
}
