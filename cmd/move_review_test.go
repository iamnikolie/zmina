package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/move"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
)

// readyBundle stops a move of dev at the moment its work is ready on alpha and returns its id and the bundle alpha hands over.
func (w *moveWorld) readyBundle() (id string, bundle []byte) {
	w.t.Helper()

	w.crash("ready")

	out, _, err := w.move("dev", "beta")
	require.ErrorContains(w.t, err, "the process died here")

	id = w.idOf(out)

	var ans move.PollAnswer

	w.as("alpha", func() {
		a := w.ownerApp(io.Discard, io.Discard)
		a.crew = w.file("alpha")

		raw, err := a.localRoleMove(mesh.MoveCall{Crew: "ops", Role: "dev", Action: "poll", Move: id, By: "hand"})
		require.NoError(w.t, err)
		require.NoError(w.t, json.Unmarshal(raw, &ans))
	})

	require.Equal(w.t, move.StateReady, ans.State)

	return id, ans.Bundle
}

// takeOnBeta hands a bundle to beta as the owner's node would, and returns beta's answer.
func (w *moveWorld) takeOnBeta(id string, bundle []byte) (ans move.TakeAnswer, err error) {
	w.t.Helper()

	w.as("beta", func() {
		raw, terr := w.peerApp().localRoleTake(mesh.MoveCall{Crew: "ops", Role: "dev", Move: id, Payload: bundle, By: "mesh alpha"})
		if terr != nil {
			err = terr

			return
		}

		err = json.Unmarshal(raw, &ans)
	})

	return ans, err
}

// stepOn runs a step of role/move as the node it is asked of and returns its answer.
func (w *moveWorld) stepOn(node, action, id string) (ans move.PollAnswer, err error) {
	w.t.Helper()

	w.as(node, func() {
		a := w.peerApp()
		a.crew = w.file(node)

		raw, serr := a.localRoleMove(mesh.MoveCall{Crew: "ops", Role: "dev", Action: action, Move: id, By: "hand"})
		if serr != nil {
			err = serr

			return
		}

		err = json.Unmarshal(raw, &ans)
	})

	return ans, err
}

// active are the tasks of a node that hold a role (not closed, not parked), with the origin they came from.
func (w *moveWorld) active(node, origin string) (out []task.Task) {
	w.t.Helper()

	for _, t := range w.tasks(node) {
		if t.Origin == origin && t.State.Active() {
			out = append(out, t)
		}
	}

	return out
}

func (w *moveWorld) layout(node string) (l crew.Layout) {
	w.t.Helper()

	w.as(node, func() { l = mustCrew(w.t, w.file(node)).Layout() })

	return l
}

