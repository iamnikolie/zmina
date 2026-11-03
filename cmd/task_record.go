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
	"strings"
)

func (a *app) taskRecordCommand(kind string) *cobra.Command {
	var file, revision string
	var asJSON, full bool
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
			src, rev, err := resolveRecord(s, t, kind, rev)
			if err != nil {
				return err
			}
			if asJSON {
				d, err := record.Load(s.Crew.StateDir(), kind, src, rev)
				if err != nil {
					return err
				}
				return a.printJSON(d)
			}
			view := recordCompact
			if full {
				view = recordFull
			}
			return a.printRecord(s, t, kind, src, rev, view)
		})
	}}
	c.Flags().StringVar(&file, "file", "", "JSON file mapping role section names to strings; - reads stdin")
	c.Flags().StringVar(&revision, "revision", "", "read a specific immutable revision")
	c.Flags().BoolVar(&asJSON, "json", false, "read JSON with shape, content and provenance")
	c.Flags().BoolVar(&full, "full", false, "every section whole (default: the summary whole, the rest clipped to a line)")
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

// Record views: the whole record, the first section whole and the others clipped, or one line.
const (
	recordFull = iota
	recordCompact
	recordLine
)

func (a *app) printTaskRecord(s *shift.Shift, t task.Task, kind, revision string, view int) error {
	return a.printRecord(s, t, kind, recordTask(t, kind, revision), revision, view)
}

// printRecord prints revision of the record kept under task src (t's own id, or the task it was carried from).
func (a *app) printRecord(s *shift.Shift, t task.Task, kind, src, revision string, view int) error {
	d, err := record.Load(s.Crew.StateDir(), kind, src, revision)
	if err != nil {
		return err
	}
	if view == recordLine {
		line := fmt.Sprintf("%s: %s · %s", kind, revision[:min(12, len(revision))], d.At.Local().Format("2006-01-02 15:04"))
		if d.Task != t.ID {
			line += " · from " + d.Task
		}
		if g := recordGist(d); g != "" {
			line += " · " + clip(oneLine(g), 200)
		}
		fmt.Fprintln(a.out, line)
		return nil
	}
	fmt.Fprintf(a.out, "%s: %s · %s · worker-reported\n", kind, revision, d.At.Local().Format("2006-01-02 15:04:05"))
	if d.Task != t.ID {
		fmt.Fprintf(a.out, "source task: %s\n", d.Task)
	}
	clipped := false
	for i, section := range d.Shape.Sections {
		v := d.Values[section.Name]
		if view == recordFull || i == gistIndex(d) {
			fmt.Fprintf(a.out, "%s:\n%s\n", section.Name, v)
			continue
		}
		short := clip(oneLine(v), 240)
		clipped = clipped || short != v
		fmt.Fprintf(a.out, "%s: %s\n", section.Name, short)
	}
	if clipped {
		// The hint opens this revision of this crew's record, not whatever is latest by the time it is run.
		fmt.Fprintf(a.out, "full: zmina --crew %s task %s %s --revision %s --full\n",
			quoteCommandArg(s.Crew.Path), kind, t.ID, revision[:min(record.ShortLen, len(revision))])
	}
	return nil
}

// gistIndex is the section that says the gist: summary when the shape has it, else the first.
func gistIndex(d record.Document) int {
	for i, section := range d.Shape.Sections {
		if section.Name == "summary" {
			return i
		}
	}
	return 0
}

func recordGist(d record.Document) string {
	if len(d.Shape.Sections) == 0 {
		return ""
	}
	return d.Values[d.Shape.Sections[gistIndex(d)].Name]
}

// oneLine folds every run of whitespace, newlines too, into one space.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// resolveRecord is the task whose records hold rev (t itself, or the task its current record was carried from) and
// rev whole: --revision takes a hash's start as well as the hash.
func resolveRecord(s *shift.Shift, t task.Task, kind, rev string) (string, string, error) {
	ref := t.Result
	if kind == "checkpoint" {
		ref = t.Checkpoint
	}
	srcs := []string{t.ID}
	if ref != nil && ref.Task != "" && ref.Task != t.ID {
		srcs = []string{ref.Task, t.ID}
		if !strings.HasPrefix(ref.Revision, rev) {
			srcs = []string{t.ID, ref.Task}
		}
	}
	var first error
	for _, src := range srcs {
		whole, err := record.Resolve(s.Crew.StateDir(), kind, src, rev)
		if err == nil {
			return src, whole, nil
		}
		if first == nil {
			first = err
		}
	}
	return "", "", first
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
