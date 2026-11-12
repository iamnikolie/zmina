package ledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/donecheck"
	"github.com/iamnikolie/zmina/internal/task"
)

var handedAt = time.Date(2026, 1, 2, 14, 2, 0, 0, time.UTC)

// handedTo is a task handed to dev as hand-out gen, to the session sess (empty: not submitted yet).
func handedTo(gen int, sess string) task.Task {
	return task.Task{ID: "T-1", Role: "dev", State: task.Open,
		Delivery: &task.Delivery{ID: "pkt-a", Role: "dev", Gen: gen, Session: sess, QueuedAt: handedAt}}
}

func standing(t *testing.T, tk task.Task, c Caller) Standing {
	t.Helper()

	l, _ := fixture(t, "")
	l.Dispatcher = "boss"

	return l.Current(tk, c)
}

func TestCurrentHoldsASessionToTheHandOutItWasMadeFor(t *testing.T) {
	live := func(string) string { return "sess-live" }
	other := func(tk task.Task) task.Task {
		tk.Role, tk.Delivery.Role = "dev#2", "dev#2"

		return tk
	}

	cases := map[string]struct {
		tk                         task.Task
		c                          Caller
		late, unbound, rebind, oth bool
		why, next                  string
	}{
		"the session it was submitted to": {tk: handedTo(3, "sess-b"), c: Caller{By: "dev", Session: "sess-b"}},
		"a session that held an earlier hand-out": {tk: handedTo(3, "sess-b"), c: Caller{By: "dev", Session: "sess-a"},
			late: true, why: "handed out again to another session", next: "zmina task handoff T-1 --queue"},
		"not yet submitted: the live session": {tk: handedTo(3, ""), c: Caller{By: "dev", Session: "sess-live", Live: live}},
		"not yet submitted: not the live session": {tk: handedTo(3, ""), c: Caller{By: "dev", Session: "sess-a", Live: live},
			late: true, why: "not the session dev runs now", next: "stop reporting on T-1 from this session"},
		"not yet submitted, no live session known: no proof": {tk: handedTo(3, ""), c: Caller{By: "dev", Session: "sess-a"},
			unbound: true},
		"the conversation changed under the running role: the live one holds it": {tk: handedTo(3, "sess-b"),
			c: Caller{By: "dev", Session: "sess-live", Live: live}, rebind: true},
		"another session of the role that is not the live one": {tk: handedTo(3, "sess-b"),
			c: Caller{By: "dev", Session: "sess-a", Live: live}, late: true, why: "not the session dev runs now"},
		"the live session is not known: the delivery's decides": {tk: handedTo(3, "sess-b"),
			c: Caller{By: "dev", Session: "sess-live", Live: func(string) string { return "" }}, late: true,
			why: "handed out again to another session"},
		"the live session of another role does not rebind": {tk: handedTo(3, "sess-b"),
			c: Caller{By: "review", Session: "sess-live", Live: live, Work: true}, late: true, oth: true, why: "handed to dev, not review"},
		"a role with no session of its own": {tk: handedTo(3, "sess-b"), c: Caller{By: "dev"}, unbound: true},
		"a typed role names the packet":     {tk: handedTo(3, "sess-b"), c: Caller{By: "dev", Handoff: "pkt-a"}},
		"a typed role names another packet": {tk: handedTo(3, "sess-b"), c: Caller{By: "dev", Handoff: "pkt-z"}, late: true,
			why: "packet is not the one", next: "zmina task show T-1"},
		"the session wins over a packet that changed": {tk: handedTo(3, "sess-b"), c: Caller{By: "dev", Session: "sess-b", Handoff: "pkt-z"}},
		"a copy is not its base, reporting": {tk: handedTo(3, "sess-b"), c: Caller{By: "dev#2", Session: "sess-b", Work: true},
			late: true, oth: true, why: "handed to dev, not dev#2", next: "zmina task set T-1 --role dev#2"},
		"a copy editing a field of its base's task": {tk: handedTo(3, "sess-b"), c: Caller{By: "dev#2", Session: "sess-b"}, oth: true},
		"the base is not its copy": {tk: other(handedTo(3, "sess-b")), c: Caller{By: "dev", Session: "sess-b", Work: true},
			late: true, oth: true, why: "handed to dev#2, not dev"},
		"another role, reporting": {tk: handedTo(3, "sess-b"), c: Caller{By: "review", Session: "sess-r", Work: true}, late: true,
			oth: true, why: "not review"},
		"another role, a note on it":                {tk: handedTo(3, "sess-b"), c: Caller{By: "review", Session: "sess-r"}, oth: true},
		"the dispatcher is never late":              {tk: handedTo(3, "sess-b"), c: Caller{By: "boss", Session: "sess-x", Work: true}},
		"the owner's hand is never late":            {tk: handedTo(3, "sess-b"), c: Caller{By: Hand, Session: "sess-x"}},
		"the owner's hand naming a packet":          {tk: handedTo(3, "sess-b"), c: Caller{By: Hand, Handoff: "pkt-z"}, late: true, why: "packet"},
		"a file from before hand-outs were counted": {tk: handedTo(0, "sess-b"), c: Caller{By: "review", Session: "sess-x", Work: true}},
		"a task never handed out":                   {tk: task.Task{ID: "T-1", Role: "dev", State: task.Open}, c: Caller{By: "review", Session: "sess-x", Work: true}},
		"handed to another role than the task names": {tk: func() task.Task {
			x := handedTo(3, "sess-b")
			x.Delivery.Role = "review"

			return x
		}(), c: Caller{By: "dev", Session: "sess-a"}},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := standing(t, c.tk, c.c)
			assert.Equal(t, c.late, got.Late, "%+v", got)
			assert.Equal(t, c.unbound, got.Unbound, "%+v", got)
			assert.Equal(t, c.rebind, got.Rebind, "%+v", got)
			assert.Equal(t, c.oth, got.Other, "%+v", got)

			if c.late {
				assert.Contains(t, got.Why, c.why)
				assert.Contains(t, got.Next, c.next)
				assert.Equal(t, c.tk.Delivery.Gen, got.Gen)
				assert.Equal(t, handedAt, got.Since)
			} else {
				assert.Empty(t, got.Why)
				assert.Empty(t, got.Next)
			}
		})
	}
}

