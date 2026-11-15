package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/route"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

// rolePick is the role a new task goes to and how that was decided: by the routes (--route), or by hand, with what
// the routes said beside it when the crew file has routes.
type rolePick struct {
	role, asked string
	// routed is true when the crew file has routes; d is then what they said.
	routed bool
	d      route.Decision
	// byRoute is true when the routes picked the role (--route); because is why a role picked by hand is not theirs.
	byRoute bool
	because string
	// outside is true when the hand picked a seat outside the pool, said no kind of work, and only the pool
	// answered: the pool shares the work among its own seats and has no word on a specialist. own is the rule that
	// names the seat (a project's, a kind's, the first review's), empty when none does.
	outside bool
	own     string
}

// pickRole decides the role of a new task from its arguments: --route leaves the role out and the routes pick it;
// otherwise the first argument is the role, and when the crew file has routes they are asked all the same, so the
// event log shows where the table and the hand disagree. Without routes nothing is asked.
func (a *app) pickRole(s *shift.Shift, ask routeAsk, useRoute bool, because string, args []string) (rolePick, error) {
	p := rolePick{byRoute: useRoute, because: strings.TrimSpace(because)}

	if useRoute {
		p.asked = strings.Join(args, " ")
	} else {
		p.role, p.asked = args[0], strings.Join(args[1:], " ")
	}

	p.routed = len(s.Crew.RouteTable().Rules) > 0

	switch {
	case useRoute && p.because != "":
		return p, errors.New("--because says why a role was picked by hand; with --route the routes pick it")
	case useRoute && !p.routed:
		return p, errors.New("the crew file has no routes: name the role, zmina task new <role> <asked...>")
	case !p.routed:
		return p, nil
	}

	d, err := a.decideRoute(s, ask)
	if err != nil {
		return p, err
	}

	p.d = d

	if !useRoute && ask.kind == "" {
		p.outside, p.own = outsidePool(s.Crew.RouteTable(), d, p.role)
	}

	if useRoute {
		switch {
		case !d.Matched():
			return p, fmt.Errorf("%s (name the role: zmina task new <role> <asked...>)", d.Headline())
		case d.Action == route.ActionTell:
			why := "that is another crew's work, not a seat of this one"
			if d.AwaySeat != "" {
				why = d.AwaySeat + " runs on another node, not on this one"
			}

			return p, &routeTellError{d: d, msg: fmt.Sprintf("the routes say %s: %s: zmina tell %s %s", d.Headline(), why,
				shellWord(d.To), quoteCommandArg(p.asked))}
		}

		p.role = d.To

		return p, nil
	}

	if s.Crew.RouteOverrideRequired() && d.Matched() && !d.Agrees(p.role) && !p.outside && p.because == "" {
		return p, fmt.Errorf("the routes say %s (rule %d %s: %s); %s instead needs --because \"<why>\" (route_override: required)",
			d.To, d.Rule, d.Name, d.Why, p.role)
	}

	return p, nil
}

// outsidePool reports whether the pool's answer d has no word on role: the pool is what answered and role is not one
// of its seats (a copy is its role). own is how the table names the seat elsewhere: the project, kind or review rule
// it stands in, so the reader sees which flag would have asked that rule.
func outsidePool(t route.Table, d route.Decision, role string) (outside bool, own string) {
	if !d.Matched() || d.Rule != route.Pool.Number() || d.Agrees(role) {
		return false, ""
	}

	base := route.Base(role)

	for _, r := range t.Rules {
		switch r.Kind {
		case route.Pool:
			for _, seat := range r.Seats {
				if seat == base {
					return false, ""
				}
			}
		case route.Project, route.WorkKind:
			for name, seat := range r.Owners {
				if seat == base && (own == "" || name < ownName(own)) {
					own = fmt.Sprintf("rule %d %s %s", r.Kind.Number(), r.Kind.Label(), name)
				}
			}
		case route.Review:
			if r.First == base && own == "" {
				own = fmt.Sprintf("rule %d %s", r.Kind.Number(), r.Kind.Label())
			}
		}
	}

	return true, own
}

