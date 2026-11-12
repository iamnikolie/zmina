package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/task"
)

// lateCrew is a crew with a dev and a review role, and T-1 for dev handed out as hand-out 3 to the session sess-new: the
// hand-out a restart makes, which the old session sess-old no longer holds.
type lateCrew struct {
	file string
	c    *crew.Crew
}

func newLateCrew(t *testing.T) *lateCrew {
	t.Helper()

	shape := "    handoff:\n      sections:\n        - name: draft\n"
	file := testCrew(t, "  dev:\n"+shape+"  review:\n"+shape)

	_, _, err := zm(t, file, "task", "new", "dev", "Answer the question")
	require.NoError(t, err)

	c, err := crew.Load(file)
	require.NoError(t, err)

	l := &lateCrew{file: file, c: c}
	l.handOut(t, task.Delivery{ID: "pkt-a", Role: "dev", Gen: 3, Session: "sess-new", QueuedAt: time.Now().UTC()})

	return l
}

// handOut writes the delivery a hand-out leaves on T-1.
func (l *lateCrew) handOut(t *testing.T, d task.Delivery) {
	t.Helper()

	tk, err := taskStore(l.c).Get("T-1")
	require.NoError(t, err)

	tk.Delivery = &d
	require.NoError(t, taskStore(l.c).Put(tk))
}

func (l *lateCrew) task(t *testing.T) task.Task {
	t.Helper()

	tk, err := taskStore(l.c).Get("T-1")
	require.NoError(t, err)

	return tk
}

// body writes a result of the draft section.
func (l *lateCrew) body(t *testing.T, text string) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "result.json")
	require.NoError(t, os.WriteFile(p, []byte(`{"draft":"`+text+`"}`), 0o600))

	return p
}

// lateEvents counts the late-report events of T-1.
func (l *lateCrew) lateEvents(t *testing.T) int {
	t.Helper()

	evs, err := eventLog(l.c).Read(events.Filter{Task: "T-1"})
	require.NoError(t, err)

	n := 0

	for _, e := range evs {
		if e.Event == "late-report" {
			n++
		}
	}

	return n
}

// asSession is a command run by role from the conversation session ("" for a process that names none).
func asSession(t *testing.T, role, session string) {
	t.Helper()

	t.Setenv("ZMINA_ROLE", role)
	t.Setenv("CLAUDE_CODE_SESSION_ID", session)
	t.Setenv("CODEX_THREAD_ID", "")
}