// The live session is asked only when the delivery cannot say, or the caller is not the one it names.
func TestCurrentAsksForTheLiveSessionOnlyWhenItHasTo(t *testing.T) {
	asked := 0
	live := func(string) string { asked++; return "sess-live" }

	standing(t, handedTo(3, "sess-b"), Caller{By: "dev", Session: "sess-b", Live: live})
	standing(t, handedTo(3, "sess-b"), Caller{By: "dev", Live: live})
	standing(t, handedTo(3, "sess-b"), Caller{By: Hand, Session: "sess-x", Live: live})
	assert.Zero(t, asked)

	standing(t, handedTo(3, "sess-b"), Caller{By: "dev", Session: "sess-a", Live: live})
	assert.Equal(t, 1, asked, "once, whichever way it is used")
}

func TestLateErrorSaysWhatWasHandedOutAgain(t *testing.T) {
	err := standing(t, handedTo(3, "sess-b"), Caller{By: "dev", Session: "sess-a"}).Err("T-1")

	assert.ErrorIs(t, err, ErrLate)
	assert.Contains(t, err.Error(), "T-1: late report: it was handed out again to another session (hand-out 3, to dev, made ")
}

// A set or a close from a session that does not hold the task is refused whole: the task is as it was.
func TestSetFromALateSessionRefusesAndChangesNothing(t *testing.T) {
	l, _ := fixture(t, "")
	l.Dispatcher = "boss"

	tk, err := l.New(NewTask{Role: "dev", Asked: "work", By: "boss"})
	require.NoError(t, err)

	tk.Delivery = &task.Delivery{ID: "pkt-a", Role: "dev", Gen: 2, Session: "sess-b", QueuedAt: handedAt}
	require.NoError(t, l.Store.Put(tk))

	late := &Caller{By: "dev", Session: "sess-a", Work: true}

	for _, c := range []Change{{State: task.Done, By: "dev", Caller: late}, {State: task.Dropped, By: "dev", Caller: late},
		{Note: "mine", By: "dev", Caller: late}, {Role: "review", By: "dev", Caller: late}} {
		_, err = l.Set(tk.ID, c)
		require.ErrorIs(t, err, ErrLate, "%+v", c)
	}

	got, err := l.Store.Get(tk.ID)
	require.NoError(t, err)
	assert.Equal(t, tk, got, "not one field of the file moved")

	// The session that holds it closes it, the closing says under which hand-out; the closing of a role that showed nothing
	// of itself says so.
	done, err := l.Set(tk.ID, Change{State: task.Done, By: "dev", Caller: &Caller{By: "dev", Session: "sess-b"}})
	require.NoError(t, err)
	require.NotNil(t, done.Closed)
	assert.Equal(t, 2, done.Closed.Gen)
	assert.False(t, done.Closed.Unbound)

	_, err = l.Set(tk.ID, Change{State: task.Open, By: "boss"})
	require.NoError(t, err)

	done, err = l.Set(tk.ID, Change{State: task.Done, By: "dev", Caller: &Caller{By: "dev"}})
	require.NoError(t, err)
	assert.Equal(t, 2, done.Closed.Gen)
	assert.True(t, done.Closed.Unbound, "a role with nothing to show passed on its name")

	// Nobody in the change is the ledger's own caller: the hand-out is not asked for.
	_, err = l.Set(tk.ID, Change{State: task.Open})
	require.NoError(t, err)
}

