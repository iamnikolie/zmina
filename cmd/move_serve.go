package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/move"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/tmux"
	"github.com/iamnikolie/zmina/internal/worktree"
)

// The nodes' side of zmina move: what a node does when the owner's node asks it a step of a move (role/move: check, begin, poll,
// commit, abort) or hands it the work (role/take). The mesh has checked that the request is the owner's, bound to this node and this
// session, and that its names, id and sizes are plain; what is left is the node's own state. The owner's own node does the same steps
// on itself through the same functions, so that a move is the same whichever end the owner stands at.

var (
	// moveGrace is how long a role that was asked to park its tasks has before zmina parks what is left; tests shorten it.
	moveGrace = shift.MoveGrace
	// movePollEvery is how often the owner's node asks the node the role leaves how far the move is; tests shorten it.
	movePollEvery = 2 * time.Second
	// moveLockWait is how long a commit or an abort waits for a step of the same move that is running at the node (a poll that is in
	// a push to origin) before it says so and leaves it to a next try; tests shorten it.
	moveLockWait = 30 * time.Second
)

// fault is where a test makes a node's process "die": the hook returns an error for the phase it names, and the step ends there, with
// whatever was written so far left as it is. It is never set outside tests.
func (a *app) fault(phase string) error {
	if a.moveFault == nil {
		return nil
	}

	if err := a.moveFault(phase); err != nil {
		return &diedError{err}
	}

	return nil
}

// diedError is a fault of a test: the process "died" there, so what it did is left as it is, with no clean-up of its own (a dead process
// cleans nothing; the next step, or the abort, does).
type diedError struct{ err error }

func (e *diedError) Error() string { return e.err.Error() }
func (e *diedError) Unwrap() error { return e.err }

func died(err error) bool {
	var d *diedError

	return errors.As(err, &d)
}

// moveApp is the app a step of a move runs on, aimed at one of this node's crews by its key (never a path), and silent: the node's
// output is the protocol's stream.
func (a *app) moveApp(key string) (*app, error) {
	c, err := a.crewApp(key)
	if err != nil {
		return nil, err
	}

	c.out, c.err, c.verb = io.Discard, io.Discard, "move"
	c.shiftSeams, c.moveFault, c.pushTransport = a.shiftSeams, a.moveFault, a.pushTransport

	return c, nil
}

// localRoleMove is a step of a move asked of this node (role/move).
func (a *app) localRoleMove(call mesh.MoveCall) ([]byte, error) {
	return a.localRoleMoveCtx(context.Background(), call)
}

// localRoleMoveCtx is localRoleMove that gives up its slow steps (the push to origin) when ctx ends: the ticker's beat has a time of its
// own, and a step it cannot finish in it goes on at the next beat.
func (a *app) localRoleMoveCtx(ctx context.Context, call mesh.MoveCall) ([]byte, error) {
	if err := mesh.CheckMoveCall("role/move", call); err != nil {
		return nil, err
	}

	c, err := a.moveApp(call.Crew)
	if err != nil {
		return nil, noCrewForMove(call.Crew, err)
	}

	var out any

	switch call.Action {
	case "check":
		out, err = c.moveCheck(call)
	case "begin":
		out, err = c.moveBegin(call)
	case "poll":
		out, err = c.movePoll(ctx, call)
	case "commit":
		out, err = c.moveCommit(ctx, call)
	case "abort":
		out, err = c.moveAbort(ctx, call)
	case "cancel":
		out, err = c.moveCancel(call)
	}

	if err != nil {
		return nil, err
	}

	return json.Marshal(out)
}

// samePlace reports whether two paths are one place, symbolic links followed.
func samePlace(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)

	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}

	return ra == rb
}

// repoFact is what this node says of the repository at a home-relative top level: whether it is a checkout's top level, and its origin.
func repoFact(top string) move.RepoFact {
	f := move.RepoFact{Top: top}
	abs := move.Expand(top)

	got, err := worktree.TopLevel(abs)
	if err != nil || !samePlace(got, abs) {
		return f
	}

	f.Found = true
	f.Origin, _ = worktree.Origin(got)
	f.Push, _ = worktree.PushOrigin(got)

	return f
}

// dirtyOf names the checkouts of the role that hold uncommitted work a move does not carry: its own directory, and the main checkout of
// a task that has no worktree of its own. The uncommitted work of a task's worktree is carried (a commit made of it).
func dirtyOf(r *crew.Role, tasks []task.Task) []string {
	var out []string

	seen := map[string]bool{}

	check := func(dir, what string) {
		top, err := worktree.TopLevel(dir)
		if err != nil || seen[top] {
			return
		}

		seen[top] = true

		if d, err := (&worktree.Tree{Repo: top, Path: top}).Dirty(); err == nil && d != "" {
			out = append(out, move.Portable(top)+" ("+what+")")
		}
	}

	if r != nil && r.Dir != "" {
		check(r.Dir, "the role's directory")
	}

	for _, t := range tasks {
		if t.Repo != "" && t.Worktree == "" {
			check(t.Repo, "the repository of "+t.ID+", which has no worktree")
		}
	}

	return out
}

