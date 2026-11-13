package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/limits"
	"github.com/iamnikolie/zmina/internal/shift"
)

// limitsSnapshot is how full the plans are as the beat judges the crew's thresholds by: read here, outside the state lock,
// because a Codex rollout is found by walking a directory. A plan no role of the crew bills is not read (a crew with no Codex
// seat never walks ~/.codex). A crew whose thresholds are all off reads nothing, and the report is empty: the beat still ends a
// hold that is on the book (the share was turned off, or its reset time passed). Nil: the beat judges none, because the crew
// file does not load; the holds already on the book still hold until their reset time.
func (a *app) limitsSnapshot(now time.Time) *limits.Report {
	c, err := crew.Load(a.crewPath())
	if err != nil {
		return nil
	}

	if !c.LimitsThresholds().Any() {
		return &limits.Report{Policy: c.LimitsPolicy()}
	}

	var homes []limits.Home

	for _, name := range c.Names() {
		if c.Roles[name].IsCodex() {
			homes = c.CodexHomes()

			break
		}
	}

	r, err := limits.Collect(c.Layout().Limits(), homes, c.LimitsPolicy(), now)
	if err != nil {
		fmt.Fprintln(a.err, "zmina: warning:", err)
	}

	return &r
}

// planHolds are the plan holds on at now, by the test the delivery makes (the holds file and the clock), with the roles each
// keeps and the lines that wait for them. A state that is not there counts no lines.
func planHolds(c *crew.Crew, st *shift.State, now time.Time) []limits.HoldInfo {
	if st == nil {
		st = shift.NewState()
	}

	s := &shift.Shift{Crew: c, State: st}

	return s.PlanHolds(now)
}

// holdWords is a hold in one line, as zmina limits and zmina ls say it: `claude 5h window at 97%, until 14:00 (6 roles, 14
// lines waiting)`.
func holdWords(h limits.HoldInfo, now time.Time) string {
	roles := fmt.Sprintf("%d %s", len(h.Roles), plural(len(h.Roles), "role", "roles"))

	return fmt.Sprintf("%s (%s, %d %s waiting)", shift.PlanHoldWords(h.Hold, now, h.Reset.Location()), roles, h.Lines,
		plural(h.Lines, "line", "lines"))
}

// holdFooter is the line zmina ls ends with for each plan hold that is on: nothing is delivered to the plan's roles until the
// window resets, and the owner's own line goes through.
func holdFooter(s *shift.Shift) []string {
	now := s.Now()

	var out []string

	for _, h := range s.PlanHolds(now) {
		out = append(out, "hold: "+holdWords(h, now)+"; the owner's own line passes (zmina limits)")
	}

	return out
}

// checkPlanHolds says which plans are held, in zmina doctor: the window, until when, what waits, and for a plan the dispatcher
// bills too, that it cannot read the note that tells of it, so the owner is told here, by the event and by the todo item that
// blocks it.
func checkPlanHolds(c *crew.Crew, st *shift.State, now time.Time, add func(name, level, format string, args ...any)) {
	for _, h := range planHolds(c, st, now) {
		words := fmt.Sprintf("%s: nothing is delivered to %s until %s; %d %s wait, the owner's own line passes (zmina limits)",
			h.Label(), strings.Join(h.Roles, ", "), h.Reset.Format("Mon 15:04 MST"), h.Lines, plural(h.Lines, "line", "lines"))

		if d := c.Roles[c.Dispatcher]; d != nil && shift.PlanOf(d) == h.Plan {
			words += "; the dispatcher is held too and cannot tell you, so this is the owner's to look at"
			if h.Ask != "" {
				words += " (todo " + h.Ask + ")"
			}
		}

		add("holds", "warn", "%s", words)
	}
}

// supersededFailureHolds takes from the failed-turn holds those a plan hold covers for as long: the role's queue waits for
// whichever ends later, and zmina doctor says that one.
func supersededFailureHolds(c *crew.Crew, st *shift.State, holds []shift.FailureHold, now time.Time) []shift.FailureHold {
	if len(holds) == 0 {
		return holds
	}

	if st == nil {
		st = shift.NewState()
	}

	s := &shift.Shift{Crew: c, State: st}

	var out []shift.FailureHold

	for _, h := range holds {
		if p, held := s.PlanHold(h.Role, now); held && !h.Until.After(p.Reset) {
			continue
		}

		out = append(out, h)
	}

	return out
}
