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
// conversation, the packet --handoff names, and the role's live one, asked when the delivery cannot say. work says the command
// reports or ends the work as the worker does (a result, a checkpoint, a close, a park).
func (a *app) caller(s *shift.Shift, handoff string, work bool) *ledger.Caller {
	return &ledger.Caller{By: by(), Session: callerSession(), Handoff: strings.TrimSpace(handoff), Live: s.LiveSession, Work: work}
}

// hold says where the caller of this command stands against the hand-out of task t. A session of the task's role that is the
// role's live one holds it by definition, also when the conversation changed under the running role since the line was
// submitted (its own /clear, a /resume in its pane): the delivery is moved to it, one `rebound` event and nobody told, and the
// task it returns is the one to write on from.
func (a *app) hold(s *shift.Shift, t task.Task, handoff string, work bool) (task.Task, ledger.Standing, error) {
	st := a.ledger(s).Current(t, *a.caller(s, handoff, work))
	if !st.Rebind {
		return t, st, nil
	}

	t, err := s.RebindDelivery(t, callerSession())

	return t, st, err
}

// lateReport keeps a late report of this command on task t, tells of it (shift.RecordLate) and ends the command with exitLate.
// what is what it did (result, checkpoint, done, drop, park, set) and revision the record it saved, when it saved one. The
// error is partial: the state, which holds the note to the dispatcher, is saved.
func (a *app) lateReport(s *shift.Shift, t task.Task, what, revision string, st ledger.Standing) error {
	_, err := s.RecordLate(t, task.Late{What: what, Revision: revision, By: by(), Session: callerSession(), Gen: st.Gen, Other: st.Other})
	if err != nil {
		return err
	}

	return shift.Partial(&exitError{code: exitLate, msg: lateMessage(t.ID, what, revision, st)})
}

// lateMessage is what the late caller is told: why it is late, which hand-out is current and for whom, what became of what it
// did, and what to do.
func lateMessage(id, what, revision string, st ledger.Standing) string {
	msg := st.Err(id).Error()

	if revision == "" {
		msg += fmt.Sprintf("; task %s refused, the task is unchanged", what)
	} else {
		msg += fmt.Sprintf("; kept as %s revision %s, not the task's %s, and the task is unchanged", what, record.Short(nil, revision), what)
	}

	if st.Next != "" {
		msg += "; " + st.Next
	}

	return msg
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

		switch {
		case l.Revision == "":
			line += " · refused"
		case l.Taken:
			line += " · the same was saved as the task's " + l.What + " since"
		case sameRevision(t, l):
			line += " · the same as the task's " + l.What
		default:
			line += " · kept, not the task's " + l.What
		}

		if l.Other {
			line += " · its role is not the task's"
		}

		fmt.Fprintln(a.out, line)
	}

	if revs {
		fmt.Fprintf(a.out, "late: read one with zmina task result|checkpoint %s --revision <revision>\n", t.ID)
	}
}

// sameRevision reports whether the late report kept the revision that is the task's own now.
func sameRevision(t task.Task, l task.Late) bool {
	ref := t.Result
	if l.What == "checkpoint" {
		ref = t.Checkpoint
	}

	return ref != nil && ref.Revision == l.Revision
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