// unfitOf names the worktrees of the tasks whose uncommitted work cannot travel: a merge, rebase, cherry-pick or revert in progress,
// conflicts, a nested repository, a worktree that is not on the task's branch. No --force carries them, so the owner is told before the
// role is touched. Each is "T-n: <what>".
func unfitOf(tasks []task.Task) []string {
	var out []string

	for _, t := range tasks {
		if t.Repo == "" || t.Branch == "" || !dirExists(t.Worktree) {
			continue
		}

		tree := &worktree.Tree{Repo: t.Repo, Path: t.Worktree, Branch: t.Branch, Base: t.Base}
		if err := tree.CheckCarry(); err != nil {
			out = append(out, t.ID+": "+err.Error())
		}
	}

	return out
}

// moveCheck is what this node says of itself for a move of the role: its crew file, its crew's place, where the file puts the role,
// and, for the node the role leaves, what the role holds. A check that names repositories (the node the role goes to) answers for those.
func (a *app) moveCheck(call mesh.MoveCall) (move.Facts, error) {
	var body move.CheckBody

	if len(call.Payload) > 0 {
		if err := json.Unmarshal(call.Payload, &body); err != nil {
			return move.Facts{}, fmt.Errorf("check: %w", err)
		}
	}

	if len(body.Repos) > 64 {
		return move.Facts{}, errors.New("check: at most 64 repositories")
	}

	me, err := openNode()
	if err != nil {
		return move.Facts{}, err
	}

	cr, err := crew.Load(a.crewPath())
	if err != nil {
		return move.Facts{}, err
	}

	f := move.Facts{Node: me.Name, Sum: cr.Sum(), Dir: move.Portable(cr.Dir), Namespace: move.Portable(cr.Namespace), Spread: cr.Spread(),
		Listed: cr.HasNode(me.Name), Moving: move.OpenID(cr.Layout(), call.Role)}

	if m, ok, err := move.OpenTaking(cr.Layout(), call.Role); err != nil {
		f.Taking = "unreadable"
	} else if ok {
		f.Taking = m.ID
	}

	r := cr.Roles[call.Role]
	if r == nil {
		return f, nil
	}

	f.Known, f.Placed = true, cr.PlacementOf(r)

	if len(body.Repos) > 0 {
		for _, top := range body.Repos {
			f.Repos = append(f.Repos, repoFact(top))
		}

		return f, nil
	}

	err = a.readState(func(s *shift.Shift) error {
		var held []task.Task

		for _, t := range s.Tasks.Active() {
			if t.Role == call.Role {
				held = append(held, t)
				f.Holds = append(f.Holds, t.ID)
			}
		}

		f.Queued = len(s.State.Role(call.Role).Inbox)

		if p, err := s.Tmux.Probe(call.Role); err == nil {
			f.Up = p.State == tmux.Up
		}

		f.Dirty = dirtyOf(r, held)
		f.Unfit = unfitOf(held)
		f.Repos = repoFactsOf(r, held)

		return nil
	})

	return f, err
}

// repoFactsOf are the repositories a move of the role needs on both nodes: the one its own directory is in, and each task's.
func repoFactsOf(r *crew.Role, tasks []task.Task) []move.RepoFact {
	var out []move.RepoFact

	seen := map[string]bool{}

	add := func(dir string) {
		top, err := worktree.TopLevel(dir)
		if err != nil || seen[top] {
			return
		}

		seen[top] = true
		out = append(out, repoFact(move.Portable(top)))
	}

	if r != nil && r.Dir != "" {
		add(r.Dir)
	}

	for _, t := range tasks {
		if t.Repo != "" {
			add(t.Repo)
		}
	}

	return out
}

// answerOf is a journal as a poll says it.
func answerOf(j move.Journal, why string) move.PollAnswer {
	if j.Phase == move.PhaseReady {
		return move.PollAnswer{State: move.StateReady, Phase: j.Phase, Bundle: j.Bundle}
	}

	return move.PollAnswer{State: move.StatePending, Phase: j.Phase, Why: why}
}

