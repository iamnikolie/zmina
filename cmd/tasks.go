package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/donecheck"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/ledger"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/tmux"
)

func (a *app) taskCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "task",
		Short: "The work ledger: who holds what, kept outside any session's memory",
	}

	c.AddCommand(a.taskNewCommand(), a.taskLsCommand(), a.taskShowCommand(), a.taskSetCommand(),
		a.taskCloseCommand("done", task.Done), a.taskCloseCommand("drop", task.Dropped), a.taskParkCommand(),
		a.taskGCCommand(), a.taskRelocateCommand(), a.taskHandoffCommand(), a.taskRecordCommand("result"), a.taskRecordCommand("checkpoint"), a.taskAcceptCommand())

	return c
}

func (a *app) taskNewCommand() *cobra.Command {
	var (
		n       ledger.NewTask
		ask     routeAsk
		asJSON  bool
		routed  bool
		because string
		check   checkFlags
	)

	c := &cobra.Command{
		Use:   "new [<role>] <asked...>",
		Short: "Record a task handed to a role (--repo, --worktree: its own git worktree and branch; --route: the routes pick the role)",
		Args: func(cmd *cobra.Command, args []string) error {
			switch {
			case routed && len(args) < 1:
				return errors.New("--route picks the role: zmina task new --route [flags] <asked...>")
			case !routed && len(args) < 2:
				return fmt.Errorf("requires at least 2 arg(s), only received %d", len(args))
			}

			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, cleared, err := check.spec(cmd)
			if err != nil {
				return err
			}

			if cleared {
				return errors.New("a new task has no check to clear")
			}

			n.Check = spec

			return a.withState(func(s *shift.Shift) error {
				ask.repo, ask.project = n.Repo, n.Project

				pick, err := a.pickRole(s, ask, routed, because, args)
				if err != nil {
					return err
				}

				t, pane, err := a.openTask(s, pick, n)
				if err != nil {
					return err
				}

				if pane.State == tmux.Up {
					if t, err = a.handOut(s, t); err != nil {
						return err
					}
				}

				if asJSON {
					return a.printJSON(t)
				}

				fmt.Fprintf(a.out, "%s · %s", t.ID, t.Role)

				if said := pick.said(); said != "" {
					fmt.Fprintf(a.out, " (%s)", said)
				}

				if t.Worktree != "" {
					fmt.Fprintf(a.out, " · worktree %s (branch %s from %s)", t.Worktree, t.Branch, t.Base)
				} else if t.Repo != "" {
					fmt.Fprintf(a.out, " · repo %s", t.Repo)
				}

				fmt.Fprintln(a.out)

				// Recorded for a role that is not up: nothing starts it, so say how.
				if pane.State != tmux.Up {
					fmt.Fprintf(a.err, "zmina: %s is down: %s starts it on the task\n", t.Role, a.upTaskLine(s, t))
				}

				return nil
			})
		},
	}

	c.Flags().StringVar(&n.Repo, "repo", "", "repository: a directory under the crew's dir, or a path")
	c.Flags().BoolVar(&n.Worktree, "worktree", false, "give the task its own git worktree and branch zmina/<id>")
	c.Flags().StringVar(&n.Base, "base", "", "branch or commit the worktree starts from (default: the repo's "+
		"current branch)")
	c.Flags().StringVar(&n.Note, "note", "", "a note")
	c.Flags().StringVar(&n.Title, "title", "", "the work in one short plain sentence, in the owner's language")
	c.Flags().StringVar(&n.Project, "project", "", "the project or product it is for (default: the repo's dir name)")
	c.Flags().StringVar(&n.URL, "url", "", "the live thing to look at")
	c.Flags().Float64Var(&n.Budget, "budget", 0, "what the task may cost, API-equivalent dollars: past it the "+
		"ticker tells the dispatcher")
	c.Flags().BoolVar(&routed, "route", false, "the crew file's routes pick the role (leave <role> out): see zmina route")
	c.Flags().StringVar(&because, "because", "", "why this role, when it is not the one the routes name (a role picked by hand)")
	c.Flags().StringArrayVar(&n.Criteria, "criteria", nil, "what must be true for the task to count as finished, one line; "+
		"repeat the flag for each criterion (the handoff packet carries them)")
	check.flags(c)
	ask.flags(c)
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	return c
}

