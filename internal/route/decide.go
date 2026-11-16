package route

import (
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/iamnikolie/zmina/internal/limits"
)

// Env is how things stand: the seats, the plans' limits, and what a copy may be.
type Env struct {
	// Seats are every role of the crew and every live copy of one.
	Seats []Seat
	// Limits is what the plans are at, already judged for staleness (limits.Collect).
	Limits limits.Report
	// Dispatcher is the role that has no copies.
	Dispatcher string
	// MaxCopy is the highest copy number (dev#9); below 2 no copy is ever made.
	MaxCopy int
}

func (e Env) seat(name string) (Seat, bool) {
	for _, s := range e.Seats {
		if s.Name == name {
			return s, true
		}
	}

	return Seat{}, false
}

// used is how full the plan a runtime bills is, in percent of its window (Claude's 7 days, Codex's week); known is
// false for a reading that is missing or stale. Nothing is guessed.
func (e Env) used(runtime string) (float64, bool) {
	switch runtime {
	case "claude":
		if c := e.Limits.Claude; c != nil && c.SevenDay != nil && !c.SevenDay.Stale {
			return c.SevenDay.Used, true
		}
	case "codex":
		if c := e.Limits.Codex; c != nil && c.Week != nil && !c.Week.Stale {
			return c.Week.Used, true
		}
	}

	return 0, false
}

// band is what the plan a runtime bills takes at used percent.
func (e Env) band(runtime string, used float64) limits.Band {
	switch runtime {
	case "claude":
		return e.Limits.Policy.Claude.Band(used)
	case "codex":
		return e.Limits.Policy.Codex.Band(used)
	}

	return limits.Open
}

// view is the plans' readings as the answer carries them; stale is true when either is unknown.
func (e Env) view() (LimitsView, bool) {
	var v LimitsView

	if u, ok := e.used("claude"); ok {
		r := int(math.Round(u))
		v.Claude = &r
	}

	if u, ok := e.used("codex"); ok {
		r := int(math.Round(u))
		v.Codex = &r
	}

	return v, v.Claude == nil || v.Codex == nil
}

// planText is a plan's share as the answer says it: claude 7d 41%.
func planText(runtime string, used float64) string {
	window := "week"
	if runtime == "claude" {
		window = "7d"
	}

	return fmt.Sprintf("%s %s %d%%", runtime, window, int(math.Round(used)))
}

// outcome is how one rule answered: a seat or a crew to tell, or why not.
type outcome struct {
	// why is why the rule did not answer, or why it picked what it did.
	why string
	// seen says the rule had an input of the work to look at: its reason counts when no rule answers.
	seen bool
	tell string
	// awaySeat is the seat a tell is for when it runs on another node: the work is its, and this node does not run it.
	awaySeat string
	seat     *Seat
	// fresh, when above 0, is the copy number a continuation takes instead of the seat itself.
	fresh int
	alts  []Alternative
}

func (o outcome) yields() bool { return o.tell != "" || o.seat != nil }

type engine struct {
	table Table
	in    Input
	env   Env
}

// Decide answers: the first rule of the table that matches and yields a seat wins, and the busy policy is applied to
// the seat it picked. With no rule left it says none, with why each rule that looked at something found no seat.
func Decide(t Table, in Input, env Env) Decision {
	d := Decision{Rule: -1, Action: ActionNone, Alternatives: []Alternative{}}
	d.Limits, d.Stale = env.view()

	if len(t.Rules) == 0 {
		d.Why = "no routes in the crew file"

		return d
	}

	e := &engine{table: t, in: in, env: env}

	var seen []string

	for _, r := range t.Rules {
		o := e.eval(r)

		if !o.yields() {
			d.Trace = addStep(d.Trace, Step{Rule: r.Kind.Number(), Name: r.Kind.Label(), Why: o.why}, r.Kind == OtherCrew)

			if o.seen {
				seen = append(seen, o.why)
			}

			continue
		}

		to, act, why, alts := e.settle(o)

		d.To, d.Action, d.Rule, d.Name, d.Why = to, act, r.Kind.Number(), r.Kind.Label(), why
		d.AwaySeat = o.awaySeat
		d.Alternatives = append(d.Alternatives, alts...)
		d.Trace = append(d.Trace, Step{Rule: d.Rule, Name: d.Name, Matched: true, Why: to + " (" + why + ")"})

		return d
	}

	if len(seen) == 0 {
		seen = []string{"nothing to route by: give --repo, --project, --kind, --after or --of"}
	}

	d.Why = strings.Join(seen, "; ") + ": pick by hand"

	return d
}