// moveBegin opens the move of the role off this node: the checks that only this node can make (the file is the one the owner means,
// the role is placed here and not moving, the queue is empty or the owner forced, no checkout holds work that would be left), then the
// journal, then the request to the role to park what it holds. The same id again is the same move.
func (a *app) moveBegin(call mesh.MoveCall) (move.PollAnswer, error) {
	var body move.BeginBody

	if err := json.Unmarshal(call.Payload, &body); err != nil || !move.ValidNode(body.To) {
		return move.PollAnswer{}, errors.New("begin: the body names the node the role goes to")
	}

	// What cannot travel at all is found before the lock is taken, and before the role is touched: git reads the worktrees, which is not
	// work for the state lock. A move that is open already is not asked again.
	if err := a.refuseUnfit(call.Role); err != nil {
		return move.PollAnswer{}, err
	}

	var out move.PollAnswer

	err := a.withState(func(s *shift.Shift) error {
		cr, role := s.Crew, call.Role
		layout := cr.Layout()

		if j, open, err := move.Open(layout, role); err != nil {
			return err
		} else if open {
			if j.ID == call.Move {
				out = answerOf(j, "the move is open already")
				out.Holds = journalHolds(j)

				return nil
			}

			return fmt.Errorf("%s has a move open already (%s to %s): zmina move %s %s --resume %s finishes it, --abort %s calls it off",
				role, j.ID, j.To, role, j.To, j.ID, j.ID)
		}

		r := cr.Roles[role]

		switch {
		case !cr.Spread():
			return errors.New("this crew's file declares no nodes:: there is nowhere to move a role to")
		case r == nil:
			return fmt.Errorf("no role %q in the crew (roles: %s)", role, strings.Join(cr.Names(), ", "))
		case role == cr.Dispatcher:
			return fmt.Errorf("%s is the dispatcher: it runs on home and on every node that holds a role, and is not moved", role)
		case cr.PlacementOf(r) != cr.Here:
			return fmt.Errorf("%s is placed on %s, not on %s: a move starts where the role runs", role, cr.PlacementOf(r), cr.Here)
		case body.To == cr.Here || !cr.HasNode(body.To):
			return fmt.Errorf("%s is not another node of nodes: (%s)", body.To, strings.Join(cr.NodeNames(), ", "))
		}

		raw, err := os.ReadFile(cr.Path)
		if err != nil {
			return fmt.Errorf("crew: %w", err)
		}

		base := crew.SumOf(raw)
		if call.Base != "" && call.Base != base {
			return fmt.Errorf("the crew file here is %s, not the one this move was made from (%s): zmina crew push", base[:12], call.Base[:12])
		}

		after, err := crew.PlaceAfter(raw, role, body.To)
		if err != nil {
			return err
		}

		held := s.MoveHeld(role)

		if dirty := dirtyOf(r, held); len(dirty) > 0 && !call.Force {
			return fmt.Errorf("%s holds uncommitted work that a move does not carry: %s; commit it, or --force leaves it where it is", role, strings.Join(dirty, ", "))
		}

		dropped, err := s.MoveQueue(role, call.Force)
		if err != nil {
			return err
		}

		j := move.Journal{ID: call.Move, Role: role, From: cr.Here, To: body.To, By: call.By, Phase: move.PhaseBegun, Force: call.Force, Started: s.Now(),
			Base: base, After: crew.SumOf(after), Dropped: dropped}

		if p, err := s.Tmux.Probe(role); err == nil {
			j.Up = p.State == tmux.Up
		}

		for _, t := range held {
			j.Tasks = append(j.Tasks, move.Task{ID: t.ID})
		}

		if err := move.Save(layout, j); err != nil {
			return err
		}

		s.MoveDropped(j)
		s.Log(role, "move-begun", call.Move, "to "+body.To+", by "+call.By)

		if err := a.fault("begun"); err != nil {
			return err
		}

		asked, err := s.MoveAsk(role)
		if err != nil {
			_ = move.Close(layout, role)

			return err
		}

		j.Phase, j.Asked = move.PhaseAsked, s.Now()
		if err := move.Save(layout, j); err != nil {
			return err
		}

		why := "the role holds nothing to park"
		if asked {
			why = "asked " + role + " to park what it holds"
		}

		out = answerOf(j, why)
		out.Holds = journalHolds(j)

		return a.fault("asked")
	})

	return out, err
}

// refuseUnfit is the refusal of a begin of a move of the role while a worktree of its tasks holds work that cannot travel, naming it
// (unfitOf); a role with no such worktree, and a role whose move is open already, pass.
func (a *app) refuseUnfit(role string) error {
	var unfit []string

	err := a.readState(func(s *shift.Shift) error {
		if _, open, _ := move.Open(s.Crew.Layout(), role); open {
			return nil
		}

		unfit = unfitOf(s.MoveHeld(role))

		return nil
	})
	if err != nil {
		return err
	}

	if len(unfit) > 0 {
		return fmt.Errorf("%s holds work a move cannot carry: %s", role, strings.Join(unfit, "; "))
	}

	return nil
}

// journalHolds are the ids of the tasks a journal carries.
func journalHolds(j move.Journal) []string {
	ids := make([]string, 0, len(j.Tasks))
	for _, t := range j.Tasks {
		ids = append(ids, t.ID)
	}

	return ids
}

// movePoll moves the move on at the node the role leaves, as far as it will go now, and says how far it is; at the node the role went to
// it says whether that node took the work, is taking it, or the move was called off there.
func (a *app) movePoll(ctx context.Context, call mesh.MoveCall) (move.PollAnswer, error) {
	cr, err := crew.Load(a.crewPath())
	if err != nil {
		return move.PollAnswer{}, err
	}

	layout := cr.Layout()

	j, open, err := move.Open(layout, call.Role)

	switch {
	case err != nil:
		return move.PollAnswer{}, err
	case open && j.ID == call.Move:
		return a.moveAdvance(ctx, call.Role, call.Move)
	case open:
		return move.PollAnswer{State: move.StateNone, Why: "another move of " + call.Role + " is open here: " + j.ID}, nil
	}

	if rec, ok, err := move.Receipt(layout, call.Move); err != nil {
		return move.PollAnswer{}, err
	} else if ok {
		return move.PollAnswer{State: move.StateTaken, Taken: &rec}, nil
	}

	if m, ok, err := move.OpenTaking(layout, call.Role); err != nil {
		return move.PollAnswer{}, err
	} else if ok && m.ID == call.Move {
		return move.PollAnswer{State: move.StateTaking, Why: takingWhy(layout, call.Role, m)}, nil
	}

	if gone, err := move.Aborted(layout, call.Move); err != nil {
		return move.PollAnswer{}, err
	} else if gone {
		return move.PollAnswer{State: move.StateAborted, Why: "the move was called off here: a take of it is refused"}, nil
	}

	return move.PollAnswer{State: move.StateNone}, nil
}

