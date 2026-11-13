package shift

import (
	"fmt"
	"slices"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/limits"
)

// A plan hold is a window of a plan at its hold share (the crew file's limits: hold, default 97 on the short window): until that
// window resets nothing is delivered to the plan's roles. The hold is kept in limits/holds.json (limits.Book), apart from the state: the
// beat starts and ends it (limitsStep), and every delivery reads it, as it reads a failed turn's record. It ends by the clock at the
// window's reset time whether or not a beat has run.
//
// What it holds is what zmina would send a model: every line and doorbell to the plan's roles (their queues stay as they are, nothing
// is counted against a line), the dispatcher's signals and notes when its own plan is held, scheduled runs, an alarm's wake of a role,
// a trim, a restart or release that would ask the role for something, a role the ticker starts, the ticker's later sweep and the
// day's lessons run. A busy role is not interrupted: it finishes its turn and gets nothing after. The owner's own line (the cockpit's
// chat) goes through, to a Codex seat as to a Claude one, as it goes through a failed turn's hold.
//
// When the last hold of a plan ends its roles are let go in turn, not all in one beat: the book keeps for each a time it waits until
// (limits.Wait), and the gate holds a role until then exactly as it holds it for a hold.

// PlanOf is the plan a role bills.
func PlanOf(r *crew.Role) limits.Plan {
	if r.IsCodex() {
		return limits.PlanCodex
	}

	return limits.PlanClaude
}

// seatHome is the Codex home a seat runs with, empty for a Claude seat: seats of another home bill another account, and are not held
// by a hold a reading of this one made.
func (s *Shift) seatHome(r *crew.Role) string { return s.Crew.CodexHomeOf(r) }

// holdBook is the holds file as read once by this shift: the delivery asks for it at every role of every pass, and a small file read
// once is what the failed-turn records cost per role. A shift is short-lived (a command, a beat, a courier's pass works on a copy), and
// the beat renews it when it writes the file (limitsStep). A file that does not read holds nothing, and a reader says nothing of it:
// the beat is the one that moves it aside (or leaves a newer zmina's alone) and says so once; zmina doctor says it too.
func (s *Shift) holdBook() limits.Book {
	if s.limitBook == nil {
		book, _ := limits.ReadBook(s.Crew.Layout().Limits())
		s.limitBook = &book
	}

	return *s.limitBook
}

// planHold is the hold that keeps a role's plan at now: of several windows held at once, the one that resets last. A role that is not
// in the crew has none; a Codex seat is held only by a hold its own home's reading made.
func (s *Shift) planHold(name string, now time.Time) (limits.Hold, bool) {
	r, err := s.Role(name)
	if err != nil {
		return limits.Hold{}, false
	}

	return s.holdOf(PlanOf(r), s.seatHome(r), now)
}

func (s *Shift) holdOf(plan limits.Plan, home string, now time.Time) (limits.Hold, bool) {
	book := s.holdBook()
	if len(book.Holds) == 0 {
		return limits.Hold{}, false
	}

	return book.HeldUntil(plan, home, now)
}

// planWait is the turn a role still waits for after a hold of its plan ended: the release is paced, and until its time the role is
// held as it was.
func (s *Shift) planWait(name string, now time.Time) (limits.Wait, bool) {
	book := s.holdBook()
	if len(book.Waits) == 0 {
		return limits.Wait{}, false
	}

	return book.WaitFor(name, now)
}

// planHeld reports whether nothing is delivered to a role at now because its plan is held, or because its turn after a hold has not come.
func (s *Shift) planHeld(name string, now time.Time) bool {
	if _, held := s.planHold(name, now); held {
		return true
	}

	_, waits := s.planWait(name, now)

	return waits
}

// planHeldWhy is planHeld with what a reader says of it: the hold in words (`claude 5h window`) and until when, or the release waiting
// for its turn.
func (s *Shift) planHeldWhy(name string, now time.Time) (what string, until time.Time, held bool) {
	if h, ok := s.planHold(name, now); ok {
		return h.Label(), h.Reset, true
	}

	if w, ok := s.planWait(name, now); ok {
		return "release of the " + string(w.Plan) + " plan", w.Until, true
	}

	return "", time.Time{}, false
}

// PlanHeldWhy is planHeldWhy for the commands that say it: the hold in words and until when, or the release waiting for its turn.
func (s *Shift) PlanHeldWhy(name string, now time.Time) (what string, until time.Time, held bool) {
	return s.planHeldWhy(name, now)
}

// PlanHold is the hold that keeps a role's plan at now, for the commands that show it (zmina doctor).
func (s *Shift) PlanHold(name string, now time.Time) (limits.Hold, bool) {
	return s.planHold(name, now)
}

// PlanHeld is planHeld for a caller outside the shift: the ticker's other duties (the later sweep, the lessons run) ask it
// before they make a model call.
func (s *Shift) PlanHeld(name string, now time.Time) bool { return s.planHeld(name, now) }

// PlanHeldFor reports whether a plan has a hold at now (of any Codex home).
func (s *Shift) PlanHeldFor(plan limits.Plan, now time.Time) bool {
	_, held := s.holdOf(plan, "", now)

	return held
}

