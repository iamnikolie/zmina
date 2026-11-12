package shift

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
)

type (
	codexRegistry = codex.Registry
	codexRecord   = codex.Record
)

// restartByHand restarts designer the way zmina restart does: the down begun under the lock, finished and the start made under it
// again, in the three calls the command makes. It returns the rotation's id and the start's error.
func (f fix) restartByHand(t *testing.T, opts UpOptions) (string, error) {
	t.Helper()

	_, claim, err := f.s.BeginRotationDown("designer", KindManual, DownOptions{Force: true})
	require.NoError(t, err)

	id, err := f.s.FinishRotationDown("designer", claim)
	require.NoError(t, err)

	return id, f.s.StartRotation("designer", id, opts, KindManual)
}

// settled is the designer of restarting with a small conversation and one beat behind it, so that the shift knows the
// conversation it is on (LastSession) and no restart is asked for.
func settled(t *testing.T) fix {
	t.Helper()

	f := restarting(t, "", 100_000, 10*time.Minute)
	f.s.Tick()
	require.Equal(t, "sid-designer", f.s.State.Role("designer").LastSession)

	return f
}

// bootedFresh is the moment the fresh session of designer is registered, idle and past its boot turn.
func bootedFresh(f fix) {
	f.newSession("designer", "sid-new")
	f.rg.endedAt["sid-new"] = f.now.Add(-time.Minute)
}

// zmina restart --fresh of a role that holds a task hands the task: the line that says why and the packet, at the head of its
// queue, and a record the beat follows to the end: boot and the packet accepted.
func TestRestartByHandFreshHandsItsTasksAndIsRebuilt(t *testing.T) {
	f := settled(t)

	id, err := f.restartByHand(t, UpOptions{Fresh: true})
	require.NoError(t, err)

	r := lastRotation(t, f, "designer")
	assert.Equal(t, id, r.ID)
	assert.Equal(t, KindManual, r.Kind)
	assert.Equal(t, whyManual, r.Why)
	assert.Equal(t, "sid-designer", r.From.Session, "the conversation it was taken from")
	assert.Equal(t, 100_000, r.From.Context)
	assert.Equal(t, []RotationArtifact{{Task: "T-1"}}, r.Artifacts, "no note was saved: none is claimed")
	assert.Equal(t, PhaseStarted, r.Phase)
	require.Len(t, r.Lines, 2, "%v", r.Lines)
	assert.True(t, strings.HasPrefix(r.Lines[0], restartLineHead+whyManual+". Your note: T-1 (none saved)"), r.Lines[0])
	assert.True(t, strings.HasPrefix(r.Lines[1], "[zmina T-1] Read handoff packet "), r.Lines[1])

	inbox := f.s.State.Role("designer").Inbox
	require.Len(t, inbox, 2, "its context, at the head of the queue: %v", inbox)
	assert.Equal(t, packetLineOf(t, f), inbox[1])

	bootedFresh(f)
	f.typedAfter("designer", "sid-new", 2)

	f.s.By = "designer"
	require.NoError(t, f.s.AcceptTask("T-1", deliveryOf(t, f).ID))
	f.s.By = "hand"

	for range 3 {
		f.s.Tick()
	}

	r = lastRotation(t, f, "designer")
	require.False(t, r.Open(), "%+v", r)
	assert.Equal(t, OutcomeRebuilt, r.Closed.Outcome)
	assert.Contains(t, r.Closed.Confirmed, "T-1 packet accepted")
	assert.Equal(t, 1, countEvents(f, "designer", "rotation"))
	assert.Zero(t, rotationNotes(f))
}

// A fresh restart by hand whose conversation never accepts what it was handed is lost, told to the dispatcher once however
// many beats follow, with the way back.
func TestRestartByHandThatIsNotAcceptedIsLostAndToldOnce(t *testing.T) {
	f := settled(t)

	_, err := f.restartByHand(t, UpOptions{Fresh: true})
	require.NoError(t, err)

	bootedFresh(f)
	f.typedAfter("designer", "sid-new", 2)
	f.s.cardSeen("designer", lastRotation(t, f, "designer").Lines[0], SeenTranscript, *f.now)

	*f.now = f.now.Add(rotationAcceptWait + time.Minute)
	f.idleSince(f.now.Add(-rotationAcceptWait - 30*time.Second))

	for range 4 {
		f.s.Tick()
		*f.now = f.now.Add(time.Minute)
		f.idleSince(f.now.Add(-rotationAcceptWait - 30*time.Second))
	}

	r := lastRotation(t, f, "designer")
	require.False(t, r.Open())
	assert.Equal(t, OutcomeLost, r.Closed.Outcome)
	assert.Contains(t, r.Closed.Detail, "it did not accept T-1")
	assert.Contains(t, r.Closed.Back, "up 'designer' --resume='sid-designer'")
	assert.Equal(t, 1, rotationNotes(f), "told once across the beats: %v", f.s.State.Notes)
	assert.Equal(t, 1, countEvents(f, "designer", "rotation"))
}