// takingWhy says what a marker of a take means: whether the take is running (its lock is held) or was cut short.
func takingWhy(layout crew.Layout, role string, m move.Taking) string {
	release, err := move.Lock(layout, role, 0)
	if errors.Is(err, move.ErrBusy) {
		return "a take of " + m.ID + " is running here"
	}

	if err == nil {
		release()
	}

	return "a take of " + m.ID + " was cut short here: zmina move " + role + " <node> --resume " + m.ID + " takes it again, --abort " + m.ID + " takes away what it made and calls the move off"
}

// moveAdvance does what the move at this node waits for, one step after another until one has to wait: the role is asked to park
// (again, if the asking did not land), what it did not park in its grace is parked for it, it is put down and kept down, each task's
// branch is pushed to origin (a worktree with uncommitted work is committed first, with the move's trailer, under a private ref), and
// the bundle is built and kept in the journal. Each step is the state as it finds it, so a move that was cut short anywhere goes on
// from there. One advance of a role's move runs at a node at a time (the move's lock): a second, from another beat, a command or the
// other node's ticker, is told what the first is doing and touches nothing.
func (a *app) moveAdvance(ctx context.Context, role, id string) (move.PollAnswer, error) {
	cr, err := crew.Load(a.crewPath())
	if err != nil {
		return move.PollAnswer{}, err
	}

	layout := cr.Layout()

	release, err := move.Lock(layout, role, 0)
	if errors.Is(err, move.ErrBusy) {
		j, _, _ := move.Open(layout, role)

		return answerOf(j, "another step of this move is running on this node (a push to origin, most likely): the next poll sees how far it got"), nil
	}

	if err != nil {
		return move.PollAnswer{}, err
	}

	defer release()

	return a.moveAdvanceLocked(ctx, role, id)
}

func (a *app) moveAdvanceLocked(ctx context.Context, role, id string) (move.PollAnswer, error) {
	var (
		j       move.Journal
		pending string
		wait    bool
		done    bool
	)

	// The state lock is held while the role is asked, parked and begun down; not while it exits, nor while git talks to the remote.
	err := a.withState(func(s *shift.Shift) error {
		var (
			open bool
			err  error
		)

		layout := s.Crew.Layout()

		if j, open, err = move.Open(layout, role); err != nil {
			return err
		}

		if !open || j.ID != id {
			return fmt.Errorf("no move %s of %s is open here", id, role)
		}

		if j.Phase == move.PhaseReleased || j.Phase == move.PhaseReady {
			return nil
		}

		if j.Phase == move.PhaseBegun {
			asked, err := s.MoveAsk(role)
			if err != nil {
				return err
			}

			_ = asked
			j.Phase, j.Asked = move.PhaseAsked, s.Now()

			if err := move.Save(layout, j); err != nil {
				return err
			}
		}

		waiting, parked, err := s.MovePark(role, j.Asked, moveGrace)
		if err != nil {
			return err
		}

		if waiting != "" {
			pending = waiting

			return nil
		}

		// What the role parked itself is in the journal from the beginning; what zmina parked now, and a task given meanwhile, are added.
		for _, t := range parked {
			if !slices.ContainsFunc(j.Tasks, func(jt move.Task) bool { return jt.ID == t.ID }) {
				j.Tasks = append(j.Tasks, move.Task{ID: t.ID})
			}
		}

		// A task the role closed during its grace (done, dropped) is no work to carry: it leaves the journal. One that is neither parked
		// nor closed was changed by hand, which is the owner's to look at.
		kept := make([]move.Task, 0, len(j.Tasks))

		for _, jt := range j.Tasks {
			t, err := s.Tasks.Get(jt.ID)
			if err != nil {
				return err
			}

			switch {
			case t.State == task.Parked:
				jt.Item = t.Later
				kept = append(kept, jt)
			case !t.State.Active():
				s.Log(role, "move", t.ID, t.ID+" is "+string(t.State)+": left out of move "+j.ID)
			default:
				return fmt.Errorf("%s is %s: a move carries a task parked, and this one was changed by hand", t.ID, t.Status())
			}
		}

		j.Tasks = kept

		if wait, err = s.MoveBeginDown(role, j.Force); err != nil {
			pending = "waiting to put " + role + " down: " + err.Error()
			wait = false

			return nil
		}

		done = true

		return move.Save(layout, j)
	})
	if err != nil {
		return move.PollAnswer{}, err
	}

	if pending != "" {
		return answerOf(j, pending), nil
	}

	if done {
		if wait {
			if s, err := a.newShift(); err == nil {
				s.WaitGone([]string{role}, shift.ExitWait)
			}
		}

		err := a.withState(func(s *shift.Shift) error {
			if err := s.FinishDown(role); err != nil {
				return err
			}

			j.Phase = move.PhaseReleased

			return move.Save(s.Crew.Layout(), j)
		})
		if err != nil {
			return move.PollAnswer{}, err
		}

		if err := a.fault("released"); err != nil {
			return move.PollAnswer{}, err
		}
	}

	if j.Phase == move.PhaseReady {
		return answerOf(j, ""), nil
	}

	return a.moveHandoff(ctx, j)
}

