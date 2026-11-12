package shift

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
)

// acceptedByOld gives T-1 a delivery the old conversation (sid-designer) accepted two hours ago, as a role that has been working
// on the task has, and returns it.
func acceptedByOld(t *testing.T, f fix) task.Delivery {
	t.Helper()

	tk, err := f.tk.Get("T-1")
	require.NoError(t, err)

	at := f.now.Add(-2 * time.Hour).UTC()
	d := task.Delivery{ID: "old-packet", Role: "designer", Runtime: "claude", Home: "/ns", Session: "sid-designer",
		Line: "[zmina T-1] Read handoff packet \"/old/packet.json\"", TaskUpdated: tk.Updated, QueuedAt: at, SentAt: at,
		AcceptedAt: at.Add(time.Minute)}
	tk.Delivery = &d
	require.NoError(t, f.tk.Put(tk))

	return d
}

// handOutLines are the lines of a rotation's hand-out: its restart line and the packets, wherever they are in the queue.
func handOutLines(lines []string) []string {
	return slices.DeleteFunc(slices.Clone(lines), func(l string) bool {
		return !strings.HasPrefix(l, restartLineHead) && !strings.HasPrefix(l, "[zmina T-1] Read handoff packet ")
	})
}

// cardsDropped counts the cards of the role's hand-out lines that were closed as dropped.
func cardsDropped(f fix, role string) int {
	n := 0

	for _, c := range f.s.cardsOf(role).cards {
		if c.Closed != nil && c.Closed.How == ClosedDropped &&
			(strings.HasPrefix(c.Head, restartLineHead) || strings.HasPrefix(c.Head, "[zmina T-1] Read handoff packet ")) {
			n++
		}
	}

	return n
}

// dropBefore removes what the hand-out found from a role's record, as a record of an older zmina has it.
func dropBefore(t *testing.T, f fix, role string) {
	t.Helper()

	path := f.s.Crew.Layout().Rotations(role)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var out []string

	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m))
		delete(m, "before")

		b, err := json.Marshal(m)
		require.NoError(t, err)

		out = append(out, string(b))
	}

	require.NoError(t, os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o600))
}

// M1. An abort leaves the old conversation as restartRollback's way back does: what the fresh session's hand-out left in the
// queue (never shown) is taken out, each card closed as dropped, and the delivery the hand-out renewed is put back as it was, so
// the conversation that had accepted the task is not left unaccepted, and nothing "restarted fresh" is typed into it.
func TestAbortTakesTheHandOutBackAsTheRollbackDoes(t *testing.T) {
	f := settled(t)
	before := acceptedByOld(t, f)

	_, err := f.restartByHand(t, UpOptions{Fresh: true})
	require.NoError(t, err)

	require.NotEqual(t, before.ID, deliveryOf(t, f).ID, "the hand-out renewed it")
	require.Len(t, handOutLines(f.s.State.Role("designer").Inbox), 2)

	bootedFresh(f)
	require.NoError(t, abort(t, f))

	assert.Empty(t, handOutLines(f.s.State.Role("designer").Inbox), "its restart line and packet are out of the queue")
	assert.Equal(t, 2, cardsDropped(f, "designer"), "each card closed as dropped")
	assert.Equal(t, before, *deliveryOf(t, f), "the delivery is as the old conversation had it: accepted")

	// Nothing of it reaches the old conversation, and no alert is told of a task it accepted.
	f.newSession("designer", "sid-designer")

	for range 12 {
		f.s.Tick()
		*f.now = f.now.Add(time.Minute)
		f.rg.byPID[f.tm.up["designer"]] = &registry.Session{Status: "idle", StatusUpdatedAt: f.now.Add(-9 * time.Minute).UnixMilli(),
			SessionID: "sid-designer"}
	}

	assert.Empty(t, handOutLines(f.tm.typed["designer"][1:]), "nothing 'restarted fresh' typed into the old conversation: %v", f.tm.typed["designer"])
	assert.Zero(t, countNotes(f, "designer: T-1 has no acceptance"), "%v", f.s.State.Notes)
	assert.Equal(t, before, *deliveryOf(t, f))
}

