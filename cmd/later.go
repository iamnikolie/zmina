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
	var n ledger.NewTask

	c := &cobra.Command{
		Use:   "take <id> <role>",
		Short: "Turn a parked item into a task for a role (--repo, --worktree as in task new) and close it",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				if _, err := s.Role(args[1]); err != nil {
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

				fmt.Fprintf(a.out, " · %s taken\n", it.ID)

				return nil
			})
		},
	}

	c.Flags().StringVar(&n.Repo, "repo", "", "repository: a directory under the crew's dir, or a path")
	c.Flags().BoolVar(&n.Worktree, "worktree", false, "give the task its own git worktree and branch zmina/<id>")
	c.Flags().StringVar(&n.Base, "base", "", "branch or commit the worktree starts from (default: the repo's "+
		"current branch)")

	return c
}

func laterStore(c *crew.Crew) *later.Store {
	return &later.Store{Dir: c.Layout().Later()}
}
