package cmd

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
)

// usageRestart is one restart of a role for its context (restart_at, restart_idle, restart_done), from the crew's
// event log.
type usageRestart struct {
	Node string    `json:"node,omitempty"`
	Crew string    `json:"crew,omitempty"`
	Role string    `json:"role"`
	At   time.Time `json:"at"`
	Task string    `json:"task,omitempty"`
	Why  string    `json:"why,omitempty"`
}

// usageRestarts reads the crews' restarts since a time; a copy's (dev#2) count as its role's, as its turns do.
func usageRestarts(crews []uiCrew, node string, since time.Time) []usageRestart {
	var out []usageRestart

	for _, uc := range crews {
		c, err := crew.Load(uc.Path)
		if err != nil {
			continue // usageCells names the crew it could not read
		}

		evs, _ := eventLog(c).Read(events.Filter{Since: since})
		for _, e := range evs {
			if e.Event != "restarted" {
				continue
			}

			role, _, _ := strings.Cut(e.Role, "#")
			why, _, _ := strings.Cut(e.Detail, ";")
			out = append(out, usageRestart{Node: node, Crew: uc.Key, Role: role, At: e.TS, Task: e.Task, Why: why})
		}
	}

	return out
}

// printRestarts says how often roles were restarted fresh, by role, and what for.
func printRestarts(out io.Writer, rs []usageRestart, manyCrews bool) {
	if len(rs) == 0 {
		return
	}

	type count struct{ all, context, idle, done int }

	by := map[string]*count{}

	var keys []string

	for _, r := range rs {
		key := r.Role
		if manyCrews {
			key = crewLabel(r.Node, r.Crew) + " " + r.Role
		}

		c := by[key]
		if c == nil {
			c = &count{}
			by[key] = c
			keys = append(keys, key)
		}

		c.all++

		switch {
		case strings.HasPrefix(r.Why, "context"):
			c.context++
		case strings.HasPrefix(r.Why, "idle"):
			c.idle++
		case strings.HasPrefix(r.Why, "task done"):
			c.done++
		}
	}

	slices.SortFunc(keys, func(a, b string) int {
		if d := by[b].all - by[a].all; d != 0 {
			return d
		}

		return strings.Compare(a, b)
	})

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		c := by[k]
		what := fmt.Sprintf("context %d, idle %d", c.context, c.idle)
		if c.done > 0 {
			what += fmt.Sprintf(", task done %d", c.done)
		}

		parts = append(parts, fmt.Sprintf("%s %d (%s)", k, c.all, what))
	}

	fmt.Fprintf(out, "\nRestarted fresh %s: %s.\n", times(len(rs)), strings.Join(parts, ", "))
}

func times(n int) string {
	switch n {
	case 1:
		return "once"
	case 2:
		return "twice"
	}

	return strconv.Itoa(n) + " times"
}
