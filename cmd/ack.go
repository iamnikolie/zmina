package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/shift"
)

func (a *app) ackCommand() *cobra.Command {
	var (
		all  bool
		role string
	)

	c := &cobra.Command{
		Use:   "ack [S-n...]",
		Short: "Lines handled: stop saying them again (ids from the lines; --all: every line of the calling role)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if all == (len(args) > 0) {
				return errors.New("name the ids to ack (S-12 S-13), or --all")
			}

			if role == "" {
				role = by()
			}

			return a.withState(func(s *shift.Shift) error {
				acked, err := s.AckLines(args, all, role)
				if err != nil {
					return err
				}

				if len(acked) == 0 {
					fmt.Fprintln(a.out, "nothing waits for an ack")

					return nil
				}

				fmt.Fprintln(a.out, "acked "+strings.Join(acked, " "))

				return nil
			})
		},
	}

	c.Flags().BoolVar(&all, "all", false, "ack every line of the role")
	c.Flags().StringVar(&role, "role", "", "the role whose lines --all acks (default $ZMINA_ROLE)")

	return c
}

func (a *app) pendingCommand() *cobra.Command {
	var (
		role         string
		brief, every bool
		asJSON       bool
	)

	c := &cobra.Command{
		Use:   "pending",
		Short: "Lines delivered and not acked yet: the calling role's (--role, --all for every role)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if role == "" && !every {
				role = by()
				if role == "hand" {
					role = ""
				}
			}

			return a.readState(func(s *shift.Shift) error {
				if brief {
					// The hook a socket role runs after a compaction: what it prints goes into the context.
					if text := s.PendingBrief(role); text != "" {
						fmt.Fprintln(a.out, text)
					}

					return nil
				}

				list := s.Pending(role)
				if asJSON {
					return a.printJSON(list)
				}

				if len(list) == 0 {
					fmt.Fprintln(a.out, "nothing waits for an ack")

					return nil
				}

				for _, p := range list {
					when := "queued"
					if !p.First.IsZero() {
						when = "since " + p.First.Local().Format("Mon 15:04")
						if p.Renotes > 0 {
							when += fmt.Sprintf(", said %d more times", p.Renotes)
						}
					}

					fmt.Fprintf(a.out, "%s  %s  %s  %s\n", p.ID, p.Role, when, clip(oneLine(p.Text), 120))
				}

				return nil
			})
		},
	}

	c.Flags().StringVar(&role, "role", "", "this role's lines (default $ZMINA_ROLE)")
	c.Flags().BoolVar(&every, "all", false, "every role's lines")
	c.Flags().BoolVar(&brief, "brief", false, "one block for the role's context (the hook after a compaction)")
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	return c
}
