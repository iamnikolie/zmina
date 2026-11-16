package shift

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/move"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// MoveGrace is how long a role that was asked to park its tasks for a move has to do it before zmina parks what is left with the
// task's title and the role's last status line: the time a released role has (releaseWait).
const MoveGrace = releaseWait

// MoveOpen is the id of the open move of the role off this node, "" when there is none. A journal that does not read is the word
// "unreadable": the role is refused all the same.
func (s *Shift) MoveOpen(name string) string { return move.OpenID(s.Crew.Layout(), name) }

// errMoving is the refusal of a start of a role whose move is open: the ticker and every start path pass it, so the role cannot come
// back up on this node while the node it goes to may be taking its work.
func (s *Shift) errMoving(name string) error {
	if why := s.movingShort(name); why != "" {
		return errors.New(name + ": " + why)
	}

	return nil
}

// movingShort is errMoving as a reason in a sentence: "its move to beta is open (mv-…) …"; "" for a role that is not moving.
func (s *Shift) movingShort(name string) string {
	j, ok, err := move.Open(s.Crew.Layout(), name)

	switch {
	case err != nil:
		return "its move journal " + s.Crew.Layout().Move(name) + " does not read, so the role is not started until it does: mend or remove it (" + err.Error() + ")"
	case !ok:
		return ""
	}

	// A journal in a crew whose file declares no nodes cannot go on (nothing to move to): only --abort ends it.
	if !s.Crew.Spread() {
		return fmt.Sprintf("a move journal to %s is open (%s, %s) in a crew whose file declares no nodes:, so it cannot go on: zmina move %s %s --abort %s "+
			"puts it back and restarts it here", j.To, j.ID, j.Phase, name, j.To, j.ID)
	}

	return fmt.Sprintf("its move to %s is open (%s, %s): zmina move %s %s --resume %s finishes it, --abort %s puts it back and restarts it here",
		j.To, j.ID, j.Phase, name, j.To, j.ID, j.ID)
}

// MovingLine is the refusal of work or a line for a role whose move is open ("dev: its move to beta is open (…)"), "" for a role that is
// not moving: what is given to a role that is going down would be left behind.
func (s *Shift) MovingLine(name string) string {
	if why := s.movingShort(name); why != "" {
		return name + ": " + why
	}

	return ""
}

// moveUp is the beat's look at a role whose move is open: nothing is started, restarted, released or put down for it, and the one thing
// it asks tmux is whether it is up, which its state follows both ways. The lines that wait for it are still delivered.
func (s *Shift) moveUp(name string) {
	p, err := s.probe(name)
	if err != nil {
		return
	}

	up := p.State == tmux.Up

	if rs := s.State.Roles[name]; rs != nil {
		rs.Up = up
	} else if up {
		s.State.Role(name).Up = true
	}
}

// MoveQueue is what a move does with the lines queued for the role, which it does not carry: none queued is nothing; lines queued are
// refused unless force, which drops them and returns them so the dispatcher can be told which.
func (s *Shift) MoveQueue(name string, force bool) ([]string, error) {
	rs := s.State.Role(name)
	if len(rs.Inbox) == 0 {
		return nil, nil
	}

	if !force {
		noun := "lines"
		if len(rs.Inbox) == 1 {
			noun = "line"
		}

		return nil, fmt.Errorf("%s has %d queued %s that a move does not carry (zmina queue %s): let them be delivered first, or "+
			"--force drops them and tells the dispatcher which", name, len(rs.Inbox), noun, name)
	}

	dropped := slices.Clone(rs.Inbox)
	s.trimInbox(name, func(string) bool { return true }, ClosedDropped, "the role was moved to another node")

	return dropped, nil
}

// moveLine is what a role that is moving is asked when its checkpoint request cannot be written.
func moveLine(ids []string) string {
	id := "<id>"
	if len(ids) == 1 {
		id = ids[0]
	}

	return fmt.Sprintf("[zmina] you are moving to another node and go down in a few minutes. For each task you hold (%s) write what remains "+
		"in one or two lines: zmina task park %s --note \"<what remains>\". Then stop.", strings.Join(ids, ", "), id)
}

