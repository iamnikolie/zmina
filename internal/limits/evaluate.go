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
	// ReasonOff: the crew file turned the window's hold share off while the hold was on.
	ReasonOff Reason = "off"
	// ReasonBad: the hold's reset time cannot be its window's (further off than the window is long): nothing can end it but this.
	ReasonBad Reason = "bad reset"
	// ReasonHand: the owner or the dispatcher lifted it (zmina limits release).
	ReasonHand Reason = "by hand"
)

// Action is one thing the thresholds ask the caller to do.
type Action struct {
	Kind   Kind
	Plan   Plan
	Window string
	// Home is the Codex home whose reading it was (empty for Claude).
	Home string
	// Used is the window's share when it was read: the reading's, or for a release with no fresh one, the hold's own.
	Used float64
	// Reset is the window's reset time, zero when it gave none.
	Reset time.Time
	// Hold is the hold that began (KindHold) or ended (KindRelease).
	Hold Hold
	// Reason and Confirmed are for a release. Confirmed says a fresh reading showed the window rolled over or under the share, or
	// that a person lifted the hold; a release by the clock alone, with no reading since the reset, is not confirmed: its data is
	// stale, and the release is paced more carefully (Stagger).
	Reason    Reason
	Confirmed bool
	// Seen is when the reading was taken (the last one, for a release that is not confirmed); zero when there was none.
	Seen time.Time
	// Superseded is set on a release in a pass that also began a hold of the same plan: the caller says only the hold.
	Superseded bool
}

// Label is the action's window in words: "claude 5h window".
func (a Action) Label() string { return string(a.Plan) + " " + WindowLabel(a.Window) }

// Roles are the roles of a plan in the order a release lets them go: the dispatcher alone first, the others the one whose oldest
// line has waited longest first.
type Roles struct {
	Dispatcher string
	Others     []string
}

// The pace of a release.
const (
	// StaggerGroup is how many roles go in one beat after the dispatcher.
	StaggerGroup = 3
	// StaggerGap is the time between two groups: a beat.
	StaggerGap = time.Minute
	// UnconfirmedWait is how long the roles after the first wait when the hold ended by the clock with no fresh reading to confirm
	// it, unless a fresh reading shows the window under its hold share before.
	UnconfirmedWait = 10 * time.Minute
)

// Input is what Evaluate judges. Report may be as read at an earlier moment: Evaluate judges it again at Now, with the one clock
// everything else uses.
type Input struct {
	Report     Report
	Thresholds Thresholds
	// Plans are the plans a crew's roles run on: nothing is said of a plan none of them bills.
	Plans []Plan
	Book  Book
	Now   time.Time
	// Order tells in what order the roles of a plan (of a Codex home) go on when a hold ends; asked only then. Nil: they all go at
	// once.
	Order func(plan Plan, home string) Roles
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
	home string
	w    *Window
	at   time.Time
}

// judged is a copy of the report with every window judged again at now: bounded, and stale or not by this clock.
func (r Report) judged(now time.Time) Report {
	if c := r.Claude; c != nil {
		cp := *c
		cp.FiveHour, cp.SevenDay = copyWindow(c.FiveHour), copyWindow(c.SevenDay)
		cp.Judge(now)
		r.Claude = &cp
	}

	if c := r.Codex; c != nil {
		cp := *c
		cp.Week = copyWindow(c.Week)
		cp.Judge(now)
		r.Codex = &cp
	}

	return r
}

func copyWindow(w *Window) *Window {
	if w == nil {
		return nil
	}

	cp := *w
	if w.Resets != nil {
		t := *w.Resets
		cp.Resets = &t
	}

	return &cp
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
				out = append(out, windowReading{plan: PlanClaude, name: w.name, w: w.w, at: c.At})
			}
		}
	}

	if c := r.Codex; c != nil && c.Week != nil {
		out = append(out, windowReading{plan: PlanCodex, name: WindowWeek, home: c.Home, w: c.Week, at: c.At})
	}

	return out
}

