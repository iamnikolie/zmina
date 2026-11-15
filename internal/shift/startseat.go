package shift

import (
	"errors"
	"fmt"
	"time"

	"github.com/iamnikolie/zmina/internal/task"
)

// startReasonMax is the most characters of a failed start's error a reason keeps.
const startReasonMax = 300

// StartBlock is why zmina must not start the seat name now on its own account, "" when nothing says so: the shift is
// stopped or the crew retired, the seat was put down by hand (Held: only a hand-run up starts it again), is asleep or
// broken, is going down, is a temporary seat past its expiry, or its plan is held (a start's boot is a model call). It is
// the one list of reasons the review loop, a task given to a seat that is down (reconcile: assign) and the ticker's own
// restarts (reconcile: restart) go by, so that none of them starts what another would leave down.
func (s *Shift) StartBlock(name string, now time.Time) string {
	rs := s.State.Roles[name]
	if rs == nil {
		rs = &RoleState{}
	}

	switch {
	case !s.State.Stopped.IsZero():
		return "the shift is stopped"
	case s.Crew.IsRetired():
		return "the crew is retired"
	case !s.Asleep(name).IsZero():
		return "it is asleep"
	case rs.Broken:
		return "it is broken (zmina ls)"
	case !rs.Downing.IsZero() || !rs.Releasing.IsZero():
		return "it is going down"
	case rs.Held:
		// The generic one, last: a sleep and a down both leave Held, and the sleep says more.
		return "it was put down by hand"
	}

	if r, err := s.Role(name); err == nil {
		if at := r.ExpiresAt(); !at.IsZero() && !now.Before(at) {
			return "its seat expired " + r.Expires
		}
	}

	if s.planHeld(name, now) {
		return "its plan is held (zmina limits)"
	}

	return ""
}

// StartOnTask starts the role of task t on it, as zmina up --task does: in the task's worktree, on the conversation the task
// recorded, with the task handed to it. It says why it did not, "" when the role was started: StartBlock says no, the role
// holds other work (which comes first), the task's worktree is gone, or the start failed.
func (s *Shift) StartOnTask(t task.Task) string {
	seat := t.Role

	if why := s.StartBlock(seat, s.Now()); why != "" {
		return why
	}

	if s.Tasks != nil {
		for _, other := range s.Tasks.Active() {
			if other.Role == seat && other.ID != t.ID {
				return "it holds " + other.ID + ", which comes first"
			}
		}
	}

	if t.Worktree != "" && !dirExists(t.Worktree) {
		return "its worktree " + t.Worktree + " is gone"
	}

	opts, _, err := s.TaskStart(seat, t, false, false)
	if err == nil {
		err = s.UpWith(seat, opts)
	}

	if err == nil {
		_, err = s.HandTask(seat, t)
	}

	if err != nil {
		return startReason(err)
	}

	return ""
}

// StartForLine starts the seat name for a line queued for it, as zmina up does: on the conversation its resume policy
// says, the tasks it holds handed with the start (UpHanding). It says why it did not, "" when the seat was started: StartBlock
// says no, or the start failed. A start that went through but could not hand a task it holds is a start, and is on the log.
func (s *Shift) StartForLine(name string) string {
	if why := s.StartBlock(name, s.Now()); why != "" {
		return why
	}

	if err := s.UpHanding(name, UpOptions{}); err != nil {
		if !errors.Is(err, ErrPartial) {
			return startReason(err)
		}

		s.log(name, "start-handout-failed", "", startReason(err))
	}

	return ""
}

// startReason is a failed start's error as one line, cut.
func startReason(err error) string {
	return clipRunes(oneLine(err.Error()), startReasonMax)
}

// StartedLine is what a command says of a seat it started: the seat, its session and the task.
func (s *Shift) StartedLine(name, taskID string) string {
	session := name

	if r, err := s.Role(name); err == nil {
		session = r.Session
	}

	line := fmt.Sprintf("%s up as %s", name, session)
	if taskID != "" {
		line += " for " + taskID
	}

	return line
}
