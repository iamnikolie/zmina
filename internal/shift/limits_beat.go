package shift

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/handoff"
	"github.com/iamnikolie/zmina/internal/limits"
	"github.com/iamnikolie/zmina/internal/task"
)

// limitCheckpointOpening starts the line that asks a role for a checkpoint because its plan's window is filling. It is a line of
// zmina's own, for the conversation it is posted to (sessionBoundOpenings).
const limitCheckpointOpening = "[zmina] limit checkpoint: "

// signalsRefusedNote starts the one note that says how many signals were refused while the dispatcher was held; holdNoteTail ends the note of a hold.
// Both are kept by the cap on the health notes while the dispatcher's plan is held, since the dispatcher cannot read them until it ends.
const (
	signalsRefusedNote = "limits: "
	holdNoteTail       = "; lines wait, the owner's pass"
)

// signalsCap is how many signals may wait for the dispatcher: signalsMax, and signalsHeldMax while its plan is held.
func (s *Shift) signalsCap() int {
	if s.Now != nil && s.planHeld(s.Crew.Dispatcher, s.Now()) {
		return signalsHeldMax
	}

	return signalsMax
}

// signalRefused counts a signal refused while the dispatcher is held, in the one note that says so: the note is replaced, not added to, so
// that the dispatcher finds one line when it reads again, and the count is the note's own.
func (s *Shift) signalRefused(most int) {
	n := 1

	for _, note := range s.State.Notes {
		if rest, ok := strings.CutPrefix(note, signalsRefusedNote); ok {
			var k int
			if _, err := fmt.Sscanf(rest, "%d signals", &k); err == nil {
				n += k
			}
		}
	}

	s.State.Notes = slices.DeleteFunc(s.State.Notes, func(note string) bool { return strings.HasPrefix(note, signalsRefusedNote) })

	s.addNotes([]string{fmt.Sprintf("%s%d signals from the roles were refused while the dispatcher was held (it takes %d): zmina queue ls, zmina note",
		signalsRefusedNote, n, most)})
}

// holdNotePrefix is how the note of a hold begins: its plan and window.
func holdNotePrefix(h limits.Hold) string {
	return fmt.Sprintf("%s: %s at ", h.Plan, limits.WindowLabel(h.Window))
}

// pinnedNote reports a note the cap on the health notes keeps while the dispatcher's plan is held: the note of a hold that is on, and the one that
// says signals were refused. The dispatcher reads neither until the hold is over, and they are what it must read first. The note of a hold that has
// ended is a note like the others.
func (s *Shift) pinnedNote(note string) bool {
	if s.Now == nil || !s.planHeld(s.Crew.Dispatcher, s.Now()) {
		return false
	}

	if strings.HasPrefix(note, signalsRefusedNote) {
		return true
	}

	return strings.HasSuffix(note, holdNoteTail) &&
		slices.ContainsFunc(s.holdBook().Active(s.Now()), func(h limits.Hold) bool { return strings.HasPrefix(note, holdNotePrefix(h)) })
}

// writeLimitsBook keeps the book; a test replaces it to make a write fail.
var writeLimitsBook = limits.WriteBook

// holdAskBy marks the todo items a plan hold opens for the owner: the beat closes one that no hold on the book answers for any more.
const holdAskBy = "limits-hold"