// --continue (or a resume policy) goes on in the conversation: the record closes resumed and nothing is handed.
func TestRestartByHandOnItsConversationIsResumed(t *testing.T) {
	f := settled(t)

	_, err := f.restartByHand(t, UpOptions{Resume: "sid-designer", Dir: f.s.State.Role("designer").Dir})
	require.NoError(t, err)

	r := lastRotation(t, f, "designer")
	require.False(t, r.Open(), "%+v", r)
	assert.Equal(t, OutcomeResumed, r.Closed.Outcome)
	assert.Contains(t, r.Closed.Detail, "continues its conversation sid-designer")
	assert.Empty(t, f.s.State.Role("designer").Inbox, "nothing is handed to a conversation that has it")
	assert.Empty(t, r.Lines)
	assert.Equal(t, 1, countEvents(f, "designer", "rotation"))
	assert.Zero(t, rotationNotes(f))

	for range 2 {
		f.s.Tick()
	}

	assert.Equal(t, 1, phaseLines(f, "designer", r.ID, PhaseClosed), "closed once")
}

// A role that holds nothing starts fresh with nothing to hand, and the record follows its boot alone: no restart line was
// queued, so none is waited for.
func TestRestartByHandOfARoleHoldingNothingIsRebuiltOnItsBoot(t *testing.T) {
	f := settled(t)
	f.tk.list = nil

	_, err := f.restartByHand(t, UpOptions{Fresh: true})
	require.NoError(t, err)

	r := lastRotation(t, f, "designer")
	assert.Empty(t, r.Artifacts)
	assert.Empty(t, r.Lines)
	assert.Empty(t, f.s.State.Role("designer").Inbox)

	f.s.Tick()
	require.True(t, lastRotation(t, f, "designer").Open(), "its boot turn is not over")

	bootedFresh(f)
	f.s.Tick()

	r = lastRotation(t, f, "designer")
	require.False(t, r.Open(), "%+v", r)
	assert.Equal(t, OutcomeRebuilt, r.Closed.Outcome)
	assert.Equal(t, []string{"boot"}, r.Closed.Confirmed)
}

// A start that fails closes the rotation lost with the way back, as zmina restart always left the role: down and held. The
// caller has the error, so the dispatcher is not told besides.
func TestRestartByHandThatCannotStartIsLost(t *testing.T) {
	f := settled(t)
	f.s.Tmux = &startsFailing{fakeTmux: f.tm, fail: 1}

	id, err := f.restartByHand(t, UpOptions{Fresh: true})
	require.ErrorContains(t, err, "tmux: no server running")

	r := lastRotation(t, f, "designer")
	require.Equal(t, id, r.ID)
	require.False(t, r.Open())
	assert.Equal(t, OutcomeLost, r.Closed.Outcome)
	assert.Contains(t, r.Closed.Detail, "no session: its start failed")
	assert.True(t, strings.HasPrefix(r.Closed.Back, "zmina --crew "), "down already: only the start: %s", r.Closed.Back)
	assert.True(t, f.s.State.Role("designer").Held)
	assert.Zero(t, rotationNotes(f), "the caller was told")
}

// A down that cannot begin (a draft in the input line) changes nothing and leaves no record.
func TestRestartByHandRefusedByADraftLeavesNoRecord(t *testing.T) {
	f := settled(t)
	f.tm.drafts["designer"] = "half a thought"

	_, _, err := f.s.BeginRotationDown("designer", KindManual, DownOptions{Force: true})
	require.ErrorContains(t, err, "a draft sits in its input line")

	rots, err := ReadRotations(f.s.Crew.Layout(), "designer")
	require.NoError(t, err)
	assert.Empty(t, rots)
	assert.True(t, f.s.State.Role("designer").Up)
}

// The record is a report: one that cannot be written changes neither the down nor the start nor the hand-out.
func TestRestartByHandWithARecordThatCannotBeWrittenGoesOn(t *testing.T) {
	f := settled(t)
	require.NoError(t, os.WriteFile(f.s.Crew.Layout().RotationsDir(), []byte("not a directory"), 0o600))

	_, err := f.restartByHand(t, UpOptions{Fresh: true})
	require.NoError(t, err)

	assert.True(t, f.tm.Has("designer"))
	assert.Len(t, f.s.State.Role("designer").Inbox, 2, "its context is handed whatever the record says")
	assert.Equal(t, 1, countEvents(f, "designer", "rotation-failed"), "told once")
	assert.Zero(t, countEvents(f, "designer", "rotation"))
}

// Another command that took the role over while the first one waited for /exit is not finished over: the pane is its own.
func TestRestartByHandLeavesAPaneThatAnotherCommandTookOver(t *testing.T) {
	f := settled(t)

	_, claim, err := f.s.BeginRotationDown("designer", KindManual, DownOptions{Force: true})
	require.NoError(t, err)

	f.s.State.Role("designer").Downing = claim.Add(time.Second) // another down began since

	_, err = f.s.FinishRotationDown("designer", claim)
	require.ErrorIs(t, err, ErrDownTaken)
	assert.False(t, f.s.State.Role("designer").Downing.IsZero(), "the other command's down stands")
}