// openTask records a task for the role picked, as task new does: the role must exist and tmux must answer about it, the
// task is made and, when the crew has routes, the route event is logged. n names the rest of the task; its role, asked
// text, author and route are the pick's. The task exists on the ledger even when the error says something went wrong after
// it was made (a worktree that could not be made: its id is in the error and the task, dropped, is on the ledger).
func (a *app) openTask(s *shift.Shift, pick rolePick, n ledger.NewTask) (task.Task, tmux.Pane, error) {
	if _, err := s.Role(pick.role); err != nil {
		return task.Task{}, tmux.Pane{}, err
	}

	pane, err := s.Tmux.Probe(pick.role)
	if err != nil {
		return task.Task{}, pane, err
	}

	n.Role, n.Asked, n.By, n.Route = pick.role, pick.asked, s.By, pick.record()

	t, err := a.ledger(s).New(n)
	if t.ID != "" {
		pick.log(s, t)
	}

	return t, pane, err
}

// handOut gives a task recorded for a role that is up its handoff packet: the line is queued, and goes now when the role can
// take it, else with the beat. It returns the task as the hand-out left it.
func (a *app) handOut(s *shift.Shift, t task.Task) (task.Task, error) {
	if _, err := s.HandTask(t.Role, t); err != nil {
		return t, fmt.Errorf("%s recorded, but handoff failed: %w", t.ID, err)
	}

	s.DeliverTo(t.Role)

	return s.Tasks.Get(t.ID)
}

// checkFlags is the flags that give a task its check: --check, with --check-timeout, --check-commit and --check-env-rc.
type checkFlags struct {
	run, timeout string
	commit       bool
	envRC        int
}

func (f *checkFlags) flags(c *cobra.Command) {
	c.Flags().StringVar(&f.run, "check", "", "the command closing the task as done runs in its worktree, and it closes "+
		"only if it exits 0 (the owner and the dispatcher set it; needs a worktree; with set, - clears it)")
	c.Flags().StringVar(&f.timeout, "check-timeout", "", "how long the check may run (default 10m, at most 30m)")
	c.Flags().BoolVar(&f.commit, "check-commit", false, "the check also wants a commit on the branch beyond its base")
	c.Flags().IntVar(&f.envRC, "check-env-rc", 0, "an exit status (3-255; 1 and 2 mean a failed check) the check uses to say it could "+
		"not run (a tool or service missing): an environment fault, not a failed check; the shell's 126 and 127 always are")
}

// spec is the check the flags give: nil when --check was not given, cleared when it was given as - (set only).
func (f *checkFlags) spec(cmd *cobra.Command) (spec *donecheck.Spec, cleared bool, err error) {
	given := cmd.Flags().Changed("check")
	extra := cmd.Flags().Changed("check-timeout") || cmd.Flags().Changed("check-commit") || cmd.Flags().Changed("check-env-rc")

	switch {
	case !given && !extra:
		return nil, false, nil
	case !given:
		return nil, false, errors.New("--check-timeout, --check-commit and --check-env-rc go with --check")
	case f.run == "-" && extra:
		return nil, false, errors.New("--check - clears the check: --check-timeout, --check-commit and --check-env-rc have " +
			"nothing to go with")
	case cmd.Flags().Changed("check-env-rc") && f.envRC == 0:
		return nil, false, errors.New("--check-env-rc 0: an exit status from 3 to 255 (leave the flag out for none)")
	case cmd.Flags().Changed("check-env-rc") && donecheck.CheckEnvFaultRC(f.envRC) != nil:
		return nil, false, fmt.Errorf("--check-env-rc: %w", donecheck.CheckEnvFaultRC(f.envRC))
	case f.run == "-":
		return nil, true, nil
	}

	return &donecheck.Spec{Run: f.run, Timeout: f.timeout, Commit: f.commit, EnvFaultRC: f.envRC}, false, nil
}

// upTaskLine is the command that starts a role on a task recorded for it: a copy (dev#2) is started as its role.
func (a *app) upTaskLine(s *shift.Shift, t task.Task) string {
	crewArg := "zmina --crew " + quoteCommandArg(s.Crew.Path)

	if base, num, ok := shift.SplitInstance(t.Role); ok {
		return fmt.Sprintf("%s up %s --as %d --task %s", crewArg, quoteCommandArg(base), num, t.ID)
	}

	return fmt.Sprintf("%s up %s --task %s", crewArg, quoteCommandArg(t.Role), t.ID)
}