// ownName is the name a rule line of outsidePool ends on.
func ownName(own string) string { return own[strings.LastIndex(own, " ")+1:] }

// outsideText says why a hand pick outside the pool is not held against the pool's answer.
func (p rolePick) outsideText() string {
	if p.own != "" {
		return fmt.Sprintf("%s is %s's seat; the kind of work was not said, so the pool has no word on it", p.role, p.own)
	}

	return p.role + " is outside the pool and no rule names it; the pool has no word on it"
}

// record is what the task keeps of the routes' answer: nothing when the crew has no routes, no rule matched, or the
// hand picked a seat the pool has no word on.
func (p rolePick) record() *task.Route {
	if !p.routed || !p.d.Matched() || p.outside {
		return nil
	}

	r := &task.Route{Rule: p.d.Rule, Name: p.d.Name, Why: p.d.Why}
	if !p.d.Agrees(p.role) {
		r.Said, r.Override = p.d.To, p.because
	}

	return r
}

// said is what task new prints beside the role: the rule that picked it, or where the hand pick left the routes'.
func (p rolePick) said() string {
	switch {
	case !p.routed || !p.d.Matched():
		return ""
	case p.outside:
		return p.outsideText()
	case p.byRoute:
		return fmt.Sprintf("rule %d: %s", p.d.Rule, p.d.Why)
	case p.d.Agrees(p.role):
		return fmt.Sprintf("rule %d, as the routes say", p.d.Rule)
	}

	out := fmt.Sprintf("route said %s, rule %d", p.d.To, p.d.Rule)
	if p.because != "" {
		out += "; override: " + p.because
	}

	return out
}

// log records the route event of a task made while the crew has routes: where it went, what the routes said, and
// why it differs when it does. Every task made so gets one, picked by the routes or by hand.
func (p rolePick) log(s *shift.Shift, t task.Task) {
	if !p.routed {
		return
	}

	e := events.Event{Role: t.Role, Event: "route", Task: t.ID, Picked: t.Role, Because: p.because}

	switch {
	case !p.d.Matched():
		e.Detail = "no rule matched; picked " + t.Role
	case p.outside:
		e.Detail = "picked by hand: " + p.outsideText()
	case p.d.Agrees(t.Role):
		rule := p.d.Rule
		e.Rule, e.RouteSaid = &rule, p.d.To
		e.Detail = fmt.Sprintf("rule %d %s: %s, as the routes say", p.d.Rule, p.d.Name, t.Role)
	default:
		rule := p.d.Rule
		e.Rule, e.RouteSaid = &rule, p.d.To
		e.Detail = fmt.Sprintf("route said %s (rule %d %s); picked %s", p.d.To, p.d.Rule, p.d.Name, t.Role)
	}

	if p.because != "" && e.RouteSaid != t.Role {
		e.Detail += "; because: " + p.because
	}

	s.LogEvent(e)
}

// routeLine is what a task keeps of the routes' answer, in a line: the rule and why, and where the hand pick left it.
func routeLine(r *task.Route) string {
	out := fmt.Sprintf("rule %d", r.Rule)
	if r.Name != "" {
		out += " " + r.Name
	}

	if r.Why != "" {
		out += ": " + r.Why
	}

	switch {
	case r.Said != "" && r.Override != "":
		return fmt.Sprintf("the routes said %s (%s); override: %s", r.Said, out, r.Override)
	case r.Said != "":
		return fmt.Sprintf("the routes said %s (%s); picked by hand, no reason given", r.Said, out)
	}

	return out
}

// routeTellError is the refusal of --route when the routes answer tell: the work is another crew's, or a seat that runs on another
// node. It keeps the answer, so a caller that is no hand (the review loop) can word it by what it was.
type routeTellError struct {
	d   route.Decision
	msg string
}

func (e *routeTellError) Error() string { return e.msg }