// A role's restart of itself has its record too: on its conversation it is resumed, with --fresh it hands what it holds.
func TestOwnRestartHasItsRecord(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		f := settled(t)

		own, inside := f.s.Inside("designer", 7, "sid-designer")
		require.True(t, inside)

		f.status("designer", "idle", f.now.Add(-time.Minute))

		opts, _, claim, err := f.s.ClaimOwnRestart("designer", own, fresh, false, false)
		require.NoError(t, err)
		require.NoError(t, f.s.FinishOwnRestart("designer", claim, opts))

		r := lastRotation(t, f, "designer")
		assert.Equal(t, KindOwn, r.Kind)
		assert.Equal(t, "sid-designer", r.From.Session)

		if fresh {
			assert.True(t, r.Open(), "%+v", r)
			assert.Len(t, r.Lines, 2)
			assert.Len(t, f.s.State.Role("designer").Inbox, 2)

			continue
		}

		require.False(t, r.Open(), "%+v", r)
		assert.Equal(t, OutcomeResumed, r.Closed.Outcome)
		assert.Empty(t, f.s.State.Role("designer").Inbox)
	}
}

// zmina up of a role that holds a task starts it fresh and hands the task, with a record; a start that resumes, or of a role
// that holds nothing, is the plain up it was.
func TestUpHandsTheTasksOfARoleStartedFresh(t *testing.T) {
	f := restarting(t, "", 100_000, 10*time.Minute)
	f.s.Tick()
	require.NoError(t, f.s.Down("designer", DownOptions{}))
	require.Equal(t, "sid-designer", f.s.State.Role("designer").LastSession)

	require.NoError(t, f.s.UpHanding("designer", UpOptions{}))

	r := lastRotation(t, f, "designer")
	assert.Equal(t, KindUp, r.Kind)
	assert.Equal(t, "sid-designer", r.From.Session)
	require.Len(t, r.Lines, 2)
	assert.Len(t, f.s.State.Role("designer").Inbox, 2)

	// On its conversation: nothing is handed, nothing is recorded.
	require.NoError(t, f.s.Down("designer", DownOptions{}))
	f.s.State.Role("designer").Inbox = nil
	before := len(rotationsOn(t, f, "designer"))

	require.NoError(t, f.s.UpHanding("designer", UpOptions{Resume: "sid-designer"}))
	assert.Empty(t, f.s.State.Role("designer").Inbox)
	assert.Len(t, rotationsOn(t, f, "designer"), before)

	// Holding nothing.
	require.NoError(t, f.s.Down("designer", DownOptions{}))
	f.tk.list = nil

	require.NoError(t, f.s.UpHanding("designer", UpOptions{Fresh: true}))
	assert.Empty(t, f.s.State.Role("designer").Inbox)
	assert.Len(t, rotationsOn(t, f, "designer"), before)
}

// crashed is designer, keep: always, holding T-1, whose session ended: the next beat reopens it after reopenPause.
func crashed(t *testing.T, resume string) fix {
	t.Helper()

	f := settled(t)
	f.s.Crew.Roles["designer"].Keep = crew.KeepAlways
	f.s.Crew.Roles["designer"].Resume = resume

	// It ran a while, then its process went: the beat that sees it gone waits reopenPause from its start before the next.
	f.s.State.Role("designer").Started = f.now.Add(-time.Minute)
	delete(f.tm.up, "designer")
	f.s.Tick()
	require.False(t, f.s.State.Role("designer").Up)

	*f.now = f.now.Add(reopenPause)

	return f
}

// A keep: always role whose session ended and that is started again fresh gets its task again: the line that says why and the
// packet. The record names the conversation that ended; the role accepting is the end of it.
func TestCrashReopenHandsTheTaskAgain(t *testing.T) {
	f := crashed(t, "")

	did := f.s.Tick()
	assert.Contains(t, did, "started designer")

	r := lastRotation(t, f, "designer")
	assert.Equal(t, KindCrash, r.Kind)
	assert.Equal(t, "sid-designer", r.From.Session, "the conversation that ended")
	assert.Equal(t, PhaseStarted, r.Phase)
	require.Len(t, r.Lines, 2)
	assert.True(t, strings.HasPrefix(r.Lines[0], restartLineHead+whyCrash), r.Lines[0])

	f.newSession("designer", "sid-new")
	typed := f.typedAfter("designer", "sid-new", 2)
	require.Len(t, typed, 2, "%v", typed)
	assert.True(t, strings.HasPrefix(typed[0], restartLineHead), "its context first: %v", typed)
	assert.True(t, strings.HasPrefix(typed[1], "[zmina T-1] Read handoff packet "), typed[1])

	f.rg.endedAt["sid-new"] = f.now.Add(-time.Minute)
	f.s.By = "designer"
	require.NoError(t, f.s.AcceptTask("T-1", deliveryOf(t, f).ID))
	f.s.By = "hand"

	f.s.Tick()
	r = lastRotation(t, f, "designer")
	require.False(t, r.Open(), "%+v", r)
	assert.Equal(t, OutcomeRebuilt, r.Closed.Outcome)
}

