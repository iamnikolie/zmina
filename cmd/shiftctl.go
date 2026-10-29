package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/shift"
)

func (a *app) stopCommand() *cobra.Command {
	var clear bool

	c := &cobra.Command{
		Use:   "stop",
		Short: "Stop the shift: the ticker starts nothing, then every role goes down, the dispatcher too (zmina start lifts it)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.withState(func(s *shift.Shift) error { s.Stop(); return nil }); err != nil {
				return err
			}

			fmt.Fprintln(a.out, "shift stopped: the ticker starts nothing until 'zmina start'")

			return a.downRoles(func(s *shift.Shift) ([]string, error) {
				var names []string

				for _, v := range s.Views() {
					if v.State == shift.PresenceUp || v.State == shift.PresenceDead {
						names = append(names, v.Role)
					}
				}

				return names, nil
			}, shift.DownOptions{Force: true, Clear: clear}, true)
		},
	}

	c.Flags().BoolVar(&clear, "clear", false, "drop drafts (C-c) and close dialogs (Escape) first instead of refusing")

	return c
}

func (a *app) startCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Lift a stop and start the dispatcher",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				if err := s.Start(); err != nil {
					return err
				}

				fmt.Fprintln(a.out, "shift started:", s.Crew.Roles[s.Crew.Dispatcher].Session, "is up")

				return nil
			})
		},
	}
}

func (a *app) queueCommand() *cobra.Command {
	var asJSON, all bool

	c := &cobra.Command{
		Use:   "queue",
		Short: "What waits to be typed: the dispatcher's signals and notes, each role's inbox and failed lines",
	}

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List the queues",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				qs := s.Queues()
				if asJSON {
					if qs == nil {
						qs = []shift.QueueView{}
					}

					return a.printJSON(qs)
				}

				if len(qs) == 0 {
					fmt.Fprintln(a.out, "nothing waits")

					return nil
				}

				for _, q := range qs {
					fmt.Fprintln(a.out, q.Role+":")

					for _, l := range q.Signals {
						fmt.Fprintln(a.out, "  signal  ", l)
					}

					for _, l := range q.Notes {
						fmt.Fprintln(a.out, "  note    ", l)
					}

					for _, l := range q.Inbox {
						fmt.Fprintln(a.out, "  inbox   ", l)
					}

					for _, f := range q.Failed {
						fmt.Fprintf(a.out, "  failed   %s (%s, %s)\n", f.Line, f.Error, f.At.Format("01-02 15:04"))
					}
				}

				return nil
			})
		},
	}
	ls.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	retry := &cobra.Command{
		Use:   "retry <role>",
		Short: "Put a role's failed lines back at the head of its inbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				n, err := s.Retry(args[0])
				if err == nil {
					fmt.Fprintf(a.out, "%s: %d lines back in the inbox\n", args[0], n)
				}

				return err
			})
		},
	}

	drop := &cobra.Command{
		Use:   "drop <role>",
		Short: "Drop a role's failed lines (--all: its inbox too; for the dispatcher also its signals and notes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				n, err := s.Drop(args[0], all)
				if err == nil {
					fmt.Fprintf(a.out, "%s: %d lines dropped\n", args[0], n)
				}

				return err
			})
		},
	}
	drop.Flags().BoolVar(&all, "all", false, "also the inbox (and the dispatcher's signals and notes)")

	c.AddCommand(ls, retry, drop)

	return c
}

func (a *app) stateCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "state",
		Short: "zmina's own state file (state.json)",
	}

	recover := &cobra.Command{
		Use:   "recover",
		Short: "Move an unreadable state.json aside (state.json.broken-<time>) so zmina starts fresh",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.loadCrew()
			if err != nil {
				return err
			}

			aside, err := shift.Recover(c.StateDir(), time.Now())
			if err != nil {
				return err
			}

			fmt.Fprintf(a.out, "moved to %s; zmina starts from an empty state — holds, queues and resume ids in it "+
				"are not applied (read them there)\n", aside)

			return nil
		},
	}

	show := &cobra.Command{
		Use:   "show",
		Short: "Print the state as zmina reads it (JSON)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.loadCrew()
			if err != nil {
				return err
			}

			st, err := shift.Read(c.StateDir())
			if err != nil {
				return err
			}

			return a.printJSON(st)
		},
	}

	c.AddCommand(recover, show)

	return c
}
