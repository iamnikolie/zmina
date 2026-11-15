package shift

import (
	"errors"
	"fmt"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/task"
)

// startReasonMax is the most characters of a failed start's error a reason keeps.
const startReasonMax = 300

// StartBlock is why zmina must not start the seat name now on its own account, "" when nothing says so. With the crew file's
// reconcile: assign on it is one list for the review loop, a task or a line given to a seat that is down and (see Reconcile) the
// rest: the shift is stopped or the crew retired, the seat was put down by hand (Held: only a hand-run up starts it again), is
// asleep or broken, is going down, is a temporary seat past its expiry, or its plan is held (a start's boot is a model call). With
// it off it is the four reasons the review loop always had, so that a crew that never set the key sees no change: a stopped shift,
// a seat asleep or broken, a plan held.
func (s *Shift) StartBlock(name string, now time.Time) string {
	// First, with the key on or off: a seat the crew file places on another node is never started here.
	if why := s.placedAwayShort(name); why != "" {
		return why
	}

	rs := s.State.Roles[name]
	if rs == nil {
		rs = &RoleState{}
	}

	if !s.Crew.Reconcile.Assign {
		switch {
		case !s.State.Stopped.IsZero():
			return "the shift is stopped"
		case !s.Asleep(name).IsZero():
			return "it is asleep"
		case rs.Broken:
			return "it is broken (zmina ls)"
		case s.planHeld(name, now):
			return "its plan is held (zmina limits)"
		}

		return ""
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
// recorded, with the task handed to it. started says whether the role is up on the task's account; why is "" when all went through. When
// the role was not started, why says why not: StartBlock says no, the role holds other work (which comes first), the task's worktree
// is gone, or the start failed. When it was started and the hand-out failed, started is true and why is the hand-out's error: the role
// is up with its packet not handed, which is nothing like not started. With reconcile: assign the start keeps the role's count of
// failed starts (a task given again and again does not wipe the ladder); without it it is zmina up's, which clears the count and the hold.
func (s *Shift) StartOnTask(t task.Task) (started bool, why string) {
	seat := t.Role

	if why := s.StartBlock(seat, s.Now()); why != "" {
		return false, why
	}

	if s.Tasks != nil {
		for _, other := range s.Tasks.Active() {
			if other.Role == seat && other.ID != t.ID {
				return false, "it holds " + other.ID + ", which comes first"
			}
		}
	}

	if t.Worktree != "" && !dirExists(t.Worktree) {
		return false, "its worktree " + t.Worktree + " is gone"
	}

	opts, _, err := s.TaskStart(seat, t, false, false)
	if err == nil {
		err = s.upFor(seat, opts)
	}

	if err != nil {
		return false, startReason(err)
	}

	if _, err := s.HandTask(seat, t); err != nil {
		return true, startReason(err)
	}

	return true, ""
}

// upFor starts a seat for the commands that give it work. A start with reconcile: assign keeps the seat's count of failed starts
// and clears its Held explicitly (StartBlock let it through, so it is not a hand's down), where zmina up's start (UpWith) clears
// the count, broken and held all at once.
func (s *Shift) upFor(name string, o UpOptions) error {
	if !s.Crew.Reconcile.Assign {
		return s.UpWith(name, o)
	}

	if err := s.start(name, o); err != nil {
		return err
	}

	s.State.Role(name).Held = false
	s.flush() // with the start it belongs to, as UpWith does

	return nil
}

// StartForLine starts the seat name for a line queued for it, as zmina up does: on the conversation its resume policy says, the tasks
// it holds handed (UpHanding), and the count of its failed starts kept (upFor). It says why it did not, "" when the seat was started:
// StartBlock says no, or the start failed. A start that went through but could not hand a task it holds is a start, and is on the log.
func (s *Shift) StartForLine(name string) string {
	if why := s.StartBlock(name, s.Now()); why != "" {
		return why
	}

	if err := s.upHanding(name, UpOptions{}, s.upFor); err != nil {
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

// ReleaseRoutineDown settles the Held a zmina down just left, with reconcile: assign on: a down run by a seat of the crew (the
// dispatcher puts a seat down after its work as a matter of course) on a keep: manual seat that holds no open task is no down on
// purpose, so the seat is not Held and a task given to it later starts it. A down by the owner's hand or the cockpit, of a keep: always
// seat or of a seat that still holds an open task, and every other kind of down, stays Held: put down on purpose, only zmina up starts
// it. The caller has begun the down (BeginDown). Without the key nothing changes.
func (s *Shift) ReleaseRoutineDown(name string) {
	if !s.Crew.Reconcile.Assign || name == s.Crew.Dispatcher {
		return
	}

	base, _, _ := SplitInstance(s.By)
	if _, ok := s.Crew.Roles[base]; !ok {
		return
	}

	if r, err := s.Role(name); err != nil || r.Keep == crew.KeepAlways {
		return
	}

	if s.Tasks != nil {
		for _, t := range s.Tasks.Active() {
			if t.Role == name && t.State == task.Open {
				return
			}
		}
	}

	s.State.Role(name).Held = false
	s.log(name, "down-routine", "", "put down by "+s.By+" with no open task: not held, a task given to it later starts it")
}