// A hand-out already typed to the fresh session is shown there and cannot be taken out; the delivery it bound to that session
// is put back all the same, from what the record kept; a record that kept none (an older one) has it re-bound to the old
// conversation, queued and not sent, not accepted: nothing is claimed, and no alert of the fresh session is left standing.
func TestAbortPutsTheDeliveryBackWhenTheFreshSessionWasTyped(t *testing.T) {
	for _, recorded := range []bool{true, false} {
		f := settled(t)
		before := acceptedByOld(t, f)

		_, err := f.restartByHand(t, UpOptions{Fresh: true})
		require.NoError(t, err)

		bootedFresh(f)
		require.Len(t, f.typedAfter("designer", "sid-new", 2), 2)

		d := deliveryOf(t, f)
		require.Equal(t, "sid-new", d.Session, "typed to the fresh session")
		require.False(t, d.SentAt.IsZero())

		if !recorded {
			dropBefore(t, f, "designer")
		}

		require.NoError(t, abort(t, f), "recorded %v", recorded)

		d = deliveryOf(t, f)

		if recorded {
			assert.Equal(t, before, *d)

			continue
		}

		assert.Equal(t, "sid-designer", d.Session, "re-bound to the conversation the role is on")
		assert.True(t, d.SentAt.IsZero())
		assert.True(t, d.AcceptedAt.IsZero(), "nothing is claimed of the old conversation")

		tk, err := f.tk.Get("T-1")
		require.NoError(t, err)
		assert.False(t, f.s.acceptanceDue(tk), "a delivery that was not sent is not due")
	}
}

// M2. The restart line is never queued beside another one of an earlier hand-out that still waits: the dispatcher's crash loop
// does not add a line a round, and zmina up, down, up leaves one.
func TestRestartLineIsNeverQueuedBesideAnEarlierOne(t *testing.T) {
	t.Run("a crash loop of the dispatcher", func(t *testing.T) {
		f := restarting(t, "", 100_000, 10*time.Minute)
		f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: "dispatcher", State: task.Open, Title: "route",
			Updated: f.now.Add(-time.Hour).UTC()})
		f.status("dispatcher", "idle", f.now.Add(-10*time.Minute))
		f.s.Tick()
		require.Equal(t, "sid-dispatcher", f.s.State.Role("dispatcher").LastSession)

		var lengths []int

		for range 6 {
			f.s.State.Role("dispatcher").Started = f.now.Add(-time.Minute)
			delete(f.tm.up, "dispatcher")
			f.s.Tick() // it ended

			*f.now = f.now.Add(reopenPause)
			f.s.Tick() // opened again, handed its task again

			inbox := f.s.State.Role("dispatcher").Inbox
			n := len(slices.DeleteFunc(slices.Clone(inbox), func(l string) bool { return !strings.HasPrefix(l, restartLineHead) }))
			assert.Equal(t, 1, n, "one restart line, whatever the round: %v", inbox)

			lengths = append(lengths, len(inbox))
		}

		assert.Equal(t, lengths[0], lengths[len(lengths)-1], "the queue does not grow with the rounds: %v", lengths)
	})

	t.Run("up, down, up", func(t *testing.T) {
		f := settled(t)
		require.NoError(t, f.s.Down("designer", DownOptions{}))
		require.NoError(t, f.s.UpHanding("designer", UpOptions{Fresh: true}))
		require.NoError(t, f.s.Down("designer", DownOptions{}))
		require.NoError(t, f.s.UpHanding("designer", UpOptions{Fresh: true}))

		inbox := f.s.State.Role("designer").Inbox
		assert.Len(t, inbox, 2, "one restart line and one packet: %v", inbox)
		assert.Len(t, slices.DeleteFunc(slices.Clone(inbox), func(l string) bool { return !strings.HasPrefix(l, restartLineHead) }), 1)
	})

	t.Run("a different line is replaced by the newest", func(t *testing.T) {
		f := settled(t)
		f.s.State.Role("designer").Inbox = []string{restartLineHead + "an older reason. Your note: T-1 (none saved), in each task's packet that follows.",
			"[dev] waited"}

		_, err := f.restartByHand(t, UpOptions{Fresh: true})
		require.NoError(t, err)

		inbox := f.s.State.Role("designer").Inbox
		require.Len(t, inbox, 3, "%v", inbox)
		assert.True(t, strings.HasPrefix(inbox[0], restartLineHead+whyManual), inbox[0])
		assert.Equal(t, "[dev] waited", inbox[2])
		assert.NotContains(t, strings.Join(inbox, "\n"), "an older reason")
	})

	t.Run("no room for the line", func(t *testing.T) {
		f := settled(t)

		var full []string
		for range inboxMax {
			full = append(full, "[dev] waits")
		}

		f.s.State.Role("designer").Inbox = full

		_, err := f.restartByHand(t, UpOptions{Fresh: true})
		require.NoError(t, err)

		inbox := f.s.State.Role("designer").Inbox
		assert.Len(t, slices.DeleteFunc(slices.Clone(inbox), func(l string) bool { return !strings.HasPrefix(l, restartLineHead) }), 0,
			"no hand-out line when it would not fit")
		assert.True(t, strings.HasPrefix(inbox[0], "[zmina T-1] Read handoff packet "), "the packet is queued by the existing rules: %v", inbox[:2])
		assert.Equal(t, inboxMax+1, len(inbox))
	})
}

