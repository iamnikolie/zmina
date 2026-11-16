package shift

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/move"
	"github.com/iamnikolie/zmina/internal/task"
)

const moveYAML = `namespace: /ns
claude: claude
home: alpha
nodes:
  alpha: {}
  beta: {}
roles:
  dispatcher:
    boot: read d and boot
  dev:
    keep: always
  ops: {}
`

var (
	moveSumA = strings.Repeat("a", 64)
	moveSumB = strings.Repeat("b", 64)
)

// openMove writes the journal of an open move of dev to beta.
func openMove(t *testing.T, f fix, phase string) move.Journal {
	t.Helper()

	j := move.Journal{ID: "mv-0123456789ab", Role: "dev", From: "alpha", To: "beta", Phase: phase, Started: f.now.UTC(), Base: moveSumA, After: moveSumB,
		Tasks: []move.Task{{ID: "T-1", Item: "L-1"}}}
	require.NoError(t, move.Save(f.s.Crew.Layout(), j))

	return j
}

// While the journal of a move is open the role is not started, by any road: the line names the move and the commands that end it.
func TestOpenMoveRefusesEveryStartOfTheRole(t *testing.T) {
	f, _ := placedFixture(t, moveYAML, "alpha")
	openMove(t, f, move.PhaseAsked)

	for name, start := range map[string]func() error{
		"up":           func() error { return f.s.Up("dev") },
		"upWith":       func() error { return f.s.UpWith("dev", UpOptions{Fresh: true}) },
		"upFor":        func() error { return f.s.upFor("dev", UpOptions{}) },
		"an auto wake": func() error { return f.s.UpWith("dev", UpOptions{Auto: true}) },
		"up with its tasks": func() error {
			return f.s.UpHanding("dev", UpOptions{})
		},
	} {
		err := start()
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "dev: its move to beta is open (mv-0123456789ab, asked)", name)
		assert.Contains(t, err.Error(), "zmina move dev beta --resume mv-0123456789ab", name)
		assert.Contains(t, err.Error(), "--abort mv-0123456789ab", name)
	}

	assert.Empty(t, f.tm.starts)

	// A seat that is no mover's is started as always.
	require.NoError(t, f.s.Up("ops"))

	// The ticker's reason and the commands' reason say the same, first.
	assert.Contains(t, f.s.StartBlock("dev", *f.now), "its move to beta is open")
	assert.Empty(t, f.s.StartBlock("ops", *f.now))

	// StartOnTask is a start of its own: refused too.
	started, why := f.s.StartOnTask(task.Task{ID: "T-1", Role: "dev", State: task.Open})
	assert.False(t, started)
	assert.Contains(t, why, "its move to beta is open")

	// A journal that does not read keeps the role refused as well.
	require.NoError(t, os.WriteFile(f.s.Crew.Layout().Move("dev"), []byte("{"), 0o600))
	err := f.s.Up("dev")
	require.ErrorContains(t, err, "does not read")
	assert.Equal(t, "unreadable", f.s.MoveOpen("dev"))
}

// The ticker, which brings a keep: always role up, leaves one whose move is open down; and leaves one that is up as it finds it, only
// following whether it is up.
func TestTickLeavesARoleWhoseMoveIsOpen(t *testing.T) {
	f, _ := placedFixture(t, moveYAML, "alpha")
	openMove(t, f, move.PhaseAsked)

	for range 3 {
		*f.now = f.now.Add(2 * time.Minute)
		f.s.Tick()
	}

	assert.NotContains(t, f.tm.starts, "dev", "keep: always, and still not started")
	assert.Contains(t, f.tm.starts, "dispatcher", "the rest of the crew runs as before")
}

