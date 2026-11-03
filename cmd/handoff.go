package cmd

import (
	"fmt"
	"github.com/iamnikolie/zmina/internal/handoff"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/spf13/cobra"
)

func (a *app) taskHandoffCommand() *cobra.Command {
	var asJSON, queue bool
	c := &cobra.Command{Use: "handoff <id>", Short: "Render the current task and its role's handoff shape", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.withState(func(s *shift.Shift) error {
			t, err := s.Tasks.Get(args[0])
			if err != nil {
				return err
			}
			if queue {
				path, err := s.HandTask(t.Role, t)
				if err != nil {
					return err
				}
				fmt.Fprintln(a.out, path)
				return nil
			}
			p, err := s.TaskPacket(t.Role, t)
			if err != nil {
				return err
			}
			if asJSON {
				return a.printJSON(p)
			}
			path, err := handoff.Write(s.Crew.StateDir(), p)
			if err != nil {
				return err
			}
			fmt.Fprintln(a.out, path)
			return nil
		})
	}}
	c.Flags().BoolVar(&asJSON, "json", false, "render the packet as JSON")
	c.Flags().BoolVar(&queue, "queue", false, "refresh and queue the current packet for its assigned role")
	c.MarkFlagsMutuallyExclusive("queue", "json")
	return c
}