// moveHandoff is the git of a move and the bundle: each task's branch goes to origin as it is, and the uncommitted work of its worktree,
// made one commit with the move's trailer, goes there too, under a private ref and never on the branch; the work that travels is put in
// the journal. It holds no state lock: git talks to a remote, and the role is down and kept down by the journal, so nothing else
// changes what it works on. It runs under the move's lock, so nothing else writes the journal meanwhile either.
func (a *app) moveHandoff(ctx context.Context, j move.Journal) (move.PollAnswer, error) {
	cr, err := crew.Load(a.crewPath())
	if err != nil {
		return move.PollAnswer{}, err
	}

	layout := cr.Layout()
	b := move.Bundle{Version: move.Version, Move: j.ID, Role: j.Role, From: j.From, To: j.To, Base: j.Base, After: j.After,
		Dir: move.Portable(cr.Dir), Namespace: move.Portable(cr.Namespace)}

	store := taskStore(cr)

	for i := range j.Tasks {
		jt := &j.Tasks[i]

		t, err := store.Get(jt.ID)
		if err != nil {
			return move.PollAnswer{}, err
		}

		bt, err := a.moveHandTask(ctx, cr, j, jt, t)
		if err != nil {
			// What is wrong is told on every poll, and git is tried again: a remote that was down, a branch that moved.
			why := "handing over " + t.ID + ": " + err.Error()
			if ctx.Err() != nil {
				why += fmt.Sprintf(" (the ticker's beat gives a step %s; zmina move %s %s --resume has no such limit)", moveBeatBudget, j.Role, j.To)
			}

			return answerOf(j, why), nil
		}

		b.Tasks = append(b.Tasks, bt)

		if err := move.Save(layout, j); err != nil {
			return move.PollAnswer{}, err
		}
	}

	raw, err := b.Marshal()
	if err != nil {
		return move.PollAnswer{}, err
	}

	j.Phase, j.Bundle = move.PhaseReady, raw
	if err := move.Save(layout, j); err != nil {
		return move.PollAnswer{}, err
	}

	if err := a.fault("ready"); err != nil {
		return move.PollAnswer{}, err
	}

	return answerOf(j, ""), nil
}

// moveHandTask is the task's part of the bundle, after its branch is on origin: the branch as it is, and the commit of the worktree's
// uncommitted work on the private ref of the move there. The journal is saved as soon as that commit is made, before anything is pushed:
// an abort that finds the journal finds the ref.
func (a *app) moveHandTask(ctx context.Context, cr *crew.Crew, j move.Journal, jt *move.Task, t task.Task) (move.BTask, error) {
	bt := move.BTask{ID: t.ID, Asked: t.Asked, Title: t.Title, Criteria: t.Criteria, Project: t.Project, URL: t.URL, Budget: t.Budget,
		Note: t.Note, Incomplete: t.HandoffIncomplete}

	if t.Repo != "" {
		origin, err := worktree.Origin(t.Repo)
		if err != nil {
			return bt, err
		}

		if origin == "" {
			return bt, fmt.Errorf("the repository %s has no origin: a move carries a branch by origin (git remote add origin <url>)", t.Repo)
		}

		bt.Repo, bt.Origin = move.Portable(t.Repo), origin
		jt.Repo = bt.Repo
	}

	if t.Branch != "" {
		tree := &worktree.Tree{Repo: t.Repo, Path: t.Worktree, Branch: t.Branch, Base: t.Base}
		n := move.TaskNumber(t.ID)

		if jt.WIP == "" && dirExists(t.Worktree) {
			commit, made, err := tree.CommitWIP(j.ID, n)
			if err != nil {
				return bt, err
			}

			if made {
				jt.WIP = commit

				if err := move.Save(cr.Layout(), j); err != nil {
					return bt, err
				}
			}
		}

		tip, err := worktree.Branch(t.Repo, t.Branch)
		if err != nil {
			return bt, err
		}

		if tip == "" {
			return bt, fmt.Errorf("the branch %s is gone from %s", t.Branch, t.Repo)
		}

		if jt.WIP != "" {
			if parent, err := worktree.RevParse(t.Repo, jt.WIP+"^"); err != nil || parent != tip {
				return bt, fmt.Errorf("the branch %s moved since the uncommitted work was committed (it is at %s, the work is over %s)", t.Branch,
					tip[:12], strings.TrimSpace(parent))
			}
		}

		if !jt.Pushed || jt.Tip != tip {
			if err := worktree.PushContext(ctx, t.Repo, t.Branch); err != nil {
				return bt, err
			}
		}

		jt.Branch, jt.Tip, jt.Pushed = t.Branch, tip, true

		if jt.WIP != "" && !jt.WIPPushed {
			if err := worktree.PushWIP(ctx, t.Repo, jt.WIP, j.ID, n); err != nil {
				return bt, err
			}

			jt.WIPPushed = true
		}

		bt.Branch, bt.Base, bt.Fork, bt.Tip, bt.WIP = t.Branch, t.Base, t.Fork, tip, jt.WIP
	}

	for kind, ref := range map[string]*record.Ref{"checkpoint": t.Checkpoint, "result": t.Result} {
		if ref == nil {
			continue
		}

		src := ref.Task
		if src == "" {
			src = t.ID
		}

		d, err := record.Load(cr.StateDir(), kind, src, ref.Revision)
		if err != nil {
			// A record that is gone is no reason to keep the work here: the handoff says it is incomplete.
			bt.Incomplete = true

			continue
		}

		if kind == "checkpoint" {
			bt.Checkpoint = &d
		} else {
			bt.Result = &d
		}
	}

	return bt, nil
}

