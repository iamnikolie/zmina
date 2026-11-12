package ledger

import (
	"errors"
	"fmt"
	"time"

	"github.com/iamnikolie/zmina/internal/task"
)

// ErrLate is returned when a session reports on a task, or tries to close or change it, after the task was handed to another:
// it is the late report of a session that does not hold it (Current).
var ErrLate = errors.New("late report")

// Caller is who reports on a task, and what it can show of the hand-out it reports for.
type Caller struct {
	// By is who runs the command: a role ($ZMINA_ROLE) or Hand.
	By string
	// Session is the conversation the command runs in, as a runtime's tool names it (CLAUDE_CODE_SESSION_ID,
	// CODEX_THREAD_ID); "" for a process that does not say.
	Session string
	// Handoff is the packet the caller names with --handoff: the id on the line the role was handed.
	Handoff string
	// Live is the conversation a role runs now, "" when it does not say or is not up; nil: not known. It is asked for a
	// delivery that was not submitted to a session yet, whose session is the live one's, and for a caller whose session is
	// not the delivery's, which is the hand-out's if it is the live one.
	Live func(role string) string
	// Work says the command reports or ends the work as the worker would: a result, a checkpoint, a close, a park. Another
	// role than the task's is refused only for these; its other changes (a note on someone else's task) are as they were.
	Work bool
}

// Standing is where a caller stands against the hand-out a task is in.
type Standing struct {
	// Late says the caller is not the one the task is handed to. Why is then in what way, and Next what to do about it.
	Late bool
	Why  string
	Next string
	// Other says the caller's role is not the task's, whichever it did.
	Other bool
	// Rebind says the caller is the live session of the task's role, which is not the one the delivery was submitted to: the
	// conversation changed under the running role (its own /clear, a /resume in its pane). It holds the hand-out by definition;
	// the delivery is to be moved to it (shift.RebindDelivery).
	Rebind bool
	// Unbound says a role passed on its name alone: the task has a hand-out, and nothing the caller showed tied it to that
	// one (no session of its own to compare, no --handoff).
	Unbound bool
	// Gen, To and Since are the current hand-out: its number, the role it is for and when it was made.
	Gen   int
	To    string
	Since time.Time
}

// LateError is the refusal of a late report.
type LateError struct {
	ID string
	Standing
}

func (e *LateError) Error() string {
	return fmt.Sprintf("%s: late report: %s (hand-out %d, to %s, made %s)", e.ID, e.Why, e.Gen, e.To,
		e.Since.Local().Format("15:04"))
}

// Is makes a LateError match ErrLate.
func (e *LateError) Is(target error) bool { return target == ErrLate }

// Err is the LateError of a late standing against task id.
func (s Standing) Err(id string) *LateError { return &LateError{ID: id, Standing: s} }

// Current says whether the caller is the one task t is handed to: the session that holds its current hand-out
// (task.Delivery.Gen), so that what a session that held an earlier one reports changes nothing of the task. It is a
// check of who is calling, not of what the report says.
//
//   - A task whose delivery has no generation (a file from before they were kept, or one never handed out) has no hand-out
//     to be late for: every caller is current.
//   - The dispatcher is current for every task, and so is the owner's hand, unless it names a packet with --handoff.
//   - A role is current only for the task its role field names, compared whole (a copy is not its base); another role is
//     held to that only when it reports or ends the work (Caller.Work).
//   - Where the caller's session and the hand-out's are both known they must be the same. The hand-out's is the one its line
//     was submitted to; before that, the role's live one (Caller.Live). A session of the task's role that is not that one
//     but is the role's live one holds the hand-out all the same, the conversation having changed under the running role:
//     Rebind. Only a session of that role that is not the live one is late. A live session that is not known leaves the
//     delivery's the one that counts. The packet id is compared only where the sessions cannot be: it is the same for an
//     unchanged task handed out again, so it tells one task state from another, not one session from another.
//   - A role with nothing of the kind to show passes on its name alone, and the closing it makes says so (Unbound).
func (l *Ledger) Current(t task.Task, c Caller) Standing {
	d := t.Delivery
	if d == nil || d.Gen == 0 {
		return Standing{}
	}

	now := Standing{Gen: d.Gen, To: t.Role, Since: d.QueuedAt}
	late := func(why, next string) Standing {
		now.Late, now.Why, now.Next = true, why, next

		return now
	}

	switch {
	case l.Dispatcher != "" && c.By == l.Dispatcher, c.By == Hand && c.Handoff == "":
		return now
	case c.By != Hand && c.By != t.Role:
		now.Other = true

		if !c.Work {
			return now
		}

		return late(fmt.Sprintf("it is handed to %s, not %s", t.Role, c.By),
			fmt.Sprintf("it is %s's task: ask the dispatcher whether it should be yours (zmina task set %s --role %s)", t.Role, t.ID, c.By))
	case d.Role != t.Role:
		// Handed to another role than the task names (a role changed on a task that was closed): no hand-out is the caller's.
		return now
	}

	live, asked := "", false
	liveOf := func() string {
		if !asked && c.By != Hand && c.Live != nil {
			live, asked = c.Live(t.Role), true
		}

		return live
	}

	held := d.Session
	if held == "" && c.Session != "" {
		held = liveOf()
	}

	switch {
	case c.Session != "" && held != "":
		switch {
		case c.Session == held:
		case d.Session != "" && liveOf() == c.Session:
			now.Rebind = true
		case liveOf() != "":
			return late(fmt.Sprintf("this is not the session %s runs now", t.Role),
				fmt.Sprintf("stop reporting on %s from this session; if it should hold the task, ask the dispatcher to run zmina task handoff %s --queue",
					t.ID, t.ID))
		default:
			return late("it was handed out again to another session",
				fmt.Sprintf("stop reporting on %s from this session; if it should hold the task, ask the dispatcher to run zmina task handoff %s --queue",
					t.ID, t.ID))
		}
	case c.Handoff != "":
		if c.Handoff != d.ID {
			return late("its packet is not the one it is handed out with now",
				fmt.Sprintf("name the packet you hold (zmina task show %s says the current one), or leave --handoff out", t.ID))
		}
	default:
		now.Unbound = true
	}

	return now
}

// handOutOf is the number of the hand-out t is in: 0 for a task with none, or a file from before they were counted.
func handOutOf(t task.Task) int {
	if t.Delivery == nil {
		return 0
	}

	return t.Delivery.Gen
}
