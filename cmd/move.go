package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/move"
)

const (
	// moveTakeTimeout is how long a take may take on the node the role goes to: a fetch from origin and a start.
	moveTakeTimeout = 10 * time.Minute
	// moveStepTimeout is how long one other step of a move may take at a node: a poll may wait for the role to exit and for a push to origin.
	moveStepTimeout = 10 * time.Minute
)

// moveEnd is one end of a move as the owner's node reaches it: its own steps when it is this node (the same functions the mesh calls
// on a node that is asked), a signed request over the mesh when it is another. A move is the same whichever end the owner stands at.
type moveEnd struct {
	node string
	// self is set for this node.
	self *app
	// me and tr reach another node.
	me *mesh.Node
	tr mesh.Transport
}

func callOf(req mesh.TerminalRequest) mesh.MoveCall {
	return mesh.MoveCall{Crew: req.Crew, Role: req.Role, Action: req.Action, Move: req.Move, Base: req.Base, Force: req.Force,
		Payload: req.Payload, By: "hand"}
}

func (e moveEnd) step(ctx context.Context, req mesh.TerminalRequest) ([]byte, error) {
	if e.self != nil {
		return e.self.localRoleMove(callOf(req))
	}

	ctx, cancel := context.WithTimeout(ctx, moveStepTimeout)
	defer cancel()

	return e.me.RoleMove(ctx, e.tr, e.node, req)
}

func (e moveEnd) take(ctx context.Context, req mesh.TerminalRequest) ([]byte, error) {
	if e.self != nil {
		return e.self.localRoleTake(callOf(req))
	}

	return e.me.RoleTake(ctx, e.tr, e.node, req)
}

// moveEndOf reaches a node of the move: this node by itself, another by the mesh unless it did not say crew_sync in its last session,
// when nothing is sent to it (an older zmina does not know the ops).
func (a *app) moveEndOf(me *mesh.Node, node string) (moveEnd, error) {
	if node == me.Name {
		return moveEnd{node: node, self: a}, nil
	}

	if why := crewSyncRefusal(peerOf(me, node)); why != "" {
		return moveEnd{}, errors.New(why)
	}

	var tr mesh.Transport = mesh.Exec{}
	if a.pushTransport != nil {
		tr = a.pushTransport
	}

	return moveEnd{node: node, me: me, tr: tr}, nil
}

// moveRun is one move as the owner's node drives it.
type moveRun struct {
	a        *app
	out      func(format string, args ...any)
	cr       *crew.Crew
	me       *mesh.Node
	key      string
	role, id string
	target   string
	src, dst moveEnd
	// said is the last waiting line said, so that a wait is told when it changes; quiet says no wait at all (the ticker's beat comes back a
	// minute later and would say it again).
	said  string
	quiet bool
}

func (r *moveRun) req(action string, payload any) (mesh.TerminalRequest, error) {
	q := mesh.TerminalRequest{Crew: r.key, Role: r.role, Action: action, Move: r.id}

	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return q, err
		}

		q.Payload = raw
	}

	return q, nil
}

// facts is what a node says of itself.
func (r *moveRun) facts(ctx context.Context, end moveEnd, body *move.CheckBody) (move.Facts, error) {
	q := mesh.TerminalRequest{Crew: r.key, Role: r.role, Action: "check"}

	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return move.Facts{}, err
		}

		q.Payload = raw
	}

	raw, err := end.step(ctx, q)
	if err != nil {
		return move.Facts{}, fmt.Errorf("%s: %w", end.node, err)
	}

	var f move.Facts
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("%s: its answer to the check does not read: %w", end.node, err)
	}

	// What a node says of itself is printed in a refusal: no control character of it reaches the terminal.
	mesh.PlainAll(&f)

	return f, nil
}

