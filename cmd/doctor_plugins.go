package cmd

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/plugin"
)

// pluginStuckMargin is what a delivery may take beyond its events' deadlines (reading the todo and the screens, writing) before
// doctor calls it stuck.
const pluginStuckMargin = 2 * time.Minute

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
		case r.Via == "PATH":
			// Found on the PATH of whoever asks: the ticker's is the one `zmina ticker install` froze, and may not have it.
			add("plugins", "warn", "%s: %s found on this shell's PATH; the ticker's PATH may differ (zmina ticker install froze "+
				"it): keep it in %s or give run an absolute path%s", r.Name, r.Program, filepath.Join(c.Layout().Plugins(), r.Name), waitingWords(r))
		case r.Last != nil:
			add("plugins", "ok", "%s: %s · last run %s %s %s%s", r.Name, r.Program, r.Last.Event, r.Last.ID, when(r.Last.At), waitingWords(r))
		default:
			add("plugins", "ok", "%s: %s · told of %s · no run yet", r.Name, r.Program, strings.Join(r.Events, ", "))
		}

		// A delivery that holds its plugin's lock far longer than a pass can take is stuck: a pass is at most MaxPerBeat events, each
		// within the plugin's deadline.
		if pl, ok := c.PluginNamed(r.Name); ok {
			held, pid, since := plugin.Holder(c.Layout().PluginLock(r.Name))
			limit := time.Duration(plugin.MaxPerBeat)*pl.TimeoutD() + pluginStuckMargin

			if held && !since.IsZero() && time.Since(since) > limit {
				add("plugins", "warn", "%s: a delivery (pid %d) has held its lock since %s, longer than a pass can take (%s): "+
					"it looks stuck; ps -p %d", r.Name, pid, when(since), limit.Round(time.Second), pid)
			}
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
