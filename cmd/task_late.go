package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/ledger"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

// exitLate is the exit code of a report a session made on a task after it was handed to another: a result or checkpoint is
// kept as a revision, a close or a change is refused, and in neither the task changed.
const exitLate = 5

// handoffFlag adds --handoff to a command that reports on a task.
func handoffFlag(c *cobra.Command, into *string) {
	c.Flags().StringVar(into, "handoff", "", "the packet id of the hand-out you report for (the id on the line you were handed): "+
		"for a role whose session zmina cannot tell from the one the task was handed to")
}

// caller is who runs this command and what it can show of the hand-out it reports for: the role it runs as, its
// conversation, the packet --handoff names, and (asked only when the task's delivery has no session yet) the role's live one.
func (a *app) caller(s *shift.Shift, handoff string) *ledger.Caller {
	return &ledger.Caller{By: by(), Session: callerSession(), Handoff: strings.TrimSpace(handoff), Live: s.LiveSession}
}

// lateReport keeps a late report of this command on task t, tells of it (shift.RecordLate) and ends the command with exitLate.
// what is what it did (result, checkpoint, done, drop, park, set) and revision the record it saved, when it saved one. The
// error is partial: the state, which holds the note to the dispatcher, is saved.
func (a *app) lateReport(s *shift.Shift, t task.Task, what, revision string, st ledger.Standing) error {
	_, err := s.RecordLate(t, task.Late{What: what, Revision: revision, By: by(), Session: callerSession(), Gen: st.Gen})
	if err != nil {
		return err
	}

	return shift.Partial(&exitError{code: exitLate, msg: lateMessage(t.ID, what, revision, st)})
}

// lateMessage is what the late caller is told: that the task was handed out again, to whom and when, and what became of
// what it did.
func lateMessage(id, what, revision string, st ledger.Standing) string {
	msg := st.Err(id).Error()

	if revision == "" {
		return fmt.Sprintf("%s; task %s refused, the task is unchanged", msg, what)
	}

	return fmt.Sprintf("%s; kept as %s revision %s, not the task's %s, and the task is unchanged", msg, what,
		record.Short(nil, revision), what)
}

// closeWhat is the word a late close or change is told by.
func closeWhat(st task.State) string {
	switch st {
	case task.Done:
		return "done"
	case task.Dropped:
		return "drop"
	default:
		return "set"
	}
}

// printLate says the late reports of a task: the newest five, all with full.
func (a *app) printLate(t task.Task, full bool) {
	late := t.Late
	if !full && len(late) > 5 {
		late = late[len(late)-5:]
	}

	revs := false

	for _, l := range late {
		line := fmt.Sprintf("late: %s", l.What)
		if l.Revision != "" {
			revs = true
			line += " " + record.Short(nil, l.Revision)
		}

		line += fmt.Sprintf(" · %s · %s", oneLine(l.By), l.At.Local().Format(time.DateTime))
		if l.Session != "" {
			line += " · session " + clip(oneLine(l.Session), 12)
		}

		line += fmt.Sprintf(" · hand-out %d was current", l.Gen)
		if l.Revision != "" {
			line += " · kept, not the task's " + l.What
		} else {
			line += " · refused"
		}

		fmt.Fprintln(a.out, line)
	}

	if revs {
		fmt.Fprintf(a.out, "late: read one with zmina task result|checkpoint %s --revision <revision>\n", t.ID)
	}
}

// printClosedGen says the hand-out a closed task was closed under, and that no identity tied the closing to it (Unbound).
func (a *app) printClosedGen(t task.Task) {
	k := t.Closed
	if k == nil || k.Gen == 0 {
		return
	}

	line := fmt.Sprintf("closed: by %s under hand-out %d", oneLine(k.By), k.Gen)
	if k.Unbound {
		line += " · unbound: the closing showed no session or packet to tie it to that hand-out"
	}

	fmt.Fprintln(a.out, line)
}