// A result from a session that held an earlier hand-out is kept as a revision and changes nothing: not the task's result, not
// its Updated (which a newer hand-out is accepted against), not its state. The caller is told, exit 5, and so is the
// dispatcher, once, whatever the old session does after.
func TestLateResultIsKeptAsARevisionAndChangesNothing(t *testing.T) {
	l := newLateCrew(t)
	before := l.task(t)

	asSession(t, "dev", "sess-old")

	out, errOut, err := zm(t, l.file, "task", "result", "1", "--file", l.body(t, "from the old session"))
	require.Error(t, err)
	assert.Equal(t, exitLate, exitCodeOf(t, err))
	assert.Empty(t, out, "nothing was saved as the task's result")
	assert.Contains(t, err.Error(), "T-1: late report: it was handed out again to another session (hand-out 3, to dev, made ")
	assert.Regexp(t, `kept as result revision [0-9a-f]{12}, not the task's result, and the task is unchanged`, err.Error())
	_ = errOut

	got := l.task(t)
	assert.Nil(t, got.Result)
	assert.Equal(t, before.Updated, got.Updated)
	assert.Equal(t, task.Open, got.State)
	assert.Equal(t, before.Delivery, got.Delivery)
	require.Len(t, got.Late, 1)
	assert.Equal(t, "result", got.Late[0].What)
	assert.Equal(t, "sess-old", got.Late[0].Session)
	assert.Equal(t, 3, got.Late[0].Gen)

	// The revision is in the store, readable by the owner.
	asSession(t, "", "")

	read, _, err := zm(t, l.file, "task", "result", "1", "--revision", got.Late[0].Revision, "--json")
	require.NoError(t, err)
	assert.Contains(t, read, "from the old session")

	// One event, one note to the dispatcher, no "result ready" for it.
	assert.Equal(t, 1, l.lateEvents(t))

	queue, _, err := zm(t, l.file, "queue", "ls")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(queue, "T-1 was handed out again (hand-out 3, to dev)"), queue)
	assert.NotContains(t, queue, "result ready")

	// The old session goes on: the same again, and a changed one. Told no more.
	asSession(t, "dev", "sess-old")

	_, _, err = zm(t, l.file, "task", "result", "1", "--file", l.body(t, "from the old session"))
	require.Error(t, err)
	_, _, err = zm(t, l.file, "task", "checkpoint", "1", "--file", l.body(t, "a note nobody asked for"))
	require.Error(t, err)
	assert.Equal(t, exitLate, exitCodeOf(t, err))

	asSession(t, "", "")

	queue, _, err = zm(t, l.file, "queue", "ls")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(queue, "was handed out again"), "told once: %s", queue)
	assert.Equal(t, 2, l.lateEvents(t), "the repeat is no new report; the checkpoint is")

	got = l.task(t)
	assert.Nil(t, got.Result)
	assert.Nil(t, got.Checkpoint, "a late checkpoint is not the task's checkpoint either")
	assert.Equal(t, before.Updated, got.Updated)
	assert.Len(t, got.Late, 2)

	// task show says the hand-out and what was kept.
	shown, _, err := zm(t, l.file, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, shown, "handoff: pkt-a · queued, awaiting acceptance · hand-out 3 to dev")
	assert.Regexp(t, `(?m)^late: result [0-9a-f]{12} · dev · .* · session sess-old · hand-out 3 was current · kept, not the task's result$`, shown)
	assert.Regexp(t, `(?m)^late: checkpoint [0-9a-f]{12} · `, shown)

	js, _, err := zm(t, l.file, "task", "show", "1", "--json")
	require.NoError(t, err)
	assert.Contains(t, js, `"gen": 3`)
	assert.Contains(t, js, `"late": [`)
}

// The session the task is handed to reports as it always did, and the result carries the hand-out it was saved under.
func TestResultOfTheCurrentSessionWorksAsToday(t *testing.T) {
	l := newLateCrew(t)
	before := l.task(t)

	asSession(t, "dev", "sess-new")

	out, _, err := zm(t, l.file, "task", "result", "1", "--file", l.body(t, "the real one"))
	require.NoError(t, err)
	assert.Regexp(t, `^T-1 result [0-9a-f]{64}\n$`, out)

	got := l.task(t)
	require.NotNil(t, got.Result)
	assert.Equal(t, 3, got.Result.Gen)
	assert.True(t, got.Updated.After(before.Updated))
	assert.Empty(t, got.Late)
	assert.Zero(t, l.lateEvents(t))

	asSession(t, "", "")

	queue, _, err := zm(t, l.file, "queue", "ls")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(queue, "T-1 result ready (worker-reported)"), queue)

	// Closing with it, from the same session: the closing says under which hand-out.
	asSession(t, "dev", "sess-new")

	_, _, err = zm(t, l.file, "task", "done", "1", "--note", "shipped")
	require.NoError(t, err)

	closed := l.task(t).Closed
	require.NotNil(t, closed)
	assert.Equal(t, 3, closed.Gen)
	assert.False(t, closed.Unbound)

	asSession(t, "", "")

	shown, _, err := zm(t, l.file, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, shown, "closed: by dev under hand-out 3\n")
}

