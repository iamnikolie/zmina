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

	if useRoute {
		switch {
		case !d.Matched():
			return p, fmt.Errorf("%s (name the role: zmina task new <role> <asked...>)", d.Headline())
		case d.Action == route.ActionTell:
			return p, fmt.Errorf("the routes say %s: that is another crew's work, not a seat of this one: zmina tell %s %s",
				d.Headline(), shellWord(d.To), quoteCommandArg(p.asked))
		}

		p.role = d.To

		return p, nil
	}

	if s.Crew.RouteOverrideRequired() && d.Matched() && !d.Agrees(p.role) && p.because == "" {
		return p, fmt.Errorf("the routes say %s (rule %d %s: %s); %s instead needs --because \"<why>\" (route_override: required)",
			d.To, d.Rule, d.Name, d.Why, p.role)
	}

	return p, nil
}

// record is what the task keeps of the routes' answer: nothing when the crew has no routes or no rule matched.
func (p rolePick) record() *task.Route {
	if !p.routed || !p.d.Matched() {
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
