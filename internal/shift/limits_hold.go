package shift

import (
	"fmt"
	"slices"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/limits"
)

// A plan hold is a window of a plan at its hold share (the crew file's limits: hold, default 97): until that window resets
// nothing is delivered to the plan's roles. The hold is kept in limits/holds.json (limits.Book), apart from the state: the
// beat starts and ends it (limitsStep), and every delivery reads it, as it reads a failed turn's record. It ends by the clock
// at the window's reset time whether or not a beat has run.
//
// What it holds is what zmina would send a model: every line and doorbell to the plan's roles (their queues stay as they
// are, nothing is counted against a line), the dispatcher's signals and notes when its own plan is held, scheduled runs, an
// alarm's wake of a role, a trim, a restart or release that would ask the role for something, a role the ticker starts, the
// ticker's later sweep and the day's lessons run. A busy role is not interrupted: it finishes its turn and gets nothing after.
// The owner's own line (the cockpit's chat) goes through, as it goes through a failed turn's hold.

// PlanOf is the plan a role bills.
func PlanOf(r *crew.Role) limits.Plan {
	if r.IsCodex() {
		return limits.PlanCodex
	}

	return limits.PlanClaude
}

// holdBook is the holds file as read once by this shift: the delivery asks for it at every role of every pass, and a small
// file read once is what the failed-turn records cost per role. A shift is short-lived (a command, a beat, a courier's pass
// works on a copy), and the beat renews it when it writes the file (limitsStep).
func (s *Shift) holdBook() limits.Book {
	if s.limitBook == nil {
		book, err := limits.ReadBook(s.Crew.Layout().Limits())
		if err != nil && s.Warn != nil {
			s.Warn(fmt.Errorf("limits: %w (nothing is held by it)", err))
		}

		s.limitBook = &book
	}

	return *s.limitBook
}

// planHold is the hold that keeps a role's plan at now: of several windows held at once, the one that resets last. A role
// that is not in the crew has none.
func (s *Shift) planHold(name string, now time.Time) (limits.Hold, bool) {
	r, err := s.Role(name)
	if err != nil {
		return limits.Hold{}, false
	}

	return s.planHoldOf(PlanOf(r), now)
}

func (s *Shift) planHoldOf(plan limits.Plan, now time.Time) (limits.Hold, bool) {
	book := s.holdBook()
	if len(book.Holds) == 0 {
		return limits.Hold{}, false
	}

	return book.HeldUntil(plan, now)
}

// planHeld reports whether nothing is delivered to a role at now because its plan is held.
func (s *Shift) planHeld(name string, now time.Time) bool {
	_, held := s.planHold(name, now)

	return held
}

// PlanHold is the hold that keeps a role's plan at now, for the commands that show it (zmina doctor).
func (s *Shift) PlanHold(name string, now time.Time) (limits.Hold, bool) {
	return s.planHold(name, now)
}

// PlanHeld is planHeld for a caller outside the shift: the ticker's other duties (the later sweep, the lessons run) ask it
// before they make a model call.
func (s *Shift) PlanHeld(name string, now time.Time) bool { return s.planHeld(name, now) }

// PlanHeldFor reports whether a plan has a hold at now.
func (s *Shift) PlanHeldFor(plan limits.Plan, now time.Time) bool {
	_, held := s.planHoldOf(plan, now)

	return held
}

// deliveryHeld reports whether a line queued for a role waits at now: its plan is held, or its last turn ended on an API
// error that still holds it (failureHeld). Whichever ends later wins, since either one alone keeps the line. The owner's own
// line passes both (ownerLine, ownerWaits, deliverInboxOnce).
func (s *Shift) deliveryHeld(name string, now time.Time) bool {
	return s.planHeld(name, now) || s.failureHeld(name, now)
}

// PlanRoles are the roles of the crew that bill a plan, the dispatcher first and each role's instances after it.
func (s *Shift) PlanRoles(plan limits.Plan) []string {
	var out []string

	for _, name := range s.Names() {
		if r, err := s.Role(name); err == nil && PlanOf(r) == plan {
			out = append(out, name)
		}
	}

	return out
}

// planLines is how many lines wait for a plan's roles: each queue, and for the dispatcher its signals and notes.
func (s *Shift) planLines(plan limits.Plan) int {
	n := 0

	for _, name := range s.PlanRoles(plan) {
		if rs := s.State.Roles[name]; rs != nil {
			n += len(rs.Inbox)
		}

		if name == s.Crew.Dispatcher {
			n += len(s.State.Signals) + len(s.State.Notes)
		}
	}

	return n
}

// PlanHolds are the plan holds that are on at now, each with the roles it keeps and the lines that wait, its times in the
// crew's location; the plans in the order claude, codex, and a plan's windows by their reset. It reads the holds file and the
// state loaded, and asks tmux nothing.
func (s *Shift) PlanHolds(now time.Time) []limits.HoldInfo {
	var out []limits.HoldInfo

	loc := s.Crew.Location()

	for _, h := range s.holdBook().Active(now) {
		h.Reset, h.Since = h.Reset.In(loc), h.Since.In(loc)
		out = append(out, limits.HoldInfo{Hold: h, Roles: s.PlanRoles(h.Plan), Lines: s.planLines(h.Plan)})
	}

	slices.SortStableFunc(out, func(a, b limits.HoldInfo) int {
		if a.Plan != b.Plan {
			if a.Plan == limits.PlanClaude {
				return -1
			}

			return 1
		}

		return a.Reset.Compare(b.Reset)
	})

	return out
}

// PlanHoldWords is a hold in a few words, as zmina ls, zmina doctor and zmina explain say it: `claude 5h window at 97%, until
// 14:00`. The share is the one the window had when the hold began.
func PlanHoldWords(h limits.Hold, now time.Time, loc *time.Location) string {
	return fmt.Sprintf("%s at %s, until %s", h.Label(), percent(h.Used), limitClock(h.Reset, now, loc))
}

// percent is a share as the notes write it: 97%.
func percent(used float64) string { return fmt.Sprintf("%.0f%%", used) }

// limitClock is a moment on the crew's clock: the time of day for today, the weekday with it within the week, else the date.
func limitClock(t, now time.Time, loc *time.Location) string {
	t, now = t.In(loc), now.In(loc)

	switch d := t.Sub(now); {
	case t.Format("2006-01-02") == now.Format("2006-01-02"):
		return t.Format("15:04")
	case d > -6*24*time.Hour && d < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	}

	return t.Format("2 Jan 15:04")
}

// limitIn is how far off a moment is, in its two biggest units: 1h12m, 45m, 2d3h. Under a minute it is 0m.
func limitIn(d time.Duration) string {
	d = d.Round(time.Minute)

	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d < 0:
		return "0m"
	}

	return fmt.Sprintf("%dm", int(d.Minutes()))
}
