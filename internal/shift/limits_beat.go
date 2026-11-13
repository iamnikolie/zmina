package shift

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/handoff"
	"github.com/iamnikolie/zmina/internal/limits"
	"github.com/iamnikolie/zmina/internal/task"
)

// limitCheckpointOpening starts the line that asks a role for a checkpoint because its plan's window is filling. It is a line of
// zmina's own, for the conversation it is posted to (sessionBoundOpenings).
const limitCheckpointOpening = "[zmina] limit checkpoint: "

// limitsStep is the thresholds of the crew file's limits: key, once a beat. It judges how full the plans were when the beat began
// (Shift.Limits, read by the caller outside the lock) by limits.Evaluate, keeps the book (limits/holds.json) and does what came
// out of it: a warn is a note for the dispatcher, a checkpoint asks the plan's roles that are up and hold a task for a checkpoint
// (the request file a restart's note uses) and tells the dispatcher whom, a hold tells the dispatcher and, when the dispatcher
// is held too, puts an item on the owner's todo that blocks it, and a release says so and counts the lines that go. Each is one
// event too: limit-warn, limit-checkpoint, limit-hold, limit-release.
//
// The book is written first and the state after it (the notes and requests it queues are saved with the beat, or at once by the
// flush at the end): a hold must stand before the first line that could be delivered under it, so a failed write of the book does
// nothing else, and a beat killed in between loses a note, never a hold. Notes are added here, not returned, so that the flush
// saves them. It returns the lines for the beat's log.
func (s *Shift) limitsStep(now time.Time) []string {
	if s.Limits == nil {
		return nil
	}

	s.mark("limits", "")

	plans := s.limitPlans()
	if len(plans) == 0 {
		return nil
	}

	dir := s.Crew.Layout().Limits()

	book, err := limits.ReadBook(dir)
	if err != nil && s.Warn != nil {
		s.Warn(fmt.Errorf("limits: %w (it holds nothing; the next write replaces it)", err))
	}

	res := limits.Evaluate(limits.Input{Report: *s.Limits, Thresholds: s.Crew.LimitsThresholds(), Plans: plans, Book: book, Now: now})
	if !res.Changed {
		s.limitBook = &res.Book

		return nil
	}

	s.openAsks(&res, now)

	if err := limits.WriteBook(dir, res.Book); err != nil {
		if s.Warn != nil {
			s.Warn(fmt.Errorf("limits: the holds file was not written, so nothing was told or held this beat: %w", err))
		}

		s.limitBook = &book

		return nil
	}

	s.limitBook = &res.Book

	var did []string

	for _, a := range res.Actions {
		kind, text := s.act(a, res.Book, now)
		if text == "" {
			continue
		}

		did = append(did, "limits: "+text)

		s.addNotes([]string{text})

		if kind != "" {
			s.log("", kind, "", text)
		}
	}

	s.flush()

	return did
}

// limitPlans are the plans the crew's roles bill: nothing is said of a plan none of them does.
func (s *Shift) limitPlans() []limits.Plan {
	var out []limits.Plan

	for _, name := range s.Names() {
		if r, err := s.Role(name); err == nil && !slices.Contains(out, PlanOf(r)) {
			out = append(out, PlanOf(r))
		}
	}

	return out
}

// openAsks puts an item on the owner's todo for each hold that is about to begin on the dispatcher's own plan: the dispatcher
// cannot read the note that tells of it, so the owner is told by the path the other owner alarms take (a todo item that blocks the
// role, which the crew's notify command pushes). The item's id goes in the hold, so that the release closes it. A todo that cannot
// be written is warned of; the event and zmina doctor say it all the same.
func (s *Shift) openAsks(res *limits.Result, now time.Time) {
	if s.Asks == nil {
		return
	}

	d, err := s.Role(s.Crew.Dispatcher)
	if err != nil {
		return
	}

	for i := range res.Actions {
		a := &res.Actions[i]
		if a.Kind != limits.KindHold || a.Plan != PlanOf(d) {
			continue
		}

		loc := s.Crew.Location()
		until := limitClock(a.Hold.Reset, now, loc)

		ask, err := s.Asks.Add(asks.Ask{Kind: asks.Check, Role: "ticker", By: "ticker", Blocks: s.Crew.Dispatcher,
			Title: fmt.Sprintf("%s at %s: held until %s", a.Label(), percent(a.Used), until),
			Text: fmt.Sprintf("The %s is at %s. zmina delivers nothing to the plan's roles, the dispatcher included, until it resets at %s: "+
				"their lines wait in their queues and your own lines still go through (zmina ls, zmina doctor). Nothing to answer: zmina closes this "+
				"item when the hold ends.", a.Label(), percent(a.Used), until)}, now)
		if err != nil {
			if s.Warn != nil {
				s.Warn(fmt.Errorf("limits: the todo item for the hold of the %s was not written: %w", a.Label(), err))
			}

			continue
		}

		a.Hold.Ask = ask.ID

		for j := range res.Book.Holds {
			if h := &res.Book.Holds[j]; h.Plan == a.Plan && h.Window == a.Window && h.Reset.Equal(a.Hold.Reset) {
				h.Ask = ask.ID
			}
		}
	}
}

