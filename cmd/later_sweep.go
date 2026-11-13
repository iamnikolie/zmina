package cmd

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/donecheck"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/ledger"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/tmux"
)

const (
	// sweepMaxDefault and sweepMaxItems are the default and the most items one sweep takes.
	sweepMaxDefault = 12
	sweepMaxItems   = 25
	// sweepSelf stands for the sweep task's own id in its asked text, which is written before the id is known.
	sweepSelf = ledger.SelfID
	// sweepBecause is why a sweep goes to the role it is given when the routes name another: the owner or the crew file
	// chose it, so a crew with route_override: required is not asked to say why.
	sweepBecause = "parked-list sweep"
	// A sweep's packet lists each item; a text or a note is cut to these many characters (later ls --full shows it whole).
	sweepTextMax = 300
	sweepNoteMax = 200
)

// laterKeepCommand is later keep: an item that is looked at and still stands, with why.
func (a *app) laterKeepCommand() *cobra.Command {
	var note string

	c := &cobra.Command{
		Use:   "keep <id>",
		Short: "Stamp an item as reviewed: it still stands, and the note says why (a sweep counts it as a verdict)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			note = strings.TrimSpace(note)
			if note == "" {
				return fmt.Errorf("later keep needs --note: say why %s still stands", later.Normalize(args[0]))
			}

			return a.withState(func(s *shift.Shift) error {
				it, err := laterStore(s.Crew).Keep(args[0], note, s.Now())
				if err != nil {
					return err
				}

				s.Log("", "later", it.ID, "kept: "+note)
				fmt.Fprintf(a.out, "%s · kept\n", it.ID)

				return nil
			})
		},
	}

	c.Flags().StringVar(&note, "note", "", "why it still stands (required; replaces the item's note)")

	return c
}

// laterSweepCommand is later sweep: one task for a role, to give every stale parked item a verdict.
func (a *app) laterSweepCommand() *cobra.Command {
	var (
		older  string
		most   int
		dryRun bool
	)

	c := &cobra.Command{
		Use: "sweep <role>",
		Short: "Hand a role the stale parked items as one task: each gets a verdict (done, drop, take, ask the owner, keep), " +
			"with evidence",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			span, err := crew.ParseSpan(older)
			if err != nil || span <= 0 {
				return fmt.Errorf("--older %q: a span of time such as 14d or 336h", older)
			}

			if most < 1 || most > sweepMaxItems {
				return fmt.Errorf("--max is a number of items from 1 to %d, not %d", sweepMaxItems, most)
			}

			run := func(s *shift.Shift) error { return a.sweepNow(s, args[0], span, most, dryRun) }
			if dryRun {
				return a.readState(run)
			}

			return a.withState(run)
		},
	}

	c.Flags().StringVar(&older, "older", "14d", "an item is stale when it is this old and was not kept within this time")
	c.Flags().IntVar(&most, "max", sweepMaxDefault, "the most items one sweep takes, oldest first")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "say which items would be swept and make nothing")

	return c
}

// sweepNow is later sweep under the state lock (dryRun: a look, no lock).
func (a *app) sweepNow(s *shift.Shift, role string, older time.Duration, most int, dryRun bool) error {
	if _, err := s.Role(role); err != nil {
		return err
	}

	tasks, _ := s.Tasks.All()
	if open, ok := later.OpenSweep(tasks); ok {
		return fmt.Errorf("%s is an open sweep (%s): finish it first (zmina later swept %s)", open.ID, sweepLeft(s, open), open.ID)
	}

	now := s.Now()
	stale := laterStale(s.Crew, now, older)

	if len(stale) == 0 {
		fmt.Fprintf(a.out, "nothing to sweep: no parked item is older than %s, not kept within %s and not waiting on a day to "+
			"come back\n", spanText(older), spanText(older))

		return nil
	}

	picked := stale[:min(len(stale), most)]

	if dryRun {
		fmt.Fprintf(a.out, "would sweep %d of %d stale items for %s (older than %s, not kept within %s):\n", len(picked),
			len(stale), role, spanText(older), spanText(older))

		w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)

		for _, it := range picked {
			text := it.Text
			if it.Note != "" {
				text += " · " + it.Note
			}

			fmt.Fprintln(w, it.ID+"\t"+age(now.Sub(it.Created))+"\t"+clip(oneLine(text), 100))
		}

		return w.Flush()
	}

	t, pane, err := a.startSweep(s, role, picked, len(stale), older)
	if err != nil {
		return err
	}

	fmt.Fprintf(a.out, "%s · %s · sweep of %d parked %s (%s)\n", t.ID, t.Role, len(t.Sweep),
		plural(len(t.Sweep), "item", "items"), sweepRange(t.Sweep))

	if pane.State != tmux.Up {
		fmt.Fprintf(a.err, "zmina: %s is down: %s starts it on the task\n", t.Role, a.upTaskLine(s, t))
	}

	return nil
}