// The owner by hand and the dispatcher are not held to a hand-out, whatever session they run in.
func TestOwnerAndDispatcherReportAndCloseAsToday(t *testing.T) {
	l := newLateCrew(t)

	// The owner's hand: no role.
	asSession(t, "", "")

	_, _, err := zm(t, l.file, "task", "result", "1", "--file", l.body(t, "by hand"))
	require.NoError(t, err)
	require.NotNil(t, l.task(t).Result)

	// The dispatcher, from a conversation of its own.
	asSession(t, "dispatcher", "sess-dispatcher")

	_, _, err = zm(t, l.file, "task", "result", "1", "--file", l.body(t, "by the dispatcher"))
	require.NoError(t, err)

	_, _, err = zm(t, l.file, "task", "set", "1", "--note", "a note")
	require.NoError(t, err)

	_, _, err = zm(t, l.file, "task", "done", "1", "--note", "closed by the dispatcher")
	require.NoError(t, err)

	got := l.task(t)
	assert.Equal(t, task.Done, got.State)
	assert.Equal(t, "dispatcher", got.Closed.By)
	assert.Equal(t, 3, got.Closed.Gen)
	assert.False(t, got.Closed.Unbound)
	assert.Empty(t, got.Late)
	assert.Zero(t, l.lateEvents(t))
}

// A close, a park or a change from a late session of the task's role is refused: the task is as it was, and the message says why
// and what to do. Another role is refused only what ends or reports the work (a copy is not its base), and told so; its plain
// edits of a task pass as they did. A task closed with --result is told once, the result kept.
func TestLateCloseAndSetAreRefusedAndChangeNothing(t *testing.T) {
	l := newLateCrew(t)
	before := l.task(t)

	closing := [][]string{{"done", "1"}, {"drop", "1"}, {"park", "1"}, {"set", "1", "done"}, {"set", "1", "dropped"}}
	editing := [][]string{{"set", "1", "--note", "mine"}, {"set", "1", "waiting"}, {"set", "1", "--role", "review"}}

	for _, who := range []struct {
		role, session, why, next string
		edits                    bool
	}{
		{"dev", "sess-old", "handed out again to another session", "zmina task handoff T-1 --queue", false},
		{"review", "sess-review", "it is handed to dev, not review", "ask the dispatcher whether it should be yours (zmina task set T-1 --role review)", true},
		{"dev#2", "sess-new", "it is handed to dev, not dev#2", "zmina task set T-1 --role dev#2", true},
	} {
		asSession(t, who.role, who.session)

		args := closing
		if !who.edits {
			args = append(args, editing...)
		}

		for _, a := range args {
			_, _, err := zm(t, l.file, append([]string{"task"}, a...)...)
			require.Error(t, err, "%s %v", who.role, a)
			assert.Equal(t, exitLate, exitCodeOf(t, err), "%s %v: %v", who.role, a, err)
			assert.Contains(t, err.Error(), who.why)
			assert.Contains(t, err.Error(), "refused, the task is unchanged")
			assert.Contains(t, err.Error(), who.next, "what to do")
		}

		got := l.task(t)
		got.Late = nil
		assert.Equal(t, before, got, "%s: not one field of the task moved", who.role)
	}

	// Another role's plain edits are as they were on a task with no hand-out numbers.
	asSession(t, "review", "sess-review")

	_, _, err := zm(t, l.file, "task", "set", "1", "--note", "from the reviewer")
	require.NoError(t, err)
	assert.Equal(t, "from the reviewer", l.task(t).Note)

	for _, e := range l.task(t).Late {
		assert.False(t, e.What == "set" && e.Other, "a plain edit by another role is no late report: %+v", e)
	}

	// Of a close with a result: the result is kept as a revision and the close is not even tried.
	asSession(t, "dev", "sess-old")

	_, errOut, err := zm(t, l.file, "task", "done", "1", "--result", l.body(t, "late and closing"))
	require.Error(t, err)
	assert.Equal(t, exitLate, exitCodeOf(t, err))
	assert.Contains(t, err.Error(), "kept as result revision")
	assert.NotContains(t, errOut, "refused")

	got := l.task(t)
	assert.Nil(t, got.Result)
	assert.Equal(t, task.Open, got.State)

	// The owner sees all of it, the dispatcher was told once for each caller of the hand-out, and a role that never held the
	// task is not said to have held it.
	asSession(t, "", "")

	queue, _, err := zm(t, l.file, "queue", "ls")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(queue, "T-1 was handed out again (hand-out 3, to dev) and a session that held it before"), "dev: %s", queue)
	assert.Equal(t, 1, strings.Count(queue, "T-1 is handed to dev (hand-out 3) and review, which does not hold it"), "review: %s", queue)
	assert.Equal(t, 1, strings.Count(queue, "T-1 is handed to dev (hand-out 3) and dev#2, which does not hold it"), "dev#2: %s", queue)
	assert.NotContains(t, queue, "closed")

	shown, _, err := zm(t, l.file, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, shown, " · its role is not the task's")
}