func (a *app) taskLsCommand() *cobra.Command {
	var (
		all    bool
		role   string
		asJSON bool
	)

	c := &cobra.Command{
		Use:   "ls [<node>/<crew>]",
		Short: "Active tasks (open, waiting); --all also the closed ones (done, dropped, parked); <node>/<crew>: another crew's",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			done, args, err := a.remote(cmd, args, false)
			if done || err != nil {
				return err
			}

			if len(args) != 0 {
				return fmt.Errorf("unknown argument %q: task ls takes only a <node>/<crew> address", args[0])
			}

			return a.readState(func(s *shift.Shift) error {
				a.warnStanding(s)

				tasks, problems, err := taskStore(s.Crew).Scan()
				if err != nil {
					return err
				}

				for _, p := range problems {
					fmt.Fprintln(a.err, "zmina: warning:", p)
				}

				out := []task.Task{}

				for _, t := range tasks {
					if (all || t.State.Active()) && (role == "" || t.Role == role) {
						out = append(out, t)
					}
				}

				if asJSON {
					return a.printJSON(out)
				}

				if len(out) == 0 {
					fmt.Fprintln(a.out, "no tasks")

					return nil
				}

				w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "ID\tROLE\tSTATE\tAGE\tPROJECT\tWORK")

				for _, t := range out {
					status := t.Status()
					if t.Closed != nil && t.Closed.Unchecked != nil {
						status += " (unchecked)"
					}

					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Role, status, age(s.Now().Sub(t.Created)),
						dash(t.Where()), clip(t.Headline(), 70))
				}

				return w.Flush()
			})
		},
	}

	c.Flags().BoolVar(&all, "all", false, "also done, dropped and parked tasks")
	c.Flags().StringVar(&role, "role", "", "only this role's tasks")
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	return c
}

func (a *app) taskShowCommand() *cobra.Command {
	var asJSON, full bool

	c := &cobra.Command{
		Use:   "show [<node>/<crew>] <id>",
		Short: "One task: what was asked (clipped), its note, result and the last events (--full: all of it); <node>/<crew>: another crew's",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			done, args, err := a.remote(cmd, args, false)
			if done || err != nil {
				return err
			}

			if len(args) != 1 {
				return fmt.Errorf("task show takes one task, or <node>/<crew> and one task; got %d arguments", len(args))
			}

			return a.readState(func(s *shift.Shift) error {
				a.warnStanding(s)

				t, err := taskStore(s.Crew).Get(args[0])
				if err != nil {
					return err
				}

				// Showing a task is reading its note, for the rotation that handed it to a fresh conversation.
				noteFetched(s, t)

				if asJSON {
					return a.printJSON(t)
				}

				fmt.Fprintf(a.out, "%s · %s · %s · by %s · %s\n", t.ID, t.Role, t.Status(), dash(t.By),
					t.Created.Local().Format(time.DateTime))
				if t.Title != "" {
					fmt.Fprintln(a.out, "title:", t.Title)
				}

				if full {
					fmt.Fprintln(a.out, "asked:", t.Asked)
				} else {
					asked := clip(oneLine(t.Asked), 300)
					if asked != t.Asked && len([]rune(asked)) < len([]rune(t.Asked)) {
						asked += fmt.Sprintf(" (+%d chars, --full)", len([]rune(oneLine(t.Asked)))-len([]rune(asked))+1)
					}
					fmt.Fprintln(a.out, "asked:", asked)
				}

				if len(t.Criteria) > 0 {
					fmt.Fprintln(a.out, "criteria:")

					for i, c := range t.Criteria {
						fmt.Fprintf(a.out, "  %d. %s\n", i+1, c)
					}
				}

				if p := t.Where(); p != "" {
					fmt.Fprintln(a.out, "project:", p)
				}

				if t.URL != "" {
					fmt.Fprintln(a.out, "url:", t.URL)
				}

				if t.Repo != "" {
					fmt.Fprintln(a.out, "repo:", t.Repo)
				}

				if t.Route != nil {
					fmt.Fprintln(a.out, "route:", routeLine(t.Route))
				}

				if t.Worktree != "" {
					fmt.Fprintf(a.out, "worktree: %s (branch %s from %s)\n", t.Worktree, t.Branch, t.Base)
				}

				a.printCheck(s, t, full)

				if t.Note != "" {
					note := t.Note
					if !full {
						note = clip(oneLine(note), 300)
					}
					fmt.Fprintln(a.out, "note:", note)
				}

				if t.Delivery != nil {
					fmt.Fprintf(a.out, "handoff: %s · %s", t.Delivery.ID, t.Delivery.Status())

					if t.Delivery.Gen > 0 {
						fmt.Fprintf(a.out, " · hand-out %d to %s", t.Delivery.Gen, t.Delivery.Role)
					}

					fmt.Fprintln(a.out)
				}

				a.printClosedGen(t)
				a.printLate(t, full)

				view := recordLine
				if full {
					view = recordFull
				}
				if t.Checkpoint != nil {
					if err := a.printTaskRecord(s, t, "checkpoint", t.Checkpoint.Revision, view); err != nil {
						return err
					}
				}
				if t.HandoffIncomplete {
					fmt.Fprintln(a.out, "handoff: incomplete; checkpoint missing, stale or unavailable")
				}
				if t.Result != nil {
					if err := a.printTaskRecord(s, t, "result", t.Result.Revision, view); err != nil {
						return err
					}
				}

				fmt.Fprintln(a.out, a.taskCost(t, s))

				last := 20
				if !full {
					last = 8
				}
				evs, _ := eventLog(s.Crew).Read(events.Filter{Task: t.ID, Last: last})
				for _, e := range evs {
					detail := e.Detail
					if !full {
						detail = oneLine(detail)
					}
					line := fmt.Sprintf("  %s  %s  %s  %s", e.TS.Local().Format(time.DateTime), e.By, e.Event, detail)
					if !full {
						line = clip(line, 160)
					}
					fmt.Fprintln(a.out, line)
				}
				if !full && (t.Result != nil || t.Checkpoint != nil || len(evs) == last) {
					fmt.Fprintf(a.out, "more: zmina task show %s --full", t.ID)
					if t.Result != nil {
						fmt.Fprintf(a.out, " · zmina task result %s", t.ID)
					}
					fmt.Fprintln(a.out)
				}

				return nil
			})
		},
	}

	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	c.Flags().BoolVar(&full, "full", false, "the asked text, result, checkpoint and the last 20 events whole")

	return c
}

