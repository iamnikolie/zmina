package cmd

import (
	"fmt"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/spf13/cobra"
	"sort"
	"strings"
)

func (a *app) reportCommand() *cobra.Command {
	var key string
	c := &cobra.Command{Use: "report <text...> --key <stable-key>", Short: "Record a worker report; identical repeats increment a count instead of notifying again", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.withState(func(s *shift.Shift) error {
			q, err := s.Report(s.By, key, strings.Join(args, " "))
			if err != nil {
				return err
			}
			fmt.Fprintf(a.out, "report %s · queued: %t\n", key, q)
			return nil
		})
	}}
	c.Flags().StringVar(&key, "key", "", "logical report identity, scoped to the reporting source")
	var asJSON bool
	ls := &cobra.Command{Use: "ls", Short: "The latest keyed reports and repeat counts", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		return a.readState(func(s *shift.Shift) error {
			if asJSON {
				return a.printJSON(s.State.Reports)
			}
			keys := make([]string, 0, len(s.State.Reports))
			for k := range s.State.Reports {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				r := s.State.Reports[k]
				fmt.Fprintf(a.out, "%s · %d observations · %s\n", k, r.Count, r.Text)
			}
			return nil
		})
	}}
	ls.Flags().BoolVar(&asJSON, "json", false, "JSON records including counts")
	var source string
	drop := &cobra.Command{Use: "drop <key>", Short: "Remove one old keyed report, allowing a future report to notify again", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.withState(func(s *shift.Shift) error {
			from := source
			if from == "" {
				from = s.By
			}
			delete(s.State.Reports, from+"/"+args[0])
			return nil
		})
	}}
	drop.Flags().StringVar(&source, "source", "", "reporting source (default: current actor)")
	c.AddCommand(ls, drop)
	return c
}