// M3. A reopen after a crash is told by the ticker's own notes (ended, started again, dies on boot): its rotation closes lost
// without a note when the session ended again before it confirmed, and a role that holds nothing gets no rotation note at all.
// Run over several rounds of a crash loop: one ended note a round, no rotation note, and a queue that does not grow.
func TestCrashLoopIsToldByTheCrashNotesAndNotByItsRotations(t *testing.T) {
	loop := func(t *testing.T, role string, holds bool) fix {
		t.Helper()

		f := restarting(t, "", 100_000, 10*time.Minute)
		if holds {
			f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: role, State: task.Open, Title: "route", Updated: f.now.Add(-time.Hour).UTC()})
		}

		f.status(role, "idle", f.now.Add(-10*time.Minute))
		f.s.Tick()
		require.Equal(t, "sid-"+role, f.s.State.Role(role).LastSession)

		for range 5 {
			f.s.State.Role(role).Started = f.now.Add(-time.Minute)
			delete(f.tm.up, role)
			f.s.Tick() // it ended

			*f.now = f.now.Add(reopenPause)
			f.s.Tick() // opened again, hand-out made

			f.newSession(role, "sid-"+role+"-new") // it runs a minute or so, never confirms
			*f.now = f.now.Add(time.Minute)
		}

		return f
	}

	for _, holds := range []bool{true, false} {
		f := loop(t, "dispatcher", holds)

		assert.Zero(t, rotationNotes(f), "holds %v: the crash notes tell it: %v", holds, f.s.State.Notes)
		assert.Equal(t, 5, countEvents(f, "dispatcher", "ended"), "it ended five times")

		rots := rotationsOn(t, f, "dispatcher")
		require.Len(t, rots, 5)

		for _, r := range rots[:4] {
			require.False(t, r.Open(), "%+v", r)
			assert.Equal(t, OutcomeLost, r.Closed.Outcome)
			assert.Contains(t, r.Closed.Detail, "its session ended")
		}

		inbox := f.s.State.Role("dispatcher").Inbox
		want := map[bool]int{true: 2, false: 0}[holds]
		assert.Len(t, inbox, want, "the queue does not grow: %v", inbox)
	}

	// A role that holds nothing and never boots is not told either, however long its last rotation stays open.
	f := loop(t, "harvester", false)
	f.newSession("harvester", "sid-harvester-new")

	for range 6 {
		*f.now = f.now.Add(10 * time.Minute)
		f.rg.byPID[f.tm.up["harvester"]] = &registry.Session{Status: "idle", StatusUpdatedAt: f.now.Add(-9 * time.Minute).UnixMilli(),
			SessionID: "sid-harvester-new"}
		f.s.Tick()
	}

	assert.Zero(t, rotationNotes(f), "%v", f.s.State.Notes)
	assert.False(t, lastRotation(t, f, "harvester").Open(), "it was closed: its boot never finished")

	// A crash rotation of a role that holds a task and runs on without accepting it is still told, once.
	g := crashed(t, "")
	g.s.Tick()
	g.newSession("designer", "sid-new")
	g.rg.endedAt["sid-new"] = g.now.Add(-time.Minute)
	g.typedAfter("designer", "sid-new", 2)
	g.s.cardSeen("designer", lastRotation(t, g, "designer").Lines[0], SeenTranscript, *g.now)

	for range 6 {
		*g.now = g.now.Add(5 * time.Minute)
		g.idleSince(g.now.Add(-rotationAcceptWait - time.Minute))
		g.s.Tick()
	}

	assert.Equal(t, 1, rotationNotes(g), "%v", g.s.State.Notes)
}