// addStep adds a rule that did not answer to the trace; with merge, a row that follows one of the same rule that did
// not answer either joins it (two other_crew rows read as one: repo not under a, b).
func addStep(trace []Step, s Step, merge bool) []Step {
	if n := len(trace); merge && n > 0 && trace[n-1].Rule == s.Rule && !trace[n-1].Matched {
		prev := &trace[n-1]

		switch {
		case prev.Why == s.Why:
		case strings.HasPrefix(prev.Why, notUnder) && strings.HasPrefix(s.Why, notUnder):
			prev.Why += ", " + strings.TrimPrefix(s.Why, notUnder)
		default:
			prev.Why += "; " + s.Why
		}

		return trace
	}

	return append(trace, s)
}

const notUnder = "repo not under "

// settle applies the busy policy to what a rule picked: the seat to give the task, how, and why.
func (e *engine) settle(o outcome) (to string, act Action, why string, alts []Alternative) {
	alts = o.alts

	if o.tell != "" {
		return o.tell, ActionTell, o.why, alts
	}

	s := *o.seat

	if o.fresh > 0 {
		return fmt.Sprintf("%s#%d", s.Base(), o.fresh), ActionCopy, o.why, alts
	}

	switch s.State {
	case StateIdle:
		return s.Name, ActionTask, o.why, alts
	case StateDown:
		return s.Name, ActionUpTask, o.why, alts
	}

	return e.busy(s, o)
}

// busy is rule 7: a seat that is busy gives way to its copy (one that is idle, else a new one: copy-or-pool), else to
// a free seat of the same plan from the pool; with none of those the task queues behind its work.
func (e *engine) busy(s Seat, o outcome) (string, Action, string, []Alternative) {
	note := s.Name + " is " + s.why()
	alts := append(slices.Clone(o.alts), Alternative{To: s.Name, Why: s.why()})
	base := s.Base()

	if e.table.Busy != PoolOnly && base != e.env.Dispatcher {
		for _, c := range e.group(base) {
			if c.Name != s.Name && c.State == StateIdle {
				return c.Name, ActionTask, join(o.why, note, c.Name+" is idle: its copy takes it"), alts
			}
		}

		if n := e.freeCopy(base); n > 0 {
			name := fmt.Sprintf("%s#%d", base, n)

			return name, ActionCopy, join(o.why, note, "a new copy "+name+" takes it"), alts
		}
	}

	if alt := e.freeOfPlan(s); alt != nil {
		return alt.Name, taskFor(alt.State), join(o.why, note, alt.Name+" is "+string(alt.State)+", the same plan"), alts
	}

	return s.Name, ActionTask, join(o.why, note, "no copy or free seat of its plan: it queues behind its work"), alts
}

func taskFor(s State) Action {
	if s == StateDown {
		return ActionUpTask
	}

	return ActionTask
}

func join(parts ...string) string {
	var out []string

	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}

	return strings.Join(out, "; ")
}

// group is a role and its live copies, the role first, then by copy number.
func (e *engine) group(base string) []Seat {
	var out []Seat

	for _, s := range e.env.Seats {
		if s.Base() == base {
			out = append(out, s)
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return copyNumber(out[i].Name) < copyNumber(out[j].Name) })

	return out
}

// freeCopy is the lowest copy number of a role no seat has, 0 when none may be made.
func (e *engine) freeCopy(base string) int {
	taken := map[int]bool{}

	for _, s := range e.group(base) {
		taken[copyNumber(s.Name)] = true
	}

	for n := 2; n <= e.env.MaxCopy; n++ {
		if !taken[n] {
			return n
		}
	}

	return 0
}

// freeOfPlan is a seat of the first pool that bills the plan s bills and is not of s's role: an idle one, else a down
// one, in the pool's order.
func (e *engine) freeOfPlan(s Seat) *Seat {
	for _, r := range e.table.Rules {
		if r.Kind != Pool {
			continue
		}

		for _, want := range []State{StateIdle, StateDown} {
			for _, name := range r.Seats {
				if c, ok := e.env.seat(name); ok && c.State == want && c.Runtime == s.Runtime && c.Base() != s.Base() && c.Away == "" {
					return &c
				}
			}
		}

		return nil
	}

	return nil
}

