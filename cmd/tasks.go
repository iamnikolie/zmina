package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/ledger"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

func (a *app) taskCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "task",
		Short: "The work ledger: who holds what, kept outside any session's memory",
	}

	c.AddCommand(a.taskNewCommand(), a.taskLsCommand(), a.taskShowCommand(), a.taskSetCommand(),
		a.taskCloseCommand("done", task.Done), a.taskCloseCommand("drop", task.Dropped), a.taskParkCommand(),
		a.taskGCCommand())

	return c
}

func (a *app) taskNewCommand() *cobra.Command {
	var (
		n      ledger.NewTask
		asJSON bool
	)

	c := &cobra.Command{
		Use:   "new <role> <asked...>",
		Short: "Record a task handed to a role (--repo, --worktree: its own git worktree and branch)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				if _, err := s.Role(args[0]); err != nil {
					return err
				}

				n.Role, n.Asked, n.By = args[0], strings.Join(args[1:], " "), s.By

				t, err := a.ledger(s).New(n)
				if err != nil {
					return err
				}

				if asJSON {
					return a.printJSON(t)
				}

				fmt.Fprintf(a.out, "%s · %s", t.ID, t.Role)

				if t.Worktree != "" {
					fmt.Fprintf(a.out, " · worktree %s (branch %s from %s)", t.Worktree, t.Branch, t.Base)
				} else if t.Repo != "" {
					fmt.Fprintf(a.out, " · repo %s", t.Repo)
				}

				fmt.Fprintln(a.out)

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
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	return c
}

func (a *app) taskLsCommand() *cobra.Command {
	var (
		all    bool
		role   string
		asJSON bool
	)

	c := &cobra.Command{
		Use:   "ls",
		Short: "Active tasks (open, waiting); --all also the closed ones (done, dropped, parked)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
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
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Role, t.Status(), age(s.Now().Sub(t.Created)),
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
	var asJSON bool

	c := &cobra.Command{
		Use:   "show <id>",
		Short: "One task with its history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				t, err := taskStore(s.Crew).Get(args[0])
				if err != nil {
					return err
				}

				if asJSON {
					return a.printJSON(t)
				}

				fmt.Fprintf(a.out, "%s · %s · %s · by %s · %s\n", t.ID, t.Role, t.Status(), dash(t.By),
					t.Created.Format(time.DateTime))
				if t.Title != "" {
					fmt.Fprintln(a.out, "title:", t.Title)
				}

				fmt.Fprintln(a.out, "asked:", t.Asked)

				if p := t.Where(); p != "" {
					fmt.Fprintln(a.out, "project:", p)
				}

				if t.URL != "" {
					fmt.Fprintln(a.out, "url:", t.URL)
				}

				if t.Repo != "" {
					fmt.Fprintln(a.out, "repo:", t.Repo)
				}

				if t.Worktree != "" {
					fmt.Fprintf(a.out, "worktree: %s (branch %s from %s)\n", t.Worktree, t.Branch, t.Base)
				}

				if t.Note != "" {
					fmt.Fprintln(a.out, "note:", t.Note)
				}

				evs, _ := eventLog(s.Crew).Read(events.Filter{Task: t.ID, Last: 20})
				for _, e := range evs {
					fmt.Fprintf(a.out, "  %s  %s  %s  %s\n", e.TS.Format(time.DateTime), e.By, e.Event, e.Detail)
				}

				return nil
			})
		},
	}

	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	return c
}

// taskChange is the flags of set, done and drop.
type taskChange struct {
	note, role, on      string
	title, project, url string
	force               bool
}

func (o *taskChange) flags(c *cobra.Command, withRole bool) {
	c.Flags().StringVar(&o.note, "note", "", "a note on the task")
	c.Flags().BoolVar(&o.force, "force", false, "close it although its worktree has uncommitted changes")

	if withRole {
		c.Flags().StringVar(&o.role, "role", "", "hand the task to another role")
		c.Flags().StringVar(&o.title, "title", "", "the work in one short plain sentence (- clears it)")
		c.Flags().StringVar(&o.project, "project", "", "the project or product it is for (- clears it)")
		c.Flags().StringVar(&o.url, "url", "", "the live thing to look at (- clears it)")
		c.Flags().StringVar(&o.on, "on", "", "with waiting: who it waits on when not the owner — a role or a person "+
			"(- the owner again); it stays off the owner's todo")
	}
}

func (a *app) taskSetCommand() *cobra.Command {
	var o taskChange

	c := &cobra.Command{
		Use:   "set <id> [state]",
		Short: "Change a task: state (open, waiting [--on who], done, dropped), --note, --role, --title, --project, --url",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var st task.State

			if len(args) == 2 {
				var err error
				if st, err = task.ParseState(args[1]); err != nil {
					return err
				}
			}

			return a.setTask(args[0], st, o)
		},
	}

	o.flags(c, true)

	return c
}

func (a *app) taskCloseCommand(use string, st task.State) *cobra.Command {
	var o taskChange

	c := &cobra.Command{
		Use:   use + " <id>",
		Short: "Close a task as " + string(st) + " (refuses a worktree with uncommitted changes without --force)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.setTask(args[0], st, o) },
	}

	o.flags(c, false)

	return c
}

func (a *app) taskParkCommand() *cobra.Command {
	var note string

	c := &cobra.Command{
		Use:   "park <id>",
		Short: "Move an active task to the later list with what remains (--note); it closes as parked L-n",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				t, err := taskStore(s.Crew).Get(args[0])
				if err != nil {
					return err
				}

				it, err := s.Park(t, note)
				if err != nil {
					return err
				}

				fmt.Fprintf(a.out, "%s · %s · parked %s\n", t.ID, t.Role, it.ID)

				return nil
			})
		},
	}

	c.Flags().StringVar(&note, "note", "", "what remains, in one or two lines (default: the task's title)")

	return c
}

func (a *app) setTask(id string, st task.State, o taskChange) error {
	return a.withState(func(s *shift.Shift) error {
		if o.role != "" {
			if _, err := s.Role(o.role); err != nil {
				return err
			}
		}

		t, err := a.ledger(s).Set(id, ledger.Change{State: st, On: o.on, Note: o.note, Role: o.role, Force: o.force,
			Title: o.title, Project: o.project, URL: o.url})
		if err != nil {
			return err
		}

		fmt.Fprintf(a.out, "%s · %s · %s\n", t.ID, t.Role, t.Status())

		return nil
	})
}

func (a *app) taskGCCommand() *cobra.Command {
	var dryRun, force bool

	c := &cobra.Command{
		Use:   "gc",
		Short: "Remove the worktrees and branches of closed tasks whose branch is merged (--force: also unmerged dropped ones)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				got, err := a.ledger(s).GC(dryRun, force)
				if err != nil {
					return err
				}

				if len(got) == 0 {
					fmt.Fprintln(a.out, "nothing to collect")
				}

				for _, c := range got {
					switch {
					case c.Kept != "":
						fmt.Fprintf(a.out, "%s: kept: %s\n", c.ID, c.Kept)
					case dryRun:
						fmt.Fprintf(a.out, "%s: would remove %s\n", c.ID, c.Detail)
					default:
						fmt.Fprintf(a.out, "%s: removed %s\n", c.ID, c.Detail)
					}
				}

				return nil
			})
		},
	}

	c.Flags().BoolVar(&dryRun, "dry-run", false, "say what would be removed")
	c.Flags().BoolVar(&force, "force", false, "also remove dropped tasks' unmerged branches and changes")

	return c
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
