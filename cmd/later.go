package cmd

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/ledger"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/tmux"
)

func (a *app) laterCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "later",
		Short: "The parked list: decisions and ideas put aside to come back to, held by no role",
	}

	c.AddCommand(a.laterAddCommand(), a.laterLsCommand(), a.laterCloseCommand("done", later.Done),
		a.laterCloseCommand("drop", later.Dropped), a.laterTakeCommand())

	return c
}

func (a *app) laterAddCommand() *cobra.Command {
	var note string

	c := &cobra.Command{
		Use:   "add <text...>",
		Short: "Park something; prints its id (L-3)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				it, err := laterStore(s.Crew).Add(later.Item{Text: strings.Join(args, " "), Note: note, By: s.By},
					s.Now())
				if err != nil {
					return err
				}

				s.Log("", "later", it.ID, "parked: "+it.Text)
				fmt.Fprintln(a.out, it.ID)

				return nil
			})
		},
	}

	c.Flags().StringVar(&note, "note", "", "a note")

	return c
}

func (a *app) laterLsCommand() *cobra.Command {
	var all, asJSON bool

	c := &cobra.Command{
		Use:   "ls",
		Short: "What is parked, oldest first; --all also the closed items",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				items, problems, err := laterStore(s.Crew).Scan()
				if err != nil {
					return err
				}

				for _, p := range problems {
					fmt.Fprintln(a.err, "zmina: warning:", p)
				}

				out := []later.Item{}

				for _, it := range items {
					if all || it.Open() {
						out = append(out, it)
					}
				}

				if asJSON {
					return a.printJSON(out)
				}

				if len(out) == 0 {
					fmt.Fprintln(a.out, "nothing parked")

					return nil
				}

				w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)

				for _, it := range out {
					line := it.ID + "\t" + age(s.Now().Sub(it.Created)) + "\t"
					if all {
						line += laterState(it) + "\t"
					}

					line += it.Text
					if it.Note != "" {
						line += " · " + it.Note
					}

					fmt.Fprintln(w, line)
				}

				return w.Flush()
			})
		},
	}

	c.Flags().BoolVar(&all, "all", false, "also done, dropped and taken items")
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	return c
}

// laterState is parked, done, dropped, or taken with the task it became.
func laterState(it later.Item) string {
	if it.State == later.Taken && it.Task != "" {
		return "taken " + it.Task
	}

	return string(it.State)
}

func (a *app) laterCloseCommand(use string, st later.State) *cobra.Command {
	var note string

	c := &cobra.Command{
		Use:   use + " <id>",
		Short: "Take an item off the list as " + string(st),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				it, err := laterStore(s.Crew).Close(args[0], st, note, s.Now())
				if err != nil {
					return err
				}

				s.Log("", "later", it.ID, string(st)+": "+it.Text)
				fmt.Fprintf(a.out, "%s · %s\n", it.ID, it.State)

				return nil
			})
		},
	}

	c.Flags().StringVar(&note, "note", "", "a note on the item")

	return c
}

func (a *app) laterTakeCommand() *cobra.Command {
	var (
		n      ledger.NewTask
		resume bool
	)

	c := &cobra.Command{
		Use: "take <id> <role>",
		Short: "Turn a parked item into a task for a role (--repo, --worktree as in task new) and close it; " +
			"--resume starts the role on the conversation it was parked from",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				role := args[1]
				if _, err := s.Role(role); err != nil {
					return err
				}

				store := laterStore(s.Crew)

				it, err := store.Get(args[0])
				if err != nil {
					return err
				}

				if !it.Open() {
					return fmt.Errorf("%s is already %s", it.ID, laterState(it))
				}

				if resume {
					if err := canResume(s, it, role); err != nil {
						return err
					}
				}

				n.Role, n.Asked, n.By = args[1], it.Text, s.By
				n.Note = "from " + it.ID
				if it.Note != "" {
					n.Note += ": " + it.Note
				}

				t, err := a.ledger(s).New(n)
				if err != nil {
					// A task dropped because its worktree failed is on the ledger; the item stays parked.
					return err
				}

				it.State, it.Task, it.Updated = later.Taken, t.ID, s.Now()
				if err := store.Put(it); err != nil {
					return fmt.Errorf("%s recorded, but %s is still parked: %w", t.ID, it.ID, err)
				}

				s.Log(t.Role, "later", it.ID, "taken as "+t.ID)
				fmt.Fprintf(a.out, "%s · %s", t.ID, t.Role)

				if t.Worktree != "" {
					fmt.Fprintf(a.out, " · worktree %s (branch %s from %s)", t.Worktree, t.Branch, t.Base)
				} else if t.Repo != "" {
					fmt.Fprintf(a.out, " · repo %s", t.Repo)
				}

				fmt.Fprintf(a.out, " · %s taken", it.ID)

				if resume {
					if err := s.UpWith(role, shift.UpOptions{Resume: it.Session}); err != nil {
						fmt.Fprintln(a.out)

						return fmt.Errorf("%s recorded, but %s did not start on its conversation: %w", t.ID, role, err)
					}

					// The resumed conversation knows the work, not the task's new id.
					if err := s.Queue(role, "[zmina "+t.ID+"] back from "+it.ID+": "+it.Text); err != nil {
						fmt.Fprintln(a.out)

						return err
					}

					fmt.Fprintf(a.out, " · %s up on %s", role, it.Session)
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
	c.Flags().BoolVar(&resume, "resume", false, "start the role (down) on the conversation the item was parked "+
		"from, and queue it the new task's id")

	return c
}

// canResume checks that later take --resume can start the role on the item's conversation: the item was parked
// from a released role, it goes back to that role (or a copy of it: a conversation belongs to its directory), and
// the role is down.
func canResume(s *shift.Shift, it later.Item, role string) error {
	if it.Session == "" {
		return fmt.Errorf("%s has no conversation to resume: it was not parked from a role's task", it.ID)
	}

	base := func(name string) string { b, _, _ := strings.Cut(name, "#"); return b }
	if base(role) != base(it.Role) {
		return fmt.Errorf("%s was parked from %s; --resume continues it only there", it.ID, it.Role)
	}

	p, err := s.Tmux.Probe(role)
	if err != nil {
		return fmt.Errorf("%s: %w", role, err)
	}

	if p.State == tmux.Up {
		return fmt.Errorf("%s is up; --resume starts a role that is down (zmina down %s first)", role, role)
	}

	return nil
}

func laterStore(c *crew.Crew) *later.Store {
	return &later.Store{Dir: c.Layout().Later()}
}