func (e *engine) eval(r Rule) outcome {
	switch r.Kind {
	case OtherCrew:
		return e.otherCrew(r)
	case Continuation:
		return e.continuation(r)
	case Project:
		return e.project(r)
	case WorkKind:
		return e.workKind(r)
	case Review:
		return e.review(r)
	case Pool:
		return e.pool(r)
	}

	return outcome{why: "unknown rule kind " + string(r.Kind)}
}

// Within reports whether path is dir or below it, both absolute and cleaned.
func Within(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	if path == dir {
		return true
	}

	rel, err := filepath.Rel(dir, path)

	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (e *engine) otherCrew(r Rule) outcome {
	if e.in.Repo == "" {
		return outcome{why: "no --repo"}
	}

	if !Within(e.in.Repo, r.Under) {
		return outcome{why: notUnder + r.UnderText}
	}

	return outcome{tell: r.Tell, why: "repo under " + r.UnderText}
}

// awayTell is the answer for a seat that runs on another node: tell that crew's dispatcher, which owns the seat.
func awayTell(s Seat, why string) outcome {
	return outcome{tell: s.Away, awaySeat: s.Name, why: join(why, s.Name+" is placed on "+s.Away+", not on this node")}
}

// usable is the seat of a name when work can be given to it, else why not.
func (e *engine) usable(name string) (Seat, string) {
	s, ok := e.env.seat(name)

	switch {
	case !ok:
		return Seat{}, name + " is not a seat of this crew"
	case s.State == StateUnavailable:
		return Seat{}, name + " is " + s.why()
	}

	return s, ""
}

func (e *engine) continuation(r Rule) outcome {
	t := e.in.After
	if t == nil {
		return outcome{why: "no --after"}
	}

	// The seat that did it: the copy if it is still there, else its role.
	target, ok := e.env.seat(t.Seat)
	if !ok {
		target, ok = e.env.seat(Base(t.Seat))
	}

	if !ok {
		return outcome{seen: true, why: fmt.Sprintf("%s was done by %s, which is not a seat of this crew", t.ID, t.Seat)}
	}

	if target.Away != "" {
		return awayTell(target, fmt.Sprintf("goes on from %s, done by %s", t.ID, target.Name))
	}

	if target.State == StateUnavailable {
		return outcome{seen: true, why: fmt.Sprintf("%s was done by %s, which is %s", t.ID, target.Name, target.why())}
	}

	fresh := r.FreshOver
	if fresh <= 0 {
		fresh = DefaultFreshOver
	}

	why := fmt.Sprintf("goes on from %s, done by %s", t.ID, target.Name)

	share, known := target.ContextShare()
	if known {
		why += fmt.Sprintf(", context %d%%", share)
	}

	if known && share > fresh {
		if n := e.freeCopy(target.Base()); n > 0 && target.Base() != e.env.Dispatcher {
			return outcome{seat: &target, fresh: n, why: fmt.Sprintf("%s was done by %s, whose context %d%% is over %d%%: a fresh copy",
				t.ID, target.Name, share, fresh)}
		}

		why += fmt.Sprintf(" is over %d%%, but no copy can be made", fresh)
	}

	return outcome{seat: &target, why: why}
}

// projectOf is the project the work is for: the one named, else the repo's directory name.
func (in Input) projectOf() string {
	if in.Project != "" {
		return in.Project
	}

	if in.Repo != "" {
		return filepath.Base(in.Repo)
	}

	return ""
}

func (e *engine) project(r Rule) outcome {
	name := e.in.projectOf()
	if name == "" {
		return outcome{why: "no --project or --repo"}
	}

	seat, ok := r.Owners[name]
	if !ok {
		return outcome{why: fmt.Sprintf("project %s has no owner", name)}
	}

	if a, ok := e.env.seat(seat); ok && a.Away != "" {
		return awayTell(a, fmt.Sprintf("project %s is owned by %s", name, seat))
	}

	s, why := e.usable(seat)
	if why != "" {
		return outcome{seen: true, why: fmt.Sprintf("project %s: %s", name, why)}
	}

	return outcome{seat: &s, why: fmt.Sprintf("project %s is owned by %s", name, seat)}
}

func (e *engine) workKind(r Rule) outcome {
	kind := e.in.Kind
	if kind == "" {
		return outcome{why: "no --kind"}
	}

	seat, ok := r.Owners[kind]
	if !ok {
		if kind == ReviewKind {
			return outcome{why: "a review is the review rule's"}
		}

		return outcome{seen: true, why: fmt.Sprintf("kind %s has no seat in routes (%s)", kind, ownerNames(r.Owners))}
	}

	if a, ok := e.env.seat(seat); ok && a.Away != "" {
		return awayTell(a, fmt.Sprintf("kind %s goes to %s", kind, seat))
	}

	s, why := e.usable(seat)
	if why != "" {
		return outcome{seen: true, why: fmt.Sprintf("kind %s: %s", kind, why)}
	}

	return outcome{seat: &s, why: fmt.Sprintf("kind %s goes to %s", kind, seat)}
}

// ownerNames lists the names a map of owners knows, sorted, six at most.
func ownerNames(m map[string]string) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}

	sort.Strings(names)

	if len(names) > 6 {
		return strings.Join(names[:6], ", ") + ", …"
	}

	return strings.Join(names, ", ")
}