// limitsStep is the thresholds of the crew file's limits: key, once a beat. It judges how full the plans were when the beat began
// (Shift.Limits, read by the caller outside the lock) by limits.Evaluate, keeps the book (limits/holds.json) and does what came
// out of it: a warn is a note for the dispatcher, a checkpoint asks the plan's roles that are up and hold a task for a checkpoint
// (the request file a restart's note uses) and tells the dispatcher whom, a hold tells the dispatcher and, when the dispatcher
// is held too, puts an item on the owner's todo that blocks it, and a release says so, lets the roles go in turn and counts the lines that
// go. Each is one event too: limit-warn, limit-checkpoint, limit-hold, limit-release.
//
// The book is written first, alone: a hold must stand before the first line that could be delivered under it, and nothing else is done
// of a book that could not be written (one event an hour says why). Then the todo items, whose ids go in the book with a second write,
// then the state's part: the notes and requests are saved with the beat, or at once by the flush at the end. A beat killed in between
// loses a note, never a hold. A holds file that does not read is moved aside and replaced; one of a newer zmina is left alone. Notes are
// added here, not returned, so that the flush saves them. It returns the lines for the beat's log.
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

	switch {
	case errors.Is(err, limits.ErrBookFuture):
		s.limitsProblem("future", 24*time.Hour, err.Error()+": zmina holds nothing by it and writes nothing over it, until it is a version this zmina reads", now)

		return nil
	case err != nil:
		to, merr := limits.MoveAside(dir, now)
		if merr != nil {
			s.limitsProblem("aside", time.Hour, "the holds file does not read ("+err.Error()+") and could not be moved aside: "+merr.Error(), now)

			return nil
		}

		s.log("", "limits-file-bad", "", "the holds file did not read ("+err.Error()+"); it is kept as "+filepath.Base(to)+" and a new one is written")
	}

	res := limits.Evaluate(limits.Input{Report: *s.Limits, Thresholds: s.Crew.LimitsThresholds(), Plans: plans, Book: book, Now: now,
		Order: s.releaseOrder})

	if res.Changed {
		if err := writeLimitsBook(dir, res.Book); err != nil {
			s.limitsProblem("write", time.Hour, "the holds file was not written, so nothing was told or held this beat: "+err.Error(), now)

			s.limitBook = &book

			return nil
		}

		// Writable again: a problem said of writing is over, and what it held back (the todo item of a hold) is no longer held back.
		s.limitsProblemClear("write")
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

	// The owner's todo, once the book stands: an item for each hold of the dispatcher's plan that has none (the one that just began, or one
	// whose item could not be written then), and the close of any that no hold answers for.
	if changed, made := s.openAsks(&res.Book, now); changed {
		if err := writeLimitsBook(dir, res.Book); err != nil {
			s.limitsProblem("write", time.Hour, "the holds file was not written with the todo item of a hold: "+err.Error(), now)

			// An item the book cannot answer for is closed, not left open: the next beat would make another, and the first would be pushed.
			for _, id := range made {
				_, _ = s.Asks.Close(id, asks.Done, "the holds file could not record it; zmina closed it", now)
			}
		} else {
			s.limitBook = &res.Book
		}
	}

	s.closeOrphanAsks(res.Book, now)

	if len(did) > 0 {
		s.flush()
	}

	return did
}

// limitsProblem says why the holds file cannot be kept: one event, not one a beat. The last time a kind of problem was said is kept in
// limits.problem in the crew's directory (the limits directory may be what cannot be written), and it is said again after every.
func (s *Shift) limitsProblem(kind string, every time.Duration, text string, now time.Time) {
	if s.limitsProblemWithin(kind, every, now) {
		return
	}

	var last struct {
		Kind string    `json:"kind"`
		At   time.Time `json:"at"`
	}

	path := filepath.Join(s.Crew.StateDir(), "limits.problem")

	s.log("", "limits-problem", "", text)

	if s.Warn != nil {
		s.Warn(fmt.Errorf("limits: %s", text))
	}

	last.Kind, last.At = kind, now
	if raw, err := json.Marshal(last); err == nil {
		_ = fsx.WriteFile(path, raw, 0o600)
	}
}

// limitsProblemClear forgets a problem of the kind that was said: it is said again if it comes back.
func (s *Shift) limitsProblemClear(kind string) {
	var last struct {
		Kind string    `json:"kind"`
		At   time.Time `json:"at"`
	}

	path := filepath.Join(s.Crew.StateDir(), "limits.problem")

	if raw, err := os.ReadFile(path); err == nil && json.Unmarshal(raw, &last) == nil && last.Kind == kind {
		_ = os.Remove(path)
	}
}

