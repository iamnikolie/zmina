package shift

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/task"
)

// reviewCrewYAML is crewYAML with auto_review on and a review rule: designer reviews.
const reviewCrewYAML = crewYAML + "auto_review: on\nroutes:\n  - review: {first: designer}\n  - pool: {seats: [designer]}\n"

// A quiet note goes as an FYI line with no ack id, waits for a line to go with for the whole quiet ride, not the 90 seconds a
// result's line waits, and is told once.
func TestNoteQuietRidesForTheQuietWindow(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	queued, err := f.s.NoteQuiet("reviewer", "T-1 accepted in review T-2 (round 1): fine")
	require.NoError(t, err)
	require.True(t, queued)
	assert.Empty(t, f.s.State.Acks, "an info line has no ack id")

	*f.now = f.now.Add(infoRide)
	assert.Empty(t, f.s.deliver(*f.now), "past the ride of a result's line, it still waits")
	assert.True(t, f.s.ridingOnly(*f.now), "and holds nothing up")

	*f.now = f.now.Add(infoRideQuiet - infoRide - time.Second)
	assert.Empty(t, f.s.deliver(*f.now))

	*f.now = f.now.Add(time.Second)
	line := f.s.deliver(*f.now)
	assert.True(t, strings.HasPrefix(line, "[zmina] FYI reviewer: T-1 accepted in review T-2 (round 1): fine"), line)

	p.showAll()
	assert.Empty(t, f.s.deliver(*f.now))
	require.Len(t, closed(f), 1, "closed when the dispatcher's conversation shows it")
	assert.Empty(t, f.s.State.Signals)
	assert.Len(t, p.raw, 1, "posted once")
}

// A line the dispatcher is posted anyway takes the quiet one along at once.
func TestNoteQuietRidesWithAnotherLine(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	_, err := f.s.NoteQuiet("reviewer", "T-1 accepted in review T-2 (round 1): fine")
	require.NoError(t, err)
	assert.Empty(t, f.s.deliver(*f.now))

	_, err = f.s.Note("mrs", "acme/site!12 approved", false)
	require.NoError(t, err)

	line := f.s.deliver(*f.now)
	assert.Contains(t, line, "acme/site!12 approved")
	assert.Contains(t, line, "FYI reviewer: T-1 accepted in review T-2")
}

// With info_lines: false it is a note like any other, with its id, and waits for nothing.
func TestNoteQuietWithoutInfoLinesIsAnActNote(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML+"info_lines: false\n")
	upNative(t, f, "dispatcher")

	_, err := f.s.NoteQuiet("reviewer", "T-1 accepted in review T-2 (round 1): fine")
	require.NoError(t, err)
	assert.Empty(t, f.s.State.Info)

	line := f.s.deliver(*f.now)
	assert.Contains(t, line, "reviewer: T-1 accepted in review T-2")
	assert.NotContains(t, line, "FYI")
	assert.NotEmpty(t, f.s.State.Acks)
}

func TestNoteOnceTellsOncePerKey(t *testing.T) {
	f := fixtureWith(t, crewYAML)

	told, err := f.s.NoteOnce("review/T-2", "reviewer", "T-1 accepted", true)
	require.NoError(t, err)
	assert.True(t, told)

	told, err = f.s.NoteOnce("review/T-2", "reviewer", "T-1 accepted", true)
	require.NoError(t, err)
	assert.False(t, told, "the same key is not told again")
	assert.Len(t, f.s.State.Signals, 1)

	told, err = f.s.NoteOnce("review/T-4", "reviewer", "T-3 accepted", false)
	require.NoError(t, err)
	assert.True(t, told)
	assert.Len(t, f.s.State.Signals, 2)
}

// A review the crew's loop runs, and the task it is of, are not told by the closed task's line: the loop's own lines are.
func TestClosedTaskOfTheReviewLoopIsNotToldByItsLine(t *testing.T) {
	f := fixtureWith(t, reviewCrewYAML)
	closedTask(f, "T-1", "designer", "shipped")
	closedTask(f, "T-2", "designer", "accept: fine")
	closedTask(f, "T-3", "designer", "plain")

	f.tk.list[0].Closed.Review = &task.ReviewDue{Round: 1, Task: "T-2"}
	f.tk.list[1].Review = &task.Review{Of: "T-1", Round: 1, Verdict: task.VerdictAccept}

	f.s.NotifyClosed()
	require.Len(t, f.s.State.Signals, 1, "only the task that has no review")
	assert.Contains(t, f.s.State.Signals[0], "T-3 done: plain")

	f.s.State = NewState()
	f.s.NotifyClosed()
	assert.Len(t, f.s.State.Signals, 1, "and it stays so when the state is lost")
}