// A role goes to beta with an uncommitted worktree (a tracked edit, an untracked file) and comes back with what it did there. Each side has
// the files as they were, git status says the same on both, origin never has a commit of the move and no private ref is left, and the
// role's next ordinary push on beta is a fast-forward (the local branch there is origin's).
func TestMoveThereAndBackWithAnUncommittedWorktree(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, wt, branch := w.work(true)

	tip := gitIn(t, wt, "rev-parse", "HEAD")
	statusAlpha := gitIn(t, wt, "status", "--porcelain")

	out, errOut, err := w.move("dev", "beta")
	require.NoError(t, err, out+errOut)

	moved := w.assertMoved(branch)

	assert.Equal(t, tip, gitIn(t, w.remote, "rev-parse", "refs/heads/"+branch), "origin's branch is the tip it was, not a commit of the move")
	assert.NotContains(t, gitIn(t, w.remote, "log", "--format=%B", "refs/heads/"+branch), "Zmina-Move")
	assert.Empty(t, gitIn(t, w.remote, "for-each-ref", "refs/zmina-move"))
	assert.Equal(t, "form v2, not committed\n", readFile(t, filepath.Join(moved.Worktree, "form.txt")))
	assert.Equal(t, "new file\n", readFile(t, filepath.Join(moved.Worktree, "extra.txt")))
	assert.Equal(t, statusAlpha, gitIn(t, moved.Worktree, "status", "--porcelain"))

	// dev works on beta: commits what it has, goes on editing, and pushes the branch as an ordinary push (no force, no pull).
	w.as("beta", func() { withRole(t, w.file("beta"), "dev", func(rs *shift.RoleState) { rs.Inbox = nil }) })

	gitIn(t, moved.Worktree, "add", ".")
	gitIn(t, moved.Worktree, "commit", "-q", "-m", "form v2")
	gitIn(t, moved.Worktree, "push", "-q", "origin", branch)

	require.NoError(t, os.WriteFile(filepath.Join(moved.Worktree, "form.txt"), []byte("form v3, still uncommitted\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(moved.Worktree, "beta.txt"), []byte("only on beta\n"), 0o600))
	require.NoError(t, os.Remove(filepath.Join(moved.Worktree, "extra.txt")))

	statusBeta := gitIn(t, moved.Worktree, "status", "--porcelain")
	betaTip := gitIn(t, moved.Worktree, "rev-parse", "HEAD")

	// Back: the earlier task's worktree on alpha has the branch checked out and is in the way; with it gone the move goes on.
	_, _, err = w.move("dev", "alpha")
	require.ErrorContains(t, err, "is checked out in")

	gitIn(t, w.repo("alpha"), "worktree", "remove", "--force", wt)

	id := w.idOf(readJournalID(t, w))

	out, errOut, err = w.move("dev", "alpha", "--resume", id)
	require.NoError(t, err, out+errOut)

	var back task.Task

	for _, tk := range w.tasks("alpha") {
		if tk.Origin == "beta/"+moved.ID && tk.State.Active() {
			back = tk
		}
	}

	require.NotEmpty(t, back.ID)
	assert.True(t, w.up("alpha", "dev"))
	assert.False(t, w.up("beta", "dev"))

	assert.Equal(t, "form v3, still uncommitted\n", readFile(t, filepath.Join(back.Worktree, "form.txt")))
	assert.Equal(t, "only on beta\n", readFile(t, filepath.Join(back.Worktree, "beta.txt")))
	assert.NoFileExists(t, filepath.Join(back.Worktree, "extra.txt"), "a file dev deleted is deleted there too")
	assert.Equal(t, betaTip, gitIn(t, back.Worktree, "rev-parse", "HEAD"))
	assert.Equal(t, statusBeta, gitIn(t, back.Worktree, "status", "--porcelain"))

	assert.Equal(t, betaTip, gitIn(t, w.remote, "rev-parse", "refs/heads/"+branch))
	assert.Equal(t, betaTip, gitIn(t, w.repo("alpha"), "rev-parse", "refs/heads/"+branch), "alpha's branch is origin's too")
	assert.NotContains(t, gitIn(t, w.remote, "log", "--format=%B", "refs/heads/"+branch), "Zmina-Move")
	assert.Empty(t, gitIn(t, w.remote, "for-each-ref", "refs/zmina-move"), "no private ref is left on origin")
	assert.Empty(t, gitIn(t, w.repo("alpha"), "for-each-ref", "refs/zmina-move"))
	assert.Empty(t, gitIn(t, w.repo("beta"), "for-each-ref", "refs/zmina-move"))
}

// A worktree whose index holds half of what the files hold (a file staged and edited again, one staged and deleted) is no state the move
// refuses: the files travel, whatever is staged.
func TestMoveCarriesTheFilesOfAPartlyStagedWorktree(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, wt, branch := w.work(false)

	require.NoError(t, os.WriteFile(filepath.Join(wt, "staged.txt"), []byte("v1\n"), 0o600))
	gitIn(t, wt, "add", "staged.txt")
	require.NoError(t, os.WriteFile(filepath.Join(wt, "staged.txt"), []byte("v2\n"), 0o600))

	require.NoError(t, os.WriteFile(filepath.Join(wt, "gone.txt"), []byte("x\n"), 0o600))
	gitIn(t, wt, "add", "gone.txt")
	require.NoError(t, os.Remove(filepath.Join(wt, "gone.txt")))

	out, errOut, err := w.move("dev", "beta")
	require.NoError(t, err, out+errOut)

	moved := w.assertMoved(branch)
	assert.Equal(t, "v2\n", readFile(t, filepath.Join(moved.Worktree, "staged.txt")), "what is in the files travels, not what was staged")
	assert.NoFileExists(t, filepath.Join(moved.Worktree, "gone.txt"))
	assert.Empty(t, gitIn(t, moved.Worktree, "diff", "--cached", "--name-only"), "nothing is staged there")
	assert.Equal(t, "?? staged.txt", gitIn(t, moved.Worktree, "status", "--porcelain"))
}

// What a worktree cannot give back (a merge, a cherry-pick or a rebase in progress, conflicts, a nested repository) is refused in the
// preflight, with the state named, before the role is parked: no journal, the role still up, the same under --force.
func TestMoveRefusesBeforeParkingAWorktreeInTheMiddleOfSomething(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, wt, _ := w.work(false)

	head := gitIn(t, wt, "rev-parse", "HEAD")
	mergeHead := gitIn(t, wt, "rev-parse", "--git-path", "MERGE_HEAD")

	if !filepath.IsAbs(mergeHead) {
		mergeHead = filepath.Join(wt, mergeHead)
	}

	refuse := func(says string) {
		t.Helper()

		for _, args := range [][]string{{"dev", "beta"}, {"dev", "beta", "--force"}} {
			_, _, err := w.move(args...)
			require.Error(t, err, args)
			assert.Contains(t, err.Error(), says, args)
			assert.Contains(t, err.Error(), "holds work a move cannot carry")
			assert.False(t, w.journal("alpha", "dev"), "nothing was begun")
			assert.True(t, w.up("alpha", "dev"), "the role was not parked or put down")
		}
	}

	// A merge that stopped before its commit.
	require.NoError(t, os.WriteFile(mergeHead, []byte(head+"\n"), 0o600))
	refuse("a merge in progress")
	require.NoError(t, os.Remove(mergeHead))

	// A nested repository.
	nested := filepath.Join(wt, "vendor", "lib")
	require.NoError(t, os.MkdirAll(nested, 0o700))
	gitIn(t, nested, "init", "-q")
	require.NoError(t, os.WriteFile(filepath.Join(nested, "x.txt"), []byte("x\n"), 0o600))
	refuse("vendor/lib")
	require.NoError(t, os.RemoveAll(filepath.Join(wt, "vendor")))

	// Back to plain: the move goes.
	out, errOut, err := w.move("dev", "beta")
	require.NoError(t, err, out+errOut)
}

// A push the remote refuses stops the move after the work was committed for it; the abort that follows leaves no commit on the branch
// and no ref behind: it was never on the branch, and the journal knew of it from the moment it was made.
func TestMoveAbortAfterAPushThatFailedLeavesTheWorktreeAsItWas(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, wt, branch := w.work(true)

	tip := gitIn(t, wt, "rev-parse", "HEAD")
	status := gitIn(t, wt, "status", "--porcelain")

	hook := filepath.Join(w.remote, "hooks", "pre-receive")
	require.NoError(t, os.MkdirAll(filepath.Dir(hook), 0o700))
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\necho refused by the test >&2\nexit 1\n"), 0o700))

	out, _, err := w.move("dev", "beta", "--wait", "300ms")
	require.ErrorContains(t, err, "not done in")
	assert.Contains(t, out, "handing over T-2")
	assert.Contains(t, out, "refused by the test")

	id := w.idOf(out)

	var j move.Journal

	w.as("alpha", func() {
		var ok bool

		var jerr error

		j, ok, jerr = move.Open(mustCrew(t, w.file("alpha")).Layout(), "dev")
		require.NoError(t, jerr)
		require.True(t, ok)
	})

	require.Len(t, j.Tasks, 1)
	assert.NotEmpty(t, j.Tasks[0].WIP, "the journal knows of the commit as soon as it was made, though nothing was pushed")
	assert.False(t, j.Tasks[0].Pushed)
	assert.NotEmpty(t, gitIn(t, w.repo("alpha"), "for-each-ref", "refs/zmina-move"))

	out, errOut, err := w.move("dev", "beta", "--abort", id)
	require.NoError(t, err, out+errOut)

	assert.Equal(t, tip, gitIn(t, wt, "rev-parse", "HEAD"), "no commit of the move is at the tip of the branch")
	assert.Equal(t, status, gitIn(t, wt, "status", "--porcelain"))
	assert.Equal(t, "form v2, not committed\n", readFile(t, filepath.Join(wt, "form.txt")))
	assert.Empty(t, gitIn(t, w.repo("alpha"), "for-each-ref", "refs/zmina-move"))
	assert.Empty(t, gitIn(t, w.remote, "for-each-ref", "refs/zmina-move"))
	assert.NotContains(t, gitIn(t, w.remote, "log", "--format=%B", "--all"), "Zmina-Move")
	assert.True(t, w.up("alpha", "dev"))
	_ = branch
}

// A task the role closes while it is asked to park does not wedge the move: it leaves the journal, and the rest of the move goes on.
func TestMoveTaskClosedDuringTheGraceLeavesTheJournal(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	w.work(false)

	w.crash("asked")

	out, _, err := w.move("dev", "beta")
	require.ErrorContains(t, err, "the process died here")

	id := w.idOf(out)

	// The role finishes its task inside the grace.
	w.as("alpha", func() {
		st := taskStore(mustCrew(t, w.file("alpha")))

		tk, err := st.Get("T-2")
		require.NoError(t, err)

		tk.State = task.Done
		require.NoError(t, st.Put(tk))
	})

	out, errOut, err := w.move("dev", "beta", "--resume", id)
	require.NoError(t, err, out+errOut)

	assert.False(t, w.journal("alpha", "dev"))
	assert.Equal(t, "beta", w.placed("alpha", "dev"))
	assert.Equal(t, "beta", w.placed("beta", "dev"))
	assert.Empty(t, w.tasks("beta"), "no task travels: the one there was is done")

	for _, it := range w.items("alpha") {
		assert.NotEqual(t, later.Parked, it.State)
	}
}

// tamperBundle makes the peer's poll answer carry a bundle that mutate has changed: a node that is certified and lies about the work.
func (w *moveWorld) tamperBundle(mutate func(b *move.Bundle)) {
	w.tamper = func(c mesh.MoveCall, data []byte) []byte {
		if c.Action != "poll" {
			return data
		}

		var ans move.PollAnswer
		require.NoError(w.t, json.Unmarshal(data, &ans))

		if ans.State != move.StateReady {
			return data
		}

		var b move.Bundle
		require.NoError(w.t, json.Unmarshal(ans.Bundle, &b))
		mutate(&b)

		raw, err := json.Marshal(b)
		require.NoError(w.t, err)

		ans.Bundle = raw
		out, err := json.Marshal(ans)
		require.NoError(w.t, err)

		return out
	}
}

// The owner relays the work of the source only after it has checked it against what its own preflight found of that node: the move, the
// role, the nodes and the file it was made from, the tasks the source said it holds, the repositories compared on both nodes. A source that
// names anything else is refused, and the node the role goes to is not asked to take anything.
func TestOwnerRefusesAWorkBundleThatNamesWhatThePreflightDidNotFind(t *testing.T) {
	w := newMoveWorld(t, "beta") // the source, alpha, is the peer the owner has to take the bundle from
	_, _, branch := w.work(false)

	cases := map[string]struct {
		mutate func(b *move.Bundle)
		says   string
	}{
		"a task the source did not say it holds": {func(b *move.Bundle) { b.Tasks = append(b.Tasks, move.BTask{ID: "T-9", Asked: "something else"}) },
			"handed over T-9, which it did not say it holds"},
		"a repository that was not compared": {func(b *move.Bundle) {
			b.Tasks[0].Repo = "~/work/other"
		}, "which was not among the ones compared on both nodes"},
		"another node to go to":      {func(b *move.Bundle) { b.To = "gamma" }, "the work goes from alpha to gamma, not from alpha to beta"},
		"another node it comes from": {func(b *move.Bundle) { b.From = "gamma" }, "the work goes from gamma to beta"},
		"another role":               {func(b *move.Bundle) { b.Role = "ops" }, "the work is of role ops, not dev"},
		"another move":               {func(b *move.Bundle) { b.Move = "mv-aaaaaaaaaaaa" }, "the work is of move mv-aaaaaaaaaaaa"},
		"another crew file":          {func(b *move.Bundle) { b.Base = strings.Repeat("e", 64) }, "was made from the crew file"},
		"another crew directory":     {func(b *move.Bundle) { b.Dir = "~/elsewhere" }, "names the crew directory ~/elsewhere"},
	}

	w.tamperBundle(func(b *move.Bundle) { b.Tasks[0].Repo = "~/work/other" })

	out, _, err := w.move("dev", "beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "handed over work that is not what the move was begun for")
	assert.Contains(t, err.Error(), "which was not among the ones compared")

	id := w.idOf(out)

	for name, c := range cases {
		w.tamperBundle(c.mutate)

		_, _, err := w.move("dev", "beta", "--resume", id)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), c.says, name)
		assert.Empty(t, w.tasks("beta"), name+": nothing was taken")
		assert.Equal(t, "alpha", w.placed("beta", "dev"), name)
		assert.True(t, w.journal("alpha", "dev"), name)
	}

	// The same source, telling the truth: the move ends.
	w.tamper = nil

	out, errOut, err := w.move("dev", "beta", "--resume", id)
	require.NoError(t, err, out+errOut)
	w.assertMoved(branch)
}

// What the source says in its texts is data: no control character of it reaches the task on the other node.
func TestATaskTakenFromAnotherNodeLosesTheControlCharactersOfItsTexts(t *testing.T) {
	w := newMoveWorld(t, "beta")
	_, _, branch := w.work(false)

	esc := "\x1b[2J\x1b]0;pwned\x07"

	w.tamperBundle(func(b *move.Bundle) {
		b.Tasks[0].Asked = "fix" + esc + "\nthe form"
		b.Tasks[0].Title = "Form" + esc
		b.Tasks[0].Note = "a note" + esc
		b.Tasks[0].Criteria = []string{"it saves" + esc}
	})

	out, errOut, err := w.move("dev", "beta")
	require.NoError(t, err, out+errOut)
	assert.NotContains(t, out+errOut, "\x1b")

	var moved task.Task

	for _, tk := range w.active("beta", "alpha/T-2") {
		moved = tk
	}

	require.NotEmpty(t, moved.ID)

	for _, text := range append([]string{moved.Asked, moved.Title, moved.Note}, moved.Criteria...) {
		assert.NotContains(t, text, "\x1b")
		assert.NotContains(t, text, "\x07")
	}

	assert.Equal(t, "fix[2J]0;pwned\nthe form", moved.Asked, "a line break of the asked text stays")
	assert.Equal(t, "Form[2J]0;pwned", moved.Title)
	_ = branch
}

// The place edit a node makes must be the one the move says: a node whose edit gives another file refuses the take, writes nothing,
// and does not tell the owner it placed the role.
func TestTakeRefusesAPlaceEditThatMakesAnotherFileThanTheMoveSays(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	w.work(false)

	id, bundle := w.readyBundle()

	var b move.Bundle
	require.NoError(t, json.Unmarshal(bundle, &b))

	b.After = strings.Repeat("e", 64)

	raw, err := json.Marshal(b)
	require.NoError(t, err)

	_, betaBefore := w.sums()

	_, err = w.takeOnBeta(id, raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the place edit makes a crew file of")
	assert.Contains(t, err.Error(), "nothing was written")

	_, betaAfter := w.sums()
	assert.Equal(t, betaBefore, betaAfter, "the file on beta is as it was")
	assert.Equal(t, "alpha", w.placed("beta", "dev"))
	assert.Empty(t, w.active("beta", "alpha/T-2"), "and what the take made is taken away")

	_, ok, err := move.Receipt(w.layout("beta"), id)
	require.NoError(t, err)
	assert.False(t, ok, "the receipt of a take that placed nothing is not kept")

	// Directly: placeRole refuses the same, and the sum it returns is the sum of the file it wrote.
	w.as("beta", func() {
		a := w.peerApp()
		a.crew = w.file("beta")

		cr := mustCrew(t, w.file("beta"))

		sum, err := a.placeRole(cr, "dev", "beta", cr.Sum(), strings.Repeat("f", 64), "test")
		require.Error(t, err)
		assert.Empty(t, sum)
		assert.Equal(t, betaBefore, cr.Sum(), "nothing was written")
	})
}

// A journal that stayed in a crew whose file declares no nodes holds its role down. --abort ends it there with no node to ask, and the
// refusals that name it say so, and doctor tells it; no hand-made rm.
func TestAbortWorksForAJournalLeftInACrewWithoutNodes(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	w.work(false)

	w.crash("asked")

	out, _, err := w.move("dev", "beta")
	require.ErrorContains(t, err, "the process died here")

	id := w.idOf(out)

	// The nodes are taken out of the file: nothing can move the role, and the journal is stray.
	flat := strings.NewReplacer("home: alpha\n", "", "nodes:\n  alpha: {prefix: al-}\n  beta: {prefix: be-}\n", "").Replace(readFile(t, w.file("alpha")))
	require.NotContains(t, flat, "nodes:")
	require.NoError(t, os.WriteFile(w.file("alpha"), []byte(flat), 0o600))

	_, err = w.on("alpha", "up", "dev")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declares no nodes:")
	assert.Contains(t, err.Error(), "--abort "+id)

	doctor, _ := w.on("alpha", "doctor")
	assert.Contains(t, doctor, "dev has a move journal ("+id+" to beta) but this crew's file declares no nodes:")

	// A wrong id is refused naming what is open.
	_, _, err = w.move("dev", "beta", "--abort", "mv-aaaaaaaaaaaa")
	require.ErrorContains(t, err, "no move mv-aaaaaaaaaaaa of dev is open on this node")

	out, errOut, err := w.move("dev", "beta", "--abort", id)
	require.NoError(t, err, out+errOut)
	assert.Contains(t, out, "called off on this node")
	assert.False(t, w.journal("alpha", "dev"), "the journal is gone")
	assert.True(t, w.up("alpha", "dev"), "the role is up on its task again")

	// --resume has nowhere to go in such a crew.
	_, _, err = w.move("dev", "beta", "--resume", id)
	require.ErrorContains(t, err, "declares no nodes:")
}

// An answer to the abort's question that cannot be had or does not read stops the abort, the same as one that cannot be asked: the node
// may have taken the work. --force goes on.
func TestMoveAbortFailsClosedOnAnAnswerItCannotRead(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	w.work(false)

	id, _ := w.readyBundle()

	for name, garbage := range map[string][]byte{"not json": []byte("<html>"), "an empty answer": nil, "a state it does not know": []byte(`{"state":"maybe"}`),
		"none": []byte(`{"state":"none"}`)} {
		w.tamper = func(c mesh.MoveCall, data []byte) []byte {
			if c.Action == "cancel" {
				return garbage
			}

			return data
		}

		_, _, err := w.move("dev", "beta", "--abort", id)
		require.Error(t, err, name)
		assert.True(t, w.journal("alpha", "dev"), name+": nothing was called off")
		assert.False(t, w.up("alpha", "dev"), name)
	}

	w.tamper = func(c mesh.MoveCall, data []byte) []byte {
		if c.Action == "cancel" {
			return []byte("<html>")
		}

		return data
	}

	out, errOut, err := w.move("dev", "beta", "--abort", id, "--force")
	require.NoError(t, err, out+errOut)
	assert.Contains(t, out, "answered with something that does not read: the move is called off anyway")
	assert.False(t, w.journal("alpha", "dev"))
	assert.True(t, w.up("alpha", "dev"))
}

// Two repositories are the same when git sends fetches and pushes to the same place: a url rewrite is applied before they are compared,
// a push URL of its own is compared too.
func TestMovePreflightComparesTheOriginsGitResolves(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, _, branch := w.work(false)

	// A push URL on alpha that beta does not have.
	other := filepath.Join(t.TempDir(), "elsewhere.git")
	gitIn(t, w.repo("alpha"), "remote", "set-url", "--push", "origin", other)

	_, _, err := w.move("dev", "beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the URL pushes to origin go to differs")
	assert.Contains(t, err.Error(), other)
	assert.False(t, w.journal("alpha", "dev"), "nothing was begun")

	gitIn(t, w.repo("alpha"), "config", "--unset", "remote.origin.pushurl")

	// The same origin said through a rewrite on alpha (a spelling), which beta spells out: one origin.
	gitIn(t, w.repo("alpha"), "config", "url."+w.remote+".insteadOf", "acme-origin:")
	gitIn(t, w.repo("alpha"), "remote", "set-url", "origin", "acme-origin:")

	out, errOut, err := w.move("dev", "beta")
	require.NoError(t, err, out+errOut)
	w.assertMoved(branch)
}

// One step of a move runs at a node at a time. A poll that finds another step in progress says so and touches nothing; a commit waits for
// it, and never closes a journal that a slower step writes after it.
func TestPollsOfOneMoveDoNotOverlapAndACommitWaitsForTheStepThatRuns(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, _, branch := w.work(false)

	w.crash("released")

	out, _, err := w.move("dev", "beta")
	require.ErrorContains(t, err, "the process died here")

	id := w.idOf(out)
	layout := w.layout("alpha")

	release, err := move.Lock(layout, "dev", 0)
	require.NoError(t, err)

	ans, err := w.stepOn("alpha", "poll", id)
	require.NoError(t, err)
	assert.Equal(t, move.StatePending, ans.State)
	assert.Contains(t, ans.Why, "another step of this move is running")

	var j move.Journal

	w.as("alpha", func() {
		var jerr error

		j, _, jerr = move.Open(layout, "dev")
		require.NoError(t, jerr)
	})

	assert.Equal(t, move.PhaseReleased, j.Phase, "the poll that found the lock taken did nothing")

	release()

	ans, err = w.stepOn("alpha", "poll", id)
	require.NoError(t, err)
	assert.Equal(t, move.StateReady, ans.State, "the lock is free: the poll goes on")

	// A slow step still holds the lock when the commit comes, and writes the journal it read before: the commit waits for it, and closes
	// the journal after it, so the journal is not there afterwards.
	var stale move.Journal

	w.as("alpha", func() {
		var jerr error

		stale, _, jerr = move.Open(layout, "dev")
		require.NoError(t, jerr)
	})

	ta, err := w.takeOnBeta(id, stale.Bundle)
	require.NoError(t, err)

	keep := moveLockWait
	moveLockWait = 5 * time.Second

	t.Cleanup(func() { moveLockWait = keep })

	locked, finished := make(chan struct{}), make(chan struct{})

	go func() {
		defer close(finished)

		slow, err := move.Lock(layout, "dev", 0)
		if err != nil {
			close(locked)

			return
		}

		close(locked)
		time.Sleep(400 * time.Millisecond)

		_ = move.Save(layout, stale)

		slow()
	}()

	<-locked

	body, err := json.Marshal(move.CommitBody{To: "beta", Taken: map[string]string{"T-2": "beta/" + ta.Tasks[0].To}, Sum: ta.Sum})
	require.NoError(t, err)

	var commit move.PollAnswer

	start := time.Now()

	w.as("alpha", func() {
		a := w.ownerApp(io.Discard, io.Discard)
		a.crew = w.file("alpha")

		raw, err := a.localRoleMove(mesh.MoveCall{Crew: "ops", Role: "dev", Action: "commit", Move: id, Payload: body, By: "hand"})
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &commit))
	})

	assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond, "the commit waited for the step that was running")
	assert.Equal(t, move.StateCommitted, commit.State)

	<-finished

	assert.False(t, w.journal("alpha", "dev"), "no slower step brought the journal back after the commit")
	w.assertMoved(branch)
}