// printCheck says what closing the task as done runs, for a task still active, and how a closed one was checked: what ran
// and at which commit, or that it was closed past its check. The tail of the output goes with --full. A check says that
// its command exited 0 at that commit, which is all it says.
func (a *app) printCheck(s *shift.Shift, t task.Task, full bool) {
	if spec, source, ok := a.ledger(s).CheckFor(t); ok && t.State.Active() {
		fmt.Fprintf(a.out, "check: %s · %s's, run by zmina task done; it passing says the command exited 0, not that the work is right\n",
			spec, source)
	}

	if len(t.Sweep) > 0 && t.State.Active() {
		items, _, _ := laterStore(s.Crew).Scan()
		fmt.Fprintf(a.out, "check: zmina later swept %s · closing it as done waits on it; %d of %d parked items have no verdict\n",
			t.ID, len(later.Unswept(items, t.Sweep, t.ID)), len(t.Sweep))
	}

	if c := t.CheckEnv; c != nil && t.State.Active() {
		fmt.Fprintf(a.out, "check could not run: %s · %s at %s · %s · the environment is broken, the work was not judged\n",
			donecheck.Line(c.Run), donecheck.Line(c.Fault()), donecheck.Line(c.Commit), c.At.Local().Format(time.DateTime))

		if full && c.Output != "" {
			fmt.Fprintln(a.out, "  "+strings.ReplaceAll(donecheck.Clean(c.Tail()), "\n", "\n  "))
		}
	}

	if t.Closed == nil {
		return
	}

	// What a file says of a command or an output is shown as plain text: a file edited by hand may hold more.
	if c := t.Closed.Check; c != nil {
		fmt.Fprintf(a.out, "checked: %s · exit %d in %s at %s · %s · %s's check\n", donecheck.Line(c.Run), c.Exit,
			c.Duration().Round(time.Millisecond), donecheck.Line(c.Commit), c.At.Local().Format(time.DateTime), donecheck.Line(c.Source))

		if full && c.Output != "" {
			fmt.Fprintln(a.out, "  "+strings.ReplaceAll(donecheck.Clean(c.Tail()), "\n", "\n  "))
		}
	}

	if u := t.Closed.Unchecked; u != nil {
		fmt.Fprintf(a.out, "unchecked: closed by %s past its check (%s), which was not run\n", donecheck.Line(u.By), donecheck.Line(u.Run))
	}
}

// taskChange is the flags of set, done and drop.
type taskChange struct {
	note, role, on      string
	title, project, url string
	budget              string
	// handoff is the packet --handoff names: the hand-out the caller reports for.
	handoff string
	force   bool
	// criteria is the values of --criteria; setCriteria the list they make when the flag was given (set only).
	criteria    []string
	setCriteria *[]string
	// check is the check --check gave, clearCheck says --check - (set only).
	check      *donecheck.Spec
	clearCheck bool
}

