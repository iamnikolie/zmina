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
context_max: %dk                     # a role idle with more than this in its conversation is compacted
restart_at: %dk                      # a role holding a task this full restarts fresh from its checkpoint note
restart_done: %dk                     # a role whose task closed, this full, restarts fresh: a session per task
# worktree_gc: daily                   # once a day, remove the worktrees of closed tasks whose branch is merged
# courier: off                         # no resident courier beside the ticker: lines wait for the next beat (default on)
# retention: 30d                       # finished work older than this moves to archive/ once a day (30d at least, or off)

roles:
  dispatcher:
    about: routes tasks to roles, starts and stops them, watches their health
`, starterNamespace(), defaultDir, agents, defaultStuckMinutes, RecommendedContextMax/1000, RecommendedRestartAt/1000,
		RecommendedRestartDone/1000)

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