// While a move is open on a node its crew file is not changed by a seat edit either (a push is refused the same, in the crew package):
// the message names the move and the way out.
func TestSeatEditIsRefusedWhileAMoveIsOpen(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	w.work(false)

	w.crash("asked")

	out, _, err := w.move("dev", "beta")
	require.ErrorContains(t, err, "the process died here")

	id := w.idOf(out)

	_, err = w.on("alpha", "seat", "set", "ops", "about=something")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a move of dev to beta is open on this node ("+id+")")
	assert.Contains(t, err.Error(), "zmina move dev beta --resume "+id)
	assert.Contains(t, err.Error(), "--abort "+id)
	assert.NotContains(t, readFile(t, w.file("alpha")), "something")

	// The way out the message names: --abort, and the file may change again.
	out, errOut, err := w.move("dev", "beta", "--abort", id)
	require.NoError(t, err, out+errOut)

	_, err = w.on("alpha", "seat", "set", "ops", "about=something")
	require.NoError(t, err)
	assert.Contains(t, readFile(t, w.file("alpha")), "something")
}

// The ticker's beat has a time of its own for the moves of a crew: a push that is slower than it stops there, the beat returns, and the
// next beat goes on. A step another beat is in is not run twice.
func TestMoveBeatStopsAtItsTimeAndTheNextBeatGoesOn(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, _, branch := w.work(false)

	w.crash("released")

	_, _, err := w.move("dev", "beta")
	require.ErrorContains(t, err, "the process died here")

	// The remote takes eight seconds to accept a push.
	hook := filepath.Join(w.remote, "hooks", "pre-receive")
	require.NoError(t, os.MkdirAll(filepath.Dir(hook), 0o700))
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nsleep 8\nexit 0\n"), 0o700))

	keep := moveBeatBudget
	moveBeatBudget = 600 * time.Millisecond

	t.Cleanup(func() { moveBeatBudget = keep })

	var lines bytes.Buffer

	beat := func() time.Duration {
		start := time.Now()

		w.as("alpha", func() {
			a := w.ownerApp(&lines, io.Discard)
			a.crew = w.file("alpha")
			a.moveBeat()
		})

		return time.Since(start)
	}

	took := beat()
	assert.Less(t, took, 5*time.Second, "the beat did not wait out the push (its time, and the moment git needs to be stopped)")
	assert.True(t, w.journal("alpha", "dev"), "the move is still open")

	var j move.Journal

	w.as("alpha", func() {
		var jerr error

		j, _, jerr = move.Open(w.layout("alpha"), "dev")
		require.NoError(t, jerr)
	})

	assert.Equal(t, move.PhaseReleased, j.Phase, "not ready: the push was stopped")
	assert.NotContains(t, lines.String(), "deadline", "a step cut by the beat's time is no error of the move")

	// A beat while another step holds the lock does nothing at all.
	release, err := move.Lock(w.layout("alpha"), "dev", 0)
	require.NoError(t, err)

	moveBeatBudget = 20 * time.Second
	took = beat()
	release()

	assert.Less(t, took, 2*time.Second, "the step that runs is not run twice, nor waited for")

	// The hook is quick again: the next beat hands the work over, takes it and ends the move.
	require.NoError(t, os.Remove(hook))

	beat()

	w.assertMoved(branch)
}

