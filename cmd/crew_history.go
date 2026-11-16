package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/shift"
)

// crewCommand is the crew file itself, as against the roles in it (zmina seat).
func (a *app) crewCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "crew",
		Short: "The crew file itself: the versions it has had, putting one back, and sending it to another node",
	}

	c.AddCommand(a.crewHistoryCommand())
	c.AddCommand(a.crewPushCommand())

	return c
}

func (a *app) crewHistoryCommand() *cobra.Command {
	var (
		diff, restore int
		asJSON        bool
	)

	c := &cobra.Command{
		Use:   "history",
		Short: "List the crew file's kept versions, newest first; --diff <n> shows what changed since version n, --restore <n> puts it back",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			wantDiff, wantRestore := cmd.Flags().Changed("diff"), cmd.Flags().Changed("restore")
			if wantDiff && wantRestore {
				return errors.New("--diff or --restore, not both")
			}

			if asJSON && (wantDiff || wantRestore) {
				return errors.New("--json lists the versions; it goes with neither --diff nor --restore")
			}

			path := a.crewPath()

			raw, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				return noCrewFile(&crew.NoFileError{Path: path})
			}

			if err != nil {
				return fmt.Errorf("crew: %w", err)
			}

			// The file as it is now is the newest version: an edit by hand since the last command is recorded first.
			if err := noticeCrewFile(path, raw); err != nil {
				fmt.Fprintln(a.err, "zmina: warning:", err)
			}

			h := crew.HistoryOf(path)

			vs, err := h.List()
			if err != nil {
				return err
			}

			// Version 0 is the file as it is now. When it could not be recorded it is not in the list: the versions
			// then count from 1 all the same, so a number means the same thing either way.
			first := 0
			if !h.Seen(raw) {
				first = 1
			}

			pick := func(n int) (crew.Version, error) {
				if i := n - first; i >= 0 && i < len(vs) {
					return vs[i], nil
				}

				return crew.Version{}, fmt.Errorf("no version %d: zmina crew history lists them (0 is the file as it is now)", n)
			}

			switch {
			case wantDiff:
				v, err := pick(diff)
				if err != nil {
					return err
				}

				old, err := h.Bytes(v)
				if err != nil {
					return err
				}

				text := crew.UnifiedDiff(old, raw, fmt.Sprintf("version %d (%s)", diff, whenLocal(v)),
					"now ("+filepath.Base(path)+")")
				if text == "" {
					fmt.Fprintf(a.out, "version %d is the file as it is now\n", diff)

					return nil
				}

				fmt.Fprint(a.out, text)

				return nil

			case wantRestore:
				return a.restoreCrewVersion(path, raw, h, pick, restore)
			}

			return a.listCrewVersions(vs, first, asJSON)
		},
	}

	c.Flags().IntVar(&diff, "diff", 0, "show what changed between version n and the file as it is now")
	c.Flags().IntVar(&restore, "restore", 0, "put version n back as the crew file (it must load; what it replaces is kept)")
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	return c
}

func (a *app) listCrewVersions(vs []crew.Version, first int, asJSON bool) error {
	if asJSON {
		type row struct {
			N int `json:"n"`
			crew.Version
		}

		rows := make([]row, 0, len(vs))
		for i, v := range vs {
			rows = append(rows, row{N: i + first, Version: v})
		}

		return a.printJSON(rows)
	}

	if len(vs) == 0 {
		fmt.Fprintln(a.out, "no versions kept yet")

		return nil
	}

	w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "N\tWHEN\tBY\tWHAT CHANGED")

	for i, v := range vs {
		text := v.How
		if v.What != "" {
			text = v.What + " · " + v.How
		}

		if i+first == 0 {
			text += "  (the file now)"
		}

		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", i+first, whenLocal(v), dash(v.By), text)
	}

	return w.Flush()
}

func whenLocal(v crew.Version) string { return v.At.Local().Format("2006-01-02 15:04") }

// restoreCrewVersion puts version n back. It takes the state lock like every other change of the crew file, and does
// not need the crew file to load: it is what a file that no longer does is mended with.
func (a *app) restoreCrewVersion(path string, now []byte, h *crew.History, pick func(int) (crew.Version, error), n int) error {
	if n == 0 {
		return errors.New("0 is the file as it is now; --restore 1 puts the version before it back")
	}

	v, err := pick(n)
	if err != nil {
		return err
	}

	raw, err := h.Bytes(v)
	if err != nil {
		return err
	}

	if string(raw) == string(now) {
		fmt.Fprintf(a.out, "version %d is the file as it is now; nothing to restore\n", n)

		return nil
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	// A Codex seat's tool has no $ZMINA_ROLE: who runs this is told by its thread, as for every other change of the file. A crew file that
	// does not load tells nothing of it, and the restore is the owner's hand then.
	who := a.actor()

	l, err := shift.Lock(filepath.Dir(abs))
	if err != nil {
		return err
	}
	defer l.Release()

	saved, err := crew.Restore(abs, raw, who, fmt.Sprintf("zmina crew history --restore %d", n))
	if err != nil {
		return err
	}

	logCrewVersions(&events.Log{Path: crew.Layout{Dir: filepath.Dir(abs)}.Events()}, saved.Versions)

	if saved.HistoryErr != nil {
		fmt.Fprintln(a.err, "zmina: warning:", saved.HistoryErr)
	}

	fmt.Fprintf(a.out, "restored version %d (%s) as %s; the file it replaced is version 1\n", n, whenLocal(v), path)

	return nil
}

// noticeCrewFile keeps the crew file in its history when it is not what was last recorded: an edit made outside
// zmina is told in a crew-changed event on the first command that loads the file. When nothing changed it reads two
// small files and writes nothing, so every command and the ticker's every beat may call it.
func noticeCrewFile(path string, raw []byte) error {
	r, err := crew.HistoryOf(path).Notice(raw)
	if err != nil {
		return err
	}

	logCrewVersions(&events.Log{Path: crew.Layout{Dir: filepath.Dir(path)}.Events()}, []crew.Recorded{r})

	return nil
}

// logCrewVersions writes a crew-changed event for each version a change kept: the role it is about when it is one,
// what changed and by which command. The first time zmina looked at the file is a start, not a change.
func logCrewVersions(log *events.Log, vs []crew.Recorded) {
	for _, r := range vs {
		if !r.Wrote || r.First {
			continue
		}

		_ = log.Add(events.Event{Role: r.Role, Event: "crew-changed", By: r.By, Detail: r.Detail()})
	}
}

// warnCrewHistory says on stderr that a change was made but its version could not be kept.
func (a *app) warnCrewHistory(s crew.Saved) {
	if s.HistoryErr != nil {
		fmt.Fprintln(a.err, "zmina: warning:", s.HistoryErr)
	}
}

// crewCommandOf is the command a seat edit ran, as the history names it.
func crewCommandOf(did string) string {
	switch did {
	case "added":
		return "zmina seat add"
	case "removed":
		return "zmina seat rm"
	}

	return "zmina seat set"
}