// On resume: always the reopen continues the conversation: the record is closed resumed and nothing is handed.
func TestCrashReopenOnItsConversationIsResumed(t *testing.T) {
	f := crashed(t, crew.ResumeAlways)

	f.s.Tick()

	r := lastRotation(t, f, "designer")
	require.False(t, r.Open(), "%+v", r)
	assert.Equal(t, KindCrash, r.Kind)
	assert.Equal(t, OutcomeResumed, r.Closed.Outcome)
	assert.Empty(t, f.s.State.Role("designer").Inbox)
}

// A reopened role that never gets going is told to the dispatcher once, however many beats pass, and a crash loop of a role
// that holds nothing is a record of each reopen and no note.
func TestCrashReopenToldOnceAcrossBeats(t *testing.T) {
	f := crashed(t, "")
	f.s.Tick()

	f.newSession("designer", "sid-new")

	for range 8 {
		*f.now = f.now.Add(3 * time.Minute)
		f.rg.byPID[f.tm.up["designer"]] = &registry.Session{Status: "idle", StatusUpdatedAt: f.now.Add(-3 * time.Minute).UnixMilli(),
			SessionID: "sid-new"}
		f.s.Tick()
	}

	r := lastRotation(t, f, "designer")
	require.False(t, r.Open())
	assert.Equal(t, OutcomeLost, r.Closed.Outcome)
	assert.Equal(t, 1, rotationNotes(f), "%v", f.s.State.Notes)
	assert.Equal(t, 1, phaseLines(f, "designer", r.ID, PhaseClosed))
}

// The first start of a keep: always seat has no conversation to be changed: no record; a task it holds is handed all the same.
func TestFirstStartOfAKeepAlwaysSeatHasNoRecord(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	f.tk.list = nil
	f.s.Tick()

	rots, err := ReadRotations(f.s.Crew.Layout(), "")
	require.NoError(t, err)
	assert.Empty(t, rots, "the crew's first start changed no conversation")

	// A task it holds is handed all the same, and the line does not say a session ended.
	f = fixtureWith(t, crewYAML)
	f.tk.list = []task.Task{{ID: "T-1", Role: "harvester", State: task.Open, Title: "sweep", Updated: f.now.Add(-time.Hour).UTC()}}
	f.s.Tick()

	inbox := f.s.State.Role("harvester").Inbox
	require.Len(t, inbox, 2, "%v", inbox)
	assert.True(t, strings.HasPrefix(inbox[0], restartLineHead+whyFirst+". Your note: T-1 (none saved)"), inbox[0])
	assert.True(t, strings.HasPrefix(inbox[1], "[zmina T-1] Read handoff packet "), inbox[1])

	rots, err = ReadRotations(f.s.Crew.Layout(), "")
	require.NoError(t, err)
	assert.Empty(t, rots)
}

// A /compact the ticker types is a rotation of its own: its record names the conversation it was typed into and the size, and
// closes resumed once the compaction shows. The beat leaves it to the trim, however many beats pass first.
func TestCompactHasARecordClosedWhenItShows(t *testing.T) {
	f := fixtureWith(t, trimCrew)
	f.s.Tick()

	*f.now = f.now.Add(10 * time.Minute)
	f.rg.context["sid-dispatcher"] = 200_000
	f.status("dispatcher", "idle", f.now.Add(-10*time.Minute))
	f.s.Tick()
	require.Equal(t, 1, compactCount(f.tm.typed["dispatcher"]))

	r := lastRotation(t, f, "dispatcher")
	assert.Equal(t, KindCompact, r.Kind)
	assert.Equal(t, "context 200k over 150k", r.Why)
	assert.Equal(t, "sid-dispatcher", r.From.Session)
	assert.Equal(t, 200_000, r.From.Context)
	assert.True(t, r.Open())

	for range 3 {
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
		assert.True(t, lastRotation(t, f, "dispatcher").Open(), "the beat's reconcile leaves a compact to the trim")
	}

	f.rg.compacted["sid-dispatcher"] = *f.now
	f.rg.context["sid-dispatcher"] = 40_000

	for range 3 {
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
	}

	r = lastRotation(t, f, "dispatcher")
	require.False(t, r.Open(), "%+v", r)
	assert.Equal(t, OutcomeResumed, r.Closed.Outcome)
	assert.Contains(t, r.Closed.Detail, "compacted")
	assert.Equal(t, 1, phaseLines(f, "dispatcher", r.ID, PhaseClosed))
	assert.Equal(t, 1, countEvents(f, "dispatcher", "rotation"))
	assert.Zero(t, rotationNotes(f))
}