// compareFacts is the preflight of a move: what the two ends say of themselves, each mismatch a refusal that names the field. Nothing is
// trusted that was not asked of both nodes.
func compareFacts(role, target string, src, dst move.Facts, force bool) error {
	switch {
	case !src.Spread:
		return errors.New("the crew file declares no nodes: on " + src.Node + ": there is nowhere to move a role to")
	case !src.Known:
		return fmt.Errorf("no role %q in the crew file on %s", role, src.Node)
	case src.Moving != "":
		return fmt.Errorf("%s already has a move open on %s (%s): zmina move %s %s --resume %s finishes it, --abort %s calls it off",
			role, src.Node, src.Moving, role, target, src.Moving, src.Moving)
	case src.Placed != src.Node:
		return fmt.Errorf("placed: the file on %s places %s on %s, not on %s: a move starts where the role runs", src.Node, role, src.Placed, src.Node)
	case !dst.Spread:
		return fmt.Errorf("nodes: the crew file on %s declares no nodes:: zmina crew push %s", dst.Node, dst.Node)
	case !dst.Listed:
		return fmt.Errorf("nodes: the crew file on %s does not list %s", dst.Node, dst.Node)
	case !dst.Known:
		return fmt.Errorf("no role %q in the crew file on %s", role, dst.Node)
	case src.Sum != dst.Sum:
		return fmt.Errorf("crew file differs: %s holds %s, %s holds %s: zmina crew push %s, then move", src.Node, short12(src.Sum), dst.Node,
			short12(dst.Sum), dst.Node)
	case dst.Placed != src.Node:
		return fmt.Errorf("placed: the file on %s places %s on %s", dst.Node, role, dst.Placed)
	case dst.Moving != "":
		return fmt.Errorf("%s has a move open on %s (%s)", role, dst.Node, dst.Moving)
	case src.Dir != dst.Dir:
		return fmt.Errorf("crew dir differs: %s on %s, %s on %s (a crew's directory is the same place on both nodes)", src.Dir, src.Node, dst.Dir, dst.Node)
	case src.Namespace != dst.Namespace:
		return fmt.Errorf("namespace differs: %s on %s, %s on %s", src.Namespace, src.Node, dst.Namespace, dst.Node)
	}

	for _, sr := range src.Repos {
		i := slices.IndexFunc(dst.Repos, func(d move.RepoFact) bool { return d.Top == sr.Top })

		switch {
		case !sr.Found:
			return fmt.Errorf("repo %s is no repository's top level on %s", sr.Top, src.Node)
		case sr.Origin == "":
			return fmt.Errorf("repo %s has no origin on %s: a move carries a branch by origin (git remote add origin <url>)", sr.Top, src.Node)
		case i < 0 || !dst.Repos[i].Found:
			return fmt.Errorf("repo %s is no repository's top level on %s (a crew's repositories are at the same place on both nodes)", sr.Top, dst.Node)
		case dst.Repos[i].Origin != sr.Origin:
			return fmt.Errorf("repo %s: origin differs: %s on %s, %s on %s", sr.Top, sr.Origin, src.Node, orNone(dst.Repos[i].Origin), dst.Node)
		}
	}

	if !force {
		switch {
		case src.Queued > 0:
			return fmt.Errorf("%s has %d queued lines that a move does not carry: let them be delivered first, or --force drops them and tells "+
				"the dispatcher which", role, src.Queued)
		case len(src.Dirty) > 0:
			return fmt.Errorf("%s holds uncommitted work that a move does not carry: %s; commit it, or --force leaves it where it is", role,
				strings.Join(src.Dirty, ", "))
		}
	}

	return nil
}

func (a *app) moveCommand() *cobra.Command {
	var (
		wait          time.Duration
		resume, abort string
		force         bool
	)

	c := &cobra.Command{
		Use:   "move <role> <node> [--wait 10m] [--resume <id> | --abort <id>] [--force]",
		Short: "Move a role, with its tasks, to another node of the crew (owner's hand, on the owner's node): park, hand the branch over by origin, place the role there",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// A move puts a role down and changes where it runs on two nodes: the owner's hand, never a role's.
			if who := a.actor(); who != "hand" {
				return fmt.Errorf("zmina move is the owner's; a role (%s) does not run it%s", who, codexThreadWords(who))
			}

			if resume != "" && abort != "" {
				return errors.New("--resume continues a move, --abort calls it off: not both")
			}

			for _, id := range []string{resume, abort} {
				if id != "" && !move.ValidID(id) {
					return fmt.Errorf("%q is no move's id (mv- and twelve hex digits, as zmina move printed it)", id)
				}
			}

			if wait <= 0 {
				return errors.New("--wait is how long to wait for the role to park its work, for example 10m")
			}

			return a.runMove(cmd.Context(), args[0], args[1], wait, resume, abort, force)
		},
	}

	c.Flags().DurationVar(&wait, "wait", 10*time.Minute, "how long to wait for the role to park what it holds and for the work to be handed over; the move stays open past it")
	c.Flags().StringVar(&resume, "resume", "", "go on with the open move of that id (after a failure, a wait that ran out, or a node that was away)")
	c.Flags().StringVar(&abort, "abort", "", "call the open move of that id off: the role is started again here on its tasks (refused when the other node took the work)")
	c.Flags().BoolVar(&force, "force", false, "drop the role's queued lines (the dispatcher is told which) and leave uncommitted work of its own directory where it is; with --abort: even when the other node cannot be asked")

	return c
}