// authorText is "T-14 by dev (claude)".
func (e *engine) authorText(t *Task) string {
	out := t.ID + " by " + t.Seat

	if s, ok := e.env.seat(t.Seat); ok {
		out += " (" + s.Runtime + ")"
	} else if s, ok := e.env.seat(Base(t.Seat)); ok {
		out += " (" + s.Runtime + ")"
	}

	return out
}

func (e *engine) review(r Rule) outcome {
	if e.in.Kind != ReviewKind {
		return outcome{why: "not a review"}
	}

	of := e.in.Of
	if of == nil {
		return outcome{seen: true, why: "a review needs --of: the author may not review"}
	}

	author := Base(of.Seat)
	text := e.authorText(of)

	var alts []Alternative

	// A seat is out when it is the author or cannot take work; why says it.
	out := func(name string) (Seat, string) {
		if Base(name) == author {
			return Seat{}, "the author"
		}

		if a, ok := e.env.seat(name); ok && a.Away != "" {
			return Seat{}, "placed on " + a.Away + ", not on this node"
		}

		return e.usable(name)
	}

	first, again := r.First, r.Again
	if len(again) == 0 {
		again = []string{first}
	}

	// awayFirst is the first review's seat when it runs on another node: it is passed over like a seat that cannot take work, and
	// the seats this node runs take the review; only with none of them is the other crew the answer (the caller tells nobody).
	var awayFirst *Seat

	if !e.in.Rereview && first != "" {
		if a, ok := e.env.seat(first); ok && a.Away != "" && Base(first) != author {
			awayFirst = &a

			alts = append(alts, Alternative{To: first, Why: "placed on " + a.Away + ", not on this node"})
		} else {
			s, why := out(first)
			if why == "" {
				return outcome{seat: &s, why: text + ", first review"}
			}

			alts = append(alts, Alternative{To: first, Why: why})
		}
	}

	var free []Seat

	for _, name := range again {
		if s, why := out(name); why == "" {
			free = append(free, s)
		}
	}

	var chosen *Seat

	for _, want := range []State{StateIdle, StateDown, StateBusy} {
		for i := range free {
			if free[i].State == want && chosen == nil {
				chosen = &free[i]
			}
		}
	}

	if chosen == nil {
		if awayFirst != nil {
			o := awayTell(*awayFirst, "first review of "+text+", and no seat of this node can take it")
			o.alts = dedupeAlts(alts)

			return o
		}

		return outcome{seen: true, why: "review of " + text + ": no seat that is not the author can take it", alts: alts}
	}

	// Seats listed before the pick that were passed over because they are busy, and the rest as alternatives.
	var passed []string

	for _, name := range again {
		if name == chosen.Name {
			break
		}

		if s, why := out(name); why == "" && s.State == StateBusy {
			passed = append(passed, name+" "+s.why())
		}
	}

	for _, name := range again {
		if name == chosen.Name {
			continue
		}

		if s, why := out(name); why != "" {
			alts = append(alts, Alternative{To: name, Why: why})
		} else {
			alts = append(alts, Alternative{To: name, Why: s.why()})
		}
	}

	kind := "review"
	if e.in.Rereview {
		kind = "re-review"
	}

	why := join(kind+", "+text, strings.Join(passed, "; "), fmt.Sprintf("%s %s, not the author", chosen.Name, chosen.State))
	if awayFirst != nil {
		why = join(why, "the first review seat "+awayFirst.Name+" is placed on "+awayFirst.Away+", not on this node")
	}

	return outcome{seat: chosen, why: why, alts: dedupeAlts(alts)}
}

// dedupeAlts keeps the first of each seat, in order.
func dedupeAlts(in []Alternative) []Alternative {
	seen := map[string]bool{}

	var out []Alternative

	for _, a := range in {
		if !seen[a.To] {
			seen[a.To] = true
			out = append(out, a)
		}
	}

	return out
}