// A compact that is never seen closes resumed, "nothing changed", when the trim lets it go; one the trim never got to settle
// (the role was never between turns again) is closed by the beat once it is twice as old as the trim waits.
func TestCompactThatIsNeverSeenClosesResumed(t *testing.T) {
	f := fixtureWith(t, trimCrew)
	f.s.Tick()

	*f.now = f.now.Add(10 * time.Minute)
	f.rg.context["sid-dispatcher"] = 200_000
	f.status("dispatcher", "idle", f.now.Add(-10*time.Minute))
	f.s.Tick()
	require.Equal(t, 1, compactCount(f.tm.typed["dispatcher"]))

	*f.now = f.now.Add(trimPause + time.Minute)
	f.status("dispatcher", "idle", f.now.Add(-10*time.Minute))

	for range 2 {
		f.s.Tick()
	}

	assert.True(t, f.ev.has("dispatcher", "trim-unseen"))

	rots := rotationsOn(t, f, "dispatcher")
	require.Len(t, rots, 2, "the first compact, then the one the trim tried again: %+v", rots)
	require.False(t, rots[0].Open())
	assert.Equal(t, OutcomeResumed, rots[0].Closed.Outcome)
	assert.Contains(t, rots[0].Closed.Detail, "nothing changed")
	assert.Zero(t, rotationNotes(f))

	// The second one: the role is busy from then on, the trim never looks again.
	f.status("dispatcher", "busy", *f.now)
	*f.now = f.now.Add(2*trimPause + time.Minute)
	f.status("dispatcher", "busy", *f.now)

	did := f.s.Tick()
	assert.Contains(t, strings.Join(did, "\n"), "closed, resumed (never seen)")
	assert.False(t, lastRotation(t, f, "dispatcher").Open())
	assert.Equal(t, 1, phaseLines(f, "dispatcher", rots[1].ID, PhaseClosed))

	f.s.Tick()
	assert.Equal(t, 1, phaseLines(f, "dispatcher", rots[1].ID, PhaseClosed), "closed once")
}

// A /clear closes rebuilt on the conversation it started: its id is the evidence, and a Claude clear and a Codex clear (a new
// thread) close the same way. A clear whose conversation did not change is resumed.
func TestClearClosesOnTheNewConversation(t *testing.T) {
	for _, codex := range []bool{false, true} {
		f := fixtureWith(t, trimCrew)
		require.NoError(t, f.s.Up("dev"))

		reg := &codexReg{}
		if codex {
			f.s.Crew.Roles["dev"].Runtime = "codex"
			f.s.Crew.Codex = "/opt/bin/codex"
			reg = newCodexReg(t, f, "dev", "thread-1")
		}

		*f.now = f.now.Add(3 * time.Minute)
		f.rg.context["sid-dev"] = 120_000
		f.status("dev", "idle", f.now.Add(-time.Hour))
		reg.set(f, "thread-1", 120_000)

		f.s.Tick()
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
		require.Contains(t, f.tm.typed["dev"], "/clear", "codex %v", codex)

		r := lastRotation(t, f, "dev")
		assert.Equal(t, KindClear, r.Kind)
		assert.True(t, r.Open())
		assert.Equal(t, map[bool]string{false: "sid-dev", true: "thread-1"}[codex], r.From.Session)

		// The clear started another conversation.
		next := map[bool]string{false: "sid-dev-2", true: "thread-2"}[codex]
		if codex {
			reg.set(f, next, 0)
		} else {
			f.rg.byPID[f.tm.up["dev"]].SessionID = next
			f.rg.context[next] = 0
		}

		*f.now = f.now.Add(time.Minute)
		f.s.Tick()

		r = lastRotation(t, f, "dev")
		require.False(t, r.Open(), "codex %v: %+v", codex, r)
		assert.Equal(t, OutcomeRebuilt, r.Closed.Outcome)
		assert.Equal(t, []string{"new conversation " + next}, r.Closed.Confirmed)
		assert.Equal(t, next, r.To.Session)
		assert.Zero(t, rotationNotes(f))
	}
}

// codexReg is the Codex runtime's registry of a test: a record of the bridge's thread.
type codexReg struct{ pid int }

func newCodexReg(t *testing.T, f fix, role, thread string) *codexReg {
	t.Helper()

	p, err := f.s.probe(role)
	require.NoError(t, err)

	return &codexReg{pid: p.PID}
}

// set writes the thread the bridge is on, idle for the last hour, its last turn over at a size.
func (c *codexReg) set(f fix, thread string, context int) {
	if c.pid == 0 {
		return
	}

	reg := &codexRegistry{Dir: f.s.Crew.StateDir() + "/codex"}
	_ = reg.Save(codexRecord{Session: registry.Session{PID: c.pid, Name: "dev", SessionID: thread, Status: "idle",
		StatusUpdatedAt: f.now.Add(-time.Hour).UnixMilli()}, Turn: registry.Turn{Context: context, Ended: true, At: f.now.Add(-time.Hour)}})
}