// With auto_review on, the result of a worktree task that is still worked on is not told: the close tells of it, and when its
// review is due the loop's lines do. A task with no worktree, and a task closed by hand, are told as always.
func TestResultLineWaitsForTheCloseWhenTheLoopRuns(t *testing.T) {
	f := fixtureWith(t, reviewCrewYAML)
	f.tk.list = append(f.tk.list,
		task.Task{ID: "T-1", Role: "designer", State: task.Open, Worktree: "/w/api-T-1"},
		task.Task{ID: "T-2", Role: "designer", State: task.Open},
	)

	saveResult(t, f, "designer", "T-1")
	saveResult(t, f, "designer", "T-2")

	f.s.NotifyRecords()
	require.Len(t, f.s.State.Signals, 1, "the task with no worktree is told")
	assert.Contains(t, f.s.State.Signals[0], "T-2 result ready")

	// Closed by hand: told as always, now.
	f.tk.list[0].State = task.Done
	f.tk.list[0].Closed = &task.Closure{By: "hand", At: *f.now, State: task.Done}
	f.s.NotifyRecords()
	require.Len(t, f.s.State.Signals, 2)
	assert.Contains(t, f.s.State.Signals[1], "T-1 result ready")
}

func TestResultLineOfAClosedTaskWithItsReviewDueIsNeverTold(t *testing.T) {
	f := fixtureWith(t, reviewCrewYAML)
	f.tk.list = append(f.tk.list,
		task.Task{ID: "T-1", Role: "dev", State: task.Open, Worktree: "/w/api-T-1"},
		task.Task{ID: "T-2", Role: "designer", State: task.Open, Review: &task.Review{Of: "T-1", Round: 1}},
	)

	saveResult(t, f, "designer", "T-1")
	saveResult(t, f, "designer", "T-2")

	f.s.NotifyRecords()
	assert.Empty(t, f.s.State.Signals, "the author's is held and the review's is the loop's")

	f.tk.list[0].State = task.Done
	f.tk.list[0].Closed = &task.Closure{By: "dev", At: *f.now, State: task.Done, Review: &task.ReviewDue{Round: 1}}
	f.s.NotifyRecords()
	assert.Empty(t, f.s.State.Signals, "closed with its review due: the loop tells how it went")

	// A reject reopens it: held again, then told once it closes with no review due.
	f.tk.list[0].State, f.tk.list[0].Closed = task.Open, nil
	f.s.NotifyRecords()
	assert.Empty(t, f.s.State.Signals)

	f.tk.list[0].State = task.Done
	f.tk.list[0].Closed = &task.Closure{By: "dev", At: *f.now, State: task.Done}
	f.s.NotifyRecords()
	assert.Empty(t, f.s.State.Signals, "its result was marked when the review was due: nothing is told of it later")
}

// With the key absent every line is what it was: a worktree task's result is told as it is saved.
func TestResultLineIsUnchangedWithoutTheKey(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.tk.list = append(f.tk.list, task.Task{ID: "T-1", Role: "designer", State: task.Open, Worktree: "/w/api-T-1"})

	saveResult(t, f, "designer", "T-1")
	f.s.NotifyRecords()
	require.Len(t, f.s.State.Signals, 1)
	assert.Contains(t, f.s.State.Signals[0], "T-1 result ready")

	closedTask(f, "T-2", "designer", "shipped")
	f.tk.list[1].Worktree = "/w/api-T-2"
	f.s.NotifyClosed()
	require.Len(t, f.s.State.Signals, 2)
	assert.Contains(t, f.s.State.Signals[1], "T-2 done: shipped")
}

// With the key on but no review rule it does nothing, so no line changes either.
func TestResultLineIsUnchangedWhenTheLoopIsInert(t *testing.T) {
	f := fixtureWith(t, crewYAML+"auto_review: on\n")
	f.tk.list = append(f.tk.list, task.Task{ID: "T-1", Role: "designer", State: task.Open, Worktree: "/w/api-T-1"})

	saveResult(t, f, "designer", "T-1")
	f.s.NotifyRecords()
	assert.Len(t, f.s.State.Signals, 1)
}

// A held result line is held for the close, but not for ever: a worker that saved its result and never closed is heard of once
// the result is half an hour old.
func TestHeldResultLineGoesOnceItIsOldEnough(t *testing.T) {
	f := fixtureWith(t, reviewCrewYAML)
	f.tk.list = append(f.tk.list, task.Task{ID: "T-1", Role: "designer", State: task.Open, Worktree: "/w/api-T-1"})

	saveResult(t, f, "designer", "T-1")

	f.s.NotifyRecords()
	assert.Empty(t, f.s.State.Signals, "held for the close")

	*f.now = f.now.Add(resultHoldMax - time.Second)
	f.s.NotifyRecords()
	assert.Empty(t, f.s.State.Signals)

	*f.now = f.now.Add(time.Second)
	f.s.NotifyRecords()
	require.Len(t, f.s.State.Signals, 1, "told as it is without the key")
	assert.Contains(t, f.s.State.Signals[0], "T-1 result ready")

	f.s.NotifyRecords()
	assert.Len(t, f.s.State.Signals, 1, "once")
}

