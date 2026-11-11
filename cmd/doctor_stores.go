package cmd

import (
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/message"
)

// checkStores is doctor's look at the stores that keep files: the report records (a file that does not read, a
// message whose work fails: reconcile skips it and said so once, doctor keeps saying it) and the daily retention
// pass.
func checkStores(c *crew.Crew, now time.Time, add func(name, level, format string, args ...any)) {
	checkMessageFiles(c, add)
	checkRetentionPass(c, now, add)
}

func checkMessageFiles(c *crew.Crew, add func(name, level, format string, args ...any)) {
	store := message.Store{Dir: filepath.Join(c.StateDir(), "messages")}

	all, bad, err := store.Scan()
	if err != nil {
		add("messages", "warn", "%v", err)

		return
	}

	named := map[string]bool{}

	for _, p := range bad {
		named[p.File] = true

		add("messages", "warn", "%s: %v: report reconcile skips it; mend the file or move it away (zmina message ls)", p.File, p.Err)
	}

	said := store.Reported()

	for _, file := range slices.Sorted(maps.Keys(said)) {
		if !named[file] {
			add("messages", "warn", "%s: %s: report reconcile skips this message (zmina log has it)", file, said[file])
		}
	}

	if len(bad) == 0 && len(said) == 0 && len(all) > 0 {
		add("messages", "ok", "%d readable", len(all))
	}
}

func checkRetentionPass(c *crew.Crew, now time.Time, add func(name, level, format string, args ...any)) {
	if !c.RetentionOn() {
		add("retention", "ok", "off (retention: off in the crew file): finished work stays where it is; zmina retention --dry-run lists what a pass would move")

		return
	}

	st := readRetentionStamp(c.StateDir())
	keep := days(c.RetentionSpan())

	if st.At.IsZero() {
		add("retention", "ok", "keeps %s · no pass yet: the ticker runs one a day (zmina retention --dry-run)", keep)

		return
	}

	detail := "keeps " + keep + " · last pass " + st.At.In(c.Location()).Format("2006-01-02 15:04")
	if len(st.Moved) > 0 {
		detail += " · moved " + movedText(st.Moved)
	}

	if now.Sub(st.At) > 3*retentionEvery {
		add("retention", "warn", "%s · no pass for %s: is the ticker running? (zmina ticker status)", detail, days(now.Sub(st.At)))

		return
	}

	add("retention", "ok", "%s", detail)
}
