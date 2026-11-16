package cmd

import (
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
)

// fault is where a test makes a node's process "die": the hook returns an error for the phase it names, and the step ends there, with
// whatever was written so far left as it is. It is never set outside tests.
func (a *app) fault(phase string) error {
	if a.moveFault == nil {
		return nil
	}

	return a.moveFault(phase)
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
		out, err = c.movePoll(call)
	case "commit":
		out, err = c.moveCommit(call)
	case "abort":
		out, err = c.moveAbort(call)
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

	var out move.PollAnswer

	err := a.withState(func(s *shift.Shift) error {
		cr, role := s.Crew, call.Role
		layout := cr.Layout()

		if j, open, err := move.Open(layout, role); err != nil {
			return err
		} else if open {
			if j.ID == call.Move {
				out = answerOf(j, "the move is open already")

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

		return a.fault("asked")
	})

	return out, err
}

// movePoll moves the move on at the node the role leaves, as far as it will go now, and says how far it is; at the node the role went to
// it says whether that node took the work.
func (a *app) movePoll(call mesh.MoveCall) (move.PollAnswer, error) {
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
		return a.moveAdvance(call.Role, call.Move)
	case open:
		return move.PollAnswer{State: move.StateNone, Why: "another move of " + call.Role + " is open here: " + j.ID}, nil
	}

	if rec, ok, err := move.Receipt(layout, call.Move); err != nil {
		return move.PollAnswer{}, err
	} else if ok {
		return move.PollAnswer{State: move.StateTaken, Taken: &rec}, nil
	}

	return move.PollAnswer{State: move.StateNone}, nil
}

// moveAdvance does what the move at this node waits for, one step after another until one has to wait: the role is asked to park
// (again, if the asking did not land), what it did not park in its grace is parked for it, it is put down and kept down, each task's
// branch is pushed to origin (a worktree with uncommitted work is committed first, with the move's trailer), and the bundle is built
// and kept in the journal. Each step is the state as it finds it, so a move that was cut short anywhere goes on from there.
func (a *app) moveAdvance(role, id string) (move.PollAnswer, error) {
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

		for i := range j.Tasks {
			t, err := s.Tasks.Get(j.Tasks[i].ID)
			if err != nil {
				return err
			}

			if t.State != task.Parked {
				return fmt.Errorf("%s is %s: a move carries a task parked, and this one was changed by hand", t.ID, t.Status())
			}

			j.Tasks[i].Item = t.Later
		}

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

	return a.moveHandoff(j)
}

// moveHandoff is the git of a move and the bundle: each task's branch goes to origin (the uncommitted work of its worktree as one commit
// with the move's trailer, the files left as they were), and the work that travels is put in the journal. It holds no state lock: git
// talks to a remote, and the role is down and kept down by the journal, so nothing else changes what it works on.
func (a *app) moveHandoff(j move.Journal) (move.PollAnswer, error) {
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

		bt, err := a.moveHandTask(cr, j, jt, t)
		if err != nil {
			// What is wrong is told on every poll, and git is tried again: a remote that was down, a branch that moved.
			return answerOf(j, "handing over "+t.ID+": "+err.Error()), nil
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

// moveHandTask is the task's part of the bundle, after its branch is on origin.
func (a *app) moveHandTask(cr *crew.Crew, j move.Journal, jt *move.Task, t task.Task) (move.BTask, error) {
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

		if jt.WIP == "" && dirExists(t.Worktree) {
			commit, made, err := tree.CommitWIP(j.ID)
			if err != nil {
				return bt, err
			}

			if made {
				jt.WIP = commit
			}
		}

		tip, err := worktree.Branch(t.Repo, t.Branch)
		if err != nil {
			return bt, err
		}

		if tip == "" {
			return bt, fmt.Errorf("the branch %s is gone from %s", t.Branch, t.Repo)
		}

		// A commit an earlier try of this step made, whose answer was lost.
		if jt.WIP == "" {
			if carries, _ := worktree.CarriesMove(t.Repo, tip, j.ID); carries {
				jt.WIP = tip
			}
		}

		if !jt.Pushed || jt.Tip != tip {
			if err := worktree.Push(t.Repo, t.Branch); err != nil {
				return bt, err
			}
		}

		jt.Branch, jt.Tip, jt.Pushed = t.Branch, tip, true
		bt.Branch, bt.Base, bt.Fork, bt.Tip, bt.WIP = t.Branch, t.Base, t.Fork, tip, jt.WIP != "" && jt.WIP == tip
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

// moveCommit ends the move at the node the role left: each parked item is closed as taken by the task it became there, the role's place
// is flipped in this node's file (the same edit the other end made, so the two files are one file again), and the journal is closed.
// A commit made again, once the journal is gone and the file places the role where the body says, is the same commit.
func (a *app) moveCommit(call mesh.MoveCall) (move.PollAnswer, error) {
	var body move.CommitBody

	if err := json.Unmarshal(call.Payload, &body); err != nil || !move.ValidNode(body.To) {
		return move.PollAnswer{}, errors.New("commit: the body names the node the role went to and the tasks it took")
	}

	for from, to := range body.Taken {
		if !move.ValidTask(from) || !strings.HasPrefix(to, body.To+"/") || !move.ValidTask(strings.TrimPrefix(to, body.To+"/")) {
			return move.PollAnswer{}, fmt.Errorf("commit: %q is no task of %s", to, body.To)
		}
	}

	var out move.PollAnswer

	err := a.withState(func(s *shift.Shift) error {
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

		if err := a.placeRole(cr, role, j.To, j.Base, j.After, call.By); err != nil {
			return err
		}

		if err := a.fault("commit-flip"); err != nil {
			return err
		}

		out = move.PollAnswer{State: move.StateCommitted, Why: "closed"}

		return move.Close(layout, role)
	})

	return out, err
}

// placeRole writes the move into this node's crew file: place: <node> for the role, the same edit on both nodes. The file is the one the
// move was made from (base), or already the one it makes (after).
func (a *app) placeRole(cr *crew.Crew, role, node, base, after, by string) error {
	cur := cr.Sum()

	if raw, err := os.ReadFile(cr.Path); err == nil {
		cur = crew.SumOf(raw)
	}

	switch cur {
	case after:
		return nil
	case base:
	default:
		return fmt.Errorf("the crew file here is %s, not the one the move was made from (%s): it was changed meanwhile; "+
			"zmina crew push, then zmina move again", cur[:12], base[:12])
	}

	saved, _, _, err := crew.MovePlace(cr.Path, role, node, base, by, "zmina move")
	if err != nil {
		return err
	}

	logCrewVersions(eventLog(cr), saved.Versions)

	return nil
}

// moveAbort calls the move off at the node the role left: the parked tasks are open again, the uncommitted work that a commit of the move
// made and that was not pushed is given back to its worktree, the items are closed, the journal is gone, and the role is started on its
// work again.
func (a *app) moveAbort(call mesh.MoveCall) (move.PollAnswer, error) {
	var out move.PollAnswer

	err := a.withState(func(s *shift.Shift) error {
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

		// A commit the move made of a worktree's work, not on origin yet, is undone; one that is on origin stays: the branch there has it.
		for _, jt := range j.Tasks {
			if jt.WIP == "" || jt.Pushed {
				continue
			}

			if t, err := s.Tasks.Get(jt.ID); err == nil && t.Worktree != "" {
				tree := &worktree.Tree{Repo: t.Repo, Path: t.Worktree, Branch: t.Branch}
				if _, err := tree.UndoWIP(j.ID); err != nil {
					return fmt.Errorf("%s: the commit the move made of %s could not be undone: %w", t.ID, t.Worktree, err)
				}
			}
		}

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

	return out, err
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