// Evaluate is the thresholds as a pure function: the limits as read, the crew's thresholds, what was told and what is held,
// and the time in; what to do and what to keep out. Each window of each plan is judged on its own, by its own reset time,
// and the clock is the one Now says: the report is judged again by it.
//
// A window that is stale (its reset passed since the reading, or the reading is old) triggers nothing and releases
// nothing: unknown stays unknown. A hold ends only by the clock, at the window's reset time, or when a fresh reading of a later
// period, drawn after the hold began, shows the window rolled over (a new window, or another account); or when the hold share was
// turned off, which needs no reading; or when its reset time cannot be its window's; or by hand (Lift). A lower figure of the same period never
// ends it: usage inside one period only grows, so a lower figure is an old one, a session redrawing what its last reply showed. A hold is only ever made on a reset
// that lies ahead of Now and within the window's length (MaxReset).
//
// In one pass a window asks for the highest threshold it is at and has not asked for (a window found at 98% holds, and does not
// also warn and ask for a checkpoint that could not be delivered), and the lower ones count as told. When the last hold of a plan
// ends, its roles are let go in turn (Stagger) and not all in one beat.
func Evaluate(in Input) Result {
	book := in.Book.clone()
	now := in.Now
	rs := in.Report.judged(now).readings()

	var acts []Action

	find := func(plan Plan, window, home string) *windowReading {
		for i := range rs {
			if rs[i].plan == plan && rs[i].name == window && holds(home, rs[i].home) {
				return &rs[i]
			}
		}

		return nil
	}

	kept := book.Holds[:0:0]
	released := map[relKey]relInfo{}

	for _, h := range book.Holds {
		if a, ended := release(h, find(h.Plan, h.Window, h.Home), in.Thresholds.For(h.Plan), now); ended {
			acts = append(acts, a)

			k := relKey{h.Plan, h.Home}

			ri, seen := released[k]
			if !seen {
				ri = relInfo{window: h.Window, reset: h.Reset, confirmed: true}
			}

			ri.confirmed = ri.confirmed && a.Confirmed
			released[k] = ri

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

	// A hold that begins in the pass that ends another of its plan says the new one and nothing of the old: no roles are let go in turn
	// (pace sees the plan held), and the release is marked, so that its caller closes what it opened and says nothing.
	for i, a := range acts {
		if a.Kind == KindRelease && slices.ContainsFunc(acts, func(b Action) bool { return b.Kind == KindHold && b.Plan == a.Plan && b.Home == a.Home }) {
			acts[i].Superseded = true
		}
	}

	book.pace(rs, released, in, now)

	book.prune(now)
	book.Postponed = slices.DeleteFunc(book.Postponed, func(p Postponed) bool { return now.Sub(p.Run) > postponedKeep })

	return Result{Book: book, Actions: acts, Changed: !book.same(in.Book)}
}

// release judges whether a hold has ended, and how.
func release(h Hold, r *windowReading, marks Shares, now time.Time) (Action, bool) {
	a := Action{Kind: KindRelease, Plan: h.Plan, Window: h.Window, Home: h.Home, Used: h.Used, Reset: h.Reset, Hold: h, Reason: ReasonReset}

	fresh := r != nil && !r.w.Stale
	if r != nil {
		a.Seen = r.at
	}

	switch {
	case !marks.HoldOf(h.Window).On():
		// The crew file turned it off: it was the owner's to turn off, and no reading is needed to say so.
		a.Reason, a.Confirmed = ReasonOff, true

		return a, true
	case !h.credible(now):
		// A reset time that cannot be this window's ends nothing by itself: it is ended here, and not trusted.
		a.Reason = ReasonBad

		return a, true
	case !now.Before(h.Reset):
		// The clock ends it. A fresh reading of another period says the window did roll over; none says nothing, and the data is stale.
		// A reading with no reset (the reader dropped it as no window's) says nothing of the window having rolled over.
		a.Confirmed = fresh && r.w.Resets != nil && !sameReset(*r.w.Resets, h.Reset) && r.at.After(h.Since)
		if fresh {
			a.Used = r.w.Used
		}

		return a, true
	case fresh && r.w.Resets != nil && r.w.Resets.After(h.Reset) && !sameReset(*r.w.Resets, h.Reset) && r.at.After(h.Since):
		// A reading of a later period, drawn after the hold began: the window rolled over before the time the hold was kept to (a new window,
		// or another account). A lower figure of the same period is no such thing: usage inside one period only grows, so it is an old figure
		// (a session redrawing the last it saw), and ends nothing; nor does a reading of an earlier period, which is an old input redrawn.
		a.Confirmed, a.Used = true, r.w.Used

		return a, true
	}

	return a, false
}

// relKey is a plan's holds of one Codex home (empty for Claude), the unit a release is paced for.
type relKey struct {
	plan Plan
	home string
}

// relInfo is what the holds of one relKey that ended in one pass have in common: the window and the reset of the first, and whether every one of
// them ended on a fresh reading (or by a person).
type relInfo struct {
	window    string
	reset     time.Time
	confirmed bool
}

// pace sets the turns of the roles of a plan whose last hold just ended, and moves on those that wait for more than the clock: a wait
// that is pending goes when a fresh reading shows the new period. Waits whose time has passed are dropped.
func (b *Book) pace(rs []windowReading, released map[relKey]relInfo, in Input, now time.Time) {
	// Pending waits first: a fresh reading of the new period (a reset after the one of the hold that ended) lets those of its plan go on, a group a
	// beat. A reading of the same period, or of an earlier one, is no news of the window having rolled over.
	for _, r := range rs {
		if r.w.Stale || r.w.Resets == nil {
			continue
		}

		var open []int

		for i, w := range b.Waits {
			if w.Pending && w.Plan == r.plan && w.Window == r.name && holds(w.Home, r.home) && now.Before(w.Until) &&
				r.w.Resets.After(w.After) && !sameReset(*r.w.Resets, w.After) {
				open = append(open, i)
			}
		}

		for n, i := range open {
			b.Waits[i].Until = now.Add(time.Duration(n/StaggerGroup) * StaggerGap)
			b.Waits[i].Pending = false
		}
	}

	keys := make([]relKey, 0, len(released))
	for k := range released {
		keys = append(keys, k)
	}

	slices.SortFunc(keys, func(a, b relKey) int {
		if a.plan != b.plan {
			return cmpString(string(a.plan), string(b.plan))
		}

		return cmpString(a.home, b.home)
	})

	for _, k := range keys {
		if _, still := b.HeldUntil(k.plan, k.home, now); still || in.Order == nil {
			continue
		}

		ri := released[k]
		b.stagger(in.Order(k.plan, k.home), k.plan, ri.window, k.home, ri.reset, ri.confirmed, now)
	}

	b.Waits = slices.DeleteFunc(b.Waits, func(w Wait) bool { return !now.Before(w.Until) })
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}

	return 0
}

// stagger puts the roles of a plan whose hold ended in their turns, after the dispatcher, who goes at once: a group of StaggerGroup
// a beat, the role whose oldest line waited longest first. A hold that ended by the clock alone, with no fresh reading, lets one
// role go with the dispatcher and keeps the others for a fresh reading under the share, or UnconfirmedWait.
func (b *Book) stagger(roles Roles, plan Plan, window, home string, after time.Time, confirmed bool, now time.Time) {
	b.Waits = slices.DeleteFunc(b.Waits, func(w Wait) bool { return w.Plan == plan && holds(w.Home, home) })

	add := func(role string, until time.Time, pending bool) {
		b.Waits = append(b.Waits, Wait{Role: role, Plan: plan, Window: window, Home: home, Until: until, Pending: pending, After: after})
	}

	for i, role := range roles.Others {
		switch {
		case confirmed:
			add(role, now.Add(time.Duration(i/StaggerGroup+1)*StaggerGap), false)
		case i > 0:
			add(role, now.Add(UnconfirmedWait), true)
		}
	}
}

// trigger adds what one fresh window asks for to acts, and what it was told to the book.
func (b *Book) trigger(acts []Action, r windowReading, marks Shares, now time.Time) []Action {
	var reset time.Time
	if r.w.Resets != nil {
		reset = *r.w.Resets
	}

	at := b.find(r.plan, r.name, r.home, reset)

	p := Period{Plan: r.plan, Window: r.name, Home: r.home, Reset: reset}
	if at >= 0 {
		p = b.Periods[at]
		if reset.After(p.Reset) {
			p.Reset = reset // the later of two resets less than two minutes apart
		}
	}

	used := r.w.Used

	// A window with no reset time has one period for ever: when it falls under a share, a later climb is a new one.
	if reset.IsZero() {
		if marks.Warn.On() && used < float64(marks.Warn) {
			p.Warn = time.Time{}
		}

		if marks.Checkpoint.On() && used < float64(marks.Checkpoint) {
			p.Checkpoint = time.Time{}
		}
	}

	act := Action{Plan: r.plan, Window: r.name, Home: r.home, Used: used, Reset: reset, Seen: r.at}
	share := marks.HoldOf(r.name)

	// A hold needs a reset time that lies ahead and is its window's: one that is not (none, past, or further than the window is
	// long) is never held, and a period the owner lifted is not held again.
	holdable := share.On() && !reset.IsZero() && now.Before(reset) && reset.Sub(now) <= MaxReset(r.name) && p.Lifted.IsZero()

	switch {
	case holdable && used >= float64(share):
		if !b.holding(r.plan, r.name, r.home, reset) {
			h := Hold{Plan: r.plan, Window: r.name, Home: r.home, Reset: reset, Since: now, Used: used}
			b.Holds = append(b.Holds, h)
			act.Kind, act.Hold = KindHold, h
			acts = append(acts, act)
		}

		p = p.stamp(now, true, true)
	case marks.Checkpoint.On() && used >= float64(marks.Checkpoint):
		if p.Checkpoint.IsZero() {
			act.Kind = KindCheckpoint
			acts = append(acts, act)
		}

		p = p.stamp(now, true, true)
	case marks.Warn.On() && used >= float64(marks.Warn):
		if p.Warn.IsZero() {
			act.Kind = KindWarn
			acts = append(acts, act)
		}

		p = p.stamp(now, true, false)
	}

	switch {
	case p.Warn.IsZero() && p.Checkpoint.IsZero() && p.Lifted.IsZero():
		if at >= 0 {
			b.Periods = slices.Delete(b.Periods, at, at+1)
		}
	case at >= 0:
		b.Periods[at] = p
	default:
		b.Periods = append(b.Periods, p)
	}

	return acts
}

// stamp sets the moments of the thresholds a period has reached that are not set yet.
func (p Period) stamp(now time.Time, warn, checkpoint bool) Period {
	if warn && p.Warn.IsZero() {
		p.Warn = now
	}

	if checkpoint && p.Checkpoint.IsZero() {
		p.Checkpoint = now
	}

	return p
}

// prune forgets the periods past their reset by toldKeep (a window with no reset: toldKeepNoReset past its last mark).
func (b *Book) prune(now time.Time) {
	b.Periods = slices.DeleteFunc(b.Periods, func(p Period) bool {
		last := p.Warn
		for _, t := range []time.Time{p.Checkpoint, p.Lifted} {
			if t.After(last) {
				last = t
			}
		}

		return (!p.Reset.IsZero() && now.After(p.Reset.Add(toldKeep))) || (p.Reset.IsZero() && now.After(last.Add(toldKeepNoReset)))
	})
}

// Lifted is what lifting a plan's holds by hand did.
type Lifted struct {
	// Actions are the release of each hold lifted, in the form a beat's release is.
	Actions []Action
	// Waits is how many roles that were waiting their turn after a release were let go with it.
	Waits int
}

// Lift ends by hand the holds of a plan that are on at now (those of one window when window is not empty; every Codex home's, for
// Codex), and marks their periods lifted: a lifted period is not held again, a later one may be. Roles still waiting their turn
// after an earlier release go too, at once: the person who lifts has said go. A hold that is lifted lets the plan's roles go in turn, as after any
// release. Nothing is lifted that is not held.
func Lift(book Book, plan Plan, window string, now time.Time, order func(plan Plan, home string) Roles) (Book, Lifted) {
	b := book.clone()

	var out Lifted

	homes := map[string]bool{}
	kept := b.Holds[:0:0]

	for _, h := range b.Holds {
		if h.Plan != plan || (window != "" && h.Window != window) || !h.Active(now) {
			kept = append(kept, h)

			continue
		}

		out.Actions = append(out.Actions, Action{Kind: KindRelease, Plan: plan, Window: h.Window, Home: h.Home, Used: h.Used, Reset: h.Reset,
			Hold: h, Reason: ReasonHand, Confirmed: true})
		homes[h.Home] = true

		p := Period{Plan: plan, Window: h.Window, Home: h.Home, Reset: h.Reset, Lifted: now}
		if at := b.find(plan, h.Window, h.Home, h.Reset); at >= 0 {
			p = b.Periods[at]
			p.Lifted = now
			b.Periods[at] = p
		} else {
			b.Periods = append(b.Periods, p)
		}
	}

	b.Holds = kept

	for _, w := range b.Waits {
		if w.Plan == plan && (window == "" || w.Window == window) && now.Before(w.Until) {
			out.Waits++
		}
	}

	b.Waits = slices.DeleteFunc(b.Waits, func(w Wait) bool {
		return w.Plan == plan && (window == "" || w.Window == window) && now.Before(w.Until)
	})

	if order != nil {
		for home := range homes {
			if _, still := b.HeldUntil(plan, home, now); !still {
				b.stagger(order(plan, home), plan, window, home, time.Time{}, true, now)
			}
		}
	}

	return b, out
}