// MoveAsk asks a role that is up and holds tasks to write its checkpoints and park them: the release's own request (the checkpoint
// request for each task, then park), queued for the role. It reports whether it asked; a role that is down, or holds nothing, is
// asked nothing.
func (s *Shift) MoveAsk(name string) (bool, error) {
	mine := s.heldTasks(name)
	if len(mine) == 0 {
		return false, nil
	}

	p, err := s.probe(name)
	if err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}

	if p.State != tmux.Up {
		return false, nil
	}

	ids := make([]string, 0, len(mine))
	for _, t := range mine {
		ids = append(ids, t.ID)
	}

	request, err := s.releaseCheckpointRequest(name, mine)
	if err != nil {
		request = moveLine(ids)

		s.addNotes([]string{name + ": checkpoint request unavailable: " + err.Error() + "; checkpoint freshness will be checked at the move"})
	}

	s.enqueue(name, request, Facts{From: "zmina"})
	s.log(name, "move-asked", strings.Join(ids, ","), "asked to park "+strings.Join(ids, ", "))

	return true, nil
}

// MovePark parks what the role still holds, as a release does: when the role is down, or a grace after it was asked, each task it
// holds is parked with its title, its checkpoint and the last status zmina saw; before that a role that is up is left to park its own.
// waiting says what it still waits for, "" when the role holds no task any more; parked are the tasks this call parked.
func (s *Shift) MovePark(name string, asked time.Time, grace time.Duration) (waiting string, parked []task.Task, err error) {
	mine := s.heldTasks(name)
	if len(mine) == 0 {
		return "", nil, nil
	}

	p, err := s.probe(name)
	up := err == nil && p.State == tmux.Up

	if up && s.Now().Sub(asked) < grace {
		ids := make([]string, 0, len(mine))
		for _, t := range mine {
			ids = append(ids, t.ID)
		}

		return fmt.Sprintf("%s has not parked %s yet (%s left before zmina parks them for it)", name, strings.Join(ids, ", "),
			(grace - s.Now().Sub(asked)).Round(time.Second)), nil, nil
	}

	rs := s.State.Role(name)

	for _, t := range mine {
		if _, err := s.park(t, s.parkText(t, rs), true); err != nil {
			return "", parked, fmt.Errorf("%s: %w", t.ID, err)
		}

		parked = append(parked, t)
	}

	return "", parked, nil
}

// MoveBeginDown puts the role down for the move: a role that is up gets /exit (clear drops a draft in its input line or closes a
// dialog first, as zmina down --clear does), and it is Held either way, so that nothing but the move's end or abort starts it. wait
// says the caller must wait for it to end outside the lock and then FinishDown.
func (s *Shift) MoveBeginDown(name string, clear bool) (wait bool, err error) {
	p, err := s.probe(name)
	if err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}

	if p.State == tmux.Absent {
		rs := s.State.Role(name)
		rs.Up, rs.Held = false, true

		return false, nil
	}

	return s.BeginDown(name, DownOptions{Clear: clear})
}

// MoveHeld are the active tasks the role holds now.
func (s *Shift) MoveHeld(name string) []task.Task { return s.heldTasks(name) }

// MoveTasks are the tasks the move found the role holding, by their ids, as the ledger has them now.
func (s *Shift) MoveTasks(ids []string) ([]task.Task, error) {
	out := make([]task.Task, 0, len(ids))

	for _, id := range ids {
		t, err := s.Tasks.Get(id)
		if err != nil {
			return nil, err
		}

		out = append(out, t)
	}

	return out, nil
}