// M4. retry refuses a rotation that was aborted, and one whose role runs the old conversation.
func TestRetryRefusesAnAbortedRotationAndTheOldConversation(t *testing.T) {
	for _, lost := range []bool{false, true} {
		f, _ := freshByHand(t)
		bootedFresh(f)

		if lost {
			f.s.rotationEnd("designer", lastRotation(t, f, "designer").ID, OutcomeLost, "it did not accept T-1", nil, false)
		}

		require.NoError(t, abort(t, f))
		f.newSession("designer", "sid-designer")

		_, err := f.s.RetryRotation("designer")
		require.ErrorContains(t, err, "was aborted: the role was put back on its old conversation", "lost %v", lost)
	}

	f, _ := freshByHand(t)
	f.rg.byPID[f.tm.up["designer"]] = &registry.Session{Status: "idle", StatusUpdatedAt: f.now.UnixMilli(), SessionID: "sid-designer"}

	_, err := f.s.RetryRotation("designer")
	require.ErrorContains(t, err, "it runs its old conversation sid-designer")
	assert.Len(t, f.s.State.Role("designer").Inbox, 2, "nothing was handed")
}

// M5. An abort of a rotation whose fresh conversation did work needs --force, and the refusal names the conversation it would leave
// and the command that comes back to it: a task it accepted, a rotation lost that accepted late, a run of over 30 minutes.
func TestAbortOfAFreshConversationThatDidWorkNeedsForce(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		f, _ := freshByHand(t)
		bootedFresh(f)
		f.typedAfter("designer", "sid-new", 2)

		// An acceptance the beat has noted, the other tasks still waited for: a second task not yet accepted.
		f.tk.list = append(f.tk.list, task.Task{ID: "T-9", Role: "designer", State: task.Open, Updated: f.now.Add(-time.Hour).UTC()})
		require.NoError(t, RecordRotationEvidence(f.s.Crew.Layout(), "designer", EvidenceAccepted, "T-1", "designer", "sid-new", *f.now))

		before := len(f.tm.typed["designer"])
		err := abort(t, f)
		require.ErrorContains(t, err, "its fresh conversation sid-new did work (it accepted T-1)")
		assert.ErrorContains(t, err, "zmina rotation abort designer --force")
		assert.ErrorContains(t, err, "up 'designer' --resume='sid-new'", "the way back to the conversation it would leave")
		assert.True(t, f.tm.Has("designer"), "nothing was put down")
		assert.Equal(t, before, len(f.tm.typed["designer"]), "no /exit")

		plan, _, err := f.s.BeginAbort("designer", DownOptions{Force: true}, true)
		require.NoError(t, err)
		assert.Equal(t, "sid-new", plan.Left)
		assert.Contains(t, plan.Return, "up 'designer' --resume='sid-new'")
		require.NoError(t, f.s.FinishAbort("designer", plan))
		assert.Equal(t, "sid-designer", f.s.State.Role("designer").RestartGaveUp)
	})

	t.Run("lost and then accepted late", func(t *testing.T) {
		g, _ := freshByHand(t)
		bootedFresh(g)
		g.typedAfter("designer", "sid-new", 2)
		g.s.cardSeen("designer", lastRotation(t, g, "designer").Lines[0], SeenTranscript, *g.now)
		*g.now = g.now.Add(rotationAcceptWait + time.Minute)
		g.idleSince(g.now.Add(-rotationAcceptWait - 30*time.Second))
		g.s.Tick()
		require.Equal(t, OutcomeLost, lastRotation(t, g, "designer").Closed.Outcome)

		g.s.By = "designer"
		require.NoError(t, g.s.AcceptTask("T-1", deliveryOf(t, g).ID))
		g.s.By = "hand"
		g.s.Tick()
		require.True(t, lastRotation(t, g, "designer").LateRebuilt())

		err := abort(t, g)
		require.ErrorContains(t, err, "did work (it accepted T-1)")
		require.NoError(t, abortWith(t, g, true))
	})

	t.Run("a long run", func(t *testing.T) {
		f, _ := freshByHand(t)
		bootedFresh(f)
		*f.now = f.now.Add(abortFreshAge + 2*time.Minute)
		f.idleSince(f.now.Add(-time.Minute))

		err := abort(t, f)
		require.ErrorContains(t, err, "its fresh conversation sid-new did work (it has run for 3")
		assert.True(t, f.tm.Has("designer"))
		require.NoError(t, abortWith(t, f, true))
	})

	t.Run("young and idle", func(t *testing.T) {
		f, _ := freshByHand(t)
		bootedFresh(f)
		require.NoError(t, abort(t, f), "no work to lose: no force needed")
	})
}

