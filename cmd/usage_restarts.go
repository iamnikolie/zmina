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
	"github.com/iamnikolie/zmina/internal/shift"
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
	// Outcome is how the restart's rotation ended (rebuilt, resumed, lost) or "open" while it has not; empty when no record of
	// it is found (an older zmina, or a record removed). It is read from the rotation records, never written.
	Outcome string `json:"outcome,omitempty"`
}

// restartRecordSlack is how far apart a restarted event and the start its rotation's record shows may be for them to be one
// restart: the event is logged right after the start's line, a beat that finished a dead one's restart a little later.
const restartRecordSlack = 5 * time.Minute

// usageRestarts reads the crews' restarts since a time; a copy's (dev#2) count as its role's, as its turns do.
func usageRestarts(crews []uiCrew, node string, since time.Time) []usageRestart {
	var out []usageRestart

	for _, uc := range crews {
		c, err := crew.Load(uc.Path)
		if err != nil {
			continue // usageCells names the crew it could not read
		}

		evs, _ := eventLog(c).Read(events.Filter{Since: since})
		recorded := restartRotations(c)

		for _, e := range evs {
			if e.Event != "restarted" {
				continue
			}

			role, _, _ := strings.Cut(e.Role, "#")
			why, _, _ := strings.Cut(e.Detail, ";")
			out = append(out, usageRestart{Node: node, Crew: uc.Key, Role: role, At: e.TS, Task: e.Task, Why: why,
				Outcome: recorded.outcomeOf(e.Role, why, e.TS)})
		}
	}

	return out
}

// restartRecords are a crew's rotations of the ticker's restarts (restart_at, restart_idle, restart_done), each used once.
type restartRecords struct {
	rots []shift.Rotation
	used []bool
}

// restartRotations reads the crew's rotation records for the restarts of the window; a record that is not there or does not read
// leaves every restart without an outcome.
func restartRotations(c *crew.Crew) *restartRecords {
	rots, _ := shift.ReadRotations(c.Layout(), "")

	rots = slices.DeleteFunc(rots, func(r shift.Rotation) bool {
		return r.Started.IsZero() || r.Kind != shift.KindRestartAt && r.Kind != shift.KindRestartIdle && r.Kind != shift.KindRestartDone
	})

	return &restartRecords{rots: rots, used: make([]bool, len(rots))}
}

// outcomeOf is how the restart of a role (a copy by its own name) for a reason, logged at a time, ended: the nearest record not
// taken by another restart that started within restartRecordSlack of it, with the same words for why. Empty when there is none.
func (rr *restartRecords) outcomeOf(role, why string, at time.Time) string {
	best, gap := -1, restartRecordSlack+1

	for i, r := range rr.rots {
		d := r.Started.Sub(at).Abs()
		if rr.used[i] || r.Role != role || r.Why != why || d > restartRecordSlack || d >= gap {
			continue
		}

		best, gap = i, d
	}

	if best < 0 {
		return ""
	}

	rr.used[best] = true

	r := rr.rots[best]
	if r.Open() {
		return "open"
	}

	return r.Closed.Outcome
}

// printRestarts says how often roles were restarted fresh, by role, and what for.
func printRestarts(out io.Writer, rs []usageRestart, manyCrews bool) {
	if len(rs) == 0 {
		return
	}

	type count struct {
		all, context, idle, done int
		// outcomes counts the restarts a record says how they ended; unrecorded those with none.
		outcomes   map[string]int
		unrecorded int
	}

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

		if r.Outcome != "" {
			if c.outcomes == nil {
				c.outcomes = map[string]int{}
			}

			c.outcomes[r.Outcome]++
		} else {
			c.unrecorded++
		}

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

		// How they ended, where the rotation records say: said only when some do, and then also how many they do not.
		if len(c.outcomes) > 0 {
			var ended []string

			for _, o := range []string{"rebuilt", "resumed", "lost", "open"} {
				if n := c.outcomes[o]; n > 0 {
					ended = append(ended, fmt.Sprintf("%s %d", o, n))
				}
			}

			if c.unrecorded > 0 {
				ended = append(ended, fmt.Sprintf("no record %d", c.unrecorded))
			}

			what += "; " + strings.Join(ended, ", ")
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
