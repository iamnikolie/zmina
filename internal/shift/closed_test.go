package shift

import (
	"strings"
	"testing"
	"time"

	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func closedTask(f fix, id, by, note string) {
	tasks := f.s.Tasks.(*fakeTasks)
	tasks.list = append(tasks.list, task.Task{ID: id, Role: "designer", State: task.Done, Created: f.now.Add(-time.Hour),
		Closed: &task.Closure{By: by, At: *f.now, State: task.Done, Note: note}})
}

// A task a role closed signals the dispatcher with its note, once, though the dispatcher is down; a closure the
// state lost is noted again on the next beat.
func TestClosedTaskSignalsTheDispatcher(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	closedTask(f, "T-1", "designer", "review completed")

	f.s.NotifyClosed()
	require.Equal(t, []string{"designer: T-1 done: review completed (zmina task show T-1)"}, f.s.State.Signals)

	f.s.NotifyClosed()
	f.s.Tick()
	assert.Len(t, f.s.State.Signals, 1, "once")

	f.s.State = NewState()
	f.s.Tick()
	require.Len(t, f.s.State.Signals, 1, "reconciled from the ledger")
	assert.Contains(t, f.s.State.Signals[0], "review completed")
}

// Closed by the owner's hand or by the dispatcher, or by a zmina that kept no closure: no signal.
func TestClosedTaskByHandOrDispatcherIsQuiet(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	closedTask(f, "T-1", "hand", "done by hand")
	closedTask(f, "T-2", "dispatcher", "done by the dispatcher")
	tasks := f.s.Tasks.(*fakeTasks)
	tasks.list = append(tasks.list, task.Task{ID: "T-3", Role: "designer", State: task.Done})

	f.s.NotifyClosed()
	assert.Empty(t, f.s.State.Signals)
}

// A result of the same assignment tells the dispatcher already: closing after it adds no second signal. One from
// an earlier assignment does not count.
func TestClosedTaskWithAResultIsNotToldTwice(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	closedTask(f, "T-1", "designer", "see the result")
	tasks := f.s.Tasks.(*fakeTasks)
	tasks.list[0].Result = &record.Ref{Revision: "r1", At: f.now.Add(-time.Minute)}

	closedTask(f, "T-2", "designer", "reworked, no new result")
	tasks.list[1].Result = &record.Ref{Revision: "r0", At: f.now.Add(-2 * time.Hour)}
	tasks.list[1].Delivery = &task.Delivery{AcceptedAt: f.now.Add(-30 * time.Minute)}

	f.s.NotifyClosed()
	require.Len(t, f.s.State.Signals, 1)
	assert.Contains(t, f.s.State.Signals[0], "T-2 done: reworked")
}

// With the dispatcher up, the closure goes to it in the next post.
func TestClosedTaskReachesALiveDispatcher(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	f.s.Tasks = &fakeTasks{}
	closedTask(f, "T-1", "designer", "review completed")

	f.s.NotifyClosed()
	text := f.s.deliver(*f.now)
	require.NotEmpty(t, text, "posts=%v", p.raw)
	assert.True(t, strings.Contains(text, "T-1 done: review completed"), text)
}