func dirExists(p string) bool {
	st, err := os.Stat(p)

	return err == nil && st.IsDir()
}

// moveRefAt is a private ref of a move in a repository of this node.
type moveRefAt struct {
	repo, ref string
}

// moveRefsOf are the private refs of the journal's move in the repositories of its tasks: the local ones that are there, found by
// listing (so that a ref a crash left before the journal was saved is found as well), and the ones of the tasks that carried uncommitted
// work, which are on origin too.
func moveRefsOf(s *shift.Shift, j move.Journal) []moveRefAt {
	var out []moveRefAt

	seen := map[string]bool{}

	add := func(repo, ref string) {
		if key := repo + "\x00" + ref; !seen[key] {
			seen[key] = true
			out = append(out, moveRefAt{repo, ref})
		}
	}

	for _, jt := range j.Tasks {
		t, err := s.Tasks.Get(jt.ID)
		if err != nil || t.Repo == "" {
			continue
		}

		if refs, err := worktree.MoveRefs(t.Repo, j.ID); err == nil {
			for _, r := range refs {
				add(t.Repo, r)
			}
		}

		if jt.WIP != "" {
			add(t.Repo, worktree.MoveRef(j.ID, move.TaskNumber(jt.ID)))
		}
	}

	return out
}

// dropMoveRefs takes the private refs of a move away: the local ones now, and origin's as far as origin answers in the time given (a
// ref that stays there is clutter, not work: nothing is lost by it).
func dropMoveRefs(ctx context.Context, refs []moveRefAt) (left []string) {
	for _, r := range refs {
		_ = worktree.DropRef(r.repo, r.ref)
	}

	ctx, cancel := context.WithTimeout(ctx, moveRefTimeout)
	defer cancel()

	done := map[string]bool{}

	for _, r := range refs {
		if key := r.repo + "\x00" + r.ref; !done[key] {
			done[key] = true

			if err := worktree.DeleteRemoteRef(ctx, r.repo, r.ref); err != nil {
				left = append(left, r.ref+" ("+err.Error()+")")
			}
		}
	}

	return left
}

// moveRefTimeout is how long taking the private refs of a move off origin may take after the move is over or called off.
const moveRefTimeout = 10 * time.Second

// lockMove takes the move's lock for a step that must not run beside another: it waits for it up to moveLockWait.
func lockMove(layout crew.Layout, role string) (func(), error) {
	release, err := move.Lock(layout, role, moveLockWait)
	if errors.Is(err, move.ErrBusy) {
		return nil, fmt.Errorf("a step of the move of %s is still running on this node (a push to origin, most likely): try again in a minute", role)
	}

	return release, err
}

// moveCommit ends the move at the node the role left: each parked item is closed as taken by the task it became there, the role's place
// is flipped in this node's file (the same edit the other end made, so the two files are one file again), and the journal is closed.
// A commit made again, once the journal is gone and the file places the role where the body says, is the same commit.
func (a *app) moveCommit(ctx context.Context, call mesh.MoveCall) (move.PollAnswer, error) {
	var body move.CommitBody

	if err := json.Unmarshal(call.Payload, &body); err != nil || !move.ValidNode(body.To) {
		return move.PollAnswer{}, errors.New("commit: the body names the node the role went to and the tasks it took")
	}

	for from, to := range body.Taken {
		if !move.ValidTask(from) || !strings.HasPrefix(to, body.To+"/") || !move.ValidTask(strings.TrimPrefix(to, body.To+"/")) {
			return move.PollAnswer{}, fmt.Errorf("commit: %q is no task of %s", to, body.To)
		}
	}

	cr, err := crew.Load(a.crewPath())
	if err != nil {
		return move.PollAnswer{}, err
	}

	// A poll that is in a push is let finish: it would write the journal after this closed it.
	release, err := lockMove(cr.Layout(), call.Role)
	if err != nil {
		return move.PollAnswer{}, err
	}

	defer release()

	var (
		out  move.PollAnswer
		refs []moveRefAt
	)

	err = a.withState(func(s *shift.Shift) error {
		cr, role := s.Crew, call.Role
		layout := cr.Layout()

		j, open, err := move.Open(layout, role)

		switch {
		case err != nil:
			return err
		case !open:
			if r := cr.Roles[role]; r != nil && cr.PlacementOf(r) == body.To {
				out = move.PollAnswer{State: move.StateCommitted, Why: "the move was ended already"}

				return nil
			}

			return fmt.Errorf("no move %s of %s is open here", call.Move, role)
		case j.ID != call.Move:
			return fmt.Errorf("the move open here is %s, not %s", j.ID, call.Move)
		case j.Phase != move.PhaseReady:
			return fmt.Errorf("the work of %s is not handed over yet (%s)", call.Move, j.Phase)
		case body.To != j.To:
			return fmt.Errorf("the move goes to %s, not %s", j.To, body.To)
		}

		for from := range body.Taken {
			if !slices.ContainsFunc(j.Tasks, func(jt move.Task) bool { return jt.ID == from }) {
				return fmt.Errorf("%s is no task of this move", from)
			}
		}

		if err := s.MoveCommit(j, body.Taken); err != nil {
			return err
		}

		if err := a.fault("commit-items"); err != nil {
			return err
		}

		if _, err := a.placeRole(cr, role, j.To, j.Base, j.After, call.By); err != nil {
			return err
		}

		if err := a.fault("commit-flip"); err != nil {
			return err
		}

		out = move.PollAnswer{State: move.StateCommitted, Why: "closed"}
		refs = moveRefsOf(s, j)

		return move.Close(layout, role)
	})
	if err != nil {
		return out, err
	}

	// The move is over: its private refs have done their work (the other node took the work, or it could not have said so).
	if left := dropMoveRefs(ctx, refs); len(left) > 0 {
		out.Why += "; left on origin: " + strings.Join(left, ", ")
	}

	return out, nil
}

