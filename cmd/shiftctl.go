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

			return a.downEveryRole(clear)
		},
	}

	c.Flags().BoolVar(&clear, "clear", false, "drop drafts (C-c) and close dialogs (Escape) first instead of refusing")

	return c
}

// downEveryRole puts every role that is up or dead down, the dispatcher too.
func (a *app) downEveryRole(clear bool) error {
	return a.downRoles(func(s *shift.Shift) ([]string, error) {
		var names []string

		for _, v := range s.Views() {
			if v.State == shift.PresenceUp || v.State == shift.PresenceDead {
				names = append(names, v.Role)
			}
		}

		return names, nil
	}, shift.DownOptions{Force: true, Clear: clear}, true)
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
	var asJSON, all, full bool

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

				// The words of a line come first and its ids after them; rest is what zmina adds to a posted or failed
				// line, never clipped away.
				line := func(kind, text, rest string) {
					fmt.Fprintln(a.out, "  "+kind+queueLine(text, full)+rest)
				}

				// What zmina saw of a line (its card, the one that is its own: QueueView.resolveCards), in one indented
				// line under it; nothing when it has none.
				facts := func(c *shift.Card) {
					if words := queueFacts(c); words != "" {
						fmt.Fprintln(a.out, queueFactsIndent+words)
					}
				}

				now := s.Now()

				for _, q := range qs {
					fmt.Fprintln(a.out, q.Role+":")

					for _, l := range q.Signals {
						line("signal   ", l, "")
					}

					for _, l := range q.Notes {
						line("note     ", l, "")
					}

					for i, l := range q.Inbox {
						line("inbox    ", l, "")
						facts(q.InboxFacts(i))
					}

					if q.Sent != nil {
						line("typed    ", q.Sent.Line, fmt.Sprintf(" (typed into its prompt %s, %s ago, not landed yet: it may have it)",
							q.Sent.At.Local().Format("01-02 15:04"), age(now.Sub(q.Sent.At))))
						facts(q.SentFacts())
					}

					for i, p := range q.Posted {
						// The lines a role's queue gave to one numbered message are shown as the lines they are, each with
						// its own facts.
						if q.Joined(p.Parts) {
							for j, part := range p.Parts {
								line("posted   ", part.Line, fmt.Sprintf(" (to its inbox socket %s, %s ago, not yet seen, %d of %d in one post)",
									p.At.Local().Format("01-02 15:04"), age(now.Sub(p.At)), j+1, len(p.Parts)))
								facts(q.PostedPartFacts(i, j))
							}

							continue
						}

						line("posted   ", p.Line, fmt.Sprintf(" (to its inbox socket %s, %s ago, not yet seen)",
							p.At.Local().Format("01-02 15:04"), age(now.Sub(p.At))))
						facts(q.PostedFacts(i))
					}

					// Two groups in words: what may have reached the role is looked at before it is sent again; what
					// did not is safe to send again.
					failed := func(unclear bool, heading string) {
						first := true

						for i, f := range q.Failed {
							if f.Unclear != unclear {
								continue
							}

							if first {
								fmt.Fprintln(a.out, "  "+heading)

								first = false
							}

							if q.Joined(f.Parts) {
								for j, part := range f.Parts {
									line("  ", part.Line, fmt.Sprintf(" (%s, %s, %s ago, %d of %d in one post)", f.Error,
										f.At.Local().Format("01-02 15:04"), age(now.Sub(f.At)), j+1, len(f.Parts)))
									facts(q.FailedPartFacts(i, j))
								}

								continue
							}

							line("  ", f.Line, fmt.Sprintf(" (%s, %s, %s ago)", f.Error,
								f.At.Local().Format("01-02 15:04"), age(now.Sub(f.At))))
							facts(q.FailedFacts(i))
						}
					}

					failed(true, "attention required (may have reached "+q.Role+"; look before retrying, zmina queue retry "+q.Role+" --unclear sends it again):")
					failed(false, "failed (nothing reached "+q.Role+"; zmina queue retry "+q.Role+" is safe):")
				}

				return nil
			})
		},
	}
	ls.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	ls.Flags().BoolVar(&full, "full", false, "every line whole (default: its words in one line of at most 160 characters, then its ids)")

	var unclear bool

	retry := &cobra.Command{
		Use:   "retry <role>",
		Short: "Put a role's failed lines back at the head of its inbox (not the ones that may have reached it, unless --unclear)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				n, skipped, err := s.Retry(args[0], unclear)
				if err == nil {
					fmt.Fprintf(a.out, "%s: %d lines back in the inbox\n", args[0], n)

					if skipped > 0 {
						fmt.Fprintf(a.out, "%s: %d lines left failed: they may have reached the role, and a retry would say them twice; "+
							"look at them (zmina queue ls), then zmina --crew %s queue retry %s --unclear sends them again, or queue drop clears them\n",
							args[0], skipped, quoteCommandArg(s.Crew.Path), quoteCommandArg(args[0]))
					}
				}

				return err
			})
		},
	}
	retry.Flags().BoolVar(&unclear, "unclear", false, "also the lines that may have reached the role (attention required in queue ls)")

	drop := &cobra.Command{
		Use:   "drop <role>",
		Short: "Drop a role's failed lines (--all: its inbox too; for the dispatcher also its signals and notes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withState(func(s *shift.Shift) error {
				n, err := s.Drop(args[0], all)
				if err == nil {
					fmt.Fprintf(a.out, "%s: %d lines dropped\n", args[0], n)
					if waiting := len(s.State.Role(args[0]).Inbox); !all && waiting > 0 {
						fmt.Fprintf(a.out, "%d inbox lines remain; use zmina --crew %s queue drop %s --all to drop them\n", waiting, quoteCommandArg(s.Crew.Path), quoteCommandArg(args[0]))
					}
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