// L1. A start by hand or a reopen whose process died after the start and before it handed anything is lost for want of its
// context, once, and retry mends it: its record was written after the start, and the beat does not call it "not begun".
func TestStartThatDiedBeforeItHandedAnythingIsLostAndRetryMendsIt(t *testing.T) {
	for _, kind := range []string{KindUp, KindCrash} {
		f := settled(t)
		require.NoError(t, f.s.Down("designer", DownOptions{}))

		from, tasks := f.s.rotationFrom("designer"), f.s.heldTasks("designer")
		require.NoError(t, f.s.UpWith("designer", UpOptions{Fresh: true}))

		// The process dies here: the record is prepared, nothing else is done.
		id := f.s.rotationPrepareFrom("designer", kind, rotationWhy(kind), from, tasks, "", time.Time{})
		require.NotEmpty(t, id)
		require.Empty(t, f.s.State.Role("designer").Inbox, "nothing was handed")

		f.s.Tick()

		r := lastRotation(t, f, "designer")
		require.True(t, r.Open(), "%s: not closed as 'not begun': %+v", kind, r.Closed)
		assert.Equal(t, PhaseStarted, r.Phase, "the beat saw the start the record follows")

		bootedFresh(f)

		for range 4 {
			*f.now = f.now.Add(rotationAcceptWait/2 + time.Minute)
			f.idleSince(f.now.Add(-rotationAcceptWait - time.Minute))
			f.s.Tick()
		}

		r = lastRotation(t, f, "designer")
		require.False(t, r.Open(), "%s", kind)
		assert.Equal(t, OutcomeLost, r.Closed.Outcome, "%s: %+v", kind, r.Closed)
		assert.Equal(t, 1, phaseLines(f, "designer", r.ID, PhaseClosed))

		if kind == KindUp {
			assert.Equal(t, 1, rotationNotes(f), "told once: %v", f.s.State.Notes)
		}

		report := retried(t, f)
		assert.Contains(t, report, "T-1: its packet is queued again", kind)
		assert.Len(t, handOutLines(f.s.State.Role("designer").Inbox), 2, kind)
	}
}