func (o *taskChange) flags(c *cobra.Command, withRole bool) {
	handoffFlag(c, &o.handoff)
	c.Flags().StringVar(&o.note, "note", "", "a note on the task")
	c.Flags().BoolVar(&o.force, "force", false, "close it although its worktree has uncommitted changes, or (done) "+
		"it has criteria and no result, or (the owner and the dispatcher) its check was not run or failed: it is then "+
		"recorded as unchecked")

	if withRole {
		c.Flags().StringVar(&o.role, "role", "", "hand the task to another role")
		c.Flags().StringVar(&o.title, "title", "", "the work in one short plain sentence (- clears it)")
		c.Flags().StringVar(&o.project, "project", "", "the project or product it is for (- clears it)")
		c.Flags().StringVar(&o.url, "url", "", "the live thing to look at (- clears it)")
		c.Flags().StringVar(&o.budget, "budget", "", "what the task may cost, API-equivalent dollars (- clears it)")
		c.Flags().StringArrayVar(&o.criteria, "criteria", nil, "what must be true for the task to count as finished, "+
			"one line, the flag repeated for each; it replaces the list (an empty value alone clears it)")
		c.Flags().StringVar(&o.on, "on", "", "with waiting: who it waits on when not the owner — a role or a person "+
			"(- the owner again); it stays off the owner's todo")
	}
}

func (a *app) taskSetCommand() *cobra.Command {
	var (
		o     taskChange
		check checkFlags
	)

	c := &cobra.Command{
		Use: "set <id> [state]",
		Short: "Change a task: state (open, waiting [--on who], done, dropped), --note, --role, --title, --project, " +
			"--url, --budget, --criteria, --check",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var st task.State

			var err error
			if o.check, o.clearCheck, err = check.spec(cmd); err != nil {
				return err
			}

			if cmd.Flags().Changed("criteria") {
				list, err := replacementCriteria(o.criteria)
				if err != nil {
					return err
				}

				o.setCriteria = &list
			}

			if len(args) == 2 {
				if st, err = task.ParseState(args[1]); err != nil {
					return err
				}
			}

			return a.setTask(args[0], st, o)
		},
	}

	o.flags(c, true)
	check.flags(c)

	return c
}

func (a *app) taskCloseCommand(use string, st task.State) *cobra.Command {
	var o taskChange
	var resultFile string

	c := &cobra.Command{
		Use:   use + " <id>",
		Short: "Close a task as " + string(st) + " (refuses " + closeRefuses(st) + " without --force)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if resultFile != "" {
				if err := a.saveRecordFile(args[0], "result", resultFile, o.handoff, cmd); err != nil {
					return err
				}
			}
			return a.setTask(args[0], st, o)
		},
	}

	if st == task.Done {
		c.Flags().StringVar(&resultFile, "result", "", "save a role-shaped JSON result before closing (file or -)")
	}

	o.flags(c, false)

	return c
}

// closeRefuses is what closing as st refuses without --force, for the command's short text.
func closeRefuses(st task.State) string {
	if st == task.Done {
		return "a worktree with uncommitted changes, a task with criteria and no saved result, and a task whose check fails,"
	}

	return "a worktree with uncommitted changes"
}

// replacementCriteria is the list a task's criteria are replaced with by the values of a repeated --criteria: an
// empty value alone clears the list.
func replacementCriteria(values []string) ([]string, error) {
	if len(values) == 1 && strings.TrimSpace(values[0]) == "" {
		return []string{}, nil
	}

	list, err := task.CleanCriteria(values)
	if err != nil {
		return nil, fmt.Errorf("%w (an empty --criteria alone clears the list)", err)
	}

	return list, nil
}

func (a *app) taskParkCommand() *cobra.Command {
	var note, handoff string

	c := &cobra.Command{
		Use:   "park <id>",
		Short: "Move an active task to the later list with what remains (--note); it closes as parked L-n",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var parked task.Task

			err := a.withState(func(s *shift.Shift) error {
				t, err := taskStore(s.Crew).Get(args[0])
				if err != nil {
					return err
				}

				// A session that does not hold the task does not park it.
				t, st, err := a.hold(s, t, handoff, true)
				if err != nil {
					return err
				}

				if st.Late {
					return a.lateReport(s, t, "park", "", st)
				}

				it, err := s.Park(t, note)
				if err != nil {
					return err
				}

				fmt.Fprintf(a.out, "%s · %s · parked %s\n", t.ID, t.Role, it.ID)
				parked = t

				return nil
			})
			if err != nil {
				return err
			}

			a.closedCost(parked)

			return nil
		},
	}

	c.Flags().StringVar(&note, "note", "", "what remains, in one or two lines (default: the task's title)")
	handoffFlag(c, &handoff)

	return c
}