// placeCheck is what placeRole asks before it writes anything, and what a take asks before it keeps its receipt: the file here is the one
// the move was made from (base) or already the one it makes (after), and the place edit this node makes of it is the one the move says
// (after): a node whose zmina writes the edit differently would otherwise leave the two files two files and say they were one. changed
// says the file has still to be written.
func placeCheck(cr *crew.Crew, role, node, base, after string) (raw []byte, changed bool, err error) {
	cur := cr.Sum()

	raw, rerr := os.ReadFile(cr.Path)
	if rerr == nil {
		cur = crew.SumOf(raw)
	}

	switch cur {
	case after:
		return raw, false, nil
	case base:
	default:
		return nil, false, fmt.Errorf("the crew file here is %s, not the one the move was made from (%s): it was changed meanwhile; "+
			"zmina crew push, then zmina move again", cur[:12], base[:12])
	}

	if rerr != nil {
		return nil, false, fmt.Errorf("crew: %w", rerr)
	}

	edited, err := crew.PlaceAfter(raw, role, node)
	if err != nil {
		return nil, false, err
	}

	if got := crew.SumOf(edited); got != after {
		return nil, false, fmt.Errorf("the place edit makes a crew file of %s here, not %s that the move was made for: the two nodes' zmina write the "+
			"file differently (update the older one); nothing was written", got[:12], after[:12])
	}

	return raw, true, nil
}

// placeRole writes the move into this node's crew file: place: <node> for the role, the same edit on both nodes (placeCheck says it is the
// edit the move means). It returns the sum of the file there.
func (a *app) placeRole(cr *crew.Crew, role, node, base, after, by string) (string, error) {
	_, changed, err := placeCheck(cr, role, node, base, after)
	if err != nil {
		return "", err
	}

	if !changed {
		return after, nil
	}

	saved, sum, _, err := crew.MovePlace(cr.Path, role, node, base, by, "zmina move")
	if err != nil {
		return "", err
	}

	if sum != after {
		return "", fmt.Errorf("the crew file written here is %s, not %s that the move was made for", sum[:12], after[:12])
	}

	logCrewVersions(eventLog(cr), saved.Versions)

	return sum, nil
}

// moveAbort calls the move off at the node the role left: the parked tasks are open again, the private refs the move made (of the
// uncommitted work: the worktrees themselves were never touched) are taken away, the items are closed, the journal is gone, and the role
// is started on its work again. It does not wait for a step of the move that is running at the node: it waits for it up to a time, then
// says so.
func (a *app) moveAbort(ctx context.Context, call mesh.MoveCall) (move.PollAnswer, error) {
	cr, err := crew.Load(a.crewPath())
	if err != nil {
		return move.PollAnswer{}, err
	}

	release, err := lockMove(cr.Layout(), call.Role)
	if err != nil {
		return move.PollAnswer{}, err
	}

	defer release()

	var (
		out  move.PollAnswer
		refs []moveRefAt
	)

	err = a.withState(func(s *shift.Shift) error {
		role := call.Role
		layout := s.Crew.Layout()

		j, open, err := move.Open(layout, role)

		switch {
		case err != nil:
			return err
		case !open:
			return fmt.Errorf("no move %s of %s is open here", call.Move, role)
		case j.ID != call.Move:
			return fmt.Errorf("the move open here is %s, not %s", j.ID, call.Move)
		}

		refs = moveRefsOf(s, j)

		back, err := s.MoveUnpark(j)
		if err != nil {
			return err
		}

		if err := move.Close(layout, role); err != nil {
			return err
		}

		s.Log(role, "move-aborted", j.ID, "called off")

		out = move.PollAnswer{State: move.StateAborted, Why: "open again: " + taskIDsOf(back)}

		if len(back) == 0 {
			return nil
		}

		if started, why := s.StartMoved(back); !started {
			out.Why += "; not started: " + why
		} else if why != "" {
			out.Why += "; started, " + why
		} else {
			out.Why += "; " + role + " is up on it"
		}

		return nil
	})
	if err != nil {
		return out, err
	}

	if left := dropMoveRefs(ctx, refs); len(left) > 0 {
		out.Why += "; left on origin: " + strings.Join(left, ", ")
	}

	return out, nil
}