func TestTickDoesNotPutDownOrRestartARoleThatIsMoving(t *testing.T) {
	f, _ := placedFixture(t, strings.Replace(moveYAML, "  dev:\n    keep: always\n", "  dev:\n    release_idle_minutes: \"30\"\n", 1), "alpha")
	require.NoError(t, f.s.Up("dev"))
	f.status("dev", "idle", f.now.Add(-5*time.Hour))
	f.tk.list = []task.Task{openTask("T-1", "dev")}
	openMove(t, f, move.PhaseAsked)
	require.NotZero(t, f.s.Crew.Roles["dev"].ReleaseAfter(), "dev would be released after 30 minutes idle")

	// Idle for hours with release configured: nothing is released, nothing restarted, nothing put down.
	for range 3 {
		*f.now = f.now.Add(10 * time.Minute)
		f.s.Tick()
	}

	assert.Contains(t, f.tm.up, "dev", "still up")
	assert.True(t, f.s.State.Role("dev").Up, "and its state follows the probe")
	assert.True(t, f.s.State.Role("dev").Releasing.IsZero(), "the release is not begun")
	assert.Equal(t, task.Open, f.tk.list[0].State, "nothing was parked by the beat")
	assert.Zero(t, eventsOf(f, "releasing"))
}

func TestMoveQueueRefusesLinesUnlessForcedAndTellsWhichWereDropped(t *testing.T) {
	f, _ := placedFixture(t, moveYAML, "alpha")

	dropped, err := f.s.MoveQueue("dev", false)
	require.NoError(t, err)
	assert.Empty(t, dropped, "nothing queued, nothing to say")

	f.s.enqueue("dev", "check the pricing page", Facts{From: "zmina"})
	f.s.enqueue("dev", "and the footer", Facts{From: "zmina"})

	_, err = f.s.MoveQueue("dev", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dev has 2 queued lines that a move does not carry")
	assert.Contains(t, err.Error(), "--force drops them and tells the dispatcher which")
	assert.Len(t, f.s.State.Role("dev").Inbox, 2, "a refusal drops nothing")

	dropped, err = f.s.MoveQueue("dev", true)
	require.NoError(t, err)
	assert.Len(t, dropped, 2)
	assert.Empty(t, f.s.State.Role("dev").Inbox)

	f.s.MoveDropped(move.Journal{ID: "mv-0123456789ab", Role: "dev", To: "beta", Dropped: dropped})
	assert.Equal(t, 1, eventsOf(f, "move-dropped"))
	require.Len(t, f.s.State.Signals, 1, "the dispatcher is told once")
	assert.Contains(t, f.s.State.Signals[0], "dev is moving to beta, which does not carry its queue: dropped 2")
	assert.Contains(t, f.s.State.Signals[0], "check the pricing page")

	f.s.MoveDropped(move.Journal{ID: "mv-0123456789ab", Role: "dev", To: "beta", Dropped: dropped})
	assert.Len(t, f.s.State.Signals, 1, "and not again for the same move")
}

// A role that is up and holds a task is asked, with the release's own checkpoint request; a role that is down, or holds nothing,
// is not.
func TestMoveAskQueuesTheReleaseRequestForARoleThatIsUp(t *testing.T) {
	f, _ := placedFixture(t, moveYAML, "alpha")
	f.tk.list = []task.Task{openTask("T-1", "dev")}

	asked, err := f.s.MoveAsk("dev")
	require.NoError(t, err)
	assert.False(t, asked, "down: nothing to ask")

	require.NoError(t, f.s.Up("dev"))

	asked, err = f.s.MoveAsk("dev")
	require.NoError(t, err)
	assert.True(t, asked)

	inbox := f.s.State.Role("dev").Inbox
	require.Len(t, inbox, 1)
	assert.Contains(t, inbox[0], "[zmina release] Read checkpoint request")
	assert.Equal(t, 1, eventsOf(f, "move-asked"))

	f.tk.list = nil

	f.s.State.Role("dev").Inbox = nil
	asked, err = f.s.MoveAsk("dev")
	require.NoError(t, err)
	assert.False(t, asked, "no task held")
}

// Within the grace a role that is up parks its own tasks; past it, or when it is down, zmina parks what is left as a release does,
// and the item names the checkpoint state.
func TestMoveParkWaitsForTheRoleThenParksWhatIsLeft(t *testing.T) {
	f, _ := placedFixture(t, moveYAML, "alpha")
	f.tk.list = []task.Task{openTask("T-1", "dev")}
	require.NoError(t, f.s.Up("dev"))

	asked := *f.now

	waiting, _, err := f.s.MovePark("dev", asked, MoveGrace)
	require.NoError(t, err)
	assert.Contains(t, waiting, "dev has not parked T-1 yet")
	assert.Equal(t, task.Open, f.tk.list[0].State)

	*f.now = f.now.Add(MoveGrace + time.Second)

	waiting, parked, err := f.s.MovePark("dev", asked, MoveGrace)
	require.NoError(t, err)
	assert.Empty(t, waiting)
	require.Len(t, parked, 1)
	assert.Equal(t, "T-1", parked[0].ID)
	assert.Equal(t, task.Parked, f.tk.list[0].State)
	require.Len(t, f.lt.list, 1)
	assert.True(t, f.lt.list[0].Incomplete, "no checkpoint was written: the handoff is marked incomplete")
	assert.Equal(t, "T-1", f.lt.list[0].From)

	// Down: parked at once, with no grace to wait for.
	g, _ := placedFixture(t, moveYAML, "alpha")
	g.tk.list = []task.Task{openTask("T-1", "dev")}

	waiting, _, err = g.s.MovePark("dev", *g.now, MoveGrace)
	require.NoError(t, err)
	assert.Empty(t, waiting)
	assert.Equal(t, task.Parked, g.tk.list[0].State)
}

func TestMoveBeginDownHoldsTheRoleDownEvenWhenItIsAlreadyGone(t *testing.T) {
	f, _ := placedFixture(t, moveYAML, "alpha")

	wait, err := f.s.MoveBeginDown("dev", false)
	require.NoError(t, err)
	assert.False(t, wait)
	assert.True(t, f.s.State.Role("dev").Held, "no tick or start brings it back")

	require.NoError(t, f.s.Up("ops"))

	wait, err = f.s.MoveBeginDown("ops", false)
	require.NoError(t, err)
	assert.True(t, wait, "up: /exit was typed, and the caller waits for it outside the lock")
	assert.True(t, f.s.State.Role("ops").Held)
}

// An abort gives the parked tasks back on their own ids, closes the items as done with a note, and frees the role.
func TestMoveUnparkReopensTheTasksAndClosesTheItems(t *testing.T) {
	f, _ := placedFixture(t, moveYAML, "alpha")
	f.tk.list = []task.Task{openTask("T-1", "dev")}

	_, _, err := f.s.MovePark("dev", *f.now, MoveGrace)
	require.NoError(t, err)
	require.Equal(t, task.Parked, f.tk.list[0].State)

	f.s.State.Role("dev").Held = true

	j := openMove(t, f, move.PhaseReleased)

	back, err := f.s.MoveUnpark(j)
	require.NoError(t, err)
	require.Len(t, back, 1)

	assert.Equal(t, task.Open, f.tk.list[0].State)
	assert.Empty(t, f.tk.list[0].Later)
	assert.Equal(t, later.Done, f.lt.list[0].State)
	assert.Contains(t, f.lt.list[0].Note, "move mv-0123456789ab aborted: back on T-1")
	assert.False(t, f.s.State.Role("dev").Held)

	// Again: nothing is parked, so nothing is done twice.
	back, err = f.s.MoveUnpark(j)
	require.NoError(t, err)
	assert.Empty(t, back)
}

// A commit closes the item as taken by the task it became there; the task on this node stays parked; the dispatcher is told once.
func TestMoveCommitClosesTheItemAsTakenByTheTaskThere(t *testing.T) {
	f, _ := placedFixture(t, moveYAML, "alpha")
	f.tk.list = []task.Task{openTask("T-1", "dev")}

	_, _, err := f.s.MovePark("dev", *f.now, MoveGrace)
	require.NoError(t, err)

	j := openMove(t, f, move.PhaseReady)

	require.NoError(t, f.s.MoveCommit(j, map[string]string{"T-1": "beta/T-9"}))

	assert.Equal(t, later.Taken, f.lt.list[0].State)
	assert.Equal(t, "beta/T-9", f.lt.list[0].Task)
	assert.Equal(t, task.Parked, f.tk.list[0].State, "no new task state: the task stays parked")
	assert.Equal(t, 1, eventsOf(f, "move"))

	// Twice: the item is closed already, the dispatcher is not told again.
	require.NoError(t, f.s.MoveCommit(j, map[string]string{"T-1": "beta/T-9"}))
	assert.Equal(t, "beta/T-9", f.lt.list[0].Task)
}