// exitCheckFailed is the exit code of a task done that its check refused: the command failed or timed out, or the
// commit it asks for is not there.
const exitCheckFailed = 4

// exitCheckEnv is the exit code of a task done whose check could not run: the environment is broken (the shell's 126 or
// 127, a shell that did not start, or the check's env_fault_rc), so the work was not judged and the task is open, with no
// failed check recorded.
const exitCheckEnv = 6

// checkSignals are the signals that end a check along with the command that runs it. A check has a session and process
// group of its own, which none of these reaches by itself: a terminal's interrupt, a pane that is killed (hangup), a
// service stopped or a quit.
var checkSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}

// doneCheck runs the check that closing task id as done needs, before the close takes the state lock, and holds no lock
// while it runs: it can take minutes, and the ticker and every other command wait for that lock. It first refuses what
// the close would refuse anyway (a change that is no good, a task already done aside), reads the task and its worktree,
// runs the command, and hands the result to the close, which writes it only if the task, its commit and its check are
// still the ones it ran against. nil: nothing to run (not a close as done, no check, a task that is done, or --force).
func (a *app) doneCheck(id string, st task.State, o taskChange) (*ledger.Checked, error) {
	if st != task.Done || o.force {
		return nil, nil //nolint:nilnil // no check to run is not an error
	}

	// The crew file and the state are read here without the lock; the crew file's warnings are said once, by the close. The
	// state names the role's live conversation, which a delivery not yet submitted to one is held by.
	quiet := *a
	quiet.err = io.Discard

	var (
		l    *ledger.Ledger
		plan *ledger.CheckPlan
	)

	err := quiet.readState(func(s *shift.Shift) error {
		if o.role != "" {
			if _, err := s.Role(o.role); err != nil {
				return err
			}
		}

		l = a.ledger(s)

		var err error

		// A session that does not hold the task is refused here, before anything runs for it.
		plan, err = l.PlanCheck(id, a.changeOf(s, st, o))

		return err
	})
	if err != nil || plan == nil {
		return nil, checkExit(err)
	}

	fmt.Fprintf(a.err, "zmina: %s: running its check: %s\n", plan.Task.ID, plan.Spec)

	ctx, stop := signal.NotifyContext(context.Background(), checkSignals...)
	defer stop()

	checked, err := l.RunCheck(ctx, plan)

	return checked, checkExit(err)
}

// checkEnvFault keeps a check that could not run on its task and queues a note to the dispatcher once for each task and cause
// (shift.RecordCheckEnv), then ends the command with exitCheckEnv: the task stays open and nothing is recorded as a failed
// check. The dispatcher is said to have been told only when the note was queued and the state that holds it was saved; the
// cause is marked told after that, so a note or a save that failed is tried again by the next run of the check. The error is
// partial: the state, which holds the note, is saved.
func (a *app) checkEnvFault(f *ledger.EnvFault) error {
	var tell shift.CheckEnvTell

	msg := f.Error()

	err := a.withState(func(s *shift.Shift) error {
		t, err := s.Tasks.Get(f.ID)
		if err != nil {
			return err
		}

		tell, err = s.RecordCheckEnv(t, by(), f.Result)

		return err
	})
	if err != nil {
		return &exitError{code: exitCheckEnv, msg: msg + "\nthe fault could not be kept on the task, and the dispatcher was not told: " + err.Error()}
	}

	switch {
	case tell.Err != nil:
		msg += "\nthe dispatcher could not be told: " + tell.Err.Error()
	case tell.Already:
		msg += "\nthe dispatcher was told of this cause already"
	case tell.Queued:
		if err := a.withState(func(s *shift.Shift) error { return s.MarkCheckEnvTold(f.ID, f.Result) }); err != nil {
			msg += "\nthe dispatcher has been told (zmina could not note it on the task, so it may be told again: " + err.Error() + ")"
		} else {
			msg += "\nthe dispatcher has been told"
		}
	}

	return &exitError{code: exitCheckEnv, msg: msg}
}

// checkExit gives a refusal by the check its own exit code.
func checkExit(err error) error {
	if errors.Is(err, ledger.ErrCheckFailed) {
		return &exitError{code: exitCheckFailed, msg: err.Error()}
	}

	return err
}

