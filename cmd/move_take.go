package cmd

import (
	"context"
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
	return a.localRoleTakeCtx(context.Background(), call)
}

// localRoleTakeCtx is localRoleTake that gives up its fetch from origin when ctx ends, as the owner's own steps do in a beat.
func (a *app) localRoleTakeCtx(ctx context.Context, call mesh.MoveCall) ([]byte, error) {
	if err := mesh.CheckMoveCall("role/take", call); err != nil {
		return nil, err
	}

	c, err := a.moveApp(call.Crew)
	if err != nil {
		return nil, noCrewForMove(call.Crew, err)
	}

	ans, err := c.moveTake(ctx, call)
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
// origin), and then, under the role's move lock and with a marker of its own kept from before its first side effect until it has its
// receipt: fetches each branch from origin and checks the tip is the commit the bundle names, makes the tasks here under new ids with
// their origin, gives each a worktree on its branch with the uncommitted work of the other node restored onto it (from the private ref
// the move kept it under on origin), saves the checkpoint and the result again under the new id, keeps the receipt, puts the role in
// place in the file, and starts the role fresh on its work. The receipt is the point of no return: before it the take is all or nothing
// (a take that fails takes away what it made; one whose process died is taken away by the next take of the move, or by the abort), after
// it the take is only finished (the placing and the start are made again until they are). A take refused a tombstone of the move
// (called off), and a take of a move whose marker is another's, make nothing. Taken again with the same move id it is the same answer.
func (a *app) moveTake(ctx context.Context, call mesh.MoveCall) (move.TakeAnswer, error) {
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
		return a.moveTaken(ctx, cr, call, rec)
	}

	if err := refuseCancelled(layout, call.Move); err != nil {
		return move.TakeAnswer{}, err
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

	// One take of a role at a time. A take that is running here answers the one that asks again to try later; its own receipt is the answer
	// then.
	release, err := move.Lock(layout, role, 0)
	if errors.Is(err, move.ErrBusy) {
		return move.TakeAnswer{}, fmt.Errorf("a take of %s is running here already: ask again in a minute (zmina move %s %s --resume %s)", b.Move, role, me.Name, b.Move)
	}

	if err != nil {
		return move.TakeAnswer{}, err
	}

	defer release()

	// What may have happened while the lock was not held: a take that ended, a move called off.
	if rec, ok, err := move.Receipt(layout, call.Move); err != nil {
		return move.TakeAnswer{}, err
	} else if ok {
		return a.moveTaken(ctx, cr, call, rec)
	}

	if err := refuseCancelled(layout, call.Move); err != nil {
		return move.TakeAnswer{}, err
	}

	marker := move.Taking{ID: b.Move, Role: role, From: b.From, Started: time.Now().UTC()}

	for i, bt := range b.Tasks {
		marker.Origins = append(marker.Origins, b.From+"/"+bt.ID)

		if repos[i] != "" && !slices.Contains(marker.Repos, repos[i]) {
			marker.Repos = append(marker.Repos, repos[i])
		}
	}

	// The marker is written under the state lock before the first side effect, after the last look at the tombstone: a cancel that comes
	// next finds it, and a cancel that came before it is found.
	err = a.withState(func(s *shift.Shift) error {
		if err := refuseCancelled(layout, b.Move); err != nil {
			return err
		}

		if m, ok, err := move.OpenTaking(layout, role); err != nil {
			return err
		} else if ok {
			if m.ID != b.Move {
				return fmt.Errorf("a take of another move of %s (%s) was cut short here: zmina move %s %s --abort %s takes away what it made", role, m.ID,
					role, m.From, m.ID)
			}

			// A take of this move that was cut short: what it made goes before this one makes anything.
			if err := a.cleanupTake(s, m); err != nil {
				return err
			}
		}

		return move.SaveTaking(layout, marker)
	})
	if err != nil {
		return move.TakeAnswer{}, err
	}

	if err := a.fault("taking"); err != nil {
		return move.TakeAnswer{}, err
	}

	var (
		ans       = move.TakeAnswer{Move: b.Move, Node: me.Name, Role: role, Sum: b.After, At: time.Now().UTC()}
		receipted bool
	)

	err = a.takeWork(ctx, b, call.By, repos, &ans, &receipted)
	if err != nil {
		// A process that died cleans nothing; a take that failed before its receipt takes away what it made, so that the work is on the
		// other node alone and the abort finds nothing here. After the receipt there is nothing to take away: the take is finished by
		// the next one.
		if !died(err) && !receipted {
			if cerr := a.withState(func(s *shift.Shift) error { return a.cleanupTake(s, marker) }); cerr != nil {
				err = fmt.Errorf("%w (and what the take made could not be taken away: %v; zmina move %s %s --abort %s does)", err, cerr, role, me.Name, b.Move)
			}
		}

		return move.TakeAnswer{}, err
	}

	a.dropTakeRefs(ctx, b, repos)

	return a.moveStart(layout, ans), nil
}

// refuseCancelled is the refusal of a take of a move that was called off at this node.
func refuseCancelled(layout crew.Layout, id string) error {
	gone, err := move.Aborted(layout, id)

	switch {
	case err != nil:
		return err
	case gone:
		return fmt.Errorf("the move %s was called off at this node: no take of it runs (it was aborted; zmina move <role> <node> --abort %s makes the other node say so too)", id, id)
	}

	return nil
}

// takeWork is the take's work, once it holds the lock and has written its marker: the git, then the tasks, the receipt and the place in
// one section of the state lock. receipted says the receipt was kept, which is the take's point of no return.
func (a *app) takeWork(ctx context.Context, b move.Bundle, by string, repos []string, ans *move.TakeAnswer, receipted *bool) error {
	// Git: each branch comes from origin, and its tip is the commit the bundle names; the branch is made here at it, unless it is there
	// (an earlier take of the same branch, or a task that went away and came back); the uncommitted work comes from its private ref.
	for i, bt := range b.Tasks {
		if bt.Branch == "" {
			continue
		}

		if err := a.takeBranch(ctx, repos[i], b.Move, bt); err != nil {
			return fmt.Errorf("%s: %w", bt.ID, err)
		}
	}

	if err := a.fault("taken-git"); err != nil {
		return err
	}

	return a.withState(func(s *shift.Shift) error {
		layout := s.Crew.Layout()

		if err := refuseCancelled(layout, b.Move); err != nil {
			return err
		}

		var taken []string

		for i, bt := range b.Tasks {
			t, err := importTask(s, b, bt, repos[i], by)
			if err != nil {
				return fmt.Errorf("%s: %w", bt.ID, err)
			}

			if t.Branch != "" {
				tree, err := worktree.AddExisting(t.Repo, t.Worktree, t.Branch)
				if err != nil {
					return fmt.Errorf("%s: the worktree of %s: %w", t.ID, bt.Branch, err)
				}

				if bt.WIP != "" {
					if err := tree.RestoreWIP(bt.WIP, b.Move); err != nil {
						return fmt.Errorf("%s: restoring the uncommitted work: %w", t.ID, err)
					}
				}
			}

			ans.Tasks = append(ans.Tasks, move.Taken{From: bt.ID, To: t.ID})
			taken = append(taken, t.ID)
		}

		if err := a.fault("taken-import"); err != nil {
			return err
		}

		// Once more, before the receipt and the place: the move may have been called off while the tasks were made (a cancel holds the
		// lock this take holds, so it cannot; the tombstone is looked at all the same).
		if err := refuseCancelled(layout, b.Move); err != nil {
			return err
		}

		// What can be refused about the place is refused before the receipt: after it only a race with another writer of the file is left.
		if _, _, err := placeCheck(s.Crew, b.Role, ans.Node, b.Base, b.After); err != nil {
			return err
		}

		if err := move.SaveReceipt(layout, *ans); err != nil {
			return err
		}

		*receipted = true

		if err := a.fault("taken-flip"); err != nil {
			return err
		}

		sum, err := a.placeRole(s.Crew, b.Role, ans.Node, b.Base, b.After, by)
		if err != nil {
			return err
		}

		ans.Sum = sum

		if err := move.SaveReceipt(layout, *ans); err != nil {
			return err
		}

		if err := move.CloseTaking(layout, b.Role); err != nil {
			return err
		}

		text := fmt.Sprintf("%s moved here from %s (move %s)", b.Role, b.From, b.Move)
		if len(taken) > 0 {
			text += ": " + strings.Join(taken, ", ")
		}

		s.Log(b.Role, "move-taken", b.Move, text)

		if _, err := s.NoteOnce("move-in/"+b.Move, "zmina", text, true); err != nil {
			s.Warn(fmt.Errorf("the dispatcher was not told of the move: %w", err))
		}

		return a.fault("taken")
	})
}

// moveTaken answers a take of a move that has its receipt: the same answer. It also finishes what the first take may have left (the
// place in the file, the marker, the start of the role), and takes the move's private refs off origin again if the first could not.
func (a *app) moveTaken(ctx context.Context, cr *crew.Crew, call mesh.MoveCall, rec move.TakeAnswer) (move.TakeAnswer, error) {
	layout := cr.Layout()

	release, err := move.Lock(layout, call.Role, 0)
	if errors.Is(err, move.ErrBusy) {
		return move.TakeAnswer{}, fmt.Errorf("a take of %s is finishing here: ask again in a minute", call.Move)
	}

	if err != nil {
		return move.TakeAnswer{}, err
	}

	defer release()

	if b, err := move.ParseBundle(call.Payload); err == nil && b.Move == call.Move && b.Role == call.Role {
		err := a.withState(func(s *shift.Shift) error {
			sum, err := a.placeRole(s.Crew, b.Role, rec.Node, b.Base, b.After, call.By)
			if err != nil {
				return err
			}

			if sum != rec.Sum {
				rec.Sum = sum

				if err := move.SaveReceipt(layout, rec); err != nil {
					return err
				}
			}

			return move.CloseTaking(layout, b.Role)
		})
		if err != nil {
			return move.TakeAnswer{}, err
		}

		repos := make([]string, len(b.Tasks))
		for i, bt := range b.Tasks {
			if bt.Repo != "" {
				repos[i] = move.Expand(bt.Repo)
			}
		}

		a.dropTakeRefs(ctx, b, repos)
	}

	if !rec.Started && len(rec.Tasks) > 0 {
		rec = a.moveStart(layout, rec)
	}

	return rec, nil
}

// dropTakeRefs takes the refs the take kept away: the local ones, and the private ref of each task's uncommitted work on origin. A ref
// that stays on origin is clutter, not work: the receipt is what the move stands on.
func (a *app) dropTakeRefs(ctx context.Context, b move.Bundle, repos []string) {
	var refs []moveRefAt

	for i, bt := range b.Tasks {
		if repos[i] == "" || bt.Branch == "" {
			continue
		}

		if local, err := worktree.MoveRefs(repos[i], b.Move); err == nil {
			for _, r := range local {
				refs = append(refs, moveRefAt{repos[i], r})
			}
		}

		if bt.WIP != "" {
			refs = append(refs, moveRefAt{repos[i], worktree.MoveRef(b.Move, move.TaskNumber(bt.ID))})
		}
	}

	if ctx.Err() != nil {
		ctx = context.Background()
	}

	dropMoveRefs(ctx, refs)
}

// takeBranch fetches the branch of origin into a private ref of the move, checks that it is the commit the bundle names, and makes the
// local branch at it, so that the local branch equals origin's and the role's next push is a plain one. The uncommitted work, when the
// task carries some, is fetched from its own private ref and must be one commit over that tip.
func (a *app) takeBranch(ctx context.Context, repo, id string, bt move.BTask) error {
	n := move.TaskNumber(bt.ID)

	sha, err := worktree.FetchContext(ctx, repo, bt.Branch, fmt.Sprintf("refs/zmina-move/%s/b%d", id, n))
	if err != nil {
		return err
	}

	if sha != bt.Tip {
		return fmt.Errorf("origin has %s at %s, not at %s that the move pushed: the branch moved since", bt.Branch, sha[:12], bt.Tip[:12])
	}

	if bt.WIP != "" {
		wip, err := worktree.FetchWIP(ctx, repo, id, n, fmt.Sprintf("refs/zmina-move/%s/w%d", id, n))
		if err != nil {
			return fmt.Errorf("the uncommitted work: %w", err)
		}

		if wip != bt.WIP {
			return fmt.Errorf("origin has the uncommitted work of the move at %s, not at %s that the bundle names", wip[:12], bt.WIP[:12])
		}

		if parent, err := worktree.RevParse(repo, wip+"^"); err != nil || parent != sha {
			return fmt.Errorf("the uncommitted work %s is not one commit over the tip %s", wip[:12], sha[:12])
		}
	}

	// A worktree that has the branch checked out is an earlier task's (this role was here before, or an earlier try of this take left a
	// task that was closed): the new task's worktree cannot have it too, and the branch is not moved under that one.
	if where, err := worktree.CheckedOutAt(repo, bt.Branch); err != nil {
		return err
	} else if where != "" {
		return fmt.Errorf("the branch %s is checked out in %s, an earlier task's worktree of this role here: remove it (git worktree remove, "+
			"zmina task gc), then --resume", bt.Branch, where)
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

	// A branch this node had of its own (the task came here before, and went away, and comes back): it goes forward to the commit the
	// move carries when that is a descendant of it.
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
