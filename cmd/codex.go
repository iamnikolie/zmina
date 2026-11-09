package cmd

import (
	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/spf13/cobra"
	"os"
)

// codexCommand is the console process owned by a Codex role's tmux pane.
func (a *app) codexCommand() *cobra.Command {
	var o codex.Options
	c := &cobra.Command{Use: "_codex", Hidden: true, Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			o.Args = args
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			o.Dir = dir
			o.Config, err = codex.LoadProfile("", o.Profile)
			if err != nil {
				return err
			}
			return codex.Run(o, os.Stdin, a.out, a.err)
		},
	}
	c.Flags().StringVar(&o.Bin, "bin", "codex", "Codex executable")
	c.Flags().StringVar(&o.Name, "session", "", "Role session name")
	c.Flags().StringVar(&o.Registry.Dir, "state-dir", "", "Adapter registry directory")
	c.Flags().StringVar(&o.Resume, "resume", "", "Thread to resume")
	c.Flags().StringVar(&o.Boot, "boot", "", "Boot instruction")
	c.Flags().StringVar(&o.Role, "role", "", "Crew role key")
	c.Flags().StringVar(&o.Transport, "transport", "stdio", "Codex transport: stdio or shared")
	c.Flags().StringVar(&o.Profile, "profile", "", "Codex profile name")
	c.Flags().StringVar(&o.Model, "model", "", "Model")
	c.Flags().StringVar(&o.Effort, "effort", "", "Reasoning effort")
	c.Flags().StringVar(&o.ServiceTier, "service-tier", "", "Codex service tier (default: the profile's, else default)")
	c.Flags().StringVar(&o.Sandbox, "sandbox", "", "Sandbox mode")
	c.Flags().StringVar(&o.Approval, "approval", "", "Approval policy")
	_ = c.MarkFlagRequired("session")
	_ = c.MarkFlagRequired("state-dir")
	return c
}
