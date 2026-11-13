package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
)

// checkPlugins is doctor's line for each plugin of the crew: where its program is (ok), that it is not found or is refused
// (a program anyone could change is not run), or how its last run failed and when, with the events still waiting for a retry.
func checkPlugins(c *crew.Crew, add func(name, level, format string, args ...any)) {
	rows, warns := pluginRows(c)

	for _, w := range warns {
		add("plugins", "warn", "%s", w)
	}

	for _, r := range rows {
		when := func(t time.Time) string { return t.In(c.Location()).Format("Mon 2 Jan 15:04") }

		switch {
		case !r.Found:
			add("plugins", "warn", "%s: %s — nothing is told to it", r.Name, r.Problem)
		case r.Last != nil && !r.Last.OK:
			add("plugins", "warn", "%s: last run failed %s (%s %s: %s)%s", r.Name, when(r.Last.At), r.Last.Event, r.Last.ID,
				clip(oneLine(r.Last.Error), 120), waitingWords(r))
		case r.Last != nil:
			add("plugins", "ok", "%s: %s · last run %s %s %s%s", r.Name, r.Program, r.Last.Event, r.Last.ID, when(r.Last.At), waitingWords(r))
		default:
			add("plugins", "ok", "%s: %s · told of %s · no run yet", r.Name, r.Program, strings.Join(r.Events, ", "))
		}
	}

}

func waitingWords(r pluginRow) string {
	out := ""

	if r.Waiting > 0 {
		out = fmt.Sprintf(" · %d waiting for a retry", r.Waiting)
	}

	if r.Partial != "" {
		out += " · last look partial: " + r.Partial + " (not judged until it reads)"
	}

	return out
}