// changeOf is the ledger's change that a close or set of a task with these flags is, made by whoever runs this command: it
// is held to the task's current hand-out (ledger.Current).
func (a *app) changeOf(s *shift.Shift, st task.State, o taskChange) ledger.Change {
	return ledger.Change{State: st, On: o.on, Note: o.note, Role: o.role, Force: o.force, Title: o.title,
		Project: o.project, URL: o.url, Budget: o.budget, Criteria: o.setCriteria, By: by(), Check: o.check,
		ClearCheck: o.clearCheck, Caller: a.caller(s, o.handoff, st == task.Done || st == task.Dropped)}
}

// alreadyDone says a task closed as done again, with nothing new, is done already: no event, no line for the
// dispatcher, exit 0. A role and the dispatcher that both close a task do not make two closures of it.
func (a *app) alreadyDone(id string) error {
	return a.readState(func(s *shift.Shift) error {
		t, err := s.Tasks.Get(id)
		if err != nil {
			return err
		}

		at := t.Updated
		if t.Closed != nil && !t.Closed.At.IsZero() {
			at = t.Closed.At
		}

		fmt.Fprintf(a.out, "%s · %s · %s already (closed %s): not closed again\n", t.ID, t.Role, t.Status(),
			at.Local().Format("Jan 2 15:04"))

		return nil
	})
}

func (a *app) setTask(id string, st task.State, o taskChange) error {
	var (
		closed *task.Task
		late   *ledger.LateError
	)

	var envFault *ledger.EnvFault

	// again is a done task closed as done with nothing more to say: said, and nothing else happens.
	var again bool

	checked, err := a.doneCheck(id, st, o)
	if errors.As(err, &late) {
		// Refused below, under the lock, where it is kept and told once; no check ran for it.
		checked, err = nil, nil
	}

	if errors.As(err, &envFault) {
		return a.checkEnvFault(envFault)
	}

	if err != nil {
		return err
	}

	err = a.withState(func(s *shift.Shift) error {
		if o.role != "" {
			if _, err := s.Role(o.role); err != nil {
				return err
			}
		}

		change := a.changeOf(s, st, o)
		change.Checked = checked

		// A session that is the role's live one holds the hand-out, whatever conversation it was submitted to: moved to it.
		if held, err := s.Tasks.Get(id); err == nil {
			if _, _, err := a.hold(s, held, o.handoff, change.Caller.Work); err != nil {
				return err
			}
		}

		t, err := a.ledger(s).Set(id, change)
		if errors.Is(err, ledger.ErrAlreadyDone) {
			again = true

			return nil
		}

		if errors.As(err, &late) {
			// A session that does not hold the task is not the one to close or change it.
			held, gerr := s.Tasks.Get(id)
			if gerr != nil {
				return gerr
			}

			return a.lateReport(s, held, closeWhat(st), "", late.Standing)
		}

		if err != nil {
			return err
		}

		if o.role != "" && t.State.Active() {
			if _, err := s.HandTask(t.Role, t); err != nil {
				return err
			}

			s.DeliverTo(t.Role)
		}
		fmt.Fprintf(a.out, "%s · %s · %s\n", t.ID, t.Role, t.Status())

		if st != "" && !st.Active() {
			closed = &t

			// A role that closes a task tells the dispatcher, with its note, like a result does.
			s.NotifyClosed()
			s.Deliver()
		}

		return nil
	})

	if err == nil && again {
		return a.alreadyDone(id)
	}

	// The cost of a closed task is read after the lock: the transcripts take a while, the ticker must not wait.
	if err == nil && closed != nil {
		a.closedCost(*closed)
	}

	return err
}

func (a *app) taskGCCommand() *cobra.Command {
	var dryRun, force, dropGone, noPreserve bool

	c := &cobra.Command{
		Use: "gc",
		Short: "Remove the worktrees and branches of closed tasks whose branch is merged (--force: also unmerged dropped ones, " +
			"their work kept in git first)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if noPreserve && !force {
				return errors.New("--no-preserve goes with --force: only a forced collection removes work that is not merged")
			}

			return a.withState(func(s *shift.Shift) error {
				got, err := a.ledger(s).GCWith(context.Background(), ledger.GCOptions{DryRun: dryRun, Force: force, DropGone: dropGone,
					NoPreserve: noPreserve})
				if err != nil {
					return err
				}

				if len(got) == 0 {
					fmt.Fprintln(a.out, "nothing to collect")
				}

				a.printCollected(got, dryRun, dropGone)

				return nil
			})
		},
	}

	c.Flags().BoolVar(&dryRun, "dry-run", false, "say what would be removed")
	c.Flags().BoolVar(&force, "force", false, "also remove dropped tasks' unmerged branches and changes; the commits and the "+
		"uncommitted changes are kept in git first (a zmina-preserve/ branch, a refs/zmina-preserve-dirty/ ref), and a task "+
		"whose work cannot be kept is not removed")
	c.Flags().BoolVar(&noPreserve, "no-preserve", false, "with --force: remove the work without keeping it, also when it "+
		"could not be kept")
	c.Flags().BoolVar(&dropGone, "drop-gone", false,
		"also drop the worktree record of a closed task whose repository and worktree directory are both gone")

	return c
}

