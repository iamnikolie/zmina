package cmd

import (
	"encoding/json"
	"fmt"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/spf13/cobra"
	"io"
	"os"
)

func (a *app) taskRecordCommand(kind string) *cobra.Command {
	var file, revision string
	var asJSON bool
	c := &cobra.Command{Use: kind + " <id>", Short: "Read or save a role-shaped worker " + kind, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if file != "" {
			return a.saveRecordFile(args[0], kind, file, cmd)
		}
		return a.readState(func(s *shift.Shift) error {
			t, err := s.Tasks.Get(args[0])
			if err != nil {
				return err
			}
			ref := t.Result
			if kind == "checkpoint" {
				ref = t.Checkpoint
			}
			rev := revision
			if rev == "" {
				if ref == nil {
					return fmt.Errorf("no %s recorded", kind)
				}
				rev = ref.Revision
			}
			if asJSON {
				d, err := record.Load(s.Crew.StateDir(), kind, recordTask(t, kind, rev), rev)
				if err != nil {
					return err
				}
				return a.printJSON(d)
			}
			return a.printTaskRecord(s, t, kind, rev)
		})
	}}
	c.Flags().StringVar(&file, "file", "", "JSON file mapping role section names to strings; - reads stdin")
	c.Flags().StringVar(&revision, "revision", "", "read a specific immutable revision")
	c.Flags().BoolVar(&asJSON, "json", false, "read JSON with shape, content and provenance")
	return c
}
func (a *app) saveRecordFile(id, kind, file string, cmd *cobra.Command) error {
	in := cmd.InOrStdin()
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		in = f
	}
	raw, err := io.ReadAll(io.LimitReader(in, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return fmt.Errorf("record exceeds 1 MiB")
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	return a.withState(func(s *shift.Shift) error {
		t, err := s.Tasks.Get(id)
		if err != nil {
			return err
		}
		r, err := s.Role(t.Role)
		if err != nil {
			return err
		}
		d := record.Document{Version: 1, Kind: kind, Task: t.ID, Role: t.Role, By: s.By, At: s.Now(), Shape: s.Crew.HandoffFor(r), Values: values}
		ref, err := record.Save(s.Crew.StateDir(), d)
		if err != nil {
			return err
		}
		previous := t.Result
		if kind == "checkpoint" {
			previous = t.Checkpoint
		}
		if previous == nil || previous.Revision != ref.Revision || (kind == "checkpoint" && (!t.CheckpointTaskUpdated.Equal(t.Updated) || t.HandoffIncomplete)) {
			if kind == "result" {
				t.Result = &ref
			} else {
				t.Checkpoint = &ref
				t.HandoffIncomplete = false
				t.CheckpointTaskUpdated = d.At
			}
			t.Updated = d.At
			if err := s.Tasks.Put(t); err != nil {
				return err
			}
			s.Log(t.Role, kind, t.ID, ref.Revision+" (worker-reported)")
		}
		s.NotifyRecords()
		fmt.Fprintf(a.out, "%s %s %s\n", t.ID, kind, ref.Revision)
		return nil
	})
}

func (a *app) printTaskRecord(s *shift.Shift, t task.Task, kind, revision string) error {
	d, err := record.Load(s.Crew.StateDir(), kind, recordTask(t, kind, revision), revision)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%s: %s · %s · worker-reported\n", kind, revision, d.At.Local().Format("2006-01-02 15:04:05"))
	if d.Task != t.ID {
		fmt.Fprintf(a.out, "source task: %s\n", d.Task)
	}
	for _, section := range d.Shape.Sections {
		fmt.Fprintf(a.out, "%s:\n%s\n", section.Name, d.Values[section.Name])
	}
	return nil
}

func recordTask(t task.Task, kind, revision string) string {
	ref := t.Result
	if kind == "checkpoint" {
		ref = t.Checkpoint
	}
	if ref != nil && ref.Revision == revision && ref.Task != "" {
		return ref.Task
	}
	return t.ID
}
