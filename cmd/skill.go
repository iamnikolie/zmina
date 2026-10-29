package cmd

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"
)

//go:embed skill.md
var skillDoc string

func (a *app) skillCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "skill",
		Short: "Print the agent reference for this CLI",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprint(a.out, skillDoc)

			return nil
		},
	}
}