// A task waiting on the owner's answer is never held: its result is told as it is saved.
func TestAWaitingTasksResultIsNeverHeld(t *testing.T) {
	f := fixtureWith(t, reviewCrewYAML)
	f.tk.list = append(f.tk.list, task.Task{ID: "T-1", Role: "designer", State: task.Waiting, Worktree: "/w/api-T-1"})

	saveResult(t, f, "designer", "T-1")
	f.s.NotifyRecords()
	require.Len(t, f.s.State.Signals, 1)
	assert.Contains(t, f.s.State.Signals[0], "T-1 result ready")
}

// With the key turned off while a review is on the ledger, nothing of the loop swallows a line: the review's close is told as any
// close, with its verdict, and the results of both are told.
func TestTurningTheKeyOffSwallowsNothing(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.tk.list = append(f.tk.list,
		task.Task{ID: "T-1", Role: "designer", State: task.Done, Worktree: "/w/api-T-1",
			Closed: &task.Closure{By: "designer", At: *f.now, State: task.Done, Note: "shipped", Review: &task.ReviewDue{Round: 1, Task: "T-2"}}},
		task.Task{ID: "T-2", Role: "harvester", State: task.Done, Review: &task.Review{Of: "T-1", Round: 1, Verdict: task.VerdictReject},
			Closed: &task.Closure{By: "harvester", At: *f.now, State: task.Done, Note: "the token is logged"}},
	)

	saveResult(t, f, "designer", "T-1")
	saveResult(t, f, "harvester", "T-2")

	f.s.NotifyRecords()
	f.s.NotifyClosed()

	var got []string
	for _, sig := range f.s.State.Signals {
		got = append(got, SignalBody(sig))
	}

	assert.Contains(t, strings.Join(got, "\n"), "T-1 result ready")
	assert.Contains(t, strings.Join(got, "\n"), "T-2 result ready")
	assert.Contains(t, got, "harvester: T-2 done (reject): the token is logged (zmina task show T-2)")
	assert.Contains(t, got, "designer: T-1 done: shipped (zmina task show T-1)")
}

// With the routes' review rule taken away the loop is inert, and so are its suppressions.
func TestTheLoopsSuppressionsEndWithTheReviewRule(t *testing.T) {
	f := fixtureWith(t, crewYAML+"auto_review: on\n")
	closedTask(f, "T-1", "designer", "shipped")
	f.tk.list[0].Closed.Review = &task.ReviewDue{Round: 1}

	f.s.NotifyClosed()
	require.Len(t, f.s.State.Signals, 1)
	assert.Contains(t, f.s.State.Signals[0], "T-1 done: shipped")
}

// A packet tells a worker to close its task when the crew has it reviewed on close, since the dispatcher hears of the result then;
// with the key off, or for a task with no worktree or a review, the packet is what it always was.
func TestPacketSaysToCloseWhenTheCrewReviewsOnClose(t *testing.T) {
	off := fixtureWith(t, crewYAML)
	on := fixtureWith(t, reviewCrewYAML)

	work := task.Task{ID: "T-1", Role: "designer", State: task.Open, Worktree: "/w/api-T-1"}

	was, err := off.s.TaskPacket("designer", work)
	require.NoError(t, err)

	r, _ := off.s.Role("designer")
	want := "Read repository instructions. Run zmina --crew " + shellWord(off.s.Crew.Path) + " task show T-1 and confirm this task is still assigned to designer; use the latest note before working.\n" +
		off.s.Crew.HandoffFor(r).Request("checkpoint or result") +
		"When done, save it with zmina --crew " + shellWord(off.s.Crew.Path) + " task result T-1 --file <json>: that tells the dispatcher, " +
		"with the gist and the command that shows it. Send the dispatcher no message of your own about the result: it would hear it twice.\n"
	assert.Equal(t, want, was.Instructions, "with the key off the packet is what it always was")

	got, err := on.s.TaskPacket("designer", work)
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(got.Instructions, "Then close the task: zmina --crew "+shellWord(on.s.Crew.Path)+
		" task done T-1. This crew has a finished task reviewed when it is closed, and the dispatcher hears of the result then.\n"), got.Instructions)

	plain, err := on.s.TaskPacket("designer", task.Task{ID: "T-2", Role: "designer", State: task.Open})
	require.NoError(t, err)
	assert.NotContains(t, plain.Instructions, "Then close the task", "no worktree, no review")

	review, err := on.s.TaskPacket("designer", task.Task{ID: "T-3", Role: "designer", State: task.Open, Worktree: "/w/x",
		Review: &task.Review{Of: "T-1", Round: 1}})
	require.NoError(t, err)
	assert.NotContains(t, review.Instructions, "Then close the task")

	inert := fixtureWith(t, crewYAML+"auto_review: on\n")
	same, err := inert.s.TaskPacket("designer", work)
	require.NoError(t, err)
	assert.NotContains(t, same.Instructions, "Then close the task", "no review rule, no review")
}
