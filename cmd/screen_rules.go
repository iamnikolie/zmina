package cmd

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/screenrules"
)

// screenRulesTold is the refusals already told in this process, by file and reason.
var screenRulesTold sync.Map

// screenRulesEvery is how often one reason is put in the event log: the ticker is a new process every beat, and a file that stays
// wrong would be an event a minute.
const screenRulesEvery = 24 * time.Hour

// screenRulesEvent is the event a refused screen-rules.yaml leaves in the log.
const screenRulesEvent = "screen-rules-refused"

// tellRefusedScreenRules says that the crew's screen-rules.yaml was refused and the embedded rules stay in force, outside doctor:
// once in this process on the command's error stream, as the crew file's warnings are, and as an event in the log, once a day
// for each distinct reason. The log itself is the record of what was told: no state is kept for it. Nothing here fails a command.
func (a *app) tellRefusedScreenRules(c *crew.Crew, o screenrules.Overrides) {
	if o.Err == nil {
		return
	}

	reason := clip(oneLineText(o.Err.Error()), 300)

	if _, told := screenRulesTold.LoadOrStore(o.Path+"\x00"+reason, true); told {
		return
	}

	fmt.Fprintf(a.err, "zmina: warning: %s is ignored and the embedded screen rules stay in force: %s (zmina doctor)\n", o.Path, reason)

	log, now := eventLog(c), time.Now()

	recent, err := log.Read(events.Filter{Since: now.Add(-screenRulesEvery)})
	if err != nil {
		return
	}

	for _, e := range recent {
		if e.Event == screenRulesEvent && e.Detail == reason {
			return
		}
	}

	_ = log.Add(events.Event{TS: now, Event: screenRulesEvent, By: by(), Detail: reason})
}

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
