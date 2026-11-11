package cmd

import (
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/shift"
)

// lockLookback is how far back doctor reads the lock's holds and waits.
const lockLookback = 24 * time.Hour

// checkLockHolds is doctor's second look at the state lock: who holds it now (with how long, and what it is doing),
// and the longest holds and waits of the last day from the events. A hold the log names is one past LockHeldNote.
func checkLockHolds(c *crew.Crew, now time.Time, add func(name, level, format string, args ...any)) {
	if h := shift.LockLook(c.StateDir()).Holder; h != nil {
		level := "ok"
		if now.Sub(h.Since) >= shift.LockHeldNote {
			level = "warn"
		}

		add("lock", level, "holder: %s", h.Describe(now))
	}

	evs, err := eventLog(c).Read(events.Filter{Since: now.Add(-lockLookback)})
	if err != nil {
		add("lock", "warn", "the lock's holds and waits could not be read: %v", err)

		return
	}

	var (
		holds, waits, gaveUp int
		longest              shift.HoldEvent
		slowest              shift.WaitEvent
	)

	for _, e := range evs {
		switch e.Event {
		case "lock-held":
			if h := shift.ParseHoldEvent(e.Detail); h.Held >= shift.LockHeldNote {
				if holds++; h.Held > longest.Held {
					longest = h
				}
			}
		case "lock-wait":
			if w := shift.ParseWaitEvent(e.Detail); w.Waited >= shift.LockWaitNote {
				if waits++; w.Waited > slowest.Waited {
					slowest = w
				}

				if w.GaveUp {
					gaveUp++
				}
			}
		}
	}

	if holds == 0 && waits == 0 {
		add("lock", "ok", "no hold over %s and no wait over %s in the last 24 h", shift.LockHeldNote, shift.LockWaitNote)

		return
	}

	level := "ok"
	if longest.Held >= shift.LockWait || gaveUp > 0 {
		level = "warn"
	}

	if holds > 0 {
		add("lock", level, "%d holds over %s in the last 24 h, longest %s: %s", holds, shift.LockHeldNote,
			longest.Held, longestText(longest))
	}

	if waits > 0 {
		add("lock", level, "%d waits over %s in the last 24 h, longest %s, %d gave up", waits, shift.LockWaitNote,
			slowest.Waited, gaveUp)
	}
}

// longestText says what the longest hold was: its verb, the phase it spent longest in, the slowest tmux call.
func longestText(h shift.HoldEvent) string {
	out := h.Verb
	if out == "" {
		out = "a command"
	}

	if h.Phase != "" {
		out += ", mostly in " + h.Phase + " (" + h.PhaseFor.String() + ")"
	}

	if h.Step != "" {
		out += ", longest step " + h.Step + " (" + h.StepFor.String() + ")"
	}

	if h.Tmux != "" {
		out += ", slowest tmux call " + h.Tmux + " (" + h.TmuxFor.String() + ")"
	}

	return out
}