// moveLine says a line of a move: to the terminal for the command, to the log of the beat for the ticker.
func (a *app) moveLine(format string, args ...any) {
	fmt.Fprintln(a.out, mesh.Plain(fmt.Sprintf(format, args...)))
}

func (a *app) runMove(ctx context.Context, role, target string, wait time.Duration, resume, abort string, force bool) error {
	cr, err := a.loadCrew()
	if err != nil {
		return err
	}

	me, err := openNode()
	if err != nil {
		return err
	}

	// The move is signed with the owner's key: only the owner's node holds it.
	if !me.HoldsOwnerKey() {
		return errors.New("zmina move is the owner's: it is signed with owner.key, which only the owner's node holds")
	}

	switch {
	case !cr.Spread():
		return errors.New("this crew's file declares no nodes:, so there is nowhere to move a role to")
	case strings.Contains(role, "#") || !move.ValidRole(role):
		return fmt.Errorf("%q is no role a move carries (a copy, dev#2, is not moved)", role)
	case cr.Roles[role] == nil:
		return fmt.Errorf("no role %q in the crew (roles: %s)", role, strings.Join(cr.Names(), ", "))
	case role == cr.Dispatcher:
		return fmt.Errorf("%s is the dispatcher: it runs on home and on every node that holds a role, and is not moved", role)
	case !cr.HasNode(target):
		return fmt.Errorf("%s is not one of nodes: (%s)", target, strings.Join(cr.NodeNames(), ", "))
	}

	key, err := a.pushKey()
	if err != nil {
		return err
	}

	r := &moveRun{a: a, out: a.moveLine, cr: cr, me: me, key: key, role: role, target: target, id: or(resume, abort)}

	if r.dst, err = a.moveEndOf(me, target); err != nil {
		return err
	}

	if resume != "" || abort != "" {
		if r.src, err = r.findSource(ctx); err != nil {
			return err
		}

		if abort != "" {
			return r.abort(ctx, force)
		}

		return r.resume(ctx, wait)
	}

	source := cr.PlacementOf(cr.Roles[role])

	switch {
	case source == target:
		return fmt.Errorf("%s is placed on %s already", role, target)
	case !cr.HasNode(source):
		return fmt.Errorf("%s is placed on %q, which is no node of nodes: (%s): fix its place in the file first", role, source,
			strings.Join(cr.NodeNames(), ", "))
	}

	if r.src, err = a.moveEndOf(me, source); err != nil {
		return err
	}

	return r.start(ctx, wait, force)
}

// start is a move from the beginning: the preflight on both nodes, then begin, then the rest.
func (r *moveRun) start(ctx context.Context, wait time.Duration, force bool) error {
	sf, err := r.facts(ctx, r.src, nil)
	if err != nil {
		return err
	}

	var body move.CheckBody

	for _, rf := range sf.Repos {
		body.Repos = append(body.Repos, rf.Top)
	}

	df, err := r.facts(ctx, r.dst, &body)
	if err != nil {
		return err
	}

	if err := compareFacts(r.role, r.target, sf, df, force); err != nil {
		return err
	}

	r.id = move.NewID()
	layout := r.cr.Layout()

	// The owner's record first: a move cut short anywhere is found by its ticker, and by --resume.
	if err := move.SaveDrive(layout, move.Drive{ID: r.id, Role: r.role, From: r.src.node, To: r.target, Base: sf.Sum, Started: time.Now().UTC()}); err != nil {
		return err
	}

	q, err := r.req("begin", move.BeginBody{To: r.target})
	if err != nil {
		return err
	}

	q.Base, q.Force = sf.Sum, force

	r.out("%s: %s to %s (move %s)", r.role, r.src.node, r.target, r.id)

	raw, err := r.src.step(ctx, q)
	if err != nil {
		_ = move.CloseDrive(layout, r.role)

		return fmt.Errorf("%s: %w (a refusal begins nothing; a connection that broke mid-way may have: zmina move %s %s --resume %s says, and "+
			"--abort %s calls it off)", r.src.node, err, r.role, r.target, r.id, r.id)
	}

	var ans move.PollAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return fmt.Errorf("%s: its answer to the beginning does not read: %w", r.src.node, err)
	}

	mesh.PlainAll(&ans)

	if ans.Why != "" {
		r.out("  %s: %s", r.src.node, ans.Why)
	}

	return r.resume(ctx, wait)
}

