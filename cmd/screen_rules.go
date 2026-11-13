package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/screenrules"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// screenRulesTold is the refusals already told in this process, by file and reason.
var screenRulesTold sync.Map

// screenRulesEvery is how often one reason is put in the event log: the ticker is a new process every beat, and a file that stays
// wrong would be an event a minute.
const screenRulesEvery = 24 * time.Hour

// screenRulesEvent is the event a refused screen-rules.yaml leaves in the log.
const screenRulesEvent = "screen-rules-refused"

// isHookRun reports whether a command is one a role's hooks or its bridge run (the plumbing commands, named with a leading
// underscore, and the SessionStart hook's `pending --brief`): what they print on the error stream reaches no one who can act on
// it, and a hook that wrote there would be read as a failure of the hook.
func isHookRun(c *cobra.Command) bool {
	if strings.HasPrefix(c.Name(), "_") {
		return true
	}

	brief, err := c.Flags().GetBool("brief")

	return c.Name() == "pending" && err == nil && brief
}

// printableLine is a text as one line of printable characters: control, format and bidirectional characters gone and white
// space folded. A reason that came out of a file or a path is printed to a terminal, where such a character is a command.
func printableLine(s string) string { return strings.Join(strings.Fields(tmux.Printable(s)), " ") }

// refusalReason is why a screen-rules.yaml was refused, as one printable line, cut.
func refusalReason(err error) string { return clip(printableLine(err.Error()), 300) }

// tellRefusedScreenRules says that the crew's screen-rules.yaml was refused and the embedded rules stay in force, outside doctor:
// once in this process on the command's error stream, as the crew file's warnings are (not for a command a hook runs), and as an
// event in the log, once a day for each distinct reason, by a small marker beside the crew's other files (ScreenRulesTold): the
// log is never read for it. Nothing here fails a command.
func (a *app) tellRefusedScreenRules(c *crew.Crew, o screenrules.Overrides) {
	if o.Err == nil {
		return
	}

	reason, path := refusalReason(o.Err), printableLine(o.Path)

	if _, told := screenRulesTold.LoadOrStore(path+"\x00"+reason, true); told {
		return
	}

	if !a.hook {
		fmt.Fprintf(a.err, "zmina: warning: %s is ignored and the embedded screen rules stay in force: %s (zmina doctor)\n", path, reason)
	}

	now := time.Now()
	if !screenRulesDue(c.Layout().ScreenRulesTold(), screenRulesKey(path, reason), now) {
		return
	}

	_ = eventLog(c).Add(events.Event{TS: now, Event: screenRulesEvent, By: by(), Detail: reason})
}

// screenRulesKey names a refusal in the marker: a hash of the file and the reason, so that the marker keeps no words of it.
func screenRulesKey(path, reason string) string {
	sum := sha256.Sum256([]byte(path + "\x00" + reason))

	return hex.EncodeToString(sum[:8])
}

// screenRulesMarkerMax bounds the marker: the reasons told within a day, the newest of them.
const screenRulesMarkerMax = 16

// screenRulesDue reports whether a refusal was not put in the log within screenRulesEvery, and notes in the marker that it is
// now. A marker that does not read, or a time in it that is yet to come, says it is due; a directory that cannot keep one tells
// every time, which is no worse than a refusal that was never kept.
func screenRulesDue(marker, key string, now time.Time) bool {
	told := map[string]time.Time{}

	if f, err := os.Open(marker); err == nil {
		raw, _ := io.ReadAll(io.LimitReader(f, 1<<16))
		_ = f.Close()

		if json.Unmarshal(raw, &told) != nil {
			told = map[string]time.Time{}
		}
	}

	if at, ok := told[key]; ok && !at.After(now) && now.Sub(at) < screenRulesEvery {
		return false
	}

	told[key] = now

	for k, at := range told {
		if now.Sub(at) >= screenRulesEvery || at.After(now) {
			delete(told, k)
		}
	}

	for len(told) > screenRulesMarkerMax {
		oldest := ""

		for k, at := range told {
			if oldest == "" || at.Before(told[oldest]) {
				oldest = k
			}
		}

		delete(told, oldest)
	}

	told[key] = now

	if raw, err := json.Marshal(told); err == nil {
		_ = fsx.WriteFile(marker, append(raw, '\n'), 0o600)
	}

	return true
}

// checkScreenRules is doctor's line about the crew's screen rules: one line when the file is there, none when it is not.
func checkScreenRules(c *crew.Crew, add func(name, level, format string, args ...any)) {
	_, o := screenrules.LoadOverrides(screenrules.Default(), c.Layout().ScreenRules())

	switch {
	case !o.Present:
	case o.Err != nil:
		add("screens", "warn", "%s is ignored and the built-in rules stay in force: %s", printableLine(o.Path), refusalReason(o.Err))
	case o.Count == 0:
		add("screens", "ok", "overrides in effect: 0 rules (%s lists none)", printableLine(o.Path))
	default:
		add("screens", "ok", "overrides in effect: %d %s from %s: %s", o.Count, plural(o.Count, "rule", "rules"), printableLine(o.Path),
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