// A compact or a clear that is still open when a restart puts the role down is over: closed resumed, "superseded", and no note.
// The evidence of a restart's rotation is still taken when a compact was typed after it.
func TestCompactDoesNotHideTheRotationBeforeItAndIsSupersededByTheNext(t *testing.T) {
	f := settled(t)

	f.s.rotationTrimOpen("designer", KindCompact, "context 120k over 100k")
	compact := lastRotation(t, f, "designer")
	require.True(t, compact.Open())

	id, err := f.restartByHand(t, UpOptions{Fresh: true})
	require.NoError(t, err)

	rots := rotationsOn(t, f, "designer")
	require.Len(t, rots, 2)
	require.False(t, rots[0].Open(), "%+v", rots[0])
	assert.Equal(t, OutcomeResumed, rots[0].Closed.Outcome)
	assert.Equal(t, "superseded by rotation "+id, rots[0].Closed.Detail)
	assert.Zero(t, rotationNotes(f))

	// A compact typed into the fresh conversation does not take the evidence of the rotation that started it.
	f.s.rotationTrimOpen("designer", KindCompact, "context 130k over 100k")
	require.NoError(t, RecordRotationEvidence(f.s.Crew.Layout(), "designer", EvidenceAccepted, "T-1", "designer", "sid-new", *f.now))

	for _, r := range rotationsOn(t, f, "designer") {
		if r.ID == id {
			assert.True(t, r.has(EvidenceAccepted, "T-1"), "%+v", r.Evidence)
		}
	}
}

// freshByHand is the moment after zmina restart --fresh of designer: its fresh session runs with its context queued.
func freshByHand(t *testing.T) (fix, []string) {
	t.Helper()

	f := settled(t)
	_, err := f.restartByHand(t, UpOptions{Fresh: true})
	require.NoError(t, err)

	lines := slices.Clone(f.s.State.Role("designer").Inbox)
	require.Len(t, lines, 2)

	return f, lines
}

// abort puts the role down and back on its old conversation, like the command: one hold of the lock to begin, one to finish.
func abort(t *testing.T, f fix) error {
	t.Helper()

	plan, _, err := f.s.BeginAbort("designer", DownOptions{Force: true})
	if err != nil {
		return err
	}

	return f.s.FinishAbort("designer", plan)
}

// An open rotation is aborted: the role goes down and comes up on the conversation the rotation took it from, the rotation
// closes resumed by whoever ran it, and the restart is given up for that conversation so it is not rotated again in it.
func TestAbortPutsAnOpenRotationsRoleBackOnItsOldConversation(t *testing.T) {
	f, _ := freshByHand(t)
	bootedFresh(f)
	f.s.State.Role("designer").Dir = ""

	require.NoError(t, abort(t, f))

	assert.Contains(t, f.tm.argv["designer"], "--resume")
	assert.Contains(t, f.tm.argv["designer"], "sid-designer")

	r := lastRotation(t, f, "designer")
	require.False(t, r.Open(), "%+v", r)
	assert.Equal(t, OutcomeResumed, r.Closed.Outcome)
	assert.Equal(t, "aborted by hand: back on its old conversation sid-designer", r.Closed.Detail)
	assert.Equal(t, "sid-designer", f.s.State.Role("designer").RestartGaveUp)
	assert.Equal(t, 1, countEvents(f, "designer", "rotation-abort"))
	assert.Equal(t, 1, countEvents(f, "designer", "rotation"))
	assert.Zero(t, rotationNotes(f))

	for range 2 {
		f.s.Tick()
	}

	assert.Equal(t, 1, phaseLines(f, "designer", r.ID, PhaseClosed), "closed once")
	assert.Zero(t, rotationNotes(f))
}

// A rotation lost closed already: its role is put back all the same, and the abort is a line of evidence on it, shown as such.
func TestAbortOfALostRotationIsEvidenceOnIt(t *testing.T) {
	f, _ := restartBegun(t, 2)
	f.finish(t)

	r := lastRotation(t, f, "designer")
	require.Equal(t, OutcomeLost, r.Closed.Outcome)
	require.False(t, f.tm.Has("designer"), "down")

	f.s.Tmux = f.tm
	f.s.By = "lead"

	require.NoError(t, abort(t, f))
	assert.True(t, f.tm.Has("designer"))
	assert.Contains(t, f.tm.argv["designer"], "sid-designer")

	r = lastRotation(t, f, "designer")
	assert.Equal(t, OutcomeLost, r.Closed.Outcome, "a rotation closes once")
	require.NotNil(t, r.Aborted())
	assert.Equal(t, "lead", r.Aborted().By)
	assert.Equal(t, 1, phaseLines(f, "designer", r.ID, PhaseClosed))
}