// checkpoint gives dev's T-2 on alpha a checkpoint, which travels with the task.
func (w *moveWorld) checkpoint() {
	w.t.Helper()

	w.as("alpha", func() {
		c := mustCrew(w.t, w.file("alpha"))
		shape := recordShape(c.HandoffFor(c.Roles["dev"]))

		values := map[string]string{}
		for _, s := range shape.Sections {
			values[s.Name] = "section " + s.Name
		}

		ref, err := record.Save(c.StateDir(), record.Document{Version: 1, Kind: "checkpoint", Task: "T-2", Role: "dev", By: "dev", At: time.Now(), Shape: shape, Values: values})
		require.NoError(w.t, err)

		st := taskStore(c)

		tk, err := st.Get("T-2")
		require.NoError(w.t, err)

		tk.Checkpoint, tk.CheckpointTaskUpdated = &ref, tk.Updated
		require.NoError(w.t, st.Put(tk))
	})
}

// A take that cannot save a record of the task after it made the task (the records' directory cannot be made) leaves no task without its
// worktree behind: the task is taken away at once, and the retry makes it whole.
func TestTakeThatFailsSavingARecordLeavesNoHalfMadeTaskForTheRetry(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, _, branch := w.work(false)
	w.checkpoint()

	blocker := filepath.Join(w.homes["beta"], ".zmina", "ops", "checkpoints")
	require.NoError(t, os.WriteFile(blocker, []byte("a file where the records' directory goes\n"), 0o600))

	out, _, err := w.move("dev", "beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the checkpoint")

	id := w.idOf(out)

	assert.Empty(t, w.active("beta", "alpha/T-2"), "the half-made task is taken away")

	_, ok, err := move.OpenTaking(w.layout("beta"), "dev")
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, os.Remove(blocker))

	out, errOut, err := w.move("dev", "beta", "--resume", id)
	require.NoError(t, err, out+errOut)

	moved := w.assertMoved(branch)
	assert.NotEmpty(t, moved.Worktree)
	assert.DirExists(t, moved.Worktree)
	require.NotNil(t, moved.Checkpoint)
}