// resume goes on with the move: the node the role leaves is asked how far it is until the work is handed over or the wait ends, then the work is
// taken and the move ended.
func (r *moveRun) resume(ctx context.Context, wait time.Duration) error {
	layout := r.cr.Layout()

	if _, ok, _ := move.OpenDrive(layout, r.role); !ok {
		_ = move.SaveDrive(layout, move.Drive{ID: r.id, Role: r.role, From: r.src.node, To: r.target, Started: time.Now().UTC()})
	}

	deadline := time.Now().Add(wait)

	for {
		done, err := r.once(ctx)
		if err != nil {
			return err
		}

		if done {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("the move is not done in %s: it stays open (%s); zmina move %s %s --resume %s goes on with it, --abort %s calls it off; "+
				"the ticker of each node also takes it on", wait, r.id, r.role, r.target, r.id, r.id)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(movePollEvery):
		}
	}
}

// say tells a wait, once for each text.
func (r *moveRun) say(text string) {
	if text != "" && text != r.said && !r.quiet {
		r.said = text
		r.out("  %s", text)
	}
}

// once asks the node the role leaves how far the move is and does what follows: the work is ready, so it is taken and the move ended.
func (r *moveRun) once(ctx context.Context) (bool, error) {
	q, err := r.req("poll", nil)
	if err != nil {
		return false, err
	}

	raw, err := r.src.step(ctx, q)
	if err != nil {
		return false, fmt.Errorf("%s: %w", r.src.node, err)
	}

	var ans move.PollAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return false, fmt.Errorf("%s: its answer does not read: %w", r.src.node, err)
	}

	mesh.PlainAll(&ans)

	switch ans.State {
	case move.StatePending:
		r.say(r.src.node + ": " + ans.Why)

		return false, nil
	case move.StateReady:
		return true, r.finish(ctx, ans.Bundle)
	}

	_ = move.CloseDrive(r.cr.Layout(), r.role)

	return false, fmt.Errorf("%s has no open move %s of %s (%s): it was ended or called off; zmina ls shows where %s runs", r.src.node, r.id, r.role,
		or(ans.Why, ans.State), r.role)
}

