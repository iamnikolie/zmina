package cmd

import (
	"fmt"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
)

// failureHolds are the roles whose queues are held for a failed turn at now, by the test the delivery and zmina ls use: only a
// record of the role's live session that still stands counts (shift.Shift.FailureHolds), so a hold the owner's message went
// through, or a turn since ended, or a restart took away, is not reported.
func failureHolds(c *crew.Crew, st *shift.State, now time.Time) []shift.FailureHold {
	if st == nil {
		st = shift.NewState()
	}

	s := &shift.Shift{Crew: c, Reg: &registry.Registry{Namespace: c.Namespace}, State: st}

	return s.FailureHolds(now)
}

// checkFailureHolds says which roles' queues wait for a failed turn that is not a transient one: a usage limit or an account
// problem. The roles' own turns end on it and nobody reads their queues; for the dispatcher, which hears of everything else,
// that is the whole crew's signals and notes, and the note that would tell the owner is one of them. So the owner is told
// here and in zmina ls, and the log has an event, and not by a note to a dispatcher that cannot read it. The times are in the
// crew's location.
func checkFailureHolds(c *crew.Crew, st *shift.State, holds []shift.FailureHold, add func(name, level, format string, args ...any)) {
	for _, h := range holds {
		if h.Class.Kind == shift.KindTransient {
			continue
		}

		until := h.Until.In(c.Location()).Format("15:04 MST")

		if h.Role != c.Dispatcher {
			add("holds", "warn", "%s: its last turn ended in an API error, %s (%s): its queued lines wait until %s, or its next turn "+
				"or a restart", h.Role, h.Class.Words(), clip(h.Text, 80), until)

			continue
		}

		waiting := ""
		if st != nil {
			waiting = fmt.Sprintf(" (%d signals of at most %d, %d notes, %d lines of its own waiting; a new signal past the most is "+
				"refused)", len(st.Signals), shift.SignalsMax, len(st.Notes), len(st.Role(h.Role).Inbox))
		}

		add("holds", "warn", "%s: its last turn ended in an API error, %s (%s): its queue, the crew's signals and the health notes "+
			"wait%s until %s, or its next turn or a restart; it cannot tell you, so this is the owner's to look at: zmina peek %s",
			h.Role, h.Class.Words(), clip(h.Text, 80), waiting, until, h.Role)
	}
}
