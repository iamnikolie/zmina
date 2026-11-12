package cmd

import (
	"fmt"
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
		Short: "Restarts of a role into a fresh conversation (restart_at, restart_idle, restart_done) and how each ended",
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

	c.AddCommand(ls, show)

	return c
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