// A move that was called off is not taken afterwards: the abort leaves a tombstone of the move at the node the role would have gone to,
// and a take of it (by a stale process of the owner that still holds the bundle) is refused, before anything is made and so also before the
// role is placed.
func TestMoveAbortThenAStaleTakeIsRefused(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	w.work(false)

	id, bundle := w.readyBundle()

	out, errOut, err := w.move("dev", "beta", "--abort", id)
	require.NoError(t, err, out+errOut)
	assert.True(t, w.up("alpha", "dev"), "the role runs on alpha again")

	gone, err := move.Aborted(w.layout("beta"), id)
	require.NoError(t, err)
	assert.True(t, gone, "beta keeps the tombstone of the move")

	ans, err := w.stepOn("beta", "poll", id)
	require.NoError(t, err)
	assert.Equal(t, move.StateAborted, ans.State)

	_, err = w.takeOnBeta(id, bundle)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "was called off at this node")

	assert.Empty(t, w.tasks("beta"), "nothing was made")
	assert.Equal(t, "alpha", w.placed("beta", "dev"), "the role is not placed on beta")
	assert.False(t, w.up("beta", "dev"))
	assert.True(t, w.up("alpha", "dev"), "the role is up on one node only")

	// Called off again (a lost answer): the journal is gone, and the owner is told so.
	out, errOut, err = w.move("dev", "beta", "--abort", id)
	require.Error(t, err, out+errOut)
	assert.Contains(t, err.Error(), "no node holds an open move")
}

