package limits

import (
	"slices"
	"time"
)

// Kind is what the thresholds ask for.
type Kind string

const (
	// KindWarn: the dispatcher is told, once a period.
	KindWarn Kind = "warn"
	// KindCheckpoint: the roles of the plan that hold a task are asked for a checkpoint, once a period.
	KindCheckpoint Kind = "checkpoint"
	// KindHold: the plan's roles get nothing from now until the window resets.
	KindHold Kind = "hold"
	// KindRelease: a hold ended.
	KindRelease Kind = "release"
)

// Reason is why a hold ended.
type Reason string

const (
	// ReasonReset: the window's reset time passed (or a reading of a later period showed it rolled over).
	ReasonReset Reason = "reset"
	// ReasonBelow: a fresh reading of the same period shows the window under the hold share.
	ReasonBelow Reason = "below"
	// ReasonOff: the crew file turned the plan's hold share off while the hold was on.
	ReasonOff Reason = "off"
)

// Action is one thing the thresholds ask the caller to do.
type Action struct {
	Kind   Kind
	Plan   Plan
	Window string
	// Used is the window's share when it was read: the reading's, or for a release with no fresh one, the hold's own.
	Used float64
	// Reset is the window's reset time, zero when it gave none.
	Reset time.Time
	// Hold is the hold that began (KindHold) or ended (KindRelease).
	Hold Hold
	// Reason and Confirmed are for a release. Confirmed says a fresh reading showed the window rolled over or under the share;
	// a release by the clock alone, with no reading since the reset, is not confirmed: its data is stale.
	Reason    Reason
	Confirmed bool
	// Seen is when the reading was taken (the last one, for a release that is not confirmed); zero when there was none.
	Seen time.Time
}

// Label is the action's window in words: "claude 5h window".
func (a Action) Label() string { return string(a.Plan) + " " + WindowLabel(a.Window) }

// Input is what Evaluate judges. Report must have been judged for staleness (Collect does).
type Input struct {
	Report     Report
	Thresholds Thresholds
	// Plans are the plans a crew's roles run on: nothing is said of a plan none of them bills.
	Plans []Plan
	Book  Book
	Now   time.Time
}

// Result is what Evaluate decided: the book as it should be kept, and what to do, in order.
type Result struct {
	Book    Book
	Actions []Action
	// Changed reports whether the book differs from the one given, so the caller writes it only then.
	Changed bool
}

type windowReading struct {
	plan Plan
	name string
	w    *Window
	at   time.Time
}

// readings lists every window the report has, Claude's then Codex's.
func (r Report) readings() []windowReading {
	var out []windowReading

	if c := r.Claude; c != nil {
		for _, w := range []struct {
			name string
			w    *Window
		}{{WindowFiveHour, c.FiveHour}, {WindowSevenDay, c.SevenDay}} {
			if w.w != nil {
				out = append(out, windowReading{PlanClaude, w.name, w.w, c.At})
			}
		}
	}

	if c := r.Codex; c != nil && c.Week != nil {
		out = append(out, windowReading{PlanCodex, WindowWeek, c.Week, c.At})
	}

	return out
}

// Evaluate is the thresholds as a pure function: the limits as read, the crew's thresholds, what was told and what is held,
// and the time in; what to do and what to keep out. Each window of each plan is judged on its own, by its own reset time.
//
// A window that is stale (its reset passed since the reading, or the reading is old) triggers nothing and releases
// nothing: unknown stays unknown. A hold ends only by the clock, at the window's reset time, or when a fresh reading of
// the same period shows the window under the hold share, or of a later period shows it rolled over; or when the hold share
// was turned off, which needs no reading.
//
// In one pass a window asks for the highest threshold it is at and has not asked for (a window found at 98% holds, and does not
// also warn and ask for a checkpoint that could not be delivered), and the lower ones count as told.
func Evaluate(in Input) Result {
	book := in.Book.clone()
	now := in.Now
	rs := in.Report.readings()

	var acts []Action

	find := func(plan Plan, window string) *windowReading {
		for i := range rs {
			if rs[i].plan == plan && rs[i].name == window {
				return &rs[i]
			}
		}

		return nil
	}

	kept := book.Holds[:0:0]

	for _, h := range book.Holds {
		if a, ended := release(h, find(h.Plan, h.Window), in.Thresholds.For(h.Plan), now); ended {
			acts = append(acts, a)

			continue
		}

		kept = append(kept, h)
	}

	book.Holds = kept

	for _, r := range rs {
		if !slices.Contains(in.Plans, r.plan) || r.w.Stale {
			continue
		}

		acts = book.trigger(acts, r, in.Thresholds.For(r.plan), now)
	}

	book.prune(now)

	return Result{Book: book, Actions: acts, Changed: !book.same(in.Book)}
}

