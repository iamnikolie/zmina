package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

// contextKey is one of the context thresholds of the crew file that doctor measures against its recommended value.
type contextKey struct {
	// name is the crew-file key; rec its recommended value in tokens.
	name string
	rec  int
	// dispatcher says whether the dispatcher is measured by it: it is never restarted for its context.
	dispatcher bool
	// crewWide is the crew-level value in tokens (0: off or unset) and crewText the key as the crew file wrote it.
	crewWide func(crew.ContextLimits) int
	crewText func(*crew.Crew) string
	// own is the text a role sets for the key itself, effective the value it ends with (0: off or none).
	own       func(*crew.Role) string
	effective func(*crew.Role) int
}

var contextKeys = []contextKey{
	{name: "context_max", rec: crew.RecommendedContextMax, dispatcher: true,
		crewWide:  func(l crew.ContextLimits) int { return l.ContextMax },
		crewText:  func(c *crew.Crew) string { return c.ContextMax },
		own:       func(r *crew.Role) string { return r.ContextMax },
		effective: func(r *crew.Role) int { return r.ContextLimit() }},
	{name: "restart_at", rec: crew.RecommendedRestartAt,
		crewWide:  func(l crew.ContextLimits) int { return l.RestartAt },
		crewText:  func(c *crew.Crew) string { return c.RestartAt },
		own:       func(r *crew.Role) string { return r.RestartAt },
		effective: func(r *crew.Role) int { return r.RestartContext() }},
	{name: "restart_done", rec: crew.RecommendedRestartDone,
		crewWide:  func(l crew.ContextLimits) int { return l.RestartDone },
		crewText:  func(c *crew.Crew) string { return c.RestartDone },
		own:       func(r *crew.Role) string { return r.RestartDone },
		effective: func(r *crew.Role) int { return r.RestartDoneAt() }},
}

// above reports whether a value is past the recommendation: off (0) is past every limit.
func above(v, rec int) bool { return v == 0 || v > rec }

// said is a threshold as a line writes it: 250k, or off.
func said(v int) string {
	if v == 0 {
		return "off"
	}

	return shift.Tokens(v)
}

// problem says what is past the recommendation in the crew-level value of the key.
func (k contextKey) problem(c *crew.Crew, v int) string {
	rec := shift.Tokens(k.rec)

	switch text := strings.TrimSpace(k.crewText(c)); {
	case text == "" && v == 0:
		return fmt.Sprintf("%s is not set, so there is no limit; recommended %s", k.name, rec)
	case text == "":
		return fmt.Sprintf("%s is %s (the default), above the recommended %s", k.name, said(v), rec)
	case v == 0:
		return fmt.Sprintf("%s is off; recommended %s", k.name, rec)
	default:
		return fmt.Sprintf("%s is %s, above the recommended %s", k.name, said(v), rec)
	}
}

// checkContext measures the crew file against the recommended context thresholds (crew.RecommendedContextMax and
// the others) for its Claude roles. All three keys work on Codex roles too, but the recommendation was measured on
// Claude, so those are left out. It is advice: a `context` line for each key whose crew-level value, which roles that
// set none of their own get, is past it; one line naming the roles whose own value is past it; and when nothing is,
// one ok line with the effective values. There is no way to silence it: a role's own value shows in the one line.
func checkContext(c *crew.Crew, add func(name, level, format string, args ...any)) {
	crewWide := c.CrewContext()
	exceptions := map[string][]string{}
	warned := false

	var seen []string

	for _, k := range contextKeys {
		var values []int

		inherit := false

		for _, name := range c.Names() {
			r := c.Roles[name]
			if r.IsCodex() || name == c.Dispatcher && !k.dispatcher {
				continue
			}

			v := k.effective(r)
			values = append(values, v)

			switch {
			case k.own(r) == "":
				inherit = true
			case above(v, k.rec):
				exceptions[name] = append(exceptions[name], k.name+" "+said(v))
			}
		}

		if v := k.crewWide(crewWide); inherit && above(v, k.rec) {
			add("context", "warn", "%s: set %s: %s at the crew file's top level", k.problem(c, v), k.name,
				shift.Tokens(k.rec))

			warned = true
		}

		if len(values) > 0 {
			lo, hi := slices.Min(values), slices.Max(values)
			if lo == hi {
				seen = append(seen, k.name+" "+said(lo))
			} else {
				seen = append(seen, fmt.Sprintf("%s %s to %s", k.name, said(lo), said(hi)))
			}
		}
	}

	if len(exceptions) > 0 {
		var who, fix []string

		for _, name := range c.Names() {
			if e, ok := exceptions[name]; ok {
				who = append(who, fmt.Sprintf("%s (%s)", name, strings.Join(e, ", ")))
			}
		}

		for _, k := range contextKeys {
			fix = append(fix, k.name+"="+shift.Tokens(k.rec))
		}

		add("context", "warn", "own values above the recommendation: %s; if deliberate, leave them, else "+
			"zmina seat set <role> %s (the keys named)", strings.Join(who, ", "), strings.Join(fix, " "))

		warned = true
	}

	switch {
	case warned:
	case len(seen) == 0:
		add("context", "ok", "no Claude role to measure against the recommended context values")
	default:
		add("context", "ok", "Claude roles at or below the recommended values: %s", strings.Join(seen, ", "))
	}
}
