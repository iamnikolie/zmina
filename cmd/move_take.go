package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/iamnikolie/zmina/internal/worktree"
)

// localRoleTake is the work of a move handed to this node (role/take): the node the role goes to takes it, or says why it cannot.
func (a *app) localRoleTake(call mesh.MoveCall) ([]byte, error) {
	if err := mesh.CheckMoveCall("role/take", call); err != nil {
		return nil, err
	}

	c, err := a.moveApp(call.Crew)
	if err != nil {
		return nil, noCrewForMove(call.Crew, err)
	}

	ans, err := c.moveTake(call)
	if err != nil {
		return nil, err
	}

	return json.Marshal(ans)
}

// crewRepo reports whether the repository at abs is one of this crew's: the crew's own directory, a role's directory, or a repository
// directly under the crew's directory, or one a task of this node's ledger already works in. The bundle names a repository by its top
// level, home-relative; nothing outside the crew's is looked at on another node's word.
func crewRepo(cr *crew.Crew, known []task.Task, abs string) bool {
	if cr.Dir != "" && (samePlace(cr.Dir, abs) || samePlace(filepath.Dir(abs), cr.Dir)) {
		return true
	}

	for _, name := range cr.Names() {
		if r := cr.Roles[name]; r != nil && r.Dir != "" {
			if top, err := worktree.TopLevel(r.Dir); err == nil && samePlace(top, abs) {
				return true
			}
		}
	}

	for _, t := range known {
		if t.Repo != "" && samePlace(t.Repo, abs) {
			return true
		}
	}

	return false
}