// While a take runs at the node, the abort is refused: poll and check say "taking", the abort says so, and nothing is called off. When the
// take was cut short (nothing holds its lock) the abort takes away what it made and goes through.
func TestMoveAbortIsRefusedWhileATakeRunsAndTakesAwayACutShortOne(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	w.work(false)

	id, _ := w.readyBundle()

	// A take that runs: its marker is there and its lock is held.
	layout := w.layout("beta")
	require.NoError(t, move.SaveTaking(layout, move.Taking{ID: id, Role: "dev", From: "alpha", Started: time.Now().UTC(), Origins: []string{"alpha/T-2"}}))

	release, err := move.Lock(layout, "dev", 0)
	require.NoError(t, err)

	ans, err := w.stepOn("beta", "poll", id)
	require.NoError(t, err)
	assert.Equal(t, move.StateTaking, ans.State)
	assert.Contains(t, ans.Why, "is running here")

	var facts move.Facts

	w.as("beta", func() {
		a := w.peerApp()
		a.crew = w.file("beta")

		raw, err := a.localRoleMove(mesh.MoveCall{Crew: "ops", Role: "dev", Action: "check", By: "hand"})
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &facts))
	})

	assert.Equal(t, id, facts.Taking, "check says it too")

	_, _, err = w.move("dev", "beta", "--abort", id)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is taking the work now")
	assert.True(t, w.journal("alpha", "dev"), "nothing was called off")
	assert.False(t, w.up("alpha", "dev"))

	gone, err := move.Aborted(layout, id)
	require.NoError(t, err)
	assert.False(t, gone, "no tombstone while a take runs")

	// The take was cut short: its lock is free, its marker is still there.
	release()

	ans, err = w.stepOn("beta", "poll", id)
	require.NoError(t, err)
	assert.Equal(t, move.StateTaking, ans.State)
	assert.Contains(t, ans.Why, "was cut short")

	out, errOut, err := w.move("dev", "beta", "--abort", id)
	require.NoError(t, err, out+errOut)

	_, ok, err := move.OpenTaking(layout, "dev")
	require.NoError(t, err)
	assert.False(t, ok, "the marker is gone")

	gone, err = move.Aborted(layout, id)
	require.NoError(t, err)
	assert.True(t, gone)
	assert.True(t, w.up("alpha", "dev"))
}

