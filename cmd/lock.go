package cmd

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/shift"
)

func (a *app) lockCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "lock",
		Short: "Who holds the state lock now and what it is doing (never waits for the lock)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.loadCrew()
			if err != nil {
				return err
			}

			printLock(a.out, shift.LockLook(c.StateDir()), time.Now())

			return nil
		},
	}
}

// printLock says in a line or two who holds the lock. The record beside the lock is advisory: one whose process is
// gone, or that the lock file does not name, is called stale and never taken for the holder.
func printLock(w io.Writer, st shift.LockStatus, now time.Time) {
	switch {
	case st.Held && st.Holder != nil:
		fmt.Fprintln(w, "held by", st.Holder.Describe(now))
	case st.Held && st.Stale != nil:
		fmt.Fprintf(w, "held by pid %d (the lock file's); the record beside it is stale: %s\n", st.PID, st.Stale.Describe(now))
	case st.Held && st.PID > 0:
		fmt.Fprintf(w, "held by pid %d (the lock file's); it left no holder record (an older zmina, or it could not "+
			"write one): ps -p %d\n", st.PID, st.PID)
	case st.Held:
		fmt.Fprintln(w, "held by another zmina; no pid is known")
	case st.Stale != nil:
		fmt.Fprintln(w, "free; a stale record is left by a zmina that ended holding it:", st.Stale.Describe(now))
	default:
		fmt.Fprintln(w, "free")
	}
}
