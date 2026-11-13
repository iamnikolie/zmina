package cmd

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/shift"
)

// rotationWhy is how many characters of a rotation's reason rotation ls shows.
const rotationWhy = 90

func (a *app) rotationCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "rotation",
		Short: "Changes of a role's conversation (restarts, zmina up, a reopen, /compact, /clear) and how each ended; abort and retry mend a lost one",
	}

	var (
		asJSON bool
		last   int
	)

	ls := &cobra.Command{
		Use:   "ls [role]",
		Short: "The rotations of the crew, or of one role, the newest first: which kind, why, and whether the fresh conversation got its context",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			role := ""
			if len(args) == 1 {
				role = args[0]
			}

			rots, err := a.readRotations(role)
			if err != nil {
				return err
			}

			slices.Reverse(rots)

			if last > 0 && len(rots) > last {
				rots = rots[:last]
			}

			if asJSON {
				if rots == nil {
					rots = []shift.Rotation{}
				}

				return a.printJSON(rots)
			}

			if len(rots) == 0 {
				fmt.Fprintln(a.out, "no rotations")

				return nil
			}

			a.printRotationList(rots, time.Now())

			return nil
		},
	}
	ls.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	ls.Flags().IntVarP(&last, "last", "n", 10, "the newest n rotations (0: all)")

	show := &cobra.Command{
		Use:   "show <role|id>",
		Short: "One rotation whole, with its phases, its evidence and how it ended: a role's newest, or the one with the id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, found, err := a.rotationNamed(args[0])
			if err != nil {
				return err
			}

			if !found {
				// A role of the crew that was never rotated: nothing to show, which is no error.
				if asJSON {
					return a.printJSON(nil)
				}

				fmt.Fprintf(a.out, "no rotations for %s\n", args[0])

				return nil
			}

			if asJSON {
				return a.printJSON(r)
			}

			a.printRotation(r, time.Now())

			return nil
		},
	}
	show.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	var clear, force bool

	abort := &cobra.Command{
		Use: "abort <role>",
		Short: "Put a role back on the conversation its newest rotation took it from (an open or a lost rotation only): the role is put down " +
			"and started on it, and the rotation closes resumed",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.rotationAbort(args[0], clear, force)
		},
	}
	abort.Flags().BoolVar(&clear, "clear", false, "drop a draft (C-c) or close a dialog (Escape) first instead of refusing")
	abort.Flags().BoolVar(&force, "force", false, "abort although the fresh conversation did work (it accepted a task, or it has run for over 30 min)")

	retry := &cobra.Command{
		Use: "retry <role>",
		Short: "Hand a rotation's context to the role's current session again: the packet of each task it handed and its restart line, " +
			"each only when it is not on record already",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.rotationRetry(args[0])
		},
	}

	c.AddCommand(ls, show, abort, retry)

	return c
}

// rotationAbort puts a role back on its old conversation in three steps like zmina down: begin under the lock (the checks, the
// down), wait for /exit outside it, finish under it (the start, the record). Run from inside the role's own session it refuses:
// putting the role down would end this very command; another session, or the owner's shell, runs it.
func (a *app) rotationAbort(name string, clear, force bool) error {
	var (
		plan shift.RotationAbort
		wait bool
	)

	err := a.withState(func(s *shift.Shift) error {
		if _, inside := s.Inside(name, os.Getpid(), callerSession()); inside {
			return fmt.Errorf("%s: this command runs in its own session, which putting it down would end; run it from another session "+
				"or a shell", name)
		}

		var err error

		plan, wait, err = s.BeginAbort(name, shift.DownOptions{Force: true, Clear: clear}, force)

		return err
	})
	if err != nil {
		return err
	}

	if wait {
		s, err := a.newShift()
		if err != nil {
			return err
		}

		s.WaitGone([]string{name}, shift.ExitWait)
	}

	return a.withState(func(s *shift.Shift) error {
		if err := s.FinishAbort(name, plan); err != nil {
			return err
		}

		fmt.Fprintf(a.out, "%s is back on its conversation %s (rotation %s aborted)\n", name, plan.Session, plan.ID)

		if plan.Left != "" {
			fmt.Fprintf(a.out, "it left conversation %s, which stays on disk; to go back to it: %s\n", plan.Left, plan.Return)
		}

		return nil
	})
}