// A take whose process dies after it made the tasks, before its receipt, leaves tasks on the node the role is not placed on. The abort
// takes them away (and their worktrees and refs), leaves the tombstone, and the role is active on one node only, with its task once.
func TestMoveAbortAfterATakeCutShortLeavesNothingActiveOnTheOtherNode(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	w.work(true)

	w.crash("taken-import")

	out, _, err := w.move("dev", "beta")
	require.ErrorContains(t, err, "the process died here")

	id := w.idOf(out)

	orphans := w.active("beta", "alpha/T-2")
	require.Len(t, orphans, 1, "the take made the task before it died")
	assert.DirExists(t, orphans[0].Worktree)

	_, ok, err := move.OpenTaking(w.layout("beta"), "dev")
	require.NoError(t, err)
	require.True(t, ok, "its marker says a take began")

	ans, err := w.stepOn("beta", "poll", id)
	require.NoError(t, err)
	assert.Equal(t, move.StateTaking, ans.State)

	out, errOut, err := w.move("dev", "beta", "--abort", id)
	require.NoError(t, err, out+errOut)

	assert.Empty(t, w.active("beta", "alpha/T-2"), "no task made by the take is active on beta")
	assert.NoDirExists(t, orphans[0].Worktree, "nor is its worktree there")
	assert.Equal(t, "alpha", w.placed("beta", "dev"))
	assert.Equal(t, "alpha", w.placed("alpha", "dev"))
	assert.False(t, w.up("beta", "dev"))
	assert.True(t, w.up("alpha", "dev"))
	assert.False(t, w.journal("alpha", "dev"))
	assert.Empty(t, gitIn(t, w.repo("beta"), "for-each-ref", "refs/zmina-move"))
	assert.Empty(t, gitIn(t, w.remote, "for-each-ref", "refs/zmina-move"))

	_, ok, err = move.OpenTaking(w.layout("beta"), "dev")
	require.NoError(t, err)
	assert.False(t, ok)

	// The task is open again on alpha, once.
	var open int

	for _, tk := range w.tasks("alpha") {
		if tk.ID == "T-2" && tk.State == task.Open {
			open++
		}
	}

	assert.Equal(t, 1, open)

	// The dropped orphan says why.
	var dropped task.Task

	for _, tk := range w.tasks("beta") {
		if tk.Origin == "alpha/T-2" {
			dropped = tk
		}
	}

	assert.Equal(t, task.Dropped, dropped.State)
	assert.Contains(t, dropped.Note, "cut short and taken away")
}