// deliveryHeld reports whether a line queued for a role waits at now: its plan is held, or its turn after a hold has not come, or its last
// turn ended on an API error that still holds it (failureHeld). Whichever ends later wins, since any one alone keeps the line. The owner's
// own line passes all of them (ownerLine, ownerWaits, deliverInboxOnce).
func (s *Shift) deliveryHeld(name string, now time.Time) bool {
	return s.planHeld(name, now) || s.failureHeld(name, now)
}

// rolesOf are the roles of the crew that bill a plan, the dispatcher first and each role's instances after it; for Codex, only the seats
// of the home when home is not empty.
func (s *Shift) rolesOf(plan limits.Plan, home string) []string {
	var out []string

	for _, name := range s.Names() {
		if r, err := s.Role(name); err == nil && PlanOf(r) == plan && (home == "" || s.seatHome(r) == home) {
			out = append(out, name)
		}
	}

	return out
}

// PlanRoles are the roles of the crew that bill a plan, the dispatcher first and each role's instances after it.
func (s *Shift) PlanRoles(plan limits.Plan) []string { return s.rolesOf(plan, "") }

// planLines is how many lines wait for roles: each queue, and for the dispatcher its signals and notes.
func (s *Shift) planLines(roles []string) int {
	n := 0

	for _, name := range roles {
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
		roles := s.rolesOf(h.Plan, h.Home)
		h.Reset, h.Since = h.Reset.In(loc), h.Since.In(loc)
		out = append(out, limits.HoldInfo{Hold: h, Roles: roles, Lines: s.planLines(roles)})
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

// PlanWaits are the roles still waiting their turn after a hold ended, at now, by when they go.
func (s *Shift) PlanWaits(now time.Time) []limits.Wait {
	var out []limits.Wait

	for _, w := range s.holdBook().Waits {
		if now.Before(w.Until) {
			w.Until = w.Until.In(s.Crew.Location())
			out = append(out, w)
		}
	}

	slices.SortStableFunc(out, func(a, b limits.Wait) int { return a.Until.Compare(b.Until) })

	return out
}

// releaseOrder is the order a release lets the roles of a plan go in: the dispatcher alone first, then the role whose oldest waiting line
// has waited longest; a role whose lines carry no time of their own after those, and the roles with nothing waiting last, by name.
func (s *Shift) releaseOrder(plan limits.Plan, home string) limits.Roles {
	type aged struct {
		name string
		rank int // 0: the age of its oldest line is known, 1: it has lines of unknown age, 2: it has none
		at   time.Time
	}

	var (
		out    limits.Roles
		others []aged
	)

	for _, name := range s.rolesOf(plan, home) {
		if name == s.Crew.Dispatcher {
			out.Dispatcher = name

			continue
		}

		a := aged{name: name, rank: 2}

		if rs := s.State.Roles[name]; rs != nil && len(rs.Inbox) > 0 {
			a.rank = 1
			cards := ReadCards(s.Crew.Layout(), name)

			for _, line := range rs.Inbox {
				for _, c := range CardsOf(cards, name, line) {
					if !c.Written.IsZero() && (a.rank == 1 || c.Written.Before(a.at)) {
						a.rank, a.at = 0, c.Written
					}
				}
			}
		}

		others = append(others, a)
	}

	slices.SortStableFunc(others, func(x, y aged) int {
		if x.rank != y.rank {
			return x.rank - y.rank
		}

		return x.at.Compare(y.at)
	})

	for _, a := range others {
		out.Others = append(out.Others, a.name)
	}

	return out
}

// limitsAskedAt is when the thresholds last asked the roles of a role's plan for a checkpoint: the checkpoint mark of the plan's newest period
// on the book (zero when none). A hold stamps it too, with nothing asked then; the window after it is held against a note's being
// taken for the answer all the same, which costs a restart one more ask and no more.
func (s *Shift) limitsAskedAt(name string) time.Time {
	r, err := s.Role(name)
	if err != nil {
		return time.Time{}
	}

	var at time.Time

	for _, p := range s.holdBook().Periods {
		if p.Plan == PlanOf(r) && (p.Home == "" || p.Home == s.seatHome(r)) && p.Checkpoint.After(at) {
			at = p.Checkpoint
		}
	}

	return at
}

// postponedOnce reports that a scheduled run is put off and has not been said so yet, and notes it in the book: the event of a postponed run
// is one for the run, not one a beat. A book that cannot be written says it again at the next beat, which is no worse than none.
func (s *Shift) postponedOnce(role string, run time.Time) bool {
	book := s.holdBook()
	if book.WasPostponed(role, run) {
		return false
	}

	book.Postpone(role, run)
	s.limitBook = &book

	_ = limits.WriteBook(s.Crew.Layout().Limits(), book)

	return true
}

// SendQueued is what Send returns for a line it queued instead of typing, because the role's plan is held: the hold, and until when. It is a
// report, not a failure: the command that sent the line says it on its output and succeeds.
type SendQueued struct {
	What  string
	Until time.Time
}

func (q *SendQueued) Error() string { return q.Say(time.Local) }

// Say is what the sender is told: `queued: claude 5h window held until 14:00`.
func (q *SendQueued) Say(loc *time.Location) string {
	return fmt.Sprintf("queued: %s held until %s", q.What, q.Until.In(loc).Format("15:04"))
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