// act does one action's part in the state and returns the event it is logged as and the note the dispatcher is told; empty text:
// nothing to say.
func (s *Shift) act(a limits.Action, book limits.Book, now time.Time) (kind, text string) {
	loc := s.Crew.Location()

	// The window as the notes of warn and checkpoint say it: `claude 5h window at 76%, resets 14:00 (in 1h12m)`.
	window := fmt.Sprintf("%s at %s", a.Label(), percent(a.Used))
	if !a.Reset.IsZero() {
		window += fmt.Sprintf(", resets %s (in %s)", limitClock(a.Reset, now, loc), limitIn(a.Reset.Sub(now)))
	} else {
		window += " (it gave no reset time)"
	}

	switch a.Kind {
	case limits.KindWarn:
		return "limit-warn", window
	case limits.KindCheckpoint:
		asked := s.askCheckpoints(a, now)
		if len(asked) == 0 {
			return "limit-checkpoint", window + ": no role of the plan that is up holds a task, so nobody was asked for a checkpoint"
		}

		return "limit-checkpoint", fmt.Sprintf("%s: asked %s for a checkpoint now (each holds a task)", window, strings.Join(asked, ", "))
	case limits.KindHold:
		roles := len(s.PlanRoles(a.Plan))

		return "limit-hold", fmt.Sprintf("%s: %s at %s, holding its %d %s until %s; lines wait, the owner's pass", a.Plan,
			limits.WindowLabel(a.Window), percent(a.Used), roles, plural(roles, "role", "roles"), limitClock(a.Reset, now, loc))
	case limits.KindRelease:
		return "limit-release", s.holdEnded(a, book, now)
	}

	return "", ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}

	return many
}

// holdEnded is the note of a hold that ended, and what ending it does: the item it opened for the owner is closed, and a role that
// was asked for its note before a restart gets its ten minutes again, from now (they ran out unread while nothing was delivered).
func (s *Shift) holdEnded(a limits.Action, book limits.Book, now time.Time) string {
	loc := s.Crew.Location()
	label := limits.WindowLabel(a.Window)

	if a.Hold.Ask != "" && s.Asks != nil {
		_, _ = s.Asks.Close(a.Hold.Ask, asks.Done, "the hold ended; zmina closed it", now)
	}

	// Another window of the plan may hold it still: then no line goes yet.
	if h, still := book.HeldUntil(a.Plan, now); still {
		why, _ := releaseWhy(a, now, loc)

		return fmt.Sprintf("%s: %s %s, but the plan is still held by its %s until %s; lines wait", a.Plan, label, why,
			limits.WindowLabel(h.Window), limitClock(h.Reset, now, loc))
	}

	for _, name := range s.PlanRoles(a.Plan) {
		if rs := s.State.Roles[name]; rs != nil && !rs.RestartAsked.IsZero() {
			rs.RestartAsked = now
		}
	}

	n := s.planLines(a.Plan)

	goes := "no line waits"

	switch {
	case n == 1:
		goes = "1 line goes now"
	case n > 1:
		goes = fmt.Sprintf("%d lines go now", n)
	}

	why, sep := releaseWhy(a, now, loc)

	return fmt.Sprintf("%s: %s %s%s%s", a.Plan, label, why, sep, goes)
}

// releaseWhy says how a hold ended — reset (the note's plain case: `5h window reset, 14 lines go now`), under the share, or by the
// clock with the data stale — and what joins it to the count of lines that go.
func releaseWhy(a limits.Action, now time.Time, loc *time.Location) (why, sep string) {
	switch {
	case a.Reason == limits.ReasonOff:
		return "hold is turned off in the crew file", "; "
	case a.Reason == limits.ReasonBelow:
		return fmt.Sprintf("is at %s, under its hold share", percent(a.Used)), "; "
	case a.Confirmed:
		return "reset", ", "
	}

	return fmt.Sprintf("reset time %s passed and no fresh reading has come, so its data is stale", limitClock(a.Reset, now, loc)), "; "
}

// askCheckpoints asks every role of the action's plan that is up and holds an active task to save a checkpoint now: the request
// file a restart's note uses, and a line that points to it. A role with no task gets nothing. It returns who was asked.
func (s *Shift) askCheckpoints(a limits.Action, now time.Time) []string {
	var asked []string

	for _, name := range s.PlanRoles(a.Plan) {
		tasks := s.heldTasks(name)
		if len(tasks) == 0 || !s.has(name) {
			continue
		}

		file, err := s.checkpointFile(name, tasks, handoff.Checkpoint, func(t task.Task) string {
			return fmt.Sprintf("The %s of your plan is at %s: zmina delivers nothing to the plan's roles once it is spent, until it resets. "+
				"Write zmina --crew %s task checkpoint %s --file <json-file>: the note a fresh session would start from after the reset. "+
				"Do not park the task.\n", limits.WindowLabel(a.Window), percent(a.Used), shellWord(s.Crew.Path), t.ID)
		})
		if err != nil {
			s.log(name, "limit-checkpoint", "", "the request was not written: "+err.Error())

			continue
		}

		line := fmt.Sprintf(limitCheckpointOpening+"the %s of your plan is at %s. Read %q, save each checkpoint it asks for now so that work can go on "+
			"from your note, then stop; no restart follows, your next line is work as usual.", limits.WindowLabel(a.Window), percent(a.Used), file)

		s.enqueue(name, line, Facts{From: "zmina"})
		s.log(name, "limit-checkpoint", taskIDs(tasks), a.Label()+" at "+percent(a.Used)+": asked for a checkpoint")

		asked = append(asked, name)
	}

	return asked
}
