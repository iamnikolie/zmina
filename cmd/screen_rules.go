package cmd

import (
	"fmt"
	"strings"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/screenrules"
)

// checkScreenRules is doctor's line about the crew's screen rules: one line when the file is there, none when it is not.
func checkScreenRules(c *crew.Crew, add func(name, level, format string, args ...any)) {
	_, o := screenrules.LoadOverrides(screenrules.Default(), c.Layout().ScreenRules())

	switch {
	case !o.Present:
	case o.Err != nil:
		add("screens", "warn", "%s is ignored and the built-in rules stay in force: %v", o.Path, o.Err)
	case o.Count == 0:
		add("screens", "ok", "overrides in effect: 0 rules (%s lists none)", o.Path)
	default:
		add("screens", "ok", "overrides in effect: %d %s from %s (%s)", o.Count, plural(o.Count, "rule", "rules"), o.Path,
			overrideWords(o))
	}
}

// overrideWords is what an override file did, as "2 added, 1 replaced, 1 disabled".
func overrideWords(o screenrules.Overrides) string {
	var parts []string

	for _, p := range []struct {
		n    int
		what string
	}{{len(o.Added), "added"}, {len(o.Replaced), "replaced"}, {len(o.Disabled), "disabled"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.what))
		}
	}

	return strings.Join(parts, ", ")
}