// L2. The tasks a start handed are the rotation's artifacts: one assigned to the role between the rotation's preparation and its
// start is followed (its acceptance is waited for), and a start that handed tasks its prepared line did not name has them.
func TestRotationFollowsTheTasksItHandedAndNotOnlyThoseItPrepared(t *testing.T) {
	f := settled(t)

	_, claim, err := f.s.BeginRotationDown("designer", KindManual, DownOptions{Force: true})
	require.NoError(t, err)

	// Assigned while the role went down.
	f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: "designer", State: task.Open, Title: "paint", Updated: f.now.Add(-time.Hour).UTC()})

	id, err := f.s.FinishRotationDown("designer", claim)
	require.NoError(t, err)
	require.NoError(t, f.s.StartRotation("designer", id, UpOptions{Fresh: true}, KindManual))

	r := lastRotation(t, f, "designer")
	require.Len(t, r.Artifacts, 2, "%+v", r.Artifacts)
	assert.Equal(t, []string{"T-1", "T-2"}, r.acceptTasks())

	// It is rebuilt only once both are accepted.
	bootedFresh(f)
	f.typedAfter("designer", "sid-new", 3)

	accept := func(id string) {
		tk, err := f.tk.Get(id)
		require.NoError(t, err)
		require.NotNil(t, tk.Delivery)

		f.s.By = "designer"
		require.NoError(t, f.s.AcceptTask(id, tk.Delivery.ID))
		f.s.By = "hand"
	}

	accept("T-1")
	f.s.Tick()
	require.True(t, lastRotation(t, f, "designer").Open(), "T-2 is not accepted")

	accept("T-2")
	f.s.Tick()
	assert.Equal(t, OutcomeRebuilt, lastRotation(t, f, "designer").Closed.Outcome)

	// The fold itself: the started line's tasks, when it names any, are the rotation's; a prepared line's stay when it names none.
	at := time.Now().UTC()
	lines := []rotationLine{
		{ID: "a", At: at, Phase: PhasePrepared, Role: "x", Artifacts: []RotationArtifact{{Task: "T-1"}}},
		{ID: "a", At: at, Phase: PhaseStarted, Artifacts: []RotationArtifact{{Task: "T-1"}, {Task: "T-2"}}},
		{ID: "b", At: at, Phase: PhasePrepared, Role: "x", Artifacts: []RotationArtifact{{Task: "T-3"}}},
		{ID: "b", At: at, Phase: PhaseStarted},
		{ID: "c", At: at, Phase: PhasePrepared, Role: "x"},
		{ID: "c", At: at, Phase: PhaseStarted, Artifacts: []RotationArtifact{{Task: "T-4"}}},
	}

	rots := foldRotations(lines)
	require.Len(t, rots, 3)
	assert.Equal(t, []string{"T-1", "T-2"}, rots[0].acceptTasks())
	assert.Equal(t, []string{"T-3"}, rots[1].acceptTasks())
	assert.Equal(t, []string{"T-4"}, rots[2].acceptTasks())
}

// L4. A clear superseded by a restart is judged by the conversation the role is on: another than the one the clear was typed
// into is the clear's (rebuilt, named), the same one is resumed.
func TestSupersededClearIsJudgedByTheConversationTheRoleIsOn(t *testing.T) {
	for _, started := range []bool{true, false} {
		f := settled(t)
		f.s.rotationTrimOpen("designer", KindClear, "context 120k over 100k")

		if started {
			f.rg.byPID[f.tm.up["designer"]].SessionID = "sid-cleared"
			f.rg.context["sid-cleared"] = 5_000 // it has had a turn: the shift remembers it as it puts the role down
		}

		id, err := f.restartByHand(t, UpOptions{Fresh: true})
		require.NoError(t, err)

		var clear Rotation

		for _, r := range rotationsOn(t, f, "designer") {
			if r.Kind == KindClear {
				clear = r
			}
		}

		require.False(t, clear.Open(), "started %v", started)
		assert.Equal(t, "superseded by rotation "+id, strings.TrimSpace(clear.Closed.Detail), "started %v", started)

		if started {
			assert.Equal(t, OutcomeRebuilt, clear.Closed.Outcome)
			assert.Equal(t, []string{"new conversation sid-cleared"}, clear.Closed.Confirmed)
			assert.Equal(t, "sid-cleared", clear.To.Session)
		} else {
			assert.Equal(t, OutcomeResumed, clear.Closed.Outcome)
		}
	}
}