// moveTake takes the work of a move. It checks the bundle against this node (the file is the one the move was made from, the role is
// placed where the bundle says, the crew's directory and namespace are the same, every repository is one of the crew's with the same
// origin), fetches each branch from origin and checks the tip is the commit the bundle names, makes the tasks here under new ids with
// their origin, gives each a worktree on its branch (the commit a move made of uncommitted work undone), saves the checkpoint and the
// result again under the new id, puts the role in place in the file, keeps a receipt, and starts the role fresh on its work. Taken again
// with the same move id it is the same answer.
func (a *app) moveTake(call mesh.MoveCall) (move.TakeAnswer, error) {
	me, err := openNode()
	if err != nil {
		return move.TakeAnswer{}, err
	}

	cr, err := crew.Load(a.crewPath())
	if err != nil {
		return move.TakeAnswer{}, err
	}

	layout := cr.Layout()

	if rec, ok, err := move.Receipt(layout, call.Move); err != nil {
		return move.TakeAnswer{}, err
	} else if ok {
		if !rec.Started && len(rec.Tasks) > 0 {
			rec = a.moveStart(layout, rec)
		}

		return rec, nil
	}

	b, err := move.ParseBundle(call.Payload)
	if err != nil {
		return move.TakeAnswer{}, err
	}

	role := call.Role

	r := cr.Roles[role]

	switch {
	case b.Move != call.Move || b.Role != role:
		return move.TakeAnswer{}, errors.New("the bundle is of another move or another role than the request")
	case call.Base != "" && call.Base != b.Base:
		return move.TakeAnswer{}, errors.New("the bundle was made from another crew file than the request says")
	case b.To != me.Name:
		return move.TakeAnswer{}, fmt.Errorf("the bundle is for %s, not %s", b.To, me.Name)
	case !cr.Spread():
		return move.TakeAnswer{}, errors.New("this node's crew file declares no nodes: at all")
	case !cr.HasNode(me.Name) || !cr.HasNode(b.From):
		return move.TakeAnswer{}, fmt.Errorf("nodes: here does not list %s and %s", b.From, me.Name)
	case r == nil:
		return move.TakeAnswer{}, fmt.Errorf("no role %q in the crew file here", role)
	case role == cr.Dispatcher:
		return move.TakeAnswer{}, errors.New(role + " is the dispatcher and is not moved")
	case move.OpenID(layout, role) != "":
		return move.TakeAnswer{}, fmt.Errorf("%s has a move off this node open here (%s)", role, move.OpenID(layout, role))
	}

	// The file: the one the move was made from, with the role placed on the node it leaves, or already the one the move makes.
	switch cr.Sum() {
	case b.Base:
		if cr.PlacementOf(r) != b.From {
			return move.TakeAnswer{}, fmt.Errorf("the file here places %s on %s, not on %s where the move found it", role, cr.PlacementOf(r), b.From)
		}
	case b.After:
		if cr.PlacementOf(r) != me.Name {
			return move.TakeAnswer{}, fmt.Errorf("the file here is the one the move makes, but it places %s on %s", role, cr.PlacementOf(r))
		}
	default:
		return move.TakeAnswer{}, fmt.Errorf("crew file differs: %s holds %s, the move was made from %s: zmina crew push %s", me.Name,
			cr.Sum()[:12], b.Base[:12], me.Name)
	}

	if got := move.Portable(cr.Dir); got != b.Dir {
		return move.TakeAnswer{}, fmt.Errorf("crew dir differs: %s here, %s on %s", got, b.Dir, b.From)
	}

	if got := move.Portable(cr.Namespace); got != b.Namespace {
		return move.TakeAnswer{}, fmt.Errorf("namespace differs: %s here, %s on %s", got, b.Namespace, b.From)
	}

	var known []task.Task

	if all, err := taskStore(cr).All(); err == nil {
		known = all
	}

	repos := make([]string, len(b.Tasks))

	for i, bt := range b.Tasks {
		if bt.Repo == "" {
			continue
		}

		abs := move.Expand(bt.Repo)

		switch f := repoFact(bt.Repo); {
		case !f.Found:
			return move.TakeAnswer{}, fmt.Errorf("repo %s (of %s) is no repository's top level on %s", bt.Repo, bt.ID, me.Name)
		case f.Origin != bt.Origin:
			return move.TakeAnswer{}, fmt.Errorf("repo %s: origin differs: %s on %s, %s here", bt.Repo, bt.Origin, b.From, orNone(f.Origin))
		case !crewRepo(cr, known, abs):
			return move.TakeAnswer{}, fmt.Errorf("repo %s is none of this crew's (its directory, a role's, one under it, or one a task here works in)", bt.Repo)
		}

		repos[i] = abs
	}

	// Git: each branch comes from origin, and its tip is the commit the bundle names; the branch is made here at it, unless it is there
	// (an earlier try of this take got that far).
	for i, bt := range b.Tasks {
		if bt.Branch == "" {
			continue
		}

		if err := a.takeBranch(repos[i], b.Move, i, bt); err != nil {
			return move.TakeAnswer{}, fmt.Errorf("%s: %w", bt.ID, err)
		}
	}

	if err := a.fault("taken-git"); err != nil {
		return move.TakeAnswer{}, err
	}

	var (
		ans   = move.TakeAnswer{Move: b.Move, Node: me.Name, Role: role, Sum: b.After, At: time.Now().UTC()}
		taken []string
	)

	err = a.withState(func(s *shift.Shift) error {
		all, err := s.Tasks.All()
		if err != nil {
			return err
		}

		for i, bt := range b.Tasks {
			origin := b.From + "/" + bt.ID

			t, found := task.Task{}, false

			for _, old := range all {
				if old.Origin == origin && old.Role == role && old.State != task.Dropped {
					t, found = old, true

					break
				}
			}

			if !found {
				if t, err = importTask(s, b, bt, repos[i], call.By); err != nil {
					return fmt.Errorf("%s: %w", bt.ID, err)
				}
			}

			if t.Branch != "" {
				if !worktree.IsWorktreeOf(t.Repo, t.Worktree, t.Branch) {
					if _, err := worktree.AddExisting(t.Repo, t.Worktree, t.Branch); err != nil {
						return fmt.Errorf("%s: the worktree of %s: %w", t.ID, bt.Branch, err)
					}
				}

				if bt.WIP {
					if _, err := (&worktree.Tree{Repo: t.Repo, Path: t.Worktree, Branch: t.Branch}).UndoWIP(b.Move); err != nil {
						return fmt.Errorf("%s: undoing the commit that carried the uncommitted work: %w", t.ID, err)
					}
				}
			}

			ans.Tasks = append(ans.Tasks, move.Taken{From: bt.ID, To: t.ID})
			taken = append(taken, t.ID)
		}

		if err := a.fault("taken-import"); err != nil {
			return err
		}

		if err := a.placeRole(s.Crew, role, me.Name, b.Base, b.After, call.By); err != nil {
			return err
		}

		if err := a.fault("taken-flip"); err != nil {
			return err
		}

		if err := move.SaveReceipt(layout, ans); err != nil {
			return err
		}

		text := fmt.Sprintf("%s moved here from %s (move %s)", role, b.From, b.Move)
		if len(taken) > 0 {
			text += ": " + strings.Join(taken, ", ")
		}

		s.Log(role, "move-taken", b.Move, text)

		if _, err := s.NoteOnce("move-in/"+b.Move, "zmina", text, true); err != nil {
			s.Warn(fmt.Errorf("the dispatcher was not told of the move: %w", err))
		}

		return a.fault("taken")
	})
	if err != nil {
		return move.TakeAnswer{}, err
	}

	// The branches' private refs have done their work.
	for i, bt := range b.Tasks {
		if bt.Branch != "" {
			_ = worktree.DropRef(repos[i], fmt.Sprintf("refs/zmina-move/%s/%d", b.Move, i))
		}
	}

	return a.moveStart(layout, ans), nil
}