// An abort refuses, and changes nothing, when the old conversation's transcript is gone (naming zmina up --fresh), when a down
// of the role is under way (the claim of a restart still finishing is not another command's to end), when the role runs the old
// conversation already, and when the newest rotation closed rebuilt or there is none.
func TestAbortRefuses(t *testing.T) {
	t.Run("no transcript", func(t *testing.T) {
		f, _ := freshByHand(t)
		f.s.HasTranscript = func(string, string) bool { return false }

		err := abort(t, f)
		require.ErrorContains(t, err, "its old conversation sid-designer is gone")
		assert.ErrorContains(t, err, "zmina up designer --fresh")
		assert.True(t, f.tm.Has("designer"), "nothing was put down")
		assert.True(t, lastRotation(t, f, "designer").Open())
	})

	t.Run("a down is under way", func(t *testing.T) {
		f, _ := restartBegun(t, 0)
		exits := func() int {
			return len(slices.DeleteFunc(slices.Clone(f.tm.typed["designer"]), func(l string) bool { return l != "/exit" }))
		}
		before := exits()
		downing := f.s.State.Role("designer").Downing

		err := abort(t, f)
		require.ErrorContains(t, err, "a down of it is under way")
		assert.Equal(t, before, exits(), "no second down of one role")
		assert.True(t, downing.Equal(f.s.State.Role("designer").Downing), "its claim stands")
		assert.NotNil(t, f.s.State.Role("designer").Pending)
	})

	t.Run("runs the old conversation", func(t *testing.T) {
		f, _ := freshByHand(t)
		f.rg.byPID[f.tm.up["designer"]] = &registry.Session{Status: "idle", StatusUpdatedAt: f.now.UnixMilli(), SessionID: "sid-designer"}

		require.ErrorContains(t, abort(t, f), "it runs its old conversation sid-designer already")
	})

	t.Run("rebuilt, or none", func(t *testing.T) {
		f := settled(t)
		require.ErrorContains(t, abort(t, f), "no rotation of it to abort")

		f, _ = freshByHand(t)
		bootedFresh(f)
		f.typedAfter("designer", "sid-new", 2)
		f.s.By = "designer"
		require.NoError(t, f.s.AcceptTask("T-1", deliveryOf(t, f).ID))
		f.s.By = "hand"
		f.s.Tick()
		require.Equal(t, OutcomeRebuilt, lastRotation(t, f, "designer").Closed.Outcome)

		err := abort(t, f)
		require.ErrorContains(t, err, "closed rebuilt; only an open or a lost one is aborted")
	})
}

// retried runs the retry the command runs and returns what it said.
func retried(t *testing.T, f fix) []string {
	t.Helper()

	report, err := f.s.RetryRotation("designer")
	require.NoError(t, err)

	return report
}

// A line that is on record already (in its queue, typed to it and not landed, posted and not shown, or failed) is never handed
// again: a retry does nothing for it, and says where it is. A failed line may have reached the role: it is zmina queue retry's.
func TestRetryHandsNothingThatIsOnRecord(t *testing.T) {
	where := map[string]func(rs *RoleState, lines []string){
		"inbox": func(rs *RoleState, lines []string) { rs.Inbox = slices.Clone(lines) },
		"sent": func(rs *RoleState, lines []string) {
			rs.Inbox, rs.Sent = nil, &Sent{Line: lines[0], Parts: []Part{{Line: lines[1]}}}
		},
		"posted": func(rs *RoleState, lines []string) {
			rs.Inbox, rs.Posted = nil, []Posted{{Line: lines[0], Parts: []Part{{Line: lines[1]}}, Session: "sid-new"}}
		},
		"failed": func(rs *RoleState, lines []string) {
			rs.Inbox, rs.Failed = nil, []Failed{{Line: lines[0], Error: "x"}, {Line: lines[1], Error: "x"}}
		},
	}

	for name, put := range where {
		t.Run(name, func(t *testing.T) {
			f, lines := freshByHand(t)
			bootedFresh(f)
			put(f.s.State.Role("designer"), lines)
			before := *f.s.State.Role("designer")

			report := retried(t, f)

			assert.Contains(t, strings.Join(report, "|"), "T-1: left, its packet is already in", "%v", report)
			assert.Equal(t, before.Inbox, f.s.State.Role("designer").Inbox, "nothing was queued")
			assert.Zero(t, countEvents(f, "designer", "rotation-retry"))
			assert.False(t, lastRotation(t, f, "designer").has(EvidenceRetried, ""))
		})
	}
}

// Nothing on record: the restart line and the packet are queued again at the head of the queue, once; a second retry finds them
// there. The retried rotation closed lost still closes rebuilt late when the role accepts; nobody is told twice.
func TestRetryHandsTheContextAgainOnce(t *testing.T) {
	f, lines := freshByHand(t)
	bootedFresh(f)
	f.s.State.Role("designer").Inbox = nil // typed to it, landed, and it went on without accepting

	report := retried(t, f)
	assert.Equal(t, []string{"T-1: its packet is queued again", "its restart line is queued again"}, report)

	inbox := f.s.State.Role("designer").Inbox
	require.Len(t, inbox, 2, "%v", inbox)
	assert.True(t, strings.HasPrefix(inbox[0], restartLineHead+whyManual), "its context first: %v", inbox)
	assert.Equal(t, lines[1], inbox[1], "the packet of the same task state is the same line")
	assert.Equal(t, 1, countEvents(f, "designer", "rotation-retry"))
	assert.True(t, lastRotation(t, f, "designer").has(EvidenceRetried, ""))

	again := retried(t, f)
	assert.Contains(t, strings.Join(again, "|"), "left, its packet is already in its queue")
	assert.Len(t, f.s.State.Role("designer").Inbox, 2, "queued once")
	assert.Equal(t, 1, countEvents(f, "designer", "rotation-retry"))

	// It is lost, told once, and then accepts what was handed again.
	f.typedAfter("designer", "sid-new", 2)
	f.s.cardSeen("designer", lastRotation(t, f, "designer").Lines[0], SeenTranscript, *f.now)
	*f.now = f.now.Add(rotationAcceptWait + time.Minute)
	f.idleSince(f.now.Add(-rotationAcceptWait - 30*time.Second))
	f.s.Tick()
	require.Equal(t, OutcomeLost, lastRotation(t, f, "designer").Closed.Outcome)
	require.Equal(t, 1, rotationNotes(f))

	f.s.By = "designer"
	require.NoError(t, f.s.AcceptTask("T-1", deliveryOf(t, f).ID))
	f.s.By = "hand"

	for range 3 {
		f.s.Tick()
	}

	r := lastRotation(t, f, "designer")
	assert.True(t, r.LateRebuilt(), "%+v", r.Late)
	assert.Equal(t, 1, rotationNotes(f), "no second note: %v", f.s.State.Notes)
}