// startSweep makes the sweep task for role over the items, the way task new makes a task: through the routes' record and
// the ledger, and handed to the role at once when it is up. It logs one later-sweep event.
func (a *app) startSweep(s *shift.Shift, role string, items []later.Item, stale int, older time.Duration) (task.Task, tmux.Pane, error) {
	asked := sweepAsked(items, stale, s.Now(), older, sweepByDispatcher(s, role))

	pick, err := a.pickRole(s, routeAsk{}, false, sweepBecause, []string{role, asked})
	if err != nil {
		return task.Task{}, tmux.Pane{}, err
	}

	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}

	t, pane, err := a.openTask(s, pick, ledger.NewTask{
		Title: fmt.Sprintf("Sweep of %d parked %s", len(ids), plural(len(ids), "item", "items")),
		Note:  "sweep of " + sweepRange(ids) + ", older than " + spanText(older),
		Sweep: ids,
	})
	if err != nil {
		return t, pane, err
	}

	s.Log(t.Role, "later-sweep", t.ID, fmt.Sprintf("%d of %d stale parked items (older than %s): %s", len(ids), stale,
		spanText(older), strings.Join(ids, ", ")))

	if pane.State == tmux.Up {
		if t, err = a.handOut(s, t); err != nil {
			return t, pane, err
		}
	}

	return t, pane, nil
}

// sweepByDispatcher says whether role is the crew's dispatcher (or a copy of it): the seat that hands work out.
func sweepByDispatcher(s *shift.Shift, role string) bool {
	base, _, _ := strings.Cut(role, "#")

	return base == s.Crew.Dispatcher
}

// sweepRange names the ids of a sweep: all of them when few, else the first and the last.
func sweepRange(ids []string) string {
	if len(ids) <= 3 {
		return strings.Join(ids, ", ")
	}

	return fmt.Sprintf("%s … %s", ids[0], ids[len(ids)-1])
}

// sweepLeft is how far a sweep task is: how many of its items have no verdict.
func sweepLeft(s *shift.Shift, t task.Task) string {
	items, _, _ := laterStore(s.Crew).Scan()

	return fmt.Sprintf("%d of %d parked items without a verdict", len(later.Unswept(items, t.Sweep, t.Created)), len(t.Sweep))
}

// laterStale is the parked items a sweep is about (later.Stale): it reads the list and the alarms and takes no lock.
func laterStale(c *crew.Crew, now time.Time, older time.Duration) []later.Item {
	items, _, _ := laterStore(c).Scan()

	return later.Stale(items, laterBack(c), now, older)
}

