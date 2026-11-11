package route

import (
	"fmt"
	"sort"
	"strings"

	"github.com/iamnikolie/zmina/internal/limits"
)

// poolSeat is a seat of the pool with what ranks it.
type poolSeat struct {
	seat  Seat
	idx   int
	used  float64
	known bool
}

// avail ranks how soon a seat can start: idle before down before busy.
func (p poolSeat) avail() int {
	switch p.seat.State {
	case StateIdle:
		return 0
	case StateDown:
		return 1
	}

	return 2
}

// pool is rule 6: the rest of the work goes to the seat of the pool whose plan is the emptier, the plan with the
// lower share used. A plan at its pinned share takes only work that needs it, one at its stop share nothing, so
// their seats are out. When a plan's reading is unknown the plans are not compared: the pool answers by how soon a
// seat can start and by fewest open tasks, and says the limits are unknown.
func (e *engine) pool(r Rule) outcome {
	var in []poolSeat

	var out []Alternative

	var pinned []string

	known := true

	for i, name := range r.Seats {
		s, ok := e.env.seat(name)

		switch {
		case !ok:
			out = append(out, Alternative{To: name, Why: "not a seat of this crew"})

			continue
		case s.State == StateUnavailable:
			out = append(out, Alternative{To: name, Why: s.why()})

			continue
		}

		used, ok := e.env.used(s.Runtime)
		if ok {
			if b := e.env.band(s.Runtime, used); b != limits.Open {
				why := planText(s.Runtime, used) + bandText(b)
				out = append(out, Alternative{To: name, Why: why})
				pinned = append(pinned, why)

				continue
			}
		}

		known = known && ok

		in = append(in, poolSeat{seat: s, idx: i, used: used, known: ok})
	}

	if len(in) == 0 {
		why := "no seat of the pool can take work now"
		if len(out) > 0 {
			parts := make([]string, len(out))
			for i, a := range out {
				parts[i] = a.To + ": " + a.Why
			}

			why += " (" + strings.Join(parts, "; ") + ")"
		}

		return outcome{seen: true, why: why, alts: out}
	}

	// The plans are compared only when every plan of an eligible seat is known, and there is more than one.
	compare := known && planCount(in) > 1

	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]

		switch {
		case compare && a.used != b.used:
			return a.used < b.used
		case a.avail() != b.avail():
			return a.avail() < b.avail()
		case len(a.seat.Tasks) != len(b.seat.Tasks):
			return len(a.seat.Tasks) < len(b.seat.Tasks)
		}

		return a.idx < b.idx
	})

	win := in[0]

	var why string

	switch {
	case compare:
		why = e.versus(win, in)
	case known:
		why = planText(win.seat.Runtime, win.used)
	default:
		why = "limits unknown: fewest open tasks"
	}

	why = join(why, strings.Join(dedupe(pinned), "; "))

	// Seats listed before the pick that it passed over because they are busy.
	var passed []string

	for _, p := range in[1:] {
		if p.idx < win.idx && p.seat.Runtime == win.seat.Runtime && p.seat.State == StateBusy && len(passed) < 2 {
			passed = append(passed, p.seat.Name+" is "+p.seat.why())
		}
	}

	why = join(why, strings.Join(passed, "; "))

	var alts []Alternative

	for _, p := range in[1:] {
		reason := p.seat.why()
		if compare && p.seat.Runtime != win.seat.Runtime {
			reason = planText(p.seat.Runtime, p.used)
		}

		alts = append(alts, Alternative{To: p.seat.Name, Why: reason})
	}

	return outcome{seat: &win.seat, why: why, alts: append(alts, out...)}
}

// planCount counts the different plans the seats bill.
func planCount(in []poolSeat) int {
	seen := map[string]bool{}
	for _, p := range in {
		seen[p.seat.Runtime] = true
	}

	return len(seen)
}

// versus says how the winner's plan compares with the best other plan in the pool: claude 7d 41% < codex week 50%.
func (e *engine) versus(win poolSeat, ranked []poolSeat) string {
	for _, p := range ranked {
		if p.seat.Runtime == win.seat.Runtime {
			continue
		}

		op := "<"
		if p.used == win.used {
			op = "="
		}

		return fmt.Sprintf("%s %s %s", planText(win.seat.Runtime, win.used), op, planText(p.seat.Runtime, p.used))
	}

	return planText(win.seat.Runtime, win.used)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}

	var out []string

	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}

	return out
}

const (
	bandPinned  = " is pinned work only"
	bandStopped = " is stopped"
)

// bandText says why a plan's seats are out of the pool.
func bandText(b limits.Band) string {
	if b == limits.Stopped {
		return bandStopped
	}

	return bandPinned
}
