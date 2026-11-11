package cmd

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/limits"
	"github.com/iamnikolie/zmina/internal/route"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/worktree"
)

// exitNoRoute is the exit code of zmina route when no rule matched: nothing is picked, pick by hand.
const exitNoRoute = 2

// routeAsk is what the work is, as the flags of zmina route and zmina task new say it.
type routeAsk struct {
	repo, project, kind, after, of string
	rereview                       bool
}

// flags adds the flags that say what the work is and are not the task's own (--repo and --project are).
func (r *routeAsk) flags(c *cobra.Command) {
	c.Flags().StringVar(&r.kind, "kind", "", "the kind of work: design, film, code, review ... (a name the routes' kind: rule knows)")
	c.Flags().StringVar(&r.after, "after", "", "the work goes on from this task: the seat that did it, a fresh copy when its context is full")
	c.Flags().StringVar(&r.of, "of", "", "with --kind review: the task under review; its seat never reviews it")
	c.Flags().BoolVar(&r.rereview, "rereview", false, "with --kind review: a review after the first one")
}

func (a *app) routeCommand() *cobra.Command {
	var (
		ask             routeAsk
		explain, asJSON bool
	)

	c := &cobra.Command{
		Use:   "route [<asked...>]",
		Short: "Which seat takes this work, by the crew file's routes (--repo, --project, --kind, --after, --of); exit 2: no rule matched",
		Long: `The crew file's routes: say which seat takes a piece of work: the first rule that matches and yields a seat wins.
The answer is advice, nothing is recorded; zmina task new --route records the task for the seat. The work is what the
flags say, never parsed from words; the asked text only goes into the command lines printed under the answer.

Exit 0 with an answer, 2 when no rule matched (pick by hand) or the crew file has no routes.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				d, err := a.decideRoute(s, ask)
				if err != nil {
					return err
				}

				if asJSON {
					err = a.printJSON(d.JSON(explain))
				} else {
					err = a.writeRoute(d, ask, strings.Join(args, " "), explain)
				}

				if err == nil && !d.Matched() {
					cmd.SilenceErrors = true

					return &exitError{code: exitNoRoute}
				}

				return err
			})
		},
	}

	c.Flags().StringVar(&ask.repo, "repo", "", "the repository the work is in: a directory under the crew's dir, or a path")
	c.Flags().StringVar(&ask.project, "project", "", "the project it is for (default: the repo's directory name)")
	ask.flags(c)
	c.Flags().BoolVar(&explain, "explain", false, "every rule asked, in order, with why it did or did not answer")
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output: to, action, rule, name, why, alternatives, limits, stale (--explain adds the rules asked)")

	return c
}

// decideRoute answers for the work, reading the seats' state and the plans' limits and changing nothing.
func (a *app) decideRoute(s *shift.Shift, ask routeAsk) (route.Decision, error) {
	in, err := a.routeInput(s, ask)
	if err != nil {
		return route.Decision{}, err
	}

	table := s.Crew.RouteTable()
	if len(table.Rules) == 0 {
		// Nothing to ask the seats or the plans.
		return route.Decide(table, in, route.Env{}), nil
	}

	now := s.Now()

	rep, err := limits.Collect(s.Crew.Layout().Limits(), s.Crew.CodexHomes(), s.Crew.LimitsPolicy(), now)
	if err != nil {
		fmt.Fprintln(a.err, "zmina: warning:", err)
	}

	return route.Decide(table, in, route.Env{
		Seats:      a.routeSeats(s, now),
		Limits:     rep,
		Dispatcher: s.Crew.Dispatcher,
		MaxCopy:    crew.MaxInstance,
	}), nil
}

// routeInput checks the flags and turns them into the work the routes look at: the repo as an absolute path, the
// tasks --after and --of as the seats that held them.
func (a *app) routeInput(s *shift.Shift, ask routeAsk) (route.Input, error) {
	in := route.Input{Project: strings.TrimSpace(ask.project), Kind: strings.TrimSpace(ask.kind), Rereview: ask.rereview}

	switch {
	case ask.rereview && in.Kind != route.ReviewKind:
		return in, errors.New("--rereview is for --kind review: a review after the first one")
	case ask.of != "" && in.Kind != route.ReviewKind:
		return in, errors.New("--of is for --kind review: the task under review")
	case in.Kind == route.ReviewKind && ask.of == "":
		return in, errors.New("--kind review needs --of T-n: the seat that did that task may not review it")
	}

	if ask.repo != "" {
		in.Repo = routeRepo(s.Crew, ask.repo)
	}

	for _, ref := range []struct {
		flag, id string
		into     **route.Task
	}{{"--after", ask.after, &in.After}, {"--of", ask.of, &in.Of}} {
		if ref.id == "" {
			continue
		}

		t, err := taskStore(s.Crew).Get(ref.id)
		if err != nil {
			return in, fmt.Errorf("%s: %w", ref.flag, err)
		}

		*ref.into = &route.Task{ID: t.ID, Seat: t.Role}
	}

	return in, nil
}

// routeRepo is a repo as the routes compare it: absolute, clean, links followed, and the top of its git repository
// when it is in one (the directory name a project defaults to).
func routeRepo(c *crew.Crew, repo string) string {
	p := crew.ResolvePath(c.Dir, repo)

	if st, err := os.Stat(p); err == nil && st.IsDir() {
		if top, err := worktree.TopLevel(p); err == nil {
			return crew.ResolvePath("", top)
		}
	}

	return p
}

// routeSeats is every seat of the crew and every live copy, as the routes see them.
func (a *app) routeSeats(s *shift.Shift, now time.Time) []route.Seat {
	views := s.Views()
	seats := make([]route.Seat, 0, len(views))

	for _, v := range views {
		budget := 0
		if r, err := s.Role(v.Role); err == nil {
			budget = r.ContextBudget()
		}

		seats = append(seats, seatFromView(v, budget, now))
	}

	return seats
}

// seatFromView is how a seat stands for the routes: idle (up, between turns, holding no task), busy (up and working
// or waiting on a dialog, or holding an active task), down (no session, holding nothing), or unavailable (asleep,
// broken, outside zmina, its directory gone, not readable).
func seatFromView(v shift.View, budget int, now time.Time) route.Seat {
	s := route.Seat{Name: v.Role, Runtime: v.Runtime, State: route.StateIdle, Tasks: v.Tasks, Context: v.Context, ContextBudget: budget}
	if s.Runtime == "" {
		s.Runtime = "claude"
	}

	busyOn := func() string {
		switch len(v.Tasks) {
		case 0:
			return "busy"
		case 1:
			return "busy on " + v.Tasks[0]
		}

		return "busy on " + strings.Join(v.Tasks, ", ")
	}

	switch {
	case !v.Asleep.IsZero():
		s.State, s.Note = route.StateUnavailable, "asleep until "+clock(v.Asleep, now)
	case v.Broken:
		s.State, s.Note = route.StateUnavailable, "broken: it dies on boot (zmina ls)"
	case v.State == shift.PresenceOutside:
		s.State, s.Note = route.StateUnavailable, "running outside zmina"
	case v.State == shift.PresenceUnknown:
		s.State, s.Note = route.StateUnavailable, "not readable: "+v.Problem
	case v.State == shift.PresenceUp:
		switch {
		case len(v.Tasks) > 0 || v.Status == "busy" || v.Status == "shell" || v.Status == "working":
			s.State, s.Note = route.StateBusy, busyOn()
		case v.Prompt:
			s.State, s.Note = route.StateBusy, "busy: a dialog waits on its screen"
		}
	case len(v.Tasks) > 0:
		// Down, but it holds work: a second task would queue behind it.
		s.State, s.Note = route.StateBusy, "busy: holds "+strings.Join(v.Tasks, ", ")+" while down"
	case v.Problem != "":
		s.State, s.Note = route.StateUnavailable, v.Problem
	default:
		s.State = route.StateDown
	}

	return s
}

// writeRoute prints the answer: with explain the rules asked first, then the answer's line, then the commands that
// carry it out.
func (a *app) writeRoute(d route.Decision, ask routeAsk, asked string, explain bool) error {
	if explain {
		if err := d.WriteExplain(a.out); err != nil {
			return err
		}

		if len(d.Trace) > 0 {
			fmt.Fprintln(a.out)
		}
	}

	fmt.Fprintln(a.out, d.Headline())

	for _, line := range a.routeCommands(d, ask, asked) {
		fmt.Fprintln(a.out, "  "+line)
	}

	return nil
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./~-]+$`)

// shellWord is a word as a shell takes it whole: quoted unless it is plain (dev#2 and ~ are not in zsh).
func shellWord(w string) string {
	if plainWord.MatchString(w) && !strings.HasPrefix(w, "~") && !strings.Contains(w, "#") {
		return w
	}

	return quoteCommandArg(w)
}

// crewArgs are the flags that aim a printed command at the crew this one was aimed at; none for the default crew.
func (a *app) crewArgs() string {
	switch {
	case a.crew != "":
		return " --crew " + shellWord(a.crew)
	case a.config != "":
		return " --config " + shellWord(a.config)
	}

	return ""
}

// routeCommands are the command lines that carry the answer out, ready to run.
func (a *app) routeCommands(d route.Decision, ask routeAsk, asked string) []string {
	text := quoteCommandArg("…")
	if asked != "" {
		text = quoteCommandArg(asked)
	}

	zmina := "zmina" + a.crewArgs()

	switch d.Action {
	case route.ActionTell:
		return []string{fmt.Sprintf("%s tell %s %s", zmina, shellWord(d.To), text)}
	case route.ActionTask, route.ActionUpTask, route.ActionCopy:
	default:
		return nil
	}

	line := fmt.Sprintf("%s task new %s", zmina, shellWord(d.To))

	if ask.repo != "" {
		line += " --repo " + shellWord(ask.repo)
		// Work in a repo goes in its own worktree, unless it goes on from or reviews a task.
		if ask.after == "" && ask.of == "" {
			line += " --worktree"
		}
	}

	if ask.project != "" {
		line += " --project " + shellWord(ask.project)
	}

	line += " --title " + quoteCommandArg("…") + " " + text
	lines := []string{line}

	switch d.Action {
	case route.ActionUpTask:
		lines = append(lines, fmt.Sprintf("%s is down: then  %s", d.To, upLine(zmina, d.To)))
	case route.ActionCopy:
		lines = append(lines, fmt.Sprintf("%s is a new copy: then  %s", d.To, upLine(zmina, d.To)))
	}

	return lines
}

// upLine starts a seat on a task not recorded yet: a copy is started as its role with --as.
func upLine(zmina, seat string) string {
	base, n, ok := shift.SplitInstance(seat)
	if !ok {
		return fmt.Sprintf("%s up %s --task T-<n>", zmina, shellWord(seat))
	}

	return fmt.Sprintf("%s up %s --as %d --task T-<n>", zmina, shellWord(base), n)
}