// sweepAsked is the text of a sweep task: what to do, the verdicts and the way to close it, then the items, each cut.
// byDispatcher says the role hands work out itself (later take); any other proposes it to the dispatcher in a note.
func sweepAsked(items []later.Item, stale int, now time.Time, older time.Duration, byDispatcher bool) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Sweep of the parked list: %d %s over %s old that were not kept in that time", len(items),
		plural(len(items), "item", "items"), spanText(older))

	if stale > len(items) {
		fmt.Fprintf(&b, " (the %d oldest of %d; the rest wait for the next sweep)", len(items), stale)
	}

	b.WriteString(". Give each item exactly one verdict, so the list shrinks instead of rotting. Before a verdict check the item " +
		"against the code and the repo's current state (git log, grep, zmina task ls --all); never answer from its text alone.\n\n")
	b.WriteString("Verdicts, one per item (L-n is its id):\n")
	b.WriteString("- resolved already: zmina later done L-n --note \"<evidence>\"\n")
	b.WriteString("- no longer wanted: zmina later drop L-n --note \"<why>\"\n")

	if byDispatcher {
		b.WriteString("- real work, ready: zmina later take L-n <role> [--repo r --worktree]\n")
	} else {
		b.WriteString("- real work, ready: handing out is the dispatcher's: zmina note \"L-n: <what to do, the seat you propose>\", " +
			"then zmina later keep L-n --note \"proposed to the dispatcher: <what>\"\n")
	}

	b.WriteString("- needs the owner's decision: zmina todo add \"<the question>\" --rec \"<your answer>\" --reason \"<why>\" " +
		"(it prints A-n), then zmina later keep L-n --note \"asked: A-n\"\n")
	b.WriteString("- still stands, nothing to do now: zmina later keep L-n --note \"<why>\"\n\n")
	b.WriteString("Here done and drop are refused without --note: it says what shows the item is resolved or gone (a commit, " +
		"file:line, a task id, command output). ")

	if byDispatcher {
		b.WriteString("Related leftovers go in one task: take the main item, and close each of the others with " +
			"zmina later done L-m --note \"dealt with in T-<n>\".\n")
	} else {
		b.WriteString("Related leftovers go in one note: name all their ids in it, and keep each with the same reference.\n")
	}

	b.WriteString("When every item has a verdict, zmina later swept " + sweepSelf + " exits 0 (else it lists the items still " +
		"without one). Then close this task: zmina task done " + sweepSelf + ", which is refused until it does.\n\n")
	b.WriteString("Items, oldest first (id · age · from · text · note); a long one is cut, zmina later ls --full shows it whole:\n")

	for _, it := range items {
		b.WriteString(sweepLine(it, now))
		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

// sweepLine is one item in a sweep's text: id, age, where it came from when it is known, its text and its note.
func sweepLine(it later.Item, now time.Time) string {
	line := it.ID + " · " + age(now.Sub(it.Created))

	if it.From != "" {
		line += " · from " + it.From
		if it.Role != "" {
			line += " (" + it.Role + ")"
		}
	}

	line += " · " + clip(oneLine(it.Text), sweepTextMax)

	if it.Note != "" {
		line += " · note: " + clip(oneLine(it.Note), sweepNoteMax)
	}

	return line
}

// laterSweptCommand is later swept: the sweep task's own check.
func (a *app) laterSweptCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "swept <task-id>",
		Short: "Exit 0 when every item a sweep task covers is closed, taken or kept since the task was made; else list the rest, exit 1",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.readState(func(s *shift.Shift) error {
				t, err := taskStore(s.Crew).Get(args[0])
				if err != nil {
					return err
				}

				if len(t.Sweep) == 0 {
					return fmt.Errorf("%s is no sweep task (zmina later sweep makes one)", t.ID)
				}

				items, _, err := laterStore(s.Crew).Scan()
				if err != nil {
					return err
				}

				left := later.Unswept(items, t.Sweep, t.Created)
				if len(left) > 0 {
					return fmt.Errorf("%s: %d of %d parked items have no verdict: %s (done, drop, take, or keep each, with --note)",
						t.ID, len(left), len(t.Sweep), strings.Join(left, ", "))
				}

				fmt.Fprintf(a.out, "%s · all %d parked items have a verdict\n", t.ID, len(t.Sweep))

				return nil
			})
		},
	}
}

// refuseBareClose is the refusal of later done or later drop of an item an open sweep lists, given no --note: the sweep
// asks for the evidence. nil for any other item, and for a note.
func refuseBareClose(s *shift.Shift, it later.Item, st later.State, note string) error {
	if strings.TrimSpace(note) != "" || !it.Open() {
		return nil
	}

	sw, ok := later.InSweep(s.Tasks.Active(), it.ID)
	if !ok {
		return nil
	}

	what := "what shows it is resolved (a commit, file:line, a task id, command output)"
	if st == later.Dropped {
		what = "why it is no longer wanted"
	}

	return fmt.Errorf("%s is listed in the open sweep %s: closing it as %s needs --note, saying %s", it.ID, sw.ID, st, what)
}

// sweepFooter is the line under later ls while a sweep is open, or "".
func sweepFooter(s *shift.Shift, items []later.Item) string {
	tasks, _ := s.Tasks.All()

	open, ok := later.OpenSweep(tasks)
	if !ok {
		return ""
	}

	return fmt.Sprintf("sweep %s open: %d of %d without a verdict (zmina later swept %s)", open.ID,
		len(later.Unswept(items, open.Sweep, open.Created)), len(open.Sweep), open.ID)
}

// spanText is a span as a person writes it in the crew file: 14d, 12h, 90s.
func spanText(d time.Duration) string {
	if d > 0 && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}

	return donecheck.Span(d)
}
