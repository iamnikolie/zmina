package cmd

import (
	"fmt"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/spf13/cobra"
)

func (a *app) taskAcceptCommand() *cobra.Command {
	var handoff, role string
	c := &cobra.Command{Use: "accept <id> --handoff <revision>", Short: "Accept the exact handed task in its assigned live role session", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.withState(func(s *shift.Shift) error {
			if role != "" {
				if s.By != "hand" && s.By != role {
					return fmt.Errorf("caller role %s cannot claim %s", s.By, role)
				}
				s.By = role
			}
			if err := s.AcceptTask(args[0], handoff); err != nil {
				return err
			}
			fmt.Fprintf(a.out, "%s accepted\n", args[0])
			return nil
		})
	}}
	c.Flags().StringVar(&handoff, "handoff", "", "revision from the handoff locator line")
	c.Flags().StringVar(&role, "role", "", "explicit assigned role for shared runtime tools without ZMINA_ROLE; live delivery/session binding still applies")
	return c
}