// rotationRetry hands a rotation's context to the session the role runs now, under the lock: nothing waits.
func (a *app) rotationRetry(name string) error {
	return a.withState(func(s *shift.Shift) error {
		report, err := s.RetryRotation(name)

		for _, line := range report {
			fmt.Fprintln(a.out, name+":", line)
		}

		if err != nil {
			return err
		}

		if len(report) == 0 {
			fmt.Fprintf(a.out, "%s: nothing to hand again\n", name)
		}

		return nil
	})
}

// readRotations is the crew's rotations, or one role's, oldest first.
func (a *app) readRotations(role string) ([]shift.Rotation, error) {
	c, err := a.loadCrew()
	if err != nil {
		return nil, err
	}

	return shift.ReadRotations(c.Layout(), role)
}

// rotationNamed is the rotation a word names: the newest of a role, else the one with that id. A role of the crew with none
// is not found and no error; a word that is neither a role of the crew nor an id is.
func (a *app) rotationNamed(word string) (shift.Rotation, bool, error) {
	c, err := a.loadCrew()
	if err != nil {
		return shift.Rotation{}, false, err
	}

	rots, err := shift.ReadRotations(c.Layout(), word)
	if err != nil {
		return shift.Rotation{}, false, err
	}

	if len(rots) > 0 {
		return rots[len(rots)-1], true, nil
	}

	all, err := shift.ReadRotations(c.Layout(), "")
	if err != nil {
		return shift.Rotation{}, false, err
	}

	if i := slices.IndexFunc(all, func(r shift.Rotation) bool { return r.ID == word }); i >= 0 {
		return all[i], true, nil
	}

	base, _, _ := shift.SplitInstance(word)
	if _, ok := c.Roles[base]; ok {
		return shift.Rotation{}, false, nil
	}

	return shift.Rotation{}, false, fmt.Errorf("no role %q in the crew and no rotation with that id (zmina rotation ls)", word)
}

// rotationState is where a rotation stands, in a word: its outcome, or the phase an open one is in. A rotation lost whose
// conversation then did accept what it was handed says so.
func rotationState(r shift.Rotation, now time.Time) string {
	if r.Closed == nil {
		since := r.Prepared
		if !r.Started.IsZero() {
			since = r.Started
		}

		return fmt.Sprintf("%s (open %s)", r.Phase, age(now.Sub(since)))
	}

	if r.LateRebuilt() {
		return "lost → rebuilt (late)"
	}

	if r.Closed.Outcome == shift.OutcomeLost && r.Aborted() != nil {
		return "lost → back on its old conversation (aborted)"
	}

	return r.Closed.Outcome
}

// rotationHow is what a rotation says of how it went, on one line: what confirmed it, or why it did not.
func rotationHow(r shift.Rotation) string {
	switch {
	case r.Closed == nil:
		var seen []string
		for _, e := range r.Evidence {
			seen = append(seen, rotationEvidenceWord(e))
		}

		if len(seen) == 0 {
			return "no evidence yet"
		}

		return "so far: " + strings.Join(seen, ", ")
	case len(r.Closed.Confirmed) > 0:
		return strings.Join(r.Closed.Confirmed, ", ")
	}

	return r.Closed.Detail
}

// rotationEvidenceWord is a piece of evidence in words.
func rotationEvidenceWord(e shift.RotationEvidence) string {
	switch e.What {
	case shift.EvidenceAccepted:
		return e.Task + " accepted"
	case shift.EvidenceNoteFetched:
		return e.Task + " note fetched"
	}

	return e.What
}

// printRotationList is a row for each rotation and, under it, its reason and how it went.
func (a *app) printRotationList(rots []shift.Rotation, now time.Time) {
	var width [3]int

	for _, r := range rots {
		width[0] = max(width[0], len(r.ID))
		width[1] = max(width[1], len([]rune(r.Role)))
		width[2] = max(width[2], len(r.Kind))
	}

	for _, r := range rots {
		fmt.Fprintf(a.out, "%-*s  %-*s  %-*s  %s  %s\n", width[0], r.ID, width[1], r.Role, width[2], r.Kind,
			r.Prepared.Local().Format("01-02 15:04"), rotationState(r, now))
		fmt.Fprintf(a.out, "      %s; %s\n", clip(oneLine(r.Why), rotationWhy), clip(oneLine(rotationHow(r)), 160))
	}
}