// limitsProblemWithin reports whether a problem of the kind was said less than every ago.
func (s *Shift) limitsProblemWithin(kind string, every time.Duration, now time.Time) bool {
	var last struct {
		Kind string    `json:"kind"`
		At   time.Time `json:"at"`
	}

	raw, err := os.ReadFile(filepath.Join(s.Crew.StateDir(), "limits.problem"))

	return err == nil && json.Unmarshal(raw, &last) == nil && last.Kind == kind && now.Sub(last.At) < every
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

// dispatcherHeldBy reports whether a hold keeps the dispatcher: its plan, and for Codex its home.
func (s *Shift) dispatcherHeldBy(h limits.Hold) bool {
	d, err := s.Role(s.Crew.Dispatcher)

	return err == nil && PlanOf(d) == h.Plan && (h.Home == "" || s.seatHome(d) == h.Home)
}

// openAsks puts an item on the owner's todo for each active hold of the dispatcher's own plan that has none: the dispatcher cannot read the
// note that tells of it, so the owner is told by the path the other owner alarms take (a todo item that blocks the role, which the crew's
// notify command pushes). An item of the hold's kind that is open already, made for this plan and window since the hold began (the beat that made it
// died before it recorded the id), is the hold's: it is adopted, not made again. Nothing is made within an hour of a write of the book that failed,
// whose second write would fail the same way. The item's id goes in the hold, so that its release closes it. A todo that cannot be written is warned
// of; the event and zmina doctor say it all the same. It reports whether it changed the book, and the ids of the items it made (as against adopted).
func (s *Shift) openAsks(book *limits.Book, now time.Time) (changed bool, made []string) {
	if s.Asks == nil {
		return false, nil
	}

	loc := s.Crew.Location()

	var open []asks.Ask

	for i := range book.Holds {
		h := &book.Holds[i]
		if h.Ask != "" || !h.Active(now) || !s.dispatcherHeldBy(*h) {
			continue
		}

		if open == nil {
			open = s.Asks.Pending()
		}

		if a, ok := adoptable(open, *h, book.Holds); ok {
			h.Ask, changed = a.ID, true

			continue
		}

		if s.limitsProblemWithin("write", time.Hour, now) {
			continue
		}

		until := limitClock(h.Reset, now, loc)

		ask, err := s.Asks.Add(asks.Ask{Kind: asks.Check, Role: "ticker", By: holdAskBy, Blocks: s.Crew.Dispatcher,
			Title: fmt.Sprintf("%s at %s: held until %s", h.Label(), percent(h.Used), until),
			Text: fmt.Sprintf("The %s is at %s. zmina delivers nothing to the plan's roles, the dispatcher included, until it resets at %s: "+
				"their lines wait in their queues and your own lines still go through (zmina ls, zmina doctor); zmina limits release %s "+
				"lifts it. Nothing to answer: zmina closes this item when the hold ends.", h.Label(), percent(h.Used), until, h.Plan)}, now)
		if err != nil {
			if s.Warn != nil {
				s.Warn(fmt.Errorf("limits: the todo item for the hold of the %s was not written: %w", h.Label(), err))
			}

			continue
		}

		h.Ask, changed = ask.ID, true
		made = append(made, ask.ID)
	}

	return changed, made
}

// adoptable is the open item of the hold kind that a hold may take for its own: made for its plan and window, since the hold began, and not the
// item of another hold on the book.
func adoptable(open []asks.Ask, h limits.Hold, holds []limits.Hold) (asks.Ask, bool) {
	for _, a := range open {
		if a.By == holdAskBy && strings.HasPrefix(a.Title, h.Label()+" at ") && !a.Created.Before(h.Since) &&
			!slices.ContainsFunc(holds, func(o limits.Hold) bool { return o.Ask == a.ID }) {
			return a, true
		}
	}

	return asks.Ask{}, false
}

// closeOrphanAsks closes the todo items a hold opened that no hold on the book answers for: the holds file was lost, or a hold ended
// with its item not closed. The next beat does it, so an item never outlives the hold it was made for by more than a beat.
func (s *Shift) closeOrphanAsks(book limits.Book, now time.Time) {
	if s.Asks == nil {
		return
	}

	for _, a := range s.Asks.Pending() {
		if a.By != holdAskBy || slices.ContainsFunc(book.Holds, func(h limits.Hold) bool { return h.Ask == a.ID }) {
			continue
		}

		if _, err := s.Asks.Close(a.ID, asks.Done, "no hold answers for it any more; zmina closed it", now); err == nil {
			s.log("", "limit-release", "", a.ID+": the todo item of a hold that is not on the book any more was closed")
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
		roles := s.rolesOf(a.Plan, a.Home)

		// A checkpoint asked a few minutes ago and not yet read would be read after the reset, about a window that is over.
		s.dropCheckpointAsks(roles, "the plan is held: it would be read after the reset")

		// The note of an earlier hold of this window that the dispatcher has not read is the old news: this one replaces it.
		s.State.Notes = slices.DeleteFunc(s.State.Notes, func(n string) bool {
			return strings.HasSuffix(n, holdNoteTail) && strings.HasPrefix(n, holdNotePrefix(a.Hold))
		})

		return "limit-hold", fmt.Sprintf("%s: %s at %s, holding its %d %s until %s%s", a.Plan,
			limits.WindowLabel(a.Window), percent(a.Used), len(roles), plural(len(roles), "role", "roles"), limitClock(a.Reset, now, loc), holdNoteTail)
	case limits.KindRelease:
		if a.Superseded {
			// Another hold of the plan began in the same pass: the one that ended is only closed, and the hold says all there is to say.
			if a.Hold.Ask != "" && s.Asks != nil {
				_, _ = s.Asks.Close(a.Hold.Ask, asks.Done, "the hold ended; zmina closed it", now)
			}

			return "", ""
		}

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

// dropCheckpointAsks takes the limit checkpoint requests out of the queues of roles: the lines asked a window ago, not read, and no
// use after the window rolled over, or while nothing is delivered.
func (s *Shift) dropCheckpointAsks(roles []string, why string) {
	for _, name := range roles {
		s.trimInbox(name, func(line string) bool { return strings.HasPrefix(line, limitCheckpointOpening) }, ClosedDropped, why)
	}
}

// holdEnded is the note of a hold that ended, and what ending it does: the item it opened for the owner is closed, the checkpoint requests
// still queued are dropped, and a role that was asked for its note before a restart gets its ten minutes again (they ran out unread while
// nothing was delivered).
func (s *Shift) holdEnded(a limits.Action, book limits.Book, now time.Time) string {
	loc := s.Crew.Location()
	label := limits.WindowLabel(a.Window)

	if a.Hold.Ask != "" && s.Asks != nil {
		_, _ = s.Asks.Close(a.Hold.Ask, asks.Done, "the hold ended; zmina closed it", now)
	}

	roles := s.rolesOf(a.Plan, a.Home)
	why := releaseWhy(a, now, loc)

	// Another window of the plan may hold it still: then no line goes yet.
	if h, still := book.HeldUntil(a.Plan, a.Home, now); still {
		return fmt.Sprintf("%s: %s %s, but the plan is still held by its %s until %s; lines wait", a.Plan, label, why.text,
			limits.WindowLabel(h.Window), limitClock(h.Reset, now, loc))
	}

	s.dropCheckpointAsks(roles, "the window it was about is over")

	for _, name := range roles {
		if rs := s.State.Roles[name]; rs != nil && !rs.RestartAsked.IsZero() {
			rs.RestartAsked = now
		}
	}

	return fmt.Sprintf("%s: %s %s%s%s", a.Plan, label, why.text, why.sep, s.linesGo(a.Plan, a.Home, roles, book, a.Confirmed, now))
}

// linesGo says how many lines go at a release and how: all at once, or the dispatcher first and the roles in turn.
func (s *Shift) linesGo(plan limits.Plan, home string, roles []string, book limits.Book, confirmed bool, now time.Time) string {
	n := s.planLines(roles)
	if n == 0 {
		return "no line waits"
	}

	lines := fmt.Sprintf("%d lines", n)
	if n == 1 {
		lines = "1 line"
	}

	waiting := 0

	for _, w := range book.Waits {
		if w.Plan == plan && (home == "" || w.Home == home) && now.Before(w.Until) {
			waiting++
		}
	}

	switch {
	case waiting == 0 && n == 1:
		return lines + " goes now"
	case waiting == 0:
		return lines + " go now"
	case confirmed:
		return fmt.Sprintf("%s go: the dispatcher now, the other %d %s %d at a time, a minute apart", lines, waiting, plural(waiting, "role", "roles"),
			limits.StaggerGroup)
	}

	return fmt.Sprintf("%s go: the dispatcher and one role now, the other %d %s when a fresh reading shows the window under its hold share, or in %d minutes",
		lines, waiting, plural(waiting, "role", "roles"), int(limits.UnconfirmedWait/time.Minute))
}

// releaseText is how a hold ended, in words, and what joins it to what follows.
type releaseText struct{ text, sep string }

// releaseWhy says how a hold ended — reset (the note's plain case: `5h window reset, 14 lines go now`), under the share, turned off, lifted
// by hand, a reset time no window can have, or by the clock with the data stale.
func releaseWhy(a limits.Action, now time.Time, loc *time.Location) releaseText {
	switch {
	case a.Reason == limits.ReasonHand:
		return releaseText{"hold lifted by hand", "; "}
	case a.Reason == limits.ReasonOff:
		return releaseText{"hold is turned off in the crew file", "; "}
	case a.Reason == limits.ReasonBad:
		return releaseText{"hold had a reset time no such window can have (" + a.Reset.In(loc).Format("2 Jan 15:04") + "), so it is lifted", "; "}
	case a.Reason == limits.ReasonBelow:
		return releaseText{fmt.Sprintf("is at %s, under its hold share", percent(a.Used)), "; "}
	case a.Confirmed:
		return releaseText{"reset", ", "}
	}

	return releaseText{fmt.Sprintf("reset time %s passed and no fresh reading has come, so its data is stale", limitClock(a.Reset, now, loc)), "; "}
}

// askCheckpoints asks every role of the action's plan (and Codex home) that is up and holds an active task to save a checkpoint now: the
// request file a restart's note uses, and a line that points to it. A role with no task gets nothing. It returns who was asked.
func (s *Shift) askCheckpoints(a limits.Action, now time.Time) []string {
	var asked []string

	for _, name := range s.rolesOf(a.Plan, a.Home) {
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

// LiftedHolds is what lifting a plan's holds by hand did, for the command to print.
type LiftedHolds struct {
	// Holds are the holds that were lifted, Waits how many roles that were waiting their turn after a release were let go with them,
	// Notes what the dispatcher was told.
	Holds []limits.Hold
	Waits int
	Notes []string
	// Turns is how many roles of the plan go in turn after it (the dispatcher goes at once).
	Turns int
}

// LiftHolds lifts by hand the holds of a plan that are on (those of one window when window is not empty): the period of each is marked
// lifted in the book, so that it is not held again, a later period may be; the roles go in turn as after any release, the todo item
// of each hold is closed, the dispatcher is told, and the event is limit-release with the reason by hand. It returns what it lifted, nothing
// when nothing is held. Call it under the state lock.
func (s *Shift) LiftHolds(plan limits.Plan, window string, now time.Time) (LiftedHolds, error) {
	dir := s.Crew.Layout().Limits()

	book, err := limits.ReadBook(dir)
	if err != nil {
		return LiftedHolds{}, fmt.Errorf("limits: %w", err)
	}

	next, lifted := limits.Lift(book, plan, window, now, s.releaseOrder)
	if len(lifted.Actions) == 0 && lifted.Waits == 0 {
		return LiftedHolds{}, nil
	}

	if err := limits.WriteBook(dir, next); err != nil {
		return LiftedHolds{}, fmt.Errorf("limits: the holds file was not written: %w", err)
	}

	s.limitBook = &next

	out := LiftedHolds{Waits: lifted.Waits}

	for _, w := range next.Waits {
		if w.Plan == plan {
			out.Turns++
		}
	}

	for _, a := range lifted.Actions {
		out.Holds = append(out.Holds, a.Hold)

		text := s.holdEnded(a, next, now)
		out.Notes = append(out.Notes, text)

		s.addNotes([]string{text})
		s.log("", "limit-release", "", text)
	}

	if len(lifted.Actions) == 0 {
		// Roles that were waiting their turn after an earlier release: nothing was held, and they are let go.
		text := fmt.Sprintf("%s: the roles still waiting their turn after a release are let go by hand", plan)
		out.Notes = append(out.Notes, text)

		s.addNotes([]string{text})
		s.log("", "limit-release", "", text)
	}

	s.flush()

	return out, nil
}