// takeBranch fetches the branch of origin into a private ref of the move, checks that it is the commit the bundle names, and makes the
// local branch at it.
func (a *app) takeBranch(repo, id string, i int, bt move.BTask) error {
	ref := fmt.Sprintf("refs/zmina-move/%s/%d", id, i)

	sha, err := worktree.Fetch(repo, bt.Branch, ref)
	if err != nil {
		return err
	}

	if sha != bt.Tip {
		return fmt.Errorf("origin has %s at %s, not at %s that the move pushed: the branch moved since", bt.Branch, sha[:12], bt.Tip[:12])
	}

	have, err := worktree.Branch(repo, bt.Branch)
	if err != nil {
		return err
	}

	switch have {
	case "":
		return worktree.MakeBranch(repo, bt.Branch, sha)
	case sha:
		return nil
	}

	// A take that got as far as undoing the commit of the uncommitted work left the branch at its parent.
	if bt.WIP {
		if parent, err := worktree.RevParse(repo, sha+"^"); err == nil && parent == have {
			return nil
		}
	}

	// A branch this node had of its own (the task came here before, and went away, and comes back): it goes forward to the commit the
	// move carries when that is a descendant of it and no worktree has it checked out. A worktree that has it is an earlier task's, and
	// is not moved under.
	if where, err := worktree.CheckedOutAt(repo, bt.Branch); err != nil {
		return err
	} else if where != "" {
		return fmt.Errorf("the branch %s is checked out in %s, an earlier task's worktree of this role here: remove it (git worktree remove, "+
			"zmina task gc), then --resume", bt.Branch, where)
	}

	if ahead, err := worktree.IsAncestor(repo, have, sha); err == nil && ahead {
		return worktree.FastForward(repo, bt.Branch, have, sha)
	}

	return fmt.Errorf("the branch %s is here already, at %s, which is not an ancestor of the commit the move carries (%s)", bt.Branch, have[:12], sha[:12])
}

// importTask makes the task here, under the next id, from what the bundle carries, with its origin; the worktree path is this node's
// own, by the new id; the checkpoint and the result are saved again under the new id.
func importTask(s *shift.Shift, b move.Bundle, bt move.BTask, repo, by string) (task.Task, error) {
	now := s.Now()
	origin := b.From + "/" + bt.ID

	note := strings.TrimSpace(bt.Note + " · moved from " + origin + " with " + b.Role + " (move " + b.Move + ")")

	t := task.Task{Role: b.Role, Asked: bt.Asked, State: task.Open, Title: bt.Title, Project: bt.Project, URL: bt.URL, Budget: bt.Budget,
		Criteria: bt.Criteria, Note: note, By: by, Origin: origin, HandoffIncomplete: bt.Incomplete}

	if repo != "" {
		t.Repo = repo

		if bt.Branch != "" {
			t.Branch, t.Base, t.Fork = bt.Branch, bt.Base, bt.Fork
		}
	}

	t, err := s.Tasks.Add(t, now)
	if err != nil {
		return t, err
	}

	if t.Branch != "" {
		t.Worktree = filepath.Join(s.Crew.Layout().Worktrees(), filepath.Base(repo)+"-"+t.ID)
	}

	for kind, doc := range map[string]*record.Document{"checkpoint": bt.Checkpoint, "result": bt.Result} {
		if doc == nil {
			continue
		}

		d := *doc
		d.Task, d.Role = t.ID, b.Role
		d.By = "moved from " + origin + " (" + d.By + ")"

		ref, err := record.Save(s.Crew.StateDir(), d)
		if err != nil {
			return t, fmt.Errorf("the %s: %w", kind, err)
		}

		if kind == "result" {
			t.Result = &ref
		} else {
			t.Checkpoint = &ref

			if !bt.Incomplete {
				t.CheckpointTaskUpdated = t.Updated
			}
		}
	}

	return t, s.Tasks.Put(t)
}

// moveStart starts the role on the tasks the take made, fresh (a task that came from another node has no conversation here to
// continue), and keeps what came of it in the receipt: whether it is up and, if not, why. A role that holds no task is not started: its
// keep policy says whether this node's ticker does.
func (a *app) moveStart(layout crew.Layout, ans move.TakeAnswer) move.TakeAnswer {
	if len(ans.Tasks) == 0 {
		ans.Started, ans.Why = false, ans.Role+" holds no task, so it was not started: keep: always starts it here, any other seat with zmina up "+ans.Role

		_ = move.SaveReceipt(layout, ans)

		return ans
	}

	err := a.withState(func(s *shift.Shift) error {
		// The role came here on purpose: a hold from an earlier time it was put down on this node (a move away) is lifted.
		rs := s.State.Role(ans.Role)
		rs.Held, rs.Broken = false, false

		ids := make([]string, 0, len(ans.Tasks))
		for _, t := range ans.Tasks {
			ids = append(ids, t.To)
		}

		ts, err := s.MoveTasks(ids)
		if err != nil {
			return err
		}

		ts = slices.DeleteFunc(ts, func(t task.Task) bool { return !t.State.Active() })

		ans.Started, ans.Why = s.StartMoved(ts)

		return nil
	})
	if err != nil {
		ans.Started, ans.Why = false, err.Error()
	}

	_ = move.SaveReceipt(layout, ans)

	return ans
}