// The same content saved by the session that holds the task after a late session kept it is the task's result: the dispatcher is
// told of it like any, and task show does not say both that it was kept as not the task's and that it is the task's.
func TestResultSavedAgainByTheHolderAfterALateOneIsTheTasksAndTold(t *testing.T) {
	l := newLateCrew(t)
	body := l.body(t, "the same words")

	asSession(t, "dev", "sess-old")

	_, _, err := zm(t, l.file, "task", "result", "1", "--file", body)
	require.Equal(t, exitLate, exitCodeOf(t, err))

	asSession(t, "", "")

	queue, _, err := zm(t, l.file, "queue", "ls")
	require.NoError(t, err)
	assert.NotContains(t, queue, "result ready", "a late revision is no result")

	asSession(t, "dev", "sess-new")

	_, _, err = zm(t, l.file, "task", "result", "1", "--file", body)
	require.NoError(t, err)

	got := l.task(t)
	require.NotNil(t, got.Result)
	require.Len(t, got.Late, 1)
	assert.Equal(t, got.Late[0].Revision, got.Result.Revision, "the one revision")
	assert.True(t, got.Late[0].Taken)

	asSession(t, "", "")

	queue, _, err = zm(t, l.file, "queue", "ls")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(queue, "T-1 result ready (worker-reported)"), "told now: %s", queue)

	shown, _, err := zm(t, l.file, "task", "show", "1")
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^late: result [0-9a-f]{12} · .* · the same was saved as the task's result since$`, shown)
	assert.NotContains(t, shown, "kept, not the task's result")

	// A late session repeating what is the task's own result: it is told, and show does not call it not the task's.
	asSession(t, "dev", "sess-old")

	_, _, err = zm(t, l.file, "task", "result", "1", "--file", body)
	require.Equal(t, exitLate, exitCodeOf(t, err), "still a session that does not hold it")

	asSession(t, "", "")

	shown, _, err = zm(t, l.file, "task", "show", "1")
	require.NoError(t, err)
	assert.NotContains(t, shown, "kept, not the task's result")
}

// A role with no session of its own to show passes on its name, and says so on the closing; naming the packet binds it.
func TestRoleWithNoSessionIsHeldToItsNameOrItsPacket(t *testing.T) {
	l := newLateCrew(t)

	asSession(t, "dev", "")

	_, _, err := zm(t, l.file, "task", "set", "1", "--note", "still going", "--handoff", "pkt-z")
	require.Error(t, err)
	assert.Equal(t, exitLate, exitCodeOf(t, err))
	assert.Contains(t, err.Error(), "its packet is not the one it is handed out with now")

	_, _, err = zm(t, l.file, "task", "set", "1", "--note", "still going", "--handoff", "pkt-a")
	require.NoError(t, err)
	assert.Equal(t, "still going", l.task(t).Note)

	_, _, err = zm(t, l.file, "task", "result", "1", "--file", l.body(t, "no session, no packet"))
	require.NoError(t, err, "a role that shows nothing passes on its name")

	_, _, err = zm(t, l.file, "task", "done", "1")
	require.NoError(t, err)

	closed := l.task(t).Closed
	require.NotNil(t, closed)
	assert.True(t, closed.Unbound)
	assert.Equal(t, 3, closed.Gen)

	asSession(t, "", "")

	shown, _, err := zm(t, l.file, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, shown, "closed: by dev under hand-out 3 · unbound")
}