// A retry leaves a task the role accepted in this session, and one it does not hold any more; it refuses where there is no
// session to hand to, or the rotation is over.
func TestRetryRefusesAndLeaves(t *testing.T) {
	f, _ := freshByHand(t)
	bootedFresh(f)
	f.typedAfter("designer", "sid-new", 2)
	f.s.By = "designer"
	require.NoError(t, f.s.AcceptTask("T-1", deliveryOf(t, f).ID))
	f.s.By = "hand"

	report := retried(t, f)
	assert.Contains(t, strings.Join(report, "|"), "T-1: left, it accepted the task in this session")

	f.tk.list[0].Role = "other"
	assert.Contains(t, strings.Join(retried(t, f), "|"), "T-1: left, the role does not hold it any more")

	f.s.Tick()
	require.Equal(t, OutcomeRebuilt, lastRotation(t, f, "designer").Closed.Outcome)

	_, err := f.s.RetryRotation("designer")
	assert.ErrorContains(t, err, "closed rebuilt; only an open or a lost one is retried")

	f, _ = freshByHand(t)
	delete(f.tm.up, "designer")
	f.s.State.Role("designer").Up = false

	_, err = f.s.RetryRotation("designer")
	assert.ErrorContains(t, err, "it is not up, there is no session to hand it to")

	f = settled(t)
	_, err = f.s.RetryRotation("designer")
	assert.ErrorContains(t, err, "no rotation of it to retry")
}

// putFails is a ledger that cannot save a delivery: the hand-out of a fresh start fails.
type putFails struct{ *fakeTasks }

func (putFails) Put(task.Task) error { return errors.New("disk full") }

// A fresh start whose hand-out fails leaves the role up and the rotation lost at once, with the way to hand again; the caller
// of a command has the error (a Partial one: what was done is kept) and the dispatcher is not told, while the beat's reopen,
// which has no one else to tell, tells the dispatcher once however many beats follow. retry then hands what could not be.
func TestFreshStartWhoseHandOutFailsIsLostAtOnce(t *testing.T) {
	t.Run("by a command", func(t *testing.T) {
		f := settled(t)
		f.s.Tasks = putFails{f.tk}

		id, err := f.restartByHand(t, UpOptions{Fresh: true})
		require.ErrorIs(t, err, ErrPartial)
		require.ErrorContains(t, err, "it started fresh, but handing its tasks to it failed: disk full")
		assert.ErrorContains(t, err, "zmina rotation retry designer hands them again")

		assert.True(t, f.tm.Has("designer"))
		assert.Empty(t, f.s.State.Role("designer").Inbox)

		r := lastRotation(t, f, "designer")
		require.Equal(t, id, r.ID)
		require.False(t, r.Open())
		assert.Equal(t, OutcomeLost, r.Closed.Outcome)
		assert.Contains(t, r.Closed.Detail, "its tasks were not handed to it")
		assert.Zero(t, rotationNotes(f), "the caller was told")

		f.s.Tasks = f.tk
		report := retried(t, f)
		assert.Contains(t, report, "T-1: its packet is queued again")
		assert.Len(t, f.s.State.Role("designer").Inbox, 2)
	})

	t.Run("by the beat", func(t *testing.T) {
		f := crashed(t, "")
		f.s.Tasks = putFails{f.tk}

		for range 4 {
			f.s.Tick()
			*f.now = f.now.Add(time.Minute)
		}

		r := lastRotation(t, f, "designer")
		require.False(t, r.Open(), "%+v", r)
		assert.Equal(t, OutcomeLost, r.Closed.Outcome)
		assert.Equal(t, 1, countNotes(f, "designer: it started fresh, but handing its tasks to it failed"), "%v", f.s.State.Notes)
		assert.Equal(t, 1, phaseLines(f, "designer", r.ID, PhaseClosed))
		assert.Equal(t, 1, countEvents(f, "designer", "rotation"))
	})
}