// moveCancel is the abort's step at the node the role would go to: a take that runs, or is done (its receipt is there), is no move to
// call off, and it says so; what a take that was cut short made is taken away; and the tombstone of the move is left, so that no take of
// it runs afterwards (a stale owner process that still holds the bundle is refused). The answer says which.
func (a *app) moveCancel(call mesh.MoveCall) (move.PollAnswer, error) {
	cr, err := crew.Load(a.crewPath())
	if err != nil {
		return move.PollAnswer{}, err
	}

	layout := cr.Layout()

	if rec, ok, err := move.Receipt(layout, call.Move); err != nil {
		return move.PollAnswer{}, err
	} else if ok {
		return move.PollAnswer{State: move.StateTaken, Taken: &rec}, nil
	}

	// The take holds this lock from its first side effect to its end: a cancel never meets one half-way.
	release, err := move.Lock(layout, call.Role, 0)
	if errors.Is(err, move.ErrBusy) {
		return move.PollAnswer{State: move.StateTaking, Why: "a take of " + call.Move + " is running here"}, nil
	}

	if err != nil {
		return move.PollAnswer{}, err
	}

	defer release()

	var out move.PollAnswer

	err = a.withState(func(s *shift.Shift) error {
		layout := s.Crew.Layout()

		// A take that ended while this waited for the lock.
		if rec, ok, err := move.Receipt(layout, call.Move); err != nil {
			return err
		} else if ok {
			out = move.PollAnswer{State: move.StateTaken, Taken: &rec}

			return nil
		}

		why := "a tombstone of the move is left: a take of it is refused here"

		if m, ok, err := move.OpenTaking(layout, call.Role); err != nil {
			return err
		} else if ok && m.ID == call.Move {
			if err := a.cleanupTake(s, m); err != nil {
				return err
			}

			why = "what a take that was cut short made is taken away, and " + why
		}

		if err := move.SaveAborted(layout, move.Tombstone{ID: call.Move, Role: call.Role, At: s.Now()}); err != nil {
			return err
		}

		s.Log(call.Role, "move-cancelled", call.Move, "no take of it runs here")

		out = move.PollAnswer{State: move.StateAborted, Why: why}

		return nil
	})

	return out, err
}

// cleanupTake takes away what a take that did not get its receipt made: the tasks it imported (found by their origin, the role, the
// move's id in their note and an active state) are dropped with their worktrees, its private refs go, and so does its marker. It runs
// under the state lock, and under the move's lock, so no take is running; it is the first thing the next take of the move does, and the
// abort's.
func (a *app) cleanupTake(s *shift.Shift, m move.Taking) error {
	all, err := s.Tasks.All()
	if err != nil {
		return err
	}

	now := s.Now()
	worktrees := s.Crew.Layout().Worktrees() + string(filepath.Separator)

	for _, t := range all {
		if t.Role != m.Role || !t.State.Active() || !slices.Contains(m.Origins, t.Origin) || !strings.Contains(t.Note, "(move "+m.ID+")") {
			continue
		}

		// The worktree the take made, never one of the role's own: it is under the crew's worktrees, by the task's id, on the task's branch.
		if t.Worktree != "" && strings.HasPrefix(t.Worktree, worktrees) && worktree.IsWorktreeOf(t.Repo, t.Worktree, t.Branch) {
			if err := worktree.RemoveWorktree(t.Repo, t.Worktree); err != nil {
				return fmt.Errorf("%s: the worktree %s the take made could not be removed: %w", t.ID, t.Worktree, err)
			}
		}

		note := "a take of move " + m.ID + " was cut short and taken away: the work is still where it came from"
		t.State, t.Updated, t.Note = task.Dropped, now, strings.TrimSpace(t.Note+" · "+note)
		t.Closed = &task.Closure{By: "zmina move", At: now, State: task.Dropped, Note: note}

		if err := s.Tasks.Put(t); err != nil {
			return fmt.Errorf("%s: %w", t.ID, err)
		}

		s.Log(m.Role, "move", t.ID, "dropped: "+note)
	}

	for _, repo := range m.Repos {
		if refs, err := worktree.MoveRefs(repo, m.ID); err == nil {
			for _, r := range refs {
				_ = worktree.DropRef(repo, r)
			}
		}
	}

	return move.CloseTaking(s.Crew.Layout(), m.Role)
}

func taskIDsOf(ts []task.Task) string {
	ids := make([]string, 0, len(ts))
	for _, t := range ts {
		ids = append(ids, t.ID)
	}

	if len(ids) == 0 {
		return "no task"
	}

	return strings.Join(ids, ", ")
}

// noCrewForMove says what a node without the crew does about it: the owner sends it the file.
func noCrewForMove(key string, err error) error {
	return fmt.Errorf("%w on this node: zmina crew push <node> --create sends %s the crew file first", err, key)
}
