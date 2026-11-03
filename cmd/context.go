package cmd

import (
	"fmt"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/recovery"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/spf13/cobra"
	"sort"
	"strings"
)

func (a *app) contextCommand() *cobra.Command {
	c := &cobra.Command{Use: "context", Short: "Dispatcher recovery packets and explicit follow-ups"}
	var asJSON bool
	pack := &cobra.Command{Use: "pack [role]", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.withState(func(s *shift.Shift) error {
			if len(args) > 0 && args[0] != s.Crew.Dispatcher {
				return fmt.Errorf("recovery packs are for dispatcher %s", s.Crew.Dispatcher)
			}
			path, p, err := s.RecoveryPack()
			if err != nil {
				return err
			}
			if asJSON {
				return a.printJSON(p)
			}
			if path == "" {
				fmt.Fprintln(a.out, "no open dispatcher threads")
			} else {
				fmt.Fprintln(a.out, path)
			}
			return nil
		})
	}}
	pack.Flags().BoolVar(&asJSON, "json", false, "bounded projection, counts and full index locator")
	follow := &cobra.Command{Use: "followup", Short: "Record unresolved promises explicitly, without transcript inference"}
	var taskID, source string
	add := &cobra.Command{Use: "add <text...>", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.withState(func(s *shift.Shift) error {
			st, err := recovery.Load(s.Crew.StateDir())
			if err != nil {
				return err
			}
			if st.Followups == nil {
				st.Followups = map[string]recovery.Followup{}
			}
			n := 1
			for {
				if _, ok := st.Followups[fmt.Sprintf("F-%d", n)]; !ok {
					break
				}
				n++
			}
			text := strings.Join(args, " ")
			if len(text) > 16000 {
				return fmt.Errorf("follow-up exceeds 16000 bytes")
			}
			if taskID != "" {
				if _, err := s.Tasks.Get(taskID); err != nil {
					return err
				}
			}
			f := recovery.Followup{ID: fmt.Sprintf("F-%d", n), Text: text, Task: taskID, Source: source, At: s.Now()}
			st.Followups[f.ID] = f
			if err := recovery.Save(s.Crew.StateDir(), st); err != nil {
				return err
			}
			fmt.Fprintln(a.out, f.ID)
			return nil
		})
	}}
	add.Flags().StringVar(&taskID, "task", "", "related task ID")
	add.Flags().StringVar(&source, "source", "", "owner message or other provenance")
	resolve := &cobra.Command{Use: "resolve <id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.withState(func(s *shift.Shift) error {
			st, err := recovery.Load(s.Crew.StateDir())
			if err != nil {
				return err
			}
			f, ok := st.Followups[args[0]]
			if !ok {
				return fmt.Errorf("no follow-up %s", args[0])
			}
			f.Resolved = s.Now()
			st.Followups[f.ID] = f
			return recovery.Save(s.Crew.StateDir(), st)
		})
	}}
	var all bool
	ls := &cobra.Command{Use: "ls", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		return a.readState(func(s *shift.Shift) error {
			st, err := recovery.Load(s.Crew.StateDir())
			if err != nil {
				return err
			}
			keys := make([]string, 0, len(st.Followups))
			for k := range st.Followups {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				f := st.Followups[k]
				if all || f.Resolved.IsZero() {
					fmt.Fprintf(a.out, "%s · %s\n", f.ID, f.Text)
				}
			}
			return nil
		})
	}}
	ls.Flags().BoolVar(&all, "all", false, "include resolved follow-ups")
	var revision string
	read := &cobra.Command{Use: "read <task>", Short: "Mark a result revision read by the dispatcher (not relayed to the owner)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.withState(func(s *shift.Shift) error {
			t, err := s.Tasks.Get(args[0])
			if err != nil {
				return err
			}
			rev := revision
			if rev == "" {
				if t.Result == nil {
					return fmt.Errorf("no result recorded")
				}
				rev = t.Result.Revision
			}
			if _, err := record.Load(s.Crew.StateDir(), "result", t.ID, rev); err != nil {
				return err
			}
			st, err := recovery.Load(s.Crew.StateDir())
			if err != nil {
				return err
			}
			if st.ReadResults == nil {
				st.ReadResults = map[string]bool{}
			}
			st.ReadResults[t.ID+"/"+rev] = true
			return recovery.Save(s.Crew.StateDir(), st)
		})
	}}
	read.Flags().StringVar(&revision, "revision", "", "result revision (default: latest)")
	follow.AddCommand(add, resolve, ls)
	c.AddCommand(pack, follow, read)
	return c
}
