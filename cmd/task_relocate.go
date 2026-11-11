package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/ledger"
	"github.com/iamnikolie/zmina/internal/shift"
)

// relocateRepairTimeout is the most `git worktree repair` may take after a relocation.
var relocateRepairTimeout = time.Minute

func (a *app) taskRelocateCommand() *cobra.Command {
	var dryRun bool

	c := &cobra.Command{
		Use:   "relocate <old-repo-path> <new-repo-path>",
		Short: "Point task records at a repository that moved, and repair its worktrees' links (--dry-run: say what would change)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var rel ledger.Relocation

			// The records are written under the state lock, like every writer of the tasks; git is not: a repair that
			// is slow must not hold the ticker.
			locked := a.withState
			if dryRun {
				locked = a.readState
			}

			err := locked(func(s *shift.Shift) error {
				r, err := a.ledger(s).Relocate(args[0], args[1], dryRun)
				rel = r

				if err != nil {
					return err
				}

				a.printRelocation(rel, dryRun)

				return nil
			})
			if err != nil {
				return err
			}

			if !rel.HasWorktrees() {
				return nil
			}

			if dryRun {
				fmt.Fprintf(a.out, "would run git worktree repair in %s\n", rel.New)

				return nil
			}

			s, err := a.newShift()
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(context.Background(), relocateRepairTimeout)
			defer cancel()

			said, err := a.ledger(s).Trees.Repair(ctx, rel.New)
			if err != nil {
				return fmt.Errorf("the records are rewritten, but git worktree repair in %s failed: %w", rel.New, err)
			}

			if said == "" {
				said = "nothing to repair"
			}

			fmt.Fprintf(a.out, "git worktree repair in %s: %s\n", rel.New, said)

			return nil
		},
	}

	c.Flags().BoolVar(&dryRun, "dry-run", false, "say what would change, change nothing")

	return c
}

// printRelocation is a line for each task relocated, and the count.
func (a *app) printRelocation(r ledger.Relocation, dryRun bool) {
	for _, t := range r.Tasks {
		if dryRun {
			fmt.Fprintf(a.out, "%s · %s: would point at %s (now %s)\n", t.ID, t.Status(), r.New, r.Old)
		} else {
			fmt.Fprintf(a.out, "%s · %s: %s -> %s\n", t.ID, t.Status(), r.Old, r.New)
		}
	}

	if dryRun {
		fmt.Fprintf(a.out, "would relocate %d task records from %s to %s\n", len(r.Tasks), r.Old, r.New)
	} else {
		fmt.Fprintf(a.out, "relocated %d task records from %s to %s\n", r.Written, r.Old, r.New)
	}

	if r.Unreadable > 0 {
		fmt.Fprintf(a.out, "%d task files could not be read and were not looked at: zmina doctor names them\n", r.Unreadable)
	}
}