// A task file from before hand-outs were counted: a delivery without a generation, or none at all, is no hand-out to be late
// for. Every caller works as it did until the next hand-out.
func TestTaskFileWithoutAGenerationIsNotGated(t *testing.T) {
	for name, d := range map[string]*task.Delivery{
		"a delivery without a generation": {ID: "pkt-a", Role: "dev", Session: "sess-new", QueuedAt: time.Now().UTC()},
		"no delivery":                     nil,
	} {
		t.Run(name, func(t *testing.T) {
			l := newLateCrew(t)

			tk := l.task(t)
			tk.Delivery = d
			require.NoError(t, taskStore(l.c).Put(tk))

			asSession(t, "review", "sess-anyone")

			_, _, err := zm(t, l.file, "task", "result", "1", "--file", l.body(t, "as it was"))
			require.NoError(t, err)
			require.NotNil(t, l.task(t).Result)
			assert.Zero(t, l.task(t).Result.Gen)

			_, _, err = zm(t, l.file, "task", "done", "1")
			require.NoError(t, err)

			closed := l.task(t).Closed
			require.NotNil(t, closed)
			assert.Zero(t, closed.Gen)
			assert.False(t, closed.Unbound)

			raw, err := os.ReadFile(filepath.Join(filepath.Dir(l.file), "tasks", "T-1.json"))
			require.NoError(t, err)
			assert.NotContains(t, string(raw), `"gen"`)
			assert.NotContains(t, string(raw), `"late"`)
			assert.Zero(t, l.lateEvents(t))
		})
	}
}

// A late session's task done is refused before its check runs: no command starts for it. The session the task is handed to
// runs it and closes under the hand-out it holds (a hand-out made while the check ran is the ledger's test).
func TestLateDoneIsRefusedBeforeTheCheckRuns(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	c := newCheckCrew(t, "")
	c.task(t, "--check", "touch "+marker)

	cr, err := crew.Load(c.file)
	require.NoError(t, err)

	tk, err := taskStore(cr).Get("T-1")
	require.NoError(t, err)

	tk.Delivery = &task.Delivery{ID: "pkt-a", Role: "dev", Gen: 3, Session: "sess-new", QueuedAt: time.Now().UTC()}
	require.NoError(t, taskStore(cr).Put(tk))

	asSession(t, "dev", "sess-old")

	_, errOut, err := zm(t, c.file, "task", "done", "1")
	require.Error(t, err)
	assert.Equal(t, exitLate, exitCodeOf(t, err))
	assert.NotContains(t, errOut, "running its check")
	assert.NoFileExists(t, marker, "the check was not run for a session that does not hold the task")

	got, err := taskStore(cr).Get("T-1")
	require.NoError(t, err)
	assert.Equal(t, task.Open, got.State)
	assert.Nil(t, got.Closed)
	require.Len(t, got.Late, 1)
	assert.Equal(t, "done", got.Late[0].What)

	asSession(t, "dev", "sess-new")

	_, errOut, err = zm(t, c.file, "task", "done", "1")
	require.NoError(t, err)
	assert.Contains(t, errOut, "running its check")
	assert.FileExists(t, marker)

	got, err = taskStore(cr).Get("T-1")
	require.NoError(t, err)
	require.NotNil(t, got.Closed)
	assert.Equal(t, 3, got.Closed.Gen)
	require.NotNil(t, got.Closed.Check, "closed on a passed check")
}

// The checkpoint of a late session is not the task's, which a fresh session is handed, and it is not lost either: it is kept in
// the store, listed on the task.
func TestLateCheckpointIsKeptInTheStoreAndNotTheTasks(t *testing.T) {
	l := newLateCrew(t)

	asSession(t, "dev", "sess-old")

	_, _, err := zm(t, l.file, "task", "checkpoint", "1", "--file", l.body(t, "stale"))
	require.Error(t, err)

	tk := l.task(t)
	require.Len(t, tk.Late, 1)
	assert.Nil(t, tk.Checkpoint)

	revs := record.Revisions(l.c.StateDir(), "checkpoint", []string{"T-1"})
	assert.Equal(t, []string{tk.Late[0].Revision}, revs, "kept in the store, nothing lost")
}