// finish takes the work on the node the role goes to, ends the move on the node it leaves, and checks the two files are one file again.
func (r *moveRun) finish(ctx context.Context, bundle []byte) error {
	b, err := move.ParseBundle(bundle)
	if err != nil {
		return fmt.Errorf("%s handed over work that does not read: %w", r.src.node, err)
	}

	for _, t := range b.Tasks {
		line := "  " + mesh.Plain(t.ID) + " handed over"
		if t.Branch != "" {
			line += ": branch " + mesh.Plain(t.Branch) + " at " + mesh.Plain(t.Tip[:12])
			if t.WIP {
				line += ", with its uncommitted work as one commit"
			}
		}

		r.out("%s", line)
	}

	if err := r.a.fault("owner-before-take"); err != nil {
		return err
	}

	tctx, cancel := context.WithTimeout(ctx, moveTakeTimeout)
	defer cancel()

	q, err := r.req("", nil)
	if err != nil {
		return err
	}

	q.Action, q.Base, q.Payload = "", b.Base, bundle

	raw, err := r.dst.take(tctx, q)
	if err != nil {
		return fmt.Errorf("%s did not take the work: %w (the move stays open on %s; zmina move %s %s --resume %s tries again, --abort %s calls it off)",
			r.target, err, r.src.node, r.role, r.target, r.id, r.id)
	}

	var ta move.TakeAnswer
	if err := json.Unmarshal(raw, &ta); err != nil {
		return fmt.Errorf("%s: its answer to the take does not read: %w", r.target, err)
	}

	mesh.PlainAll(&ta)

	for _, t := range ta.Tasks {
		r.out("  %s took %s as %s", r.target, t.From, t.To)
	}

	if ta.Started {
		r.out("  %s is up on %s", r.role, r.target)
	} else if ta.Why != "" {
		r.out("  %s: %s", r.role, ta.Why)
	}

	if err := r.a.fault("owner-after-take"); err != nil {
		return err
	}

	taken := map[string]string{}
	for _, t := range ta.Tasks {
		taken[t.From] = r.target + "/" + t.To
	}

	cq, err := r.req("commit", move.CommitBody{To: r.target, Taken: taken, Sum: ta.Sum})
	if err != nil {
		return err
	}

	if _, err := r.src.step(ctx, cq); err != nil {
		return fmt.Errorf("%s took the work, but its end could not be made on %s: %w (zmina move %s %s --resume %s makes it)", r.target, r.src.node, err,
			r.role, r.target, r.id)
	}

	r.out("  %s: %s is placed on %s, the parked items closed as taken", r.src.node, r.role, r.target)

	_ = move.CloseDrive(r.cr.Layout(), r.role)

	r.verify(ctx)

	return nil
}

// verify says whether the two nodes hold one file again, which a move leaves them with: the same edit on both.
func (r *moveRun) verify(ctx context.Context) {
	sf, serr := r.facts(ctx, r.src, nil)
	df, derr := r.facts(ctx, r.dst, nil)

	switch {
	case serr != nil || derr != nil:
		r.out("  could not read the crew files back to compare them (%v %v): zmina node ls shows whether they are in sync", serr, derr)
	case sf.Sum == df.Sum:
		r.out("crew files equal on %s and %s (%s)", r.src.node, r.target, short12(sf.Sum))

		// Both nodes told, signed, the same file: it is the file this node last confirmed on each, which the next push to either is based on.
		for _, n := range []string{r.src.node, r.target} {
			if n != r.me.Name {
				_ = r.me.MarkPushed(n, r.key, sf.Sum)
			}
		}
	default:
		r.out("WARNING: the crew files differ after the move: %s holds %s, %s holds %s: zmina crew push %s sends one file to the other",
			r.src.node, short12(sf.Sum), r.target, short12(df.Sum), r.target)
	}

	var others []string

	for _, n := range r.cr.NodeNames() {
		if n != r.src.node && n != r.target {
			others = append(others, n)
		}
	}

	if len(others) > 0 {
		r.out("other nodes of the crew (%s) hold the old placement: zmina crew push <node> sends them this file", strings.Join(others, ", "))
	}
}

// findSource is the node that holds the journal of the move: where the file places the role, or, when the take already put it on the node it
// goes to, the node of nodes: that says it holds the move.
func (r *moveRun) findSource(ctx context.Context) (moveEnd, error) {
	place := r.cr.PlacementOf(r.cr.Roles[r.role])

	candidates := []string{}
	if place != "" && place != r.target {
		candidates = append(candidates, place)
	}

	for _, n := range r.cr.NodeNames() {
		if n != r.target && !slices.Contains(candidates, n) {
			candidates = append(candidates, n)
		}
	}

	var why []string

	for _, n := range candidates {
		end, err := r.a.moveEndOf(r.me, n)
		if err != nil {
			why = append(why, err.Error())

			continue
		}

		// A check moves nothing: only a poll of the journal's own id moves the move on, and this is asked before anything is decided.
		f, err := r.facts(ctx, end, nil)
		if err != nil {
			why = append(why, err.Error())

			continue
		}

		if f.Moving == r.id {
			return end, nil
		}
	}

	msg := fmt.Sprintf("no node holds an open move %s of %s", r.id, r.role)
	if len(why) > 0 {
		msg += " (" + strings.Join(why, "; ") + ")"
	}

	return moveEnd{}, errors.New(msg)
}

