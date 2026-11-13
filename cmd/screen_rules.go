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
		add("screens", "ok", "overrides in effect: %d %s from %s: %s", o.Count, plural(o.Count, "rule", "rules"), o.Path,
			overrideWords(o))
	}
}

// doctorIDsMax is how many rows doctor names before it counts the rest.
const doctorIDsMax = 12

// overrideWords is what an override file did, row by row: "added a, b; replaced c; disabled d". The ids are plain (the loader
// refuses any other), at most doctorIDsMax of them are named, and the rest are counted.
func overrideWords(o screenrules.Overrides) string {
	left := doctorIDsMax

	var parts []string

	for _, g := range []struct {
		what string
		ids  []string
	}{{"added", o.Added}, {"replaced", o.Replaced}, {"disabled", o.Disabled}} {
		if len(g.ids) == 0 || left == 0 {
			continue
		}

		shown := g.ids[:min(len(g.ids), left)]
		left -= len(shown)
		parts = append(parts, g.what+" "+strings.Join(shown, ", "))
	}

	words := strings.Join(parts, "; ")

	if more := len(o.Added) + len(o.Replaced) + len(o.Disabled) - (doctorIDsMax - left); more > 0 {
		words += fmt.Sprintf("; and %d more", more)
	}

	return words
}