// L5. retry with no ledger is an error, not a panic; an abort whose command died between its two holds of the lock leaves a
// role the stale-down finisher puts right: down, not held, so a keep: always role is opened again, and the dispatcher is told once.
func TestRetryWithNoLedgerAndAnAbortThatDiedDuringItsWait(t *testing.T) {
	f, _ := freshByHand(t)
	f.s.Tasks = nil

	_, err := f.s.RetryRotation("designer")
	require.ErrorContains(t, err, "no task ledger")

	f, _ = freshByHand(t)
	f.s.Crew.Roles["designer"].Keep = crew.KeepAlways
	bootedFresh(f)

	plan, _, err := f.s.BeginAbort("designer", DownOptions{Force: true}, false)
	require.NoError(t, err)
	require.True(t, plan.Begun)

	rs := f.s.State.Role("designer")
	require.True(t, rs.Held, "put down, held, as any down")
	require.NotNil(t, rs.Pending)
	require.Equal(t, pendingAbort, rs.Pending.Kind)

	// Its command is gone: the ticker finishes it after the down's own time, once.
	*f.now = f.now.Add(downingFor + time.Second)

	for range 3 {
		f.s.Tick()
		*f.now = f.now.Add(10 * time.Second)
	}

	rs = f.s.State.Role("designer")
	assert.False(t, rs.Held, "not held: a keep: always role is opened again")
	assert.True(t, rs.Downing.IsZero())
	assert.Nil(t, rs.Pending)
	assert.Equal(t, 1, countNotes(f, "designer: rotation "+plan.ID+" abort ended before it finished"), "%v", f.s.State.Notes)

	*f.now = f.now.Add(reopenPause)
	did := f.s.Tick()
	assert.Contains(t, did, "started designer", "the ticker opens it again")

	// A plain down that died stays held, as ever.
	g := settled(t)
	g.s.Crew.Roles["designer"].Keep = crew.KeepAlways
	_, err = g.s.BeginDown("designer", DownOptions{})
	require.NoError(t, err)
	*g.now = g.now.Add(staleDown + time.Second)
	g.s.Tick()
	assert.True(t, g.s.State.Role("designer").Held)
}

// L6. A retry's hand-out is the rotation's own: an unaccepted task is told once, by the rotation when it is lost, and the
// acceptance alert of the re-queued packet is not told besides.
func TestRetryHandOutIsTheRotationsToTellAndNotTheAlerts(t *testing.T) {
	f, _ := freshByHand(t)
	bootedFresh(f)
	f.s.State.Role("designer").Inbox = nil // typed, landed, and not accepted

	report := retried(t, f)
	require.Contains(t, report, "T-1: its packet is queued again")

	typed := f.typedAfter("designer", "sid-new", 2)
	require.Len(t, typed, 2, "%v", typed)

	d := deliveryOf(t, f)
	require.False(t, d.SentAt.IsZero(), "the retry's packet was sent to the fresh session")

	f.s.cardSeen("designer", lastRotation(t, f, "designer").Lines[0], SeenTranscript, *f.now)

	alerts := func() int { return countNotes(f, "designer: T-1 has no acceptance after 5 min") }
	idle := func(at time.Time) {
		*f.now = at
		f.rg.byPID[f.tm.up["designer"]] = &registry.Session{Status: "idle", StatusUpdatedAt: d.SentAt.UnixMilli(), SessionID: "sid-new"}
		f.s.Tick()
	}

	idle(d.SentAt.Add(6 * time.Minute))
	assert.Zero(t, alerts(), "the rotation that handed the packet again says it, not the alert: %v", f.s.State.Notes)

	idle(d.SentAt.Add(rotationAcceptWait + time.Minute))
	assert.Zero(t, alerts(), "%v", f.s.State.Notes)
	assert.Equal(t, 1, rotationNotes(f), "told once, by the rotation: %v", f.s.State.Notes)

	idle(d.SentAt.Add(rotationAcceptWait + 5*time.Minute))
	assert.Zero(t, alerts())
	assert.Equal(t, 1, rotationNotes(f))
}

// L3. The restart line a retry queues names only the tasks the role still holds.
func TestRetryLineNamesOnlyTheTasksTheRoleStillHolds(t *testing.T) {
	f := settled(t)
	f.tk.list = append(f.tk.list, task.Task{ID: "T-2", Role: "designer", State: task.Open, Title: "paint", Updated: f.now.Add(-time.Hour).UTC()})

	_, err := f.restartByHand(t, UpOptions{Fresh: true})
	require.NoError(t, err)
	require.Equal(t, []string{"T-1", "T-2"}, lastRotation(t, f, "designer").acceptTasks())

	bootedFresh(f)

	f.tk.list[1].Role = "other" // handed to another role since
	f.s.State.Role("designer").Inbox = nil

	report := retried(t, f)
	assert.Contains(t, report, "T-2: left, the role does not hold it any more")

	inbox := f.s.State.Role("designer").Inbox
	require.NotEmpty(t, inbox)
	assert.True(t, strings.HasPrefix(inbox[0], restartLineHead+whyManual+". Your note: T-1 (none saved), in each"), inbox[0])
	assert.NotContains(t, inbox[0], "T-2")
}