// release judges whether a hold has ended, and how.
func release(h Hold, r *windowReading, marks Shares, now time.Time) (Action, bool) {
	a := Action{Kind: KindRelease, Plan: h.Plan, Window: h.Window, Used: h.Used, Reset: h.Reset, Hold: h, Reason: ReasonReset}

	fresh := r != nil && !r.w.Stale
	if r != nil {
		a.Seen = r.at
	}

	switch {
	case !marks.Hold.On():
		// The crew file turned it off: it was the owner's to turn off, and no reading is needed to say so.
		a.Reason, a.Confirmed = ReasonOff, true

		return a, true
	case !now.Before(h.Reset):
		// The clock ends it. A fresh reading of another period says the window did roll over; none says nothing, and the data is stale.
		a.Confirmed = fresh && (r.w.Resets == nil || minute(*r.w.Resets) != minute(h.Reset))
		if fresh {
			a.Used = r.w.Used
		}

		return a, true
	case fresh && r.w.Resets != nil && minute(*r.w.Resets) > minute(h.Reset):
		// A later period already: the window rolled over before the time the hold was kept to.
		a.Confirmed, a.Used = true, r.w.Used

		return a, true
	case fresh && r.w.Resets != nil && minute(*r.w.Resets) == minute(h.Reset) && r.w.Used < float64(marks.Hold):
		a.Reason, a.Confirmed, a.Used = ReasonBelow, true, r.w.Used

		return a, true
	}

	return a, false
}

// trigger adds what one fresh window asks for to acts, and what it was told to the book.
func (b *Book) trigger(acts []Action, r windowReading, marks Shares, now time.Time) []Action {
	var reset time.Time
	if r.w.Resets != nil {
		reset = *r.w.Resets
	}

	key := Key(r.plan, r.name, r.w.Resets)
	told := b.Told[key]
	told.Reset = reset
	used := r.w.Used

	// A window with no reset time has one period for ever: when it falls under a share, a later climb is a new one.
	if reset.IsZero() {
		if marks.Warn.On() && used < float64(marks.Warn) {
			told.Warn = time.Time{}
		}

		if marks.Checkpoint.On() && used < float64(marks.Checkpoint) {
			told.Checkpoint = time.Time{}
		}
	}

	act := Action{Plan: r.plan, Window: r.name, Used: used, Reset: reset, Seen: r.at}

	switch {
	case marks.Hold.On() && !reset.IsZero() && used >= float64(marks.Hold):
		// A hold needs a time to end at: a window that gives none is never held.
		if !b.holding(r.plan, r.name, reset) {
			h := Hold{Plan: r.plan, Window: r.name, Reset: reset, Since: now, Used: used}
			b.Holds = append(b.Holds, h)
			act.Kind, act.Hold = KindHold, h
			acts = append(acts, act)
		}

		told = told.stamp(now, true, true)
	case marks.Checkpoint.On() && used >= float64(marks.Checkpoint):
		if told.Checkpoint.IsZero() {
			act.Kind = KindCheckpoint
			acts = append(acts, act)
		}

		told = told.stamp(now, true, true)
	case marks.Warn.On() && used >= float64(marks.Warn):
		if told.Warn.IsZero() {
			act.Kind = KindWarn
			acts = append(acts, act)
		}

		told = told.stamp(now, true, false)
	}

	if told.Warn.IsZero() && told.Checkpoint.IsZero() {
		delete(b.Told, key)

		return acts
	}

	if b.Told == nil {
		b.Told = map[string]Told{}
	}

	b.Told[key] = told

	return acts
}

// stamp sets the moments of the thresholds a period has reached that are not set yet.
func (t Told) stamp(now time.Time, warn, checkpoint bool) Told {
	if warn && t.Warn.IsZero() {
		t.Warn = now
	}

	if checkpoint && t.Checkpoint.IsZero() {
		t.Checkpoint = now
	}

	return t
}

// prune forgets the periods past their reset by toldKeep (a window with no reset: toldKeepNoReset past its last mark).
func (b *Book) prune(now time.Time) {
	for key, t := range b.Told {
		last := t.Warn
		if t.Checkpoint.After(last) {
			last = t.Checkpoint
		}

		if (!t.Reset.IsZero() && now.After(t.Reset.Add(toldKeep))) || (t.Reset.IsZero() && now.After(last.Add(toldKeepNoReset))) {
			delete(b.Told, key)
		}
	}
}