// A take that fails (not dies) part-way takes back what it made in the same call: the task of the first attempt is dropped with its
// worktree, no marker stays, and the retry makes the task again, once.
func TestTakeThatFailsPartWayTakesBackWhatItMadeAndTheRetryStartsClean(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, _, branch := w.work(true)

	// The path of the worktree the take would make is taken by a file: the take fails after it made the task.
	obstacle := filepath.Join(w.homes["beta"], ".zmina", "ops", "worktrees", "acme-T-1")
	require.NoError(t, os.MkdirAll(filepath.Dir(obstacle), 0o700))
	require.NoError(t, os.WriteFile(obstacle, []byte("in the way\n"), 0o600))

	out, _, err := w.move("dev", "beta")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not take the work")
	assert.Contains(t, err.Error(), "already exists")

	id := w.idOf(out)

	assert.Empty(t, w.active("beta", "alpha/T-2"), "what the take made is taken away")

	var first task.Task

	for _, tk := range w.tasks("beta") {
		if tk.Origin == "alpha/T-2" {
			first = tk
		}
	}

	assert.Equal(t, task.Dropped, first.State)

	_, ok, err := move.OpenTaking(w.layout("beta"), "dev")
	require.NoError(t, err)
	assert.False(t, ok, "no marker stays after a take that failed")
	assert.Equal(t, "alpha", w.placed("beta", "dev"))
	assert.True(t, w.journal("alpha", "dev"), "the move stays open on alpha")
	assert.Empty(t, gitIn(t, w.repo("beta"), "for-each-ref", "refs/zmina-move"))

	require.NoError(t, os.Remove(obstacle))

	out, errOut, err := w.move("dev", "beta", "--resume", id)
	require.NoError(t, err, out+errOut)

	moved := w.assertMoved(branch)
	assert.Equal(t, "T-2", moved.ID, "a fresh task: T-1 stays the dropped one of the first attempt")
	assert.Len(t, w.active("beta", "alpha/T-2"), 1)
	assert.Equal(t, "form v2, not committed\n", readFile(t, filepath.Join(moved.Worktree, "form.txt")))
}

// A task an interrupted take made and somebody closed afterwards is not the task of a later take: the later take makes its own, which is
// active, and the work is not left active on neither node.
func TestATakeDoesNotReuseAClosedTaskOfAnEarlierTry(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, _, branch := w.work(false)

	w.crash("taken-import")

	out, _, err := w.move("dev", "beta")
	require.ErrorContains(t, err, "the process died here")

	id := w.idOf(out)

	orphan := w.active("beta", "alpha/T-2")
	require.Len(t, orphan, 1)

	w.as("beta", func() {
		st := taskStore(mustCrew(t, w.file("beta")))

		tk, err := st.Get(orphan[0].ID)
		require.NoError(t, err)

		tk.State = task.Done
		require.NoError(t, st.Put(tk))
	})

	// The closed task's worktree still has the branch: the take says so and makes nothing (it does not take the closed task for its own).
	_, _, err = w.move("dev", "beta", "--resume", id)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is checked out in")
	assert.Empty(t, w.active("beta", "alpha/T-2"), "no task of the work is active: the failed take made none that stayed")

	gitIn(t, w.repo("beta"), "worktree", "remove", "--force", orphan[0].Worktree)

	out, errOut, err := w.move("dev", "beta", "--resume", id)
	require.NoError(t, err, out+errOut)

	moved := w.assertMoved(branch)
	assert.NotEqual(t, orphan[0].ID, moved.ID, "the closed task was not taken for the work")
	assert.Equal(t, task.Open, moved.State)
	assert.True(t, w.up("beta", "dev"))
}

// A take is all or nothing before its receipt, and only finished after it: a process that dies between the receipt and the place leaves a
// node that took the work, so the abort is refused, and the next take of the move places the role.
func TestMoveAbortIsRefusedOnceTheReceiptIsKeptEvenBeforeTheRoleIsPlaced(t *testing.T) {
	w := newMoveWorld(t, "alpha")
	_, _, branch := w.work(false)

	w.crash("taken-flip")

	out, _, err := w.move("dev", "beta")
	require.ErrorContains(t, err, "the process died here")

	id := w.idOf(out)

	_, ok, err := move.Receipt(w.layout("beta"), id)
	require.NoError(t, err)
	require.True(t, ok, "the receipt is the point of no return")
	assert.Equal(t, "alpha", w.placed("beta", "dev"), "the role is not placed yet")

	_, _, err = w.move("dev", "beta", "--abort", id)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "took the work already")
	assert.True(t, w.journal("alpha", "dev"))

	out, errOut, err := w.move("dev", "beta", "--resume", id)
	require.NoError(t, err, out+errOut)

	w.assertMoved(branch)
	assert.Len(t, w.active("beta", "alpha/T-2"), 1, "once")
}