// The check of a task is not run for a session that does not hold it: PlanCheck refuses before a command starts.
func TestPlanCheckRefusesALateSessionBeforeAnythingRuns(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	l, _, tk, _ := checkFixture(t, &donecheck.Spec{Run: "touch " + marker})

	tk.Delivery = &task.Delivery{ID: "pkt-a", Role: "dev", Gen: 4, Session: "sess-b", QueuedAt: handedAt}
	require.NoError(t, l.Store.Put(tk))

	_, err := l.PlanCheck(tk.ID, Change{State: task.Done, By: "dev", Caller: &Caller{By: "dev", Session: "sess-a"}})
	require.ErrorIs(t, err, ErrLate)
	assert.NoFileExists(t, marker)

	plan, err := l.PlanCheck(tk.ID, Change{State: task.Done, By: "dev", Caller: &Caller{By: "dev", Session: "sess-b"}})
	require.NoError(t, err)
	require.NotNil(t, plan)

	// The hand-out moved while the check ran: the close under the lock refuses the session that ran it.
	checked, err := l.RunCheck(t.Context(), plan)
	require.NoError(t, err)
	assert.FileExists(t, marker)
	require.NoError(t, os.Remove(marker))

	moved := plan.Task
	moved.Delivery = &task.Delivery{ID: "pkt-a", Role: "dev", Gen: 5, Session: "sess-c", QueuedAt: handedAt}
	require.NoError(t, l.Store.Put(moved))

	_, err = l.Set(tk.ID, Change{State: task.Done, By: "dev", Checked: checked, Caller: &Caller{By: "dev", Session: "sess-b"}})
	require.ErrorIs(t, err, ErrLate)

	got, err := l.Store.Get(tk.ID)
	require.NoError(t, err)
	assert.Equal(t, task.Open, got.State)
	assert.Nil(t, got.Closed)
}

// A check that ran for one hand-out does not close the next: it is no longer the answer for the session that holds the task,
// whoever closes it, the dispatcher too.
func TestCheckThatRanInAnEarlierHandOutClosesNothing(t *testing.T) {
	l, _, tk, _ := checkFixture(t, &donecheck.Spec{Run: "true"})

	tk.Delivery = &task.Delivery{ID: "pkt-a", Role: "dev", Gen: 3, Session: "sess-b", QueuedAt: handedAt}
	require.NoError(t, l.Store.Put(tk))

	plan, err := l.PlanCheck(tk.ID, Change{State: task.Done, By: "boss"})
	require.NoError(t, err)

	checked, err := l.RunCheck(t.Context(), plan)
	require.NoError(t, err)
	assert.Equal(t, 3, checked.Gen)

	moved := plan.Task
	moved.Delivery = &task.Delivery{ID: "pkt-a", Role: "dev", Gen: 4, Session: "sess-c", QueuedAt: handedAt}
	require.NoError(t, l.Store.Put(moved))

	_, err = l.Set(tk.ID, Change{State: task.Done, By: "boss", Checked: checked})
	require.ErrorIs(t, err, ErrCheckMoved)
	assert.Contains(t, err.Error(), "handed out again (hand-out 4, it ran in 3)")

	// Planned again in the hand-out it is in, it closes.
	plan, err = l.PlanCheck(tk.ID, Change{State: task.Done, By: "boss"})
	require.NoError(t, err)

	checked, err = l.RunCheck(t.Context(), plan)
	require.NoError(t, err)

	done, err := l.Set(tk.ID, Change{State: task.Done, By: "boss", Checked: checked})
	require.NoError(t, err)
	assert.Equal(t, 4, done.Closed.Gen)
}

// Another role than the task's is refused only for what ends or reports the work: its plain edits of a task are as they were.
func TestAnotherRoleMayEditATaskButNotCloseIt(t *testing.T) {
	l, _ := fixture(t, "")
	l.Dispatcher = "boss"

	tk, err := l.New(NewTask{Role: "dev", Asked: "work", By: "boss"})
	require.NoError(t, err)

	tk.Delivery = &task.Delivery{ID: "pkt-a", Role: "dev", Gen: 2, Session: "sess-b", QueuedAt: handedAt}
	require.NoError(t, l.Store.Put(tk))

	edit := &Caller{By: "review", Session: "sess-r"}

	got, err := l.Set(tk.ID, Change{Note: "from the reviewer", By: "review", Caller: edit})
	require.NoError(t, err)
	assert.Equal(t, "from the reviewer", got.Note)

	_, err = l.Set(tk.ID, Change{State: task.Done, By: "review", Caller: &Caller{By: "review", Session: "sess-r", Work: true}})
	require.ErrorIs(t, err, ErrLate)

	got, err = l.Store.Get(tk.ID)
	require.NoError(t, err)
	assert.Equal(t, task.Open, got.State)
}