// printRotation is one rotation whole.
func (a *app) printRotation(r shift.Rotation, now time.Time) {
	at := func(t time.Time) string { return t.Local().Format("01-02 15:04:05") }

	fmt.Fprintf(a.out, "%s · %s · %s · %s\n", r.ID, r.Role, r.Kind, rotationState(r, now))
	fmt.Fprintln(a.out, "why:", r.Why)

	from := "none known"
	if r.From.Session != "" {
		from = r.From.Runtime + " conversation " + r.From.Session
		if r.From.Context > 0 {
			from += ", " + shift.Tokens(r.From.Context) + " in it"
		}

		if r.From.Dir != "" {
			from += ", in " + r.From.Dir
		}
	}

	fmt.Fprintln(a.out, "from:", from)

	if r.To.Session != "" {
		fmt.Fprintln(a.out, "to:", r.To.Session)
	}

	for _, art := range r.Artifacts {
		switch {
		case art.Checkpoint != "":
			fmt.Fprintf(a.out, "handed: %s, note %s\n", art.Task, art.Checkpoint)
		case art.Result != "":
			fmt.Fprintf(a.out, "followed: %s, result %s\n", art.Task, art.Result)
		default:
			fmt.Fprintf(a.out, "followed: %s\n", art.Task)
		}
	}

	step := func(label string, t time.Time, what string) {
		fmt.Fprintf(a.out, "%-10s %s  %s\n", label, at(t), what)
	}

	if !r.AskedAt.IsZero() {
		step("asked", r.AskedAt, "its note was asked for")
	}

	step("prepared", r.Prepared, "its note is durable, the role is to be put down")

	if !r.Switching.IsZero() {
		step("switching", r.Switching, "the role is being put down")
	}

	if !r.Started.IsZero() {
		step("started", r.Started, "the fresh session runs")
	}

	for _, line := range r.Lines {
		fmt.Fprintln(a.out, "  line:", clip(oneLine(line), 160))
	}

	for _, e := range r.Evidence {
		step("evidence", e.At, rotationEvidenceWord(e))
	}

	if k := r.Closed; k != nil {
		step("closed", k.At, k.Outcome)

		if len(k.Confirmed) > 0 {
			fmt.Fprintln(a.out, "confirmed:", strings.Join(k.Confirmed, ", "))
		}

		if k.Detail != "" {
			fmt.Fprintln(a.out, "detail:", k.Detail)
		}

		if k.Back != "" {
			fmt.Fprintln(a.out, "back to its old conversation:", k.Back)
		}
	}

	for _, e := range r.Late {
		step("late", e.At, rotationEvidenceWord(e))
	}
}

// rotationMark is where a rotation in flight shows in zmina ls: "rotating r7k2m9qd (started 2m)". Empty when the role has none
// open: a rotation that ended is not news in the table (rotationLast says it).
func rotationMark(v shift.View, now time.Time) string {
	r := v.Rotation
	if r == nil || !r.Open {
		return ""
	}

	return fmt.Sprintf("rotating %s (%s %s)", r.ID, r.Phase, age(now.Sub(r.At)))
}

// rotationLast is a role's last rotation in a word and a time, for zmina ls --wide and ls <role>: "rebuilt 14:02 (restart_at)",
// "lost → rebuilt (late) 11-02 10:12 (restart_idle)", or the mark of one in flight. Empty when the role was never rotated.
func rotationLast(v shift.View, now time.Time) string {
	r := v.Rotation
	if r == nil {
		return ""
	}

	if r.Open {
		return fmt.Sprintf("%s (%s)", rotationMark(v, now), r.Kind)
	}

	how := r.Outcome

	switch r.Then {
	case "rebuilt":
		how = "lost → rebuilt (late)"
	case "aborted":
		how = "lost → back on its old conversation (aborted)"
	}

	layout := "15:04"
	if y1, m1, d1 := r.At.Local().Date(); y1 != now.Year() || m1 != now.Month() || d1 != now.Day() {
		layout = "01-02 15:04"
	}

	return fmt.Sprintf("%s %s (%s)", how, r.At.Local().Format(layout), r.Kind)
}
