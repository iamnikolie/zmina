package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/shift"
)

// exitDown is note --if-up's exit code when the dispatcher is not up.
const exitDown = 3

func (a *app) noteCommand() *cobra.Command {
	var (
		from string
		ifUp bool
	)

	c := &cobra.Command{
		Use:   "note [--from <source>] [--if-up] <text>",
		Short: "Queue a task signal for the dispatcher; the ticker types it with the health notes once it is idle",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if from == "" {
				from = by()
			}

			return a.withState(func(s *shift.Shift) error {
				queued, err := s.Note(from, strings.Join(args, " "), ifUp)
				if errors.Is(err, shift.ErrNotUp) {
					return &exitError{code: exitDown, msg: "the dispatcher is not up; nothing queued"}
				}

				if err != nil {
					return err
				}

				if !queued {
					fmt.Fprintln(a.out, "already queued for the dispatcher")

					return nil
				}

				fmt.Fprintln(a.out, "queued for the dispatcher: typed once it is idle")

				return nil
			})
		},
	}

	c.Flags().StringVar(&from, "from", "", "who the note is from (default $ZMINA_ROLE, else hand)")
	c.Flags().BoolVar(&ifUp, "if-up", false, "queue nothing and exit 3 when the dispatcher is not up")

	return c
}