// printCollected is what `task gc` says: a line for each task, one line for each repository that is not where its
// tasks record it, and a summary.
func (a *app) printCollected(got []ledger.Collected, dryRun, dropGone bool) {
	rest, missing := ledger.GroupMissing(got)

	removed, dropped, kept := 0, 0, 0

	for _, c := range rest {
		switch {
		case c.Kept != "":
			kept++

			fmt.Fprintf(a.out, "%s: kept: %s\n", c.ID, c.Kept)
		case c.Dropped && dryRun:
			dropped++

			fmt.Fprintf(a.out, "%s: would drop the worktree record of %s\n", c.ID, c.Detail)
		case c.Dropped:
			dropped++

			fmt.Fprintf(a.out, "%s: dropped the worktree record of %s\n", c.ID, c.Detail)
		case dryRun:
			removed++

			fmt.Fprintf(a.out, "%s: would remove %s\n", c.ID, c.Detail)

			if c.WouldPreserve {
				fmt.Fprintf(a.out, "%s: its unmerged commits and uncommitted changes, if it has any, would be kept in git first (ignored files are not)\n", c.ID)
			}
		default:
			removed++

			fmt.Fprintf(a.out, "%s: removed %s\n", c.ID, c.Detail)
		}

		a.printPreserved(c)
	}

	noTree := 0

	for _, m := range missing {
		kept += len(m.Tasks)
		noTree += m.NoTree

		fmt.Fprintln(a.out, m.Line())
	}

	if len(missing) > 0 {
		fmt.Fprintln(a.out, "zmina task relocate <old-repo-path> <new-repo-path> points their records at the new place")
	}

	if noTree > 0 && !dropGone {
		fmt.Fprintf(a.out, "%d of them have no worktree directory left: zmina task gc --drop-gone drops those worktree records\n", noTree)
	}

	if len(got) == 0 {
		return
	}

	verb := [3]string{"removed", "dropped", "kept"}
	if dryRun {
		verb = [3]string{"would remove", "would drop", "kept"}
	}

	summary := fmt.Sprintf("%s %d", verb[0], removed)

	if dropGone || dropped > 0 {
		summary += fmt.Sprintf(", %s %d worktree records", verb[1], dropped)
	}

	fmt.Fprintf(a.out, "%s, %s %d\n", summary, verb[2], kept)
}

// printPreserved says where the work of a collected task was kept before its worktree went, and how to get it back: a
// branch for the commits, a worktree that checks the uncommitted changes out. Nothing for a task that had none.
func (a *app) printPreserved(c ledger.Collected) {
	p := c.Preserved
	if p.Empty() {
		return
	}

	repo := quoteCommandArg(c.Repo)
	dir := "<a new directory>"

	for _, b := range p.Branches {
		fmt.Fprintf(a.out, "%s: kept %d commit(s) beyond the base on branch %s; restore: git -C %s worktree add %s %s\n", c.ID, b.Commits,
			b.Name, repo, dir, b.Name)
	}

	if p.Dirty != "" {
		over := ""
		if p.DirtyOver != "" {
			over = " (a commit over " + p.DirtyOver + ")"
		}

		fmt.Fprintf(a.out, "%s: kept its uncommitted changes, untracked files included (ignored files are not kept), as %s%s; restore: git -C %s worktree add --detach %s %s\n",
			c.ID, p.Dirty, over, repo, dir, p.Dirty)
	}

	if p.Index != "" {
		over := ""
		if p.IndexOver != "" {
			over = " (a commit over " + p.IndexOver + ")"
		}

		fmt.Fprintf(a.out, "%s: kept what its own index holds, which is neither the commit nor the files (a file staged and gone from the disk, an ignored file added with force), as %s%s; restore: git -C %s worktree add --detach %s %s\n",
			c.ID, p.Index, over, repo, dir, p.Index)
	}
}

func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")

	return enc.Encode(v)
}

func age(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}

	return string(r[:n-1]) + "…"
}