// From the hand-out to the report, through the registry's live session: a task is handed to a role that is up, its line is
// submitted to the conversation the role runs, the role accepts it, and then the conversation changes under the running role
// (a /clear in its pane): the role's reports from the new conversation go through and move the delivery to it, with no note,
// while the conversation it left is a late reporter from then on.
func TestReportsFollowTheRolesLiveConversationAfterAClearInItsPane(t *testing.T) {
	file := testCrew(t, "  dev:\n    handoff:\n      sections:\n        - name: draft\n")
	n := &nowRoles{status: "idle"}
	n.up(t, file, time.Now().Add(-30*time.Second).UTC().Truncate(time.Second))

	c, err := crew.Load(file)
	require.NoError(t, err)

	delivery := func() task.Delivery {
		tk, err := taskStore(c).Get("T-1")
		require.NoError(t, err)
		require.NotNil(t, tk.Delivery)

		return *tk.Delivery
	}

	body := filepath.Join(t.TempDir(), "note.json")
	require.NoError(t, os.WriteFile(body, []byte(`{"draft":"half done"}`), 0o600))

	// Hand: the line is posted to the conversation the role runs, which the delivery is bound to; the role accepts it.
	_, err = n.zm(t, file, "task", "new", "dev", "Review the change")
	require.NoError(t, err)
	require.Len(t, n.posts, 1)

	first := delivery()
	assert.Equal(t, 1, first.Gen)
	assert.Equal(t, "sid-1", first.Session, "submitted to the conversation it was posted to")

	asSession(t, "dev", "sid-1")

	out, err := n.zm(t, file, "task", "accept", "1", "--handoff", first.ID)
	require.NoError(t, err)
	assert.Equal(t, "1 accepted\n", out)

	accepted := delivery()
	require.False(t, accepted.AcceptedAt.IsZero())

	// The conversation changes under the running role.
	n.sid = "sid-2"
	asSession(t, "dev", "sid-2")

	_, err = n.zm(t, file, "task", "checkpoint", "1", "--file", body)
	require.NoError(t, err, "the live conversation of the task's role holds the task")

	moved := delivery()
	want := accepted
	want.Session = "sid-2"
	assert.Equal(t, want, moved, "the hand-out, its number and its acceptance are as they were: only its conversation moved")

	tk, err := taskStore(c).Get("T-1")
	require.NoError(t, err)
	require.NotNil(t, tk.Checkpoint, "its checkpoint is the task's (the restart note the role was asked for)")
	assert.Equal(t, 1, tk.Checkpoint.Gen)
	assert.Empty(t, tk.Late)

	evs, err := eventLog(c).Read(events.Filter{Task: "T-1"})
	require.NoError(t, err)

	rebound, late := 0, 0

	for _, e := range evs {
		switch e.Event {
		case "rebound":
			rebound++
		case "late-report":
			late++
		}
	}

	assert.Equal(t, 1, rebound)
	assert.Zero(t, late)
	assert.Empty(t, stateOf(t, file).Signals, "nobody is told")

	// The conversation it left is not the live one: its report is late, kept and told.
	asSession(t, "dev", "sid-1")

	_, err = n.zm(t, file, "task", "result", "1", "--file", body)
	require.Error(t, err)
	assert.Equal(t, exitLate, exitCodeOf(t, err))
	assert.Contains(t, err.Error(), "this is not the session dev runs now")
	assert.Contains(t, err.Error(), "stop reporting on T-1 from this session")
	assert.Equal(t, moved, delivery(), "a late report moves nothing back")

	tk, err = taskStore(c).Get("T-1")
	require.NoError(t, err)
	assert.Nil(t, tk.Result)
	require.Len(t, tk.Late, 1)

	// The live conversation closes it, under the hand-out it holds.
	asSession(t, "dev", "sid-2")

	_, err = n.zm(t, file, "task", "done", "1")
	require.NoError(t, err)

	tk, err = taskStore(c).Get("T-1")
	require.NoError(t, err)
	require.NotNil(t, tk.Closed)
	assert.Equal(t, 1, tk.Closed.Gen)
	assert.False(t, tk.Closed.Unbound)
}