// abort calls the move off: refused when the node the role goes to holds the receipt of a take (the role runs there: calling it off would
// run it twice), or cannot be asked and the owner did not say --force.
func (r *moveRun) abort(ctx context.Context, force bool) error {
	q, err := r.req("poll", nil)
	if err != nil {
		return err
	}

	raw, err := r.dst.step(ctx, q)

	var ans move.PollAnswer

	switch {
	case err != nil && !force:
		return fmt.Errorf("%s could not be asked whether it took the work (%w): it may have, and the role would run on two nodes; "+
			"--abort --force calls the move off anyway", r.target, err)
	case err == nil && json.Unmarshal(raw, &ans) == nil && ans.State == move.StateTaken:
		return fmt.Errorf("%s took the work already (%s): the role runs there; zmina move %s %s --resume %s ends the move", r.target, r.id, r.role, r.target, r.id)
	}

	aq, err := r.req("abort", nil)
	if err != nil {
		return err
	}

	raw, err = r.src.step(ctx, aq)
	if err != nil {
		return fmt.Errorf("%s: %w", r.src.node, err)
	}

	if err := json.Unmarshal(raw, &ans); err != nil {
		return fmt.Errorf("%s: its answer does not read: %w", r.src.node, err)
	}

	mesh.PlainAll(&ans)

	_ = move.CloseDrive(r.cr.Layout(), r.role)

	r.out("%s: move %s called off on %s: %s", r.role, r.id, r.src.node, ans.Why)

	return nil
}

// moveBeat is the ticker's part of moves, for this crew: the node a role is leaving moves its open move on (the role parks, goes down, its
// branches are pushed, the bundle is built), and the owner's node takes the moves it drives on from where they stopped (the take, then the
// end), so that a node that was away or a wait that ran out finishes by itself. It says what is new and nothing else: an error is told when it
// changes.
func (a *app) moveBeat() {
	cr, err := crew.Load(a.crewPath())
	if err != nil || !cr.Spread() || !cr.Runs() {
		return
	}

	layout := cr.Layout()

	for _, role := range cr.Names() {
		j, ok, err := move.Open(layout, role)
		if err != nil || !ok {
			continue
		}

		_, aerr := a.moveAdvance(role, j.ID)
		a.printMoveErr(layout, role, j, aerr)
	}

	drives := move.Drives(layout)
	if len(drives) == 0 {
		return
	}

	me, err := openNode()
	if err != nil || !me.HoldsOwnerKey() {
		return
	}

	key := a.crewKey()

	for _, d := range drives {
		a.driveOnce(me, cr, key, d)
	}
}

// printMoveErr says the error of the beat's advance of a move when it is not the one it said last.
func (a *app) printMoveErr(layout crew.Layout, role string, j move.Journal, err error) {
	last := ""
	if err != nil {
		last = err.Error()
	}

	if last == j.LastErr {
		return
	}

	if cur, ok, _ := move.Open(layout, role); ok && cur.ID == j.ID {
		cur.LastErr = last
		_ = move.Save(layout, cur)
	}

	if last != "" {
		a.printTick([]string{fmt.Sprintf("move %s of %s: %s", j.ID, role, last)})
	}
}

// driveOnce is the owner's node taking one of its moves on a step: the node the role leaves is asked, and when the work is ready it is taken and the
// move ended.
func (a *app) driveOnce(me *mesh.Node, cr *crew.Crew, key string, d move.Drive) {
	layout := cr.Layout()

	var lines []string

	r := &moveRun{a: a, cr: cr, me: me, key: key, role: d.Role, id: d.ID, target: d.To, quiet: true,
		out: func(format string, args ...any) {
			lines = append(lines, mesh.Plain(fmt.Sprintf("move %s: "+format, append([]any{d.ID}, args...)...)))
		}}

	var err error

	if r.src, err = a.moveEndOf(me, d.From); err == nil {
		r.dst, err = a.moveEndOf(me, d.To)
	}

	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), moveTakeTimeout+peerActTimeout)
		_, err = r.once(ctx)

		cancel()
	}

	last := ""
	if err != nil {
		last = err.Error()
	}

	if cur, ok, _ := move.OpenDrive(layout, d.Role); ok && cur.LastErr != last {
		cur.LastErr = last
		_ = move.SaveDrive(layout, cur)

		if last != "" {
			lines = append(lines, "move "+d.ID+": "+last)
		}
	}

	a.printTick(lines)
}