// MoveUnpark gives back what the move parked, for an abort: each task the journal lists that is still parked as the item the move
// made becomes open again on its own id (its worktree was never touched), the item is closed as done with a note, and the role is no
// longer Held. A task that is not parked any more (the owner did something by hand) is left as it is. It returns the tasks open again.
func (s *Shift) MoveUnpark(j move.Journal) ([]task.Task, error) {
	var back []task.Task

	now := s.Now()

	for _, jt := range j.Tasks {
		t, err := s.Tasks.Get(jt.ID)
		if err != nil {
			return back, err
		}

		if t.State != task.Parked || (jt.Item != "" && t.Later != jt.Item) {
			continue
		}

		item := t.Later

		t.State, t.Later, t.Updated = task.Open, "", now
		if err := s.Tasks.Put(t); err != nil {
			return back, fmt.Errorf("%s: %w", t.ID, err)
		}

		if it, err := s.Later.Get(item); err == nil && it.Open() {
			it.State, it.Updated = later.Done, now
			it.Note = strings.TrimSpace(it.Note + " · move " + j.ID + " aborted: back on " + t.ID)

			if err := s.Later.Put(it); err != nil {
				return back, fmt.Errorf("%s: %w", item, err)
			}
		}

		s.log(t.Role, "move", t.ID, "aborted "+j.ID+": "+t.ID+" is open again")
		back = append(back, t)
	}

	s.State.Role(j.Role).Held = false

	return back, nil
}

// MoveCommit ends the source's side of a move: each parked item the journal lists is closed as taken by the task it became on the
// other node ("beta/T-9"), and the dispatcher is told once. taken maps a task of this node to its task there.
func (s *Shift) MoveCommit(j move.Journal, taken map[string]string) error {
	now := s.Now()

	var told []string

	for _, jt := range j.Tasks {
		there := taken[jt.ID]
		if there == "" || jt.Item == "" {
			continue
		}

		it, err := s.Later.Get(jt.Item)
		if err != nil {
			return err
		}

		if it.Open() {
			it.State, it.Task, it.Updated = later.Taken, there, now
			it.Note = strings.TrimSpace(it.Note + " · moved with " + j.Role + " to " + j.To)

			if err := s.Later.Put(it); err != nil {
				return fmt.Errorf("%s: %w", jt.Item, err)
			}
		}

		told = append(told, jt.ID+" → "+there)
	}

	text := fmt.Sprintf("%s moved to %s (move %s)", j.Role, j.To, j.ID)
	if len(told) > 0 {
		text += ": " + strings.Join(told, ", ")
	}

	s.log(j.Role, "move", "", text)

	if _, err := s.NoteOnce("move/"+j.ID, "zmina", text, true); err != nil {
		s.Warn(fmt.Errorf("the dispatcher was not told of the move: %w", err))
	}

	return nil
}

// MoveDropped tells the dispatcher which queued lines a forced move dropped.
func (s *Shift) MoveDropped(j move.Journal) {
	if len(j.Dropped) == 0 {
		return
	}

	lines := make([]string, 0, len(j.Dropped))
	for _, l := range j.Dropped {
		lines = append(lines, "«"+clipRunes(oneLine(l), 160)+"»")
	}

	text := fmt.Sprintf("%s is moving to %s, which does not carry its queue: dropped %d: %s", j.Role, j.To, len(j.Dropped), strings.Join(lines, " "))

	if _, err := s.NoteOnce("move-dropped/"+j.ID, "zmina", text, false); err != nil {
		s.Warn(fmt.Errorf("the dispatcher was not told of the dropped lines: %w", err))
	}

	s.log(j.Role, "move-dropped", "", fmt.Sprintf("%d queued lines dropped by move %s", len(j.Dropped), j.ID))
}

// StartMoved starts the role on the tasks a take made, as zmina up --task does for a fresh start: in the first task's worktree, on a new
// conversation (a task that came from another node has none to continue), the task handed to it, and the others handed as well.
// started says the role is up on their account; why is "" when all went through, else why not (StartOnTask) or what of the hand-out
// failed.
func (s *Shift) StartMoved(tasks []task.Task) (started bool, why string) {
	if len(tasks) == 0 {
		return false, "no task to start on"
	}

	started, why = s.StartOnTask(tasks[0])
	if !started {
		return false, why
	}

	var rest []string

	for _, t := range tasks[1:] {
		if _, err := s.HandTask(t.Role, t); err != nil {
			rest = append(rest, t.ID+": "+startReason(err))
		}
	}

	if len(rest) > 0 {
		why = strings.TrimSpace(why + " " + strings.Join(rest, "; "))
	}

	return true, why
}
