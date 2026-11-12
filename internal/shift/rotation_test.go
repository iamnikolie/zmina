package shift

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/registry"
)

// rotationsOn are a role's rotations as the record has them, oldest first.
func rotationsOn(t *testing.T, f fix, role string) []Rotation {
	t.Helper()

	rots, err := ReadRotations(f.s.Crew.Layout(), role)
	require.NoError(t, err)

	return rots
}

// lastRotation is the role's newest rotation.
func lastRotation(t *testing.T, f fix, role string) Rotation {
	t.Helper()

	rots := rotationsOn(t, f, role)
	require.NotEmpty(t, rots, "no rotation in the record")

	return rots[len(rots)-1]
}

// phaseLines counts the lines of a phase a rotation has in the record.
func phaseLines(f fix, role, id, phase string) int {
	n := 0

	for _, l := range readRotationLines(f.s.Crew.Layout().Rotations(role), 1<<20) {
		if l.ID == id && l.Phase == phase {
			n++
		}
	}

	return n
}

func countEvents(f fix, role, event string) int {
	n := 0

	for _, e := range f.ev.list {
		if e.Role == role && e.Event == event {
			n++
		}
	}

	return n
}

// rotationNotes counts the dispatcher's notes that are about a rotation.
func rotationNotes(f fix) int {
	n := 0

	for _, note := range f.s.State.Notes {
		if strings.Contains(note, ": rotation ") {
			n++
		}
	}

	return n
}

// freshSession is the moment after a restart finished: the fresh session runs, registered and idle, its boot turn over.
func freshSession(t *testing.T, f fix, booted bool) {
	t.Helper()

	f.finish(t)
	f.newSession("designer", "sid-new")

	if booted {
		f.rg.endedAt["sid-new"] = f.now.Add(-time.Minute)
	}
}

// idleSince puts the fresh session idle since a time, as of now.
func (f fix) idleSince(at time.Time) {
	f.rg.byPID[f.tm.up["designer"]] = &registry.Session{Status: "idle", StatusUpdatedAt: at.UnixMilli(), SessionID: "sid-new"}
}

// A restart's record has each step as it goes: prepared with what it was on and what it checked, switching with the claim
// of the down, started with the lines it queued.
func TestRotationRecordsARestartStepByStep(t *testing.T) {
	f, _ := restartBegun(t, 0)

	r := lastRotation(t, f, "designer")
	assert.Equal(t, PhaseSwitching, r.Phase)
	assert.True(t, r.Open())
	assert.Equal(t, KindRestartAt, r.Kind)
	assert.Equal(t, "context 300k over restart_at 250k", r.Why)
	assert.Equal(t, "sid-designer", r.From.Session, "the conversation to go back to")
	assert.Equal(t, 300_000, r.From.Context)
	assert.Equal(t, "claude", r.From.Runtime)
	assert.Len(t, r.ID, 8)

	tk, err := f.tk.Get("T-1")
	require.NoError(t, err)
	assert.Equal(t, []RotationArtifact{{Task: "T-1", Checkpoint: shortRev(tk.Checkpoint.Revision)}}, r.Artifacts)

	pending := f.s.State.Role("designer").Pending
	require.NotNil(t, pending)
	assert.True(t, r.Claim.Equal(pending.Claim), "the claim is the one the pending down carries")
	assert.Zero(t, countEvents(f, "designer", "rotation"), "nothing closed yet")

	f.finish(t)

	r = lastRotation(t, f, "designer")
	assert.Equal(t, PhaseStarted, r.Phase)
	require.Len(t, r.Lines, 2, "the restart's line and the packet")
	assert.True(t, strings.HasPrefix(r.Lines[0], restartLineHead+"context 300k over restart_at 250k."), r.Lines[0])
	assert.True(t, strings.HasPrefix(r.Lines[1], "[zmina T-1] Read handoff packet "), r.Lines[1])
	assert.True(t, r.Open())
	assert.Equal(t, 1, phaseLines(f, "designer", r.ID, PhaseStarted))
}

// The time a role was asked for its note is copied into the record; a restart with no ask has none.
func TestRotationKeepsWhenTheNoteWasAskedFor(t *testing.T) {
	f := restarting(t, "", 300_000, 10*time.Minute)
	f.note(t, "T-1", f.now.Add(-10*time.Minute))

	rs := f.s.State.Role("designer")
	asked := f.now.Add(-4 * time.Minute)
	rs.RestartAsked, rs.RestartSession, rs.RestartTasks = asked, "sid-designer", []string{"T-1"}

	f.s.beginRestart("designer", f.s.heldTasks("designer"), "idle 1h at 120k, near restart_idle 1h", "")

	r := lastRotation(t, f, "designer")
	assert.True(t, r.AskedAt.Equal(asked), "%v", r.AskedAt)
	assert.Equal(t, KindRestartIdle, r.Kind)
	assert.True(t, rs.RestartAsked.IsZero(), "the ask is over, the record keeps its time")
}

// A restart after a task was done has no task to accept: its artifact is the finished task's result, its kind restart_done.
func TestRotationOfARestartAfterATaskWasDone(t *testing.T) {
	f := doneRestarting(t, "", 180_000, 11*time.Minute)
	f.s.Tick()
	require.Len(t, f.s.Restarts, 1)

	r := lastRotation(t, f, "designer")
	assert.Equal(t, KindRestartDone, r.Kind)
	assert.Equal(t, []RotationArtifact{{Task: "T-1"}}, r.Artifacts, "the task has no result in this ledger")
	assert.Empty(t, r.acceptTasks(), "it holds no task")
}

// A fresh restart that confirmed is rebuilt, and the record names what confirmed it. The beat that closes it closes it
// once: again, it changes nothing and says nothing.
func TestRotationRebuiltNamesItsEvidence(t *testing.T) {
	f, _ := restartBegun(t, 0)
	freshSession(t, f, true)
	f.typedAfter("designer", "sid-new", 2)

	d := deliveryOf(t, f)
	f.s.By = "designer"
	require.NoError(t, f.s.AcceptTask("T-1", d.ID))
	f.s.By = "hand"

	layout := f.s.Crew.Layout()
	require.NoError(t, RecordRotationEvidence(layout, "designer", EvidenceHookStart, "", "designer", "sid-new", *f.now))
	require.NoError(t, RecordRotationEvidence(layout, "designer", EvidenceNoteFetched, "T-1", "designer", "sid-new", *f.now))

	for range 2 {
		f.s.Tick()

		r := lastRotation(t, f, "designer")
		require.False(t, r.Open(), "%+v", r)
		assert.Equal(t, OutcomeRebuilt, r.Closed.Outcome)
		assert.Contains(t, r.Closed.Confirmed, "boot")
		assert.Contains(t, r.Closed.Confirmed, "session hook ran")
		assert.Contains(t, r.Closed.Confirmed, "T-1 packet accepted")
		assert.Contains(t, r.Closed.Confirmed, "T-1 note fetched")
		assert.Equal(t, "sid-new", r.To.Session, "the conversation it started")
		assert.Equal(t, 1, phaseLines(f, "designer", r.ID, PhaseClosed))
		assert.Equal(t, 1, countEvents(f, "designer", "rotation"))
		assert.Zero(t, rotationNotes(f), "rebuilt is silent: %v", f.s.State.Notes)
	}

	for _, e := range f.ev.list {
		if e.Event == "rotation" {
			assert.Equal(t, "T-1", e.Task)
			assert.True(t, strings.HasPrefix(e.Detail, "rebuilt "+lastRotation(t, f, "designer").ID+": context 300k over restart_at 250k; boot, "),
				e.Detail)
		}
	}
}

// A role that holds no task is rebuilt once it booted and its restart line was shown to it; nothing is accepted.
func TestRotationOfARoleWithNoTaskNeedsItsLineShown(t *testing.T) {
	f := doneRestarting(t, "", 180_000, 11*time.Minute)
	f.s.Tick()
	freshSession(t, f, true)

	f.s.Tick()
	r := lastRotation(t, f, "designer")
	require.True(t, r.Open(), "booted, but nothing showed it why it was restarted: %+v", r.Evidence)
	assert.True(t, r.has(EvidenceBoot, ""))
	assert.False(t, r.has(EvidenceShown, ""))

	line := r.Lines[0]
	f.s.cardSeen("designer", line, SeenTranscript, *f.now)
	f.s.Tick()

	r = lastRotation(t, f, "designer")
	require.False(t, r.Open())
	assert.Equal(t, OutcomeRebuilt, r.Closed.Outcome)
	assert.Equal(t, []string{"boot", "restart line shown"}, r.Closed.Confirmed)
}

// The restart line of an earlier rotation, same words, is not this one's: a card is its rotation's only from the time the
// rotation was prepared.
func TestRotationShownIsOfItsOwnLine(t *testing.T) {
	f := doneRestarting(t, "", 180_000, 11*time.Minute)
	f.s.Tick()
	freshSession(t, f, true)

	r := lastRotation(t, f, "designer")
	f.s.cardsOf("designer").add(Card{ID: CardID("designer", r.Lines[0]), From: "zmina", Head: r.Lines[0],
		Written: r.Prepared.Add(-time.Hour), Seen: &CardSeen{At: r.Prepared.Add(-time.Hour), By: SeenTranscript}})

	f.s.Tick()
	assert.True(t, lastRotation(t, f, "designer").Open(), "an old card of the same words shows nothing of this restart")
}

// A rotation lost is told once, with the way back, and for each way it is lost.
func TestRotationLostOnItsTimeouts(t *testing.T) {
	type step func(t *testing.T, f fix)

	cases := map[string]struct {
		booted bool
		prep   step
		why    string
	}{
		"no boot turn within boot_grace and the margin after it": {
			prep: func(t *testing.T, f fix) {
				// The beats a role that never finishes a turn gets: its lines are typed at boot_grace, and nothing comes of them.
				for range 7 {
					f.idleSince(*f.now)
					f.s.Tick()
					*f.now = f.now.Add(time.Minute)
				}
			},
			why: "its boot turn never finished",
		},
		"idle without accepting what it was shown": {
			booted: true,
			prep: func(t *testing.T, f fix) {
				// The restart line was shown to it and the role went idle: it has its context and does not act on it.
				f.s.cardSeen("designer", lastRotation(t, f, "designer").Lines[0], SeenTranscript, *f.now)
				*f.now = f.now.Add(rotationAcceptWait + time.Minute)
				f.idleSince(f.now.Add(-rotationAcceptWait - 30*time.Second))
			},
			why: "it did not accept T-1 within 10m0s idle after its context was shown",
		},
		"its session ended": {
			booted: true,
			prep: func(t *testing.T, f fix) {
				f.s.Tick()
				delete(f.tm.up, "designer")
			},
			why: "its session ended before it confirmed (T-1 is not accepted)",
		},
		"its conversation changed": {
			booted: true,
			prep: func(t *testing.T, f fix) {
				f.s.Tick()
				require.Equal(t, "sid-new", lastRotation(t, f, "designer").To.Session)
				f.rg.byPID[f.tm.up["designer"]] = &registry.Session{Status: "idle", StatusUpdatedAt: f.now.UnixMilli(), SessionID: "sid-cleared"}
			},
			why: "its conversation changed before it confirmed (T-1 is not accepted)",
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, _ := restartBegun(t, 0)
			freshSession(t, f, c.booted)

			if c.prep != nil {
				c.prep(t, f)
			}

			// Run twice: what the first closed, the second leaves as it is.
			for range 2 {
				*f.now = f.now.Add(time.Second)
				f.s.Tick()

				r := lastRotation(t, f, "designer")
				require.False(t, r.Open(), "%+v", r)
				assert.Equal(t, OutcomeLost, r.Closed.Outcome)
				assert.Equal(t, c.why, r.Closed.Detail)
				assert.Equal(t, 1, phaseLines(f, "designer", r.ID, PhaseClosed))
				assert.Equal(t, 1, countEvents(f, "designer", "rotation"))
				assert.Equal(t, 1, rotationNotes(f), "told once: %v", f.s.State.Notes)
			}

			r := lastRotation(t, f, "designer")
			assert.Contains(t, r.Closed.Back, "up 'designer' --resume='sid-designer'", "the way back to the old conversation")

			note := strings.Join(f.s.State.Notes, "|")
			assert.Contains(t, note, "rotation "+r.ID+" (restart_at) lost: "+c.why)
			assert.Contains(t, note, "rotation show 'designer'")
			assert.Contains(t, note, r.Closed.Back)
		})
	}
}

// A role that sits down to nothing is down: its way back does not put it down first.
func TestRotationLostWithItsSessionGoneNamesTheStartOnly(t *testing.T) {
	f, _ := restartBegun(t, 0)
	freshSession(t, f, true)
	f.s.Tick()
	delete(f.tm.up, "designer")
	f.s.Tick()

	r := lastRotation(t, f, "designer")
	require.False(t, r.Open())
	assert.False(t, strings.Contains(r.Closed.Back, " down "), r.Closed.Back)
	assert.True(t, strings.HasPrefix(r.Closed.Back, "zmina --crew "), r.Closed.Back)
}

// A role at work on its first turn is never timed out. If it stays unconfirmed 15 min the dispatcher is told once, and
// the record stays open.
func TestRotationOfABusyRoleIsNotTimedOutButToldOnce(t *testing.T) {
	f, _ := restartBegun(t, 0)
	freshSession(t, f, false)

	for range 3 {
		*f.now = f.now.Add(6 * time.Minute)
		f.rg.byPID[f.tm.up["designer"]] = &registry.Session{Status: "busy", StatusUpdatedAt: f.now.UnixMilli(), SessionID: "sid-new"}
		f.s.Tick()
	}

	r := lastRotation(t, f, "designer")
	require.True(t, r.Open(), "at work: %+v", r)
	assert.False(t, r.SlowTold.IsZero())
	assert.Equal(t, 1, rotationNotes(f), "%v", f.s.State.Notes)
	assert.Contains(t, strings.Join(f.s.State.Notes, "|"), "is open 20m after its start, not confirmed yet (its boot turn is not over, T-1 is not accepted)")

	for range 2 {
		*f.now = f.now.Add(6 * time.Minute)
		f.rg.byPID[f.tm.up["designer"]] = &registry.Session{Status: "busy", StatusUpdatedAt: f.now.UnixMilli(), SessionID: "sid-new"}
		f.s.Tick()
	}

	assert.Equal(t, 1, rotationNotes(f), "told once")
	slow := 0

	for _, l := range readRotationLines(f.s.Crew.Layout().Rotations("designer"), 1<<20) {
		if l.What == evidenceSlow {
			slow++
		}
	}

	assert.Equal(t, 1, slow, "one line for it")
}

// The API error that ended a boot turn is in the reason a rotation lost to it.
func TestRotationLostToAnAPIErrorSaysSo(t *testing.T) {
	f, _ := restartBegun(t, 0)
	freshSession(t, f, false)

	_, err := RecordFailure(f.s.Crew.Layout(), "designer", []byte(`{"session_id":"sid-new","error":"rate_limit"}`), *f.now)
	require.NoError(t, err)

	for range 8 {
		f.idleSince(*f.now)
		f.s.Tick()
		*f.now = f.now.Add(time.Minute)
	}

	r := lastRotation(t, f, "designer")
	require.False(t, r.Open())
	assert.Contains(t, r.Closed.Detail, "its boot turn never finished; its last turn ended on an API error: rate_limit")
}

// A lost rotation whose conversation then accepts what it was handed stays lost, and is shown as late rebuilt. Nobody is told
// a second time.
func TestRotationLateAcceptIsRecordedWithNoSecondNote(t *testing.T) {
	f, _ := restartBegun(t, 0)
	freshSession(t, f, true)
	f.typedAfter("designer", "sid-new", 2)

	r := lastRotation(t, f, "designer")
	f.s.cardSeen("designer", r.Lines[0], SeenTranscript, *f.now)
	*f.now = f.now.Add(rotationAcceptWait + time.Minute)
	f.idleSince(f.now.Add(-rotationAcceptWait - 30*time.Second))
	f.s.Tick()

	r = lastRotation(t, f, "designer")
	require.Equal(t, OutcomeLost, r.Closed.Outcome, "%+v", r)
	require.Equal(t, 1, rotationNotes(f))

	d := deliveryOf(t, f)
	f.s.By = "designer"
	require.NoError(t, f.s.AcceptTask("T-1", d.ID))
	f.s.By = "hand"

	for range 2 {
		f.s.Tick()
	}

	r = lastRotation(t, f, "designer")
	assert.Equal(t, OutcomeLost, r.Closed.Outcome, "a rotation closes once")
	require.Len(t, r.Late, 1)
	assert.Equal(t, EvidenceAccepted, r.Late[0].What)
	assert.True(t, r.LateRebuilt())
	assert.Equal(t, 1, phaseLines(f, "designer", r.ID, PhaseClosed))
	assert.Equal(t, 1, rotationNotes(f), "no second note: %v", f.s.State.Notes)
	assert.Equal(t, 1, countEvents(f, "designer", "rotation"))
}

// The acceptance alert is the rotation's to say while it is open, and after it was lost and told: the dispatcher hears one
// note, not two. A task the role is handed otherwise is told as ever (the alert's own tests).
func TestRotationHoldsBackTheAcceptanceAlert(t *testing.T) {
	f, _ := restartBegun(t, 0)
	freshSession(t, f, true)
	f.typedAfter("designer", "sid-new", 2)

	r := lastRotation(t, f, "designer")
	f.s.cardSeen("designer", r.Lines[0], SeenTranscript, *f.now)

	sent := deliveryOf(t, f).SentAt
	require.False(t, sent.IsZero())

	alerts := func() int { return countNotes(f, "designer: T-1 has no acceptance after 5 min") }

	*f.now = sent.Add(6 * time.Minute)
	f.idleSince(sent)
	f.s.Tick()

	require.True(t, lastRotation(t, f, "designer").Open(), "still inside its time")
	assert.Zero(t, alerts(), "the rotation is open: it owns the alert")

	*f.now = sent.Add(rotationAcceptWait + 2*time.Minute)
	f.idleSince(sent)
	f.s.Tick()
	f.s.Tick()

	require.Equal(t, OutcomeLost, lastRotation(t, f, "designer").Closed.Outcome)
	assert.Zero(t, alerts(), "nor after it was lost and told: %v", f.s.State.Notes)
	assert.Equal(t, 1, rotationNotes(f))

	// A hand-out made after the rotation started is not its own: the alert is as it was.
	tk, err := f.tk.Get("T-1")
	require.NoError(t, err)
	assert.True(t, f.s.rotationOwns(tk))
	tk.Delivery.QueuedAt = lastRotation(t, f, "designer").Started.Add(time.Minute)
	assert.False(t, f.s.rotationOwns(tk))
}

// With no record of a rotation for the task, the alert is told as it always was.
func TestAcceptanceAlertFiresWhenNoRotationOwnsTheTask(t *testing.T) {
	f, _ := restartBegun(t, 0)
	freshSession(t, f, true)
	f.typedAfter("designer", "sid-new", 2)

	sent := deliveryOf(t, f).SentAt
	require.False(t, sent.IsZero())
	require.NoError(t, os.RemoveAll(f.s.Crew.Layout().RotationsDir()))

	*f.now = sent.Add(6 * time.Minute)
	f.idleSince(sent)
	f.s.Tick()

	assert.Equal(t, 1, countNotes(f, "designer: T-1 has no acceptance after 5 min"), "%v", f.s.State.Notes)
}

// A role asked to restart that was never put down closes resumed: the process died between the record's first line and the
// down (the state kept nothing of it).
func TestRotationNotBegunClosesResumed(t *testing.T) {
	f := restarting(t, "", 300_000, 10*time.Minute)
	f.note(t, "T-1", f.now.Add(-10*time.Minute))
	f.s.State.Role("designer").RestartGaveUp = "sid-designer" // the beat is not to restart it itself

	id := f.s.rotationPrepare("designer", f.s.heldTasks("designer"), "context 300k over restart_at 250k", "", time.Time{})
	require.NotEmpty(t, id)
	require.Equal(t, PhasePrepared, lastRotation(t, f, "designer").Phase)

	*f.now = f.now.Add(time.Second)

	for range 2 {
		f.s.Tick()

		r := lastRotation(t, f, "designer")
		require.False(t, r.Open())
		assert.Equal(t, OutcomeResumed, r.Closed.Outcome)
		assert.Contains(t, r.Closed.Detail, "not begun")
		assert.Equal(t, 1, phaseLines(f, "designer", id, PhaseClosed))
		assert.Zero(t, rotationNotes(f), "resumed is silent")
	}
}

// A down that fails to begin closes the rotation resumed, as it was: the role was not put down.
func TestRotationOfADownThatCannotBeginIsResumed(t *testing.T) {
	f := restarting(t, "", 300_000, 10*time.Minute)
	f.note(t, "T-1", f.now.Add(-10*time.Minute))
	f.tm.probeErr["designer"] = fmt.Errorf("tmux: no server running")

	line := f.s.beginRestart("designer", f.s.heldTasks("designer"), "context 300k over restart_at 250k", "")
	assert.Contains(t, line, "no server running")

	r := lastRotation(t, f, "designer")
	require.False(t, r.Open())
	assert.Equal(t, OutcomeResumed, r.Closed.Outcome)
	assert.Contains(t, r.Closed.Detail, "not put down: ")
	assert.Zero(t, rotationNotes(f))
}

// The process dies after the start and before it is written: no started line. The record is as the state shows it.
func TestRotationSurvivesTheProcessDyingBetweenPhases(t *testing.T) {
	f, _ := restartBegun(t, 0)
	f.finish(t)

	started := f.s.State.Role("designer").Started
	r := lastRotation(t, f, "designer")
	dropLine(t, f, "designer", r.ID, PhaseStarted)

	r = lastRotation(t, f, "designer")
	require.Equal(t, PhaseSwitching, r.Phase, "what the dead process left")

	*f.now = f.now.Add(time.Second)
	did := f.s.Tick()

	r = lastRotation(t, f, "designer")
	assert.Equal(t, PhaseStarted, r.Phase)
	assert.True(t, r.Started.Equal(started), "the time of the start, not of the beat that found it: %v", r.Started)
	assert.Contains(t, strings.Join(did, "|"), "rotation "+r.ID+" started, seen by the beat")

	f.s.Tick()
	assert.Equal(t, 1, phaseLines(f, "designer", r.ID, PhaseStarted), "found once")

	// And what the fresh session then does is judged as ever.
	f.newSession("designer", "sid-new")
	f.rg.endedAt["sid-new"] = f.now.Add(-time.Second)
	f.typedAfter("designer", "sid-new", 2)
	f.s.By = "designer"
	require.NoError(t, f.s.AcceptTask("T-1", deliveryOf(t, f).ID))
	f.s.By = "hand"
	f.s.Tick()

	r = lastRotation(t, f, "designer")
	require.False(t, r.Open(), "%+v", r)
	assert.Equal(t, OutcomeRebuilt, r.Closed.Outcome)
}

// dropLine removes a rotation's lines of a phase from the record, as if the process that wrote them died first.
func dropLine(t *testing.T, f fix, role, id, phase string) {
	t.Helper()

	path := f.s.Crew.Layout().Rotations(role)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var kept []string

	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var l rotationLine
		require.NoError(t, json.Unmarshal([]byte(line), &l))

		if l.ID == id && l.Phase == phase {
			continue
		}

		kept = append(kept, line)
	}

	require.NoError(t, os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o600))
}

// A beat that dies during the down leaves the restart to the next one, which finishes it from the pending down it saved:
// the rotation is found by its claim, started once, and closes once.
func TestRotationOfARestartFinishedByAStaleBeat(t *testing.T) {
	f, _ := restartBegun(t, 0)
	before := lastRotation(t, f, "designer")
	require.Equal(t, PhaseSwitching, before.Phase)

	// The beat dies: its in-memory job is gone.
	f.s.Restarts = nil

	*f.now = f.now.Add(30 * time.Second)
	f.s.Tick()
	assert.Equal(t, PhaseSwitching, lastRotation(t, f, "designer").Phase, "inside downingFor the beat that began it may finish it")

	*f.now = f.now.Add(downingFor)
	f.s.Tick()
	require.True(t, f.ev.has("designer", "restart-late"))

	r := lastRotation(t, f, "designer")
	assert.Equal(t, before.ID, r.ID, "the same rotation: no second one")
	assert.Equal(t, PhaseStarted, r.Phase)
	assert.Len(t, rotationsOn(t, f, "designer"), 1)
	assert.Equal(t, 1, phaseLines(f, "designer", r.ID, PhaseStarted))
	assert.Len(t, r.Lines, 2)

	f.newSession("designer", "sid-new")
	f.rg.endedAt["sid-new"] = f.now.Add(-time.Second)
	f.typedAfter("designer", "sid-new", 2)
	f.s.By = "designer"
	require.NoError(t, f.s.AcceptTask("T-1", deliveryOf(t, f).ID))
	f.s.By = "hand"

	for range 2 {
		f.s.Tick()
	}

	r = lastRotation(t, f, "designer")
	require.False(t, r.Open())
	assert.Equal(t, OutcomeRebuilt, r.Closed.Outcome)
	assert.Equal(t, 1, phaseLines(f, "designer", r.ID, PhaseClosed))
}

// A down whose beat died and that was finished by a command, not by this restart, is a start the record did not make: the
// role is up on a fresh session after the claim, and it is found so. A role that never came up is lost once the down is stale.
func TestRotationOfADownThatNoRestartFinishesIsLost(t *testing.T) {
	f, _ := restartBegun(t, 0)
	f.s.Restarts = nil

	// Another command ended the down and put the role down for good.
	rs := f.s.State.Role("designer")
	rs.Downing, rs.Pending = time.Time{}, nil

	*f.now = f.now.Add(staleDown - time.Second)
	f.s.Tick()
	assert.True(t, lastRotation(t, f, "designer").Open(), "a command may still be finishing the down")

	*f.now = f.now.Add(2 * time.Second)

	for range 2 {
		f.s.Tick()

		r := lastRotation(t, f, "designer")
		require.False(t, r.Open())
		assert.Equal(t, OutcomeLost, r.Closed.Outcome)
		assert.Contains(t, r.Closed.Detail, "no session")
		assert.Equal(t, 1, rotationNotes(f))
	}
}

// A rotation open for a role when the next one puts it down is closed, once, with what is true of it, no way back and no
// note: lost when its fresh conversation did not boot and show its context, rebuilt (before it accepted) when it did.
func TestRotationSupersededByANewOne(t *testing.T) {
	for name, c := range map[string]struct {
		evidence []string
		outcome  string
		detail   string
	}{
		"nothing was confirmed": {outcome: OutcomeLost, detail: "superseded by rotation %s"},
		"it booted and was shown its context": {evidence: []string{EvidenceBoot, EvidenceShown}, outcome: OutcomeRebuilt,
			detail: "superseded by rotation %s, before it accepted its tasks"},
	} {
		t.Run(name, func(t *testing.T) {
			f := restarting(t, "", 300_000, 10*time.Minute)
			f.note(t, "T-1", f.now.Add(-10*time.Minute))
			path := f.s.Crew.Layout().Rotations("designer")

			first := f.s.rotationPrepare("designer", f.s.heldTasks("designer"), "context 300k over restart_at 250k", "", time.Time{})
			f.s.rotationSwitching("designer", first, *f.now)
			require.NoError(t, appendRotationLine(path, rotationLine{ID: first, At: *f.now, Phase: PhaseStarted}))

			for _, what := range c.evidence {
				require.NoError(t, appendRotationLine(path, rotationLine{ID: first, At: *f.now, Phase: phaseEvidence, What: what, By: "designer"}))
			}

			second := f.s.rotationPrepare("designer", f.s.heldTasks("designer"), "context 310k over restart_at 250k", "", time.Time{})
			require.NotEqual(t, first, second)

			// Prepared is no down: the first is as it was.
			assert.True(t, rotationsOn(t, f, "designer")[0].Open())

			f.s.rotationSwitching("designer", second, *f.now)

			rots := rotationsOn(t, f, "designer")
			require.Len(t, rots, 2)
			require.False(t, rots[0].Open())
			assert.Equal(t, c.outcome, rots[0].Closed.Outcome)
			assert.Equal(t, fmt.Sprintf(c.detail, second), rots[0].Closed.Detail)
			assert.Empty(t, rots[0].Closed.Back, "the role is being put down: no way back to name")
			assert.True(t, rots[1].Open())
			assert.Zero(t, rotationNotes(f), "superseded is silent: %v", f.s.State.Notes)

			assert.False(t, f.s.rotationEnd("designer", first, OutcomeRebuilt, "", nil, false), "a rotation closes once")
			assert.Equal(t, 1, phaseLines(f, "designer", first, PhaseClosed))
		})
	}
}

// A restart whose down fails does not close the rotation of the session that goes on: it was not superseded, and it is
// followed as before.
func TestRotationNotSupersededByARestartThatCannotBeginItsDown(t *testing.T) {
	f, _ := restartBegun(t, 0)
	freshSession(t, f, true)
	older := lastRotation(t, f, "designer")
	require.True(t, older.Open())

	f.s.State.Role("designer").RestartGaveUp = ""
	f.tm.probeErr["designer"] = fmt.Errorf("tmux: no server running")
	f.s.beginRestart("designer", f.s.heldTasks("designer"), "context 310k over restart_at 250k", "")

	rots := rotationsOn(t, f, "designer")
	require.Len(t, rots, 2)
	assert.True(t, rots[0].Open(), "its session still runs: %+v", rots[0].Closed)
	assert.Zero(t, rotationNotes(f), "%v", f.s.State.Notes)
	require.False(t, rots[1].Open())
	assert.Equal(t, OutcomeResumed, rots[1].Closed.Outcome)

	// And the older one is still followed by the beat, though it is not the newest in the record.
	delete(f.tm.probeErr, "designer")
	f.typedAfter("designer", "sid-new", 2)
	f.s.By = "designer"
	require.NoError(t, f.s.AcceptTask("T-1", deliveryOf(t, f).ID))
	f.s.By = "hand"
	f.s.Tick()
	assert.False(t, rotationsOn(t, f, "designer")[0].Open())
	assert.Equal(t, OutcomeRebuilt, rotationsOn(t, f, "designer")[0].Closed.Outcome)
}

// A fresh start that fails and goes back to the old conversation closes resumed; one that cannot go back closes lost with
// no session. Each is told by the restart's own note, which names the rotation: no second note.
func TestRotationOfAFailedFreshStart(t *testing.T) {
	t.Run("back on its conversation", func(t *testing.T) {
		f, _ := restartBegun(t, 1)
		f.finish(t)

		r := lastRotation(t, f, "designer")
		require.False(t, r.Open(), "%+v", r)
		assert.Equal(t, OutcomeResumed, r.Closed.Outcome)
		assert.Contains(t, r.Closed.Detail, "its fresh start failed: designer: tmux: no server running; back on its conversation")
		assert.Equal(t, 1, countNotes(f, "designer: restart for its context failed"), "%v", f.s.State.Notes)
		assert.Zero(t, rotationNotes(f), "no note of the rotation's own")
		assert.Contains(t, strings.Join(f.s.State.Notes, "|"), "; rotation "+r.ID)
		assert.Equal(t, 1, countEvents(f, "designer", "rotation"))

		// The beat leaves it as it is, however many times it looks.
		for range 2 {
			f.s.Tick()
		}

		assert.Equal(t, 1, phaseLines(f, "designer", r.ID, PhaseClosed))
		assert.Equal(t, 1, countNotes(f, "designer: restart for its context failed"))
	})

	t.Run("both starts fail", func(t *testing.T) {
		f, _ := restartBegun(t, 2)
		f.finish(t)

		r := lastRotation(t, f, "designer")
		require.False(t, r.Open(), "%+v", r)
		assert.Equal(t, OutcomeLost, r.Closed.Outcome)
		assert.Contains(t, r.Closed.Detail, "no session: its fresh start failed")
		assert.Contains(t, r.Closed.Detail, "starting it on its conversation failed too")
		assert.True(t, strings.HasPrefix(r.Closed.Back, "zmina --crew "), "the role is down: only the start: %s", r.Closed.Back)
		assert.NotContains(t, r.Closed.Back, " down ")
		assert.Equal(t, 1, countNotes(f, "designer: restart for its context failed"))
		assert.Zero(t, rotationNotes(f), "no note of the rotation's own")
		assert.Contains(t, strings.Join(f.s.State.Notes, "|"), "; rotation "+r.ID)

		for range 2 {
			f.s.Tick()
		}

		assert.Equal(t, 1, phaseLines(f, "designer", r.ID, PhaseClosed))
	})

	t.Run("a restart after a task was done", func(t *testing.T) {
		f := doneRestarting(t, "", 180_000, 11*time.Minute)
		f.s.Tick()
		require.Len(t, f.s.Restarts, 1)

		f.s.Tmux = &startsFailing{fakeTmux: f.tm, fail: 1}
		f.finish(t)

		r := lastRotation(t, f, "designer")
		require.False(t, r.Open())
		assert.Equal(t, KindRestartDone, r.Kind)
		assert.Equal(t, OutcomeResumed, r.Closed.Outcome)
	})
}

// The old session that cannot be removed ends the rotation lost, with the restart's own note.
func TestRotationOfAnOldSessionThatCannotBeRemoved(t *testing.T) {
	f, _ := restartBegun(t, 0)
	f.tm.probeErr["designer"] = fmt.Errorf("tmux: gone")

	f.finish(t)

	r := lastRotation(t, f, "designer")
	require.False(t, r.Open())
	assert.Equal(t, OutcomeLost, r.Closed.Outcome)
	assert.Contains(t, r.Closed.Detail, "the session it was on could not be removed")
	assert.Equal(t, 1, countNotes(f, "designer: restart for its context failed"))
	assert.Zero(t, rotationNotes(f))
}

// The record is a report: a record that cannot be written changes no restart, and is told once.
func TestRotationRecordThatCannotBeWrittenFailsNoRestart(t *testing.T) {
	f := restarting(t, "", 300_000, 10*time.Minute)
	f.note(t, "T-1", f.now.Add(-10*time.Minute))

	// Where the record's directory is to be there is a file.
	require.NoError(t, os.WriteFile(f.s.Crew.Layout().RotationsDir(), []byte("not a directory"), 0o600))

	did := f.s.Tick()
	require.Len(t, f.s.Restarts, 1, "%v", did)
	assert.True(t, f.tm.typed["designer"][len(f.tm.typed["designer"])-1] == "/exit")

	out := f.finish(t)
	require.Len(t, out, 1)
	assert.Contains(t, out[0], "restarted fresh")
	assert.True(t, f.tm.Has("designer"))
	assert.Len(t, f.s.State.Role("designer").Inbox, 2, "the hand-out is made whatever the record says")

	f.newSession("designer", "sid-new")

	for range 3 {
		f.s.Tick()
	}

	assert.Equal(t, 1, countEvents(f, "designer", "rotation-failed"), "told once")
	assert.Zero(t, countEvents(f, "designer", "rotation"))
	assert.Zero(t, rotationNotes(f))
}

// A record with garbage in it, and a last line a dead write left unfinished, is no obstacle: the restart goes on and its lines
// are written whole after the garbage.
func TestRotationRecordWithGarbageInItStillRecords(t *testing.T) {
	f := restarting(t, "", 300_000, 10*time.Minute)
	f.note(t, "T-1", f.now.Add(-10*time.Minute))

	path := f.s.Crew.Layout().Rotations("designer")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("\x00\x01 garbage\n{\"id\":\"old\",\"at\":\"2026-10-24T11:00:00Z\",\"phase\":\"prep"), 0o600))

	f.s.Tick()
	require.Len(t, f.s.Restarts, 1)
	f.finish(t)

	r := lastRotation(t, f, "designer")
	assert.Equal(t, PhaseStarted, r.Phase)
	assert.Len(t, rotationsOn(t, f, "designer"), 1)
	assert.Zero(t, countEvents(f, "designer", "rotation-failed"))
}

// A record that is gone changes no restart or delivery: the same run with and without it hands out the same, in the same
// order, and leaves the same state.
func TestRotationRecordIsNotRead(t *testing.T) {
	run := func(t *testing.T, remove bool) (*State, []string) {
		f, _ := restartBegun(t, 0)

		if remove {
			require.NoError(t, os.RemoveAll(f.s.Crew.Layout().RotationsDir()))
		}

		f.finish(t)
		f.newSession("designer", "sid-new")
		typed := f.typedAfter("designer", "sid-new", 2)

		return f.s.State, typed
	}

	with, typedWith := run(t, false)
	without, typedWithout := run(t, true)

	assert.Equal(t, len(typedWith), len(typedWithout))
	assert.Equal(t, with.Role("designer").Inbox, without.Role("designer").Inbox)
	assert.Equal(t, with.Role("designer").Up, without.Role("designer").Up)
}

// Lines that do not read, a line cut short by a write that died, and a line of a rotation whose start is not in what was read
// are left out; the rest folds.
func TestRotationFoldToleratesWhatDoesNotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotations", "designer.jsonl")
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	line := func(l rotationLine) {
		t.Helper()
		require.NoError(t, appendRotationLine(path, l))
	}

	line(rotationLine{ID: "r1", At: at, Phase: PhasePrepared, Role: "designer", Kind: KindRestartAt, Why: "context 300k over restart_at 250k"})
	line(rotationLine{ID: "gone", At: at, Phase: phaseEvidence, What: EvidenceBoot})

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString("this is not json\n{\"id\":\"r1\",\"phase\":\"nonsense\",\"at\":\"2026-10-24T12:00:00Z\"}\n\n")
	require.NoError(t, err)
	// A write that died: no closing brace, no newline.
	_, err = f.WriteString(`{"id":"r1","at":"2026-10-24T12:01:00Z","phase":"switching","cla`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	rots := foldRotations(readRotationLines(path, 1<<20))
	require.Len(t, rots, 1)
	assert.Equal(t, PhasePrepared, rots[0].Phase, "the torn line is not a step")

	// The next line is not joined to it.
	line(rotationLine{ID: "r1", At: at.Add(2 * time.Minute), Phase: PhaseSwitching, Claim: at})

	rots = foldRotations(readRotationLines(path, 1<<20))
	require.Len(t, rots, 1)
	assert.Equal(t, PhaseSwitching, rots[0].Phase)

	// The first closing line wins; what comes after it is late, and a second prepared line of the id is not a new rotation.
	line(rotationLine{ID: "r1", At: at.Add(3 * time.Minute), Phase: PhaseClosed, Outcome: OutcomeLost, Detail: "first"})
	line(rotationLine{ID: "r1", At: at.Add(4 * time.Minute), Phase: PhaseClosed, Outcome: OutcomeRebuilt, Detail: "second"})
	line(rotationLine{ID: "r1", At: at.Add(5 * time.Minute), Phase: phaseEvidence, What: EvidenceAccepted, Task: "T-1"})
	line(rotationLine{ID: "r1", At: at.Add(6 * time.Minute), Phase: PhasePrepared, Role: "other"})

	rots = foldRotations(readRotationLines(path, 1<<20))
	require.Len(t, rots, 1)
	assert.Equal(t, "designer", rots[0].Role)
	assert.Equal(t, "first", rots[0].Closed.Detail)
	assert.Equal(t, OutcomeLost, rots[0].Closed.Outcome)
	require.Len(t, rots[0].Late, 1)
	assert.Empty(t, rots[0].Evidence)
}

// Only the newest lines are read: a record bigger than the window loses its oldest lines and the line cut by the window's
// edge, and what is left folds.
func TestRotationFoldOfATruncatedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "designer.jsonl")
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	for i := range 40 {
		id := fmt.Sprintf("r%02d", i)
		require.NoError(t, appendRotationLine(path, rotationLine{ID: id, At: at.Add(time.Duration(i) * time.Minute),
			Phase: PhasePrepared, Role: "designer", Why: strings.Repeat("w", 100)}))
		require.NoError(t, appendRotationLine(path, rotationLine{ID: id, At: at.Add(time.Duration(i)*time.Minute + time.Second),
			Phase: PhaseClosed, Outcome: OutcomeResumed}))
	}

	st, err := os.Stat(path)
	require.NoError(t, err)

	rots := foldRotations(readRotationLines(path, st.Size()/3))
	require.NotEmpty(t, rots)
	require.Less(t, len(rots), 40)

	last := rots[len(rots)-1]
	assert.Equal(t, "r39", last.ID)
	assert.False(t, last.Open())

	for _, r := range rots {
		assert.NotEmpty(t, r.Why, "a rotation whose first line was cut off is not in it: %s", r.ID)
	}
}

// A line the role's own command appends without the lock and one the shift appends under it do not tear each other: every
// line of every writer is there whole.
func TestRotationAppendsFromManyWritersInterleaveSafely(t *testing.T) {
	path := filepath.Join(t.TempDir(), "designer.jsonl")
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	require.NoError(t, appendRotationLine(path, rotationLine{ID: "r1", At: at, Phase: PhasePrepared, Role: "designer"}))

	var wg sync.WaitGroup

	const writers, each = 8, 40

	for w := range writers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range each {
				l := rotationLine{ID: "r1", At: at.Add(time.Second), Phase: phaseEvidence, What: EvidenceNoteFetched,
					Task: fmt.Sprintf("T-%d-%d", w, i), By: "designer", Session: strings.Repeat("s", 300)}
				assert.NoError(t, appendRotationLine(path, l))
			}
		}()
	}

	wg.Wait()

	rots := foldRotations(readRotationLines(path, 1<<20))
	require.Len(t, rots, 1)
	assert.Len(t, rots[0].Evidence, writers*each)
}

// RecordRotationEvidence records only what an open rotation waits for, of the fresh session, once.
func TestRecordRotationEvidenceTakesOnlyWhatARotationWaitsFor(t *testing.T) {
	f, _ := restartBegun(t, 0)
	layout := f.s.Crew.Layout()
	r := lastRotation(t, f, "designer")
	now := *f.now

	record := func(what, task, session string) {
		t.Helper()
		require.NoError(t, RecordRotationEvidence(layout, "designer", what, task, "designer", session, now))
	}

	count := func() int { return len(lastRotation(t, f, "designer").Evidence) }

	// The role is going down: what the old session still does is its own, and one that does not say whose it is waits for the
	// fresh session.
	record(EvidenceNoteFetched, "T-1", "sid-designer")
	record(EvidenceHookStart, "", "sid-designer")
	record(EvidenceNoteFetched, "T-1", "")
	assert.Zero(t, count())

	// The fresh session's SessionStart hook can run before the start is written: it is taken, by its session.
	record(EvidenceHookStart, "", "sid-new")
	assert.Equal(t, 1, count())

	// Once; and not a task the rotation did not hand over.
	record(EvidenceHookStart, "", "sid-new")
	record(EvidenceNoteFetched, "T-9", "sid-new")
	record(EvidenceNoteFetched, "T-1", "sid-new")
	assert.Equal(t, 2, count())

	f.finish(t)
	record(EvidenceAccepted, "T-1", "")
	assert.Equal(t, 3, count(), "a caller that does not say its session, once the old one is gone")

	// A role with no record, or one that is no file name, is no error.
	require.NoError(t, RecordRotationEvidence(layout, "harvester", EvidenceHookStart, "", "harvester", "sid-x", now))
	require.NoError(t, RecordRotationEvidence(layout, "../x", EvidenceHookStart, "", "x", "sid-x", now))
	assert.NoFileExists(t, layout.Rotations("harvester"))

	// A closed rotation takes nothing, unless it was lost and the evidence is an accept of a task it handed.
	require.True(t, f.s.rotationEnd("designer", r.ID, OutcomeRebuilt, "", []string{"boot"}, false))
	record(EvidenceNoteFetched, "T-1", "sid-new")
	record(EvidenceAccepted, "T-1", "sid-new")
	assert.Empty(t, lastRotation(t, f, "designer").Late)
}

// A copy of a role has its record under its own name, as its delivery cards do, and never shares one with a role whose name
// reads like the file's: each is listed under its own name, whichever file a line is in.
func TestRotationRecordOfACopyIsItsOwn(t *testing.T) {
	f := restarting(t, "", 100, time.Minute)
	layout := f.s.Crew.Layout()
	assert.Equal(t, filepath.Join(layout.RotationsDir(), "designer#2.jsonl"), layout.Rotations("designer#2"))
	assert.NotEqual(t, layout.Rotations("designer-2"), layout.Rotations("designer#2"))

	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	require.NoError(t, appendRotationLine(layout.Rotations("designer#2"), rotationLine{ID: "r2", At: at, Phase: PhasePrepared,
		Role: "designer#2"}))
	require.NoError(t, appendRotationLine(layout.Rotations("designer-2"), rotationLine{ID: "r3", At: at, Phase: PhasePrepared,
		Role: "designer-2"}))

	assert.Equal(t, "r2", lastRotation(t, f, "designer#2").ID)
	assert.Equal(t, "r3", lastRotation(t, f, "designer-2").ID)
	assert.Len(t, rotationsOn(t, f, ""), 2)

	// Every reader filters by the role a rotation names: a line of another role's in a role's file is not its rotation.
	require.NoError(t, appendRotationLine(layout.Rotations("designer#2"), rotationLine{ID: "r4", At: at.Add(time.Minute),
		Phase: PhasePrepared, Role: "designer-2"}))
	assert.Len(t, rotationsOn(t, f, "designer#2"), 1)
	assert.Len(t, f.s.rotationsOf("designer#2"), 1)
}

// PruneRotations removes the files nothing was written to since the cutoff, whole; a file past what a look reads is set
// aside once its newest rotation is closed, and left while one is open.
func TestPruneRotations(t *testing.T) {
	f := restarting(t, "", 100, time.Minute)
	layout := f.s.Crew.Layout()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	write := func(role string, ids int, open bool, mtime time.Time) string {
		path := layout.Rotations(role)

		for i := range ids {
			id := fmt.Sprintf("%s-%d", role, i)
			require.NoError(t, appendRotationLine(path, rotationLine{ID: id, At: now, Phase: PhasePrepared, Role: role}))

			if open && i == ids-1 {
				continue
			}

			require.NoError(t, appendRotationLine(path, rotationLine{ID: id, At: now, Phase: PhaseClosed, Outcome: OutcomeResumed}))
		}

		require.NoError(t, os.Chtimes(path, mtime, mtime))

		return path
	}

	old := write("old", 3, false, now.AddDate(0, 0, -40))
	oldAside := old + ".old"
	require.NoError(t, os.WriteFile(oldAside, []byte("x\n"), 0o600))
	require.NoError(t, os.Chtimes(oldAside, now.AddDate(0, 0, -40), now.AddDate(0, 0, -40)))
	fresh := write("fresh", 2, false, now.AddDate(0, 0, -1))

	// Past what a look reads: enough rotations with long reasons, the last of them closed or open.
	grow := func(role string, open bool) string {
		path := layout.Rotations(role)

		for i := 0; ; i++ {
			id := fmt.Sprintf("%s-%d", role, i)
			require.NoError(t, appendRotationLine(path, rotationLine{ID: id, At: now, Phase: PhasePrepared, Role: role,
				Why: strings.Repeat("w", 2000)}))

			st, err := os.Stat(path)
			require.NoError(t, err)

			if open && st.Size() > rotationReadMax {
				break
			}

			require.NoError(t, appendRotationLine(path, rotationLine{ID: id, At: now, Phase: PhaseClosed, Outcome: OutcomeResumed}))

			if st.Size() > rotationReadMax {
				break
			}
		}

		require.NoError(t, os.Chtimes(path, now.AddDate(0, 0, -1), now.AddDate(0, 0, -1)))

		return path
	}

	big := grow("big", false)
	bigOpen := grow("bigopen", true)
	bigRotations := len(foldRotations(readRotationLines(big, rotationReadMax)))
	require.Positive(t, bigRotations)

	// The set-aside file of the big one that an earlier pass left, kept (younger than the cutoff): this pass replaces it.
	require.NoError(t, os.WriteFile(big+".old", []byte(`{"id":"e1","at":"2026-10-23T12:00:00Z","phase":"prepared","role":"big"}`+"\n"), 0o600))
	require.NoError(t, os.Chtimes(big+".old", now.AddDate(0, 0, -2), now.AddDate(0, 0, -2)))

	cutoff := now.AddDate(0, 0, -30)
	want := []RotationPruned{
		{File: "big.jsonl", Rotations: bigRotations, HadAside: true, Replaces: 1},
		{File: "old.jsonl", Rotations: 3, Removed: true},
		{File: "old.jsonl.old", Rotations: 1, Removed: true},
	}

	pruned, err := PruneRotations(layout, cutoff, true)
	require.NoError(t, err)
	assert.Equal(t, want, pruned, "dry: what would go, and what the set aside file replaces")
	assert.FileExists(t, old)
	assert.FileExists(t, big)
	assert.Equal(t, 1, len(foldRotations(readRotationLines(big+".old", 1<<20))), "dry: the earlier set aside file stays")

	pruned, err = PruneRotations(layout, cutoff, false)
	require.NoError(t, err)
	assert.Equal(t, want, pruned)
	assert.NoFileExists(t, old)
	assert.NoFileExists(t, oldAside)
	assert.FileExists(t, fresh)
	assert.NoFileExists(t, big, "set aside")
	assert.Equal(t, bigRotations, len(foldRotations(readRotationLines(big+".old", rotationReadMax))), "the new one replaced the old one set aside")
	assert.FileExists(t, bigOpen, "its newest rotation is open: left where it is")
	assert.NoFileExists(t, bigOpen+".old")

	pruned, err = PruneRotations(layout, cutoff, false)
	require.NoError(t, err)
	assert.Empty(t, pruned, "nothing more")

	_, err = PruneRotations(crew.Layout{Dir: t.TempDir()}, now, false)
	require.NoError(t, err, "no directory: nothing to do")
}

// The beat in which a fresh role's boot_grace ends is the one in which its restart line is typed: a rotation is not lost for a
// boot turn that has not finished before the role was given anything to boot on, nor in the few beats after the line was.
func TestRotationIsNotLostBeforeTheRoleWasGivenItsContext(t *testing.T) {
	f, _ := restartBegun(t, 0)
	freshSession(t, f, false)

	typed := len(f.tm.typed["designer"])
	f.idleSince(*f.now)
	f.s.Tick()

	r := lastRotation(t, f, "designer")
	require.True(t, r.Open(), "closed at the very beat that types its first line: %+v", r.Closed)
	assert.Zero(t, rotationNotes(f), "%v", f.s.State.Notes)

	line := f.tm.typed["designer"][typed:]
	require.NotEmpty(t, line, "the restart line is typed at this beat")
	assert.True(t, strings.HasPrefix(line[0], restartLineHead), line[0])

	// The beats after it, each a minute, inside the margin the line and the turn it starts are given.
	for range 3 {
		*f.now = f.now.Add(time.Minute)
		f.idleSince(*f.now)
		f.s.Tick()
	}

	assert.True(t, lastRotation(t, f, "designer").Open(), "inside the margin")
	assert.Zero(t, rotationNotes(f))
}

// A role whose restart line cannot be typed (a draft in its input line) is not blamed for a boot turn it was never given.
func TestRotationOfARoleWhoseLineIsHeldBackStaysOpen(t *testing.T) {
	f, _ := restartBegun(t, 0)
	freshSession(t, f, false)
	f.tm.drafts["designer"] = "half a sentence"

	for range 16 {
		f.idleSince(*f.now)
		f.s.Tick()
		*f.now = f.now.Add(time.Minute)
	}

	r := lastRotation(t, f, "designer")
	require.True(t, r.Open(), "its line waits in the queue: %+v", r.Closed)
	assert.Contains(t, f.s.State.Role("designer").Inbox[0], restartLineHead)
	assert.Equal(t, 1, rotationNotes(f), "told once that it stays open, not that it was lost: %v", f.s.State.Notes)
	assert.NotContains(t, strings.Join(f.s.State.Notes, "|"), "lost")
}

// A role that is booted and idle, holding tasks, whose restart line left its queue and was never seen is lost once it has sat idle
// for the wait: its context never reached it. While the line is still to be delivered the rotation stays open.
func TestRotationLostWhenItsContextWasNeverShown(t *testing.T) {
	f, _ := restartBegun(t, 0)
	freshSession(t, f, true)

	// The restart line leaves the queue and no card says it was seen (a bell it never fetched, a line given up).
	rs := f.s.State.Role("designer")
	rs.Inbox = slices.DeleteFunc(rs.Inbox, func(l string) bool { return strings.HasPrefix(l, restartLineHead) })

	for range 2 {
		*f.now = f.now.Add(rotationAcceptWait + time.Minute)
		f.idleSince(f.now.Add(-rotationAcceptWait - 30*time.Second))
		f.s.Tick()

		r := lastRotation(t, f, "designer")
		require.False(t, r.Open(), "%+v", r)
		assert.Equal(t, OutcomeLost, r.Closed.Outcome)
		assert.Equal(t, "its restart line was never shown to it", r.Closed.Detail)
		assert.Equal(t, 1, rotationNotes(f), "told once: %v", f.s.State.Notes)
	}
}

// The same role with its line still waiting in the queue is not lost, however long it sits idle.
func TestRotationOfAnIdleRoleWhoseLineWaitsIsNotLostForNotShowingIt(t *testing.T) {
	f, _ := restartBegun(t, 0)
	freshSession(t, f, true)
	f.tm.drafts["designer"] = "half a sentence"

	*f.now = f.now.Add(rotationAcceptWait + 5*time.Minute)
	f.idleSince(f.now.Add(-rotationAcceptWait - 4*time.Minute))
	f.s.Tick()

	r := lastRotation(t, f, "designer")
	require.True(t, r.Open(), "%+v", r.Closed)
	assert.Contains(t, f.s.State.Role("designer").Inbox[0], restartLineHead)
}

// A role that runs its old conversation again (a command started it on that after the restart died) has no fresh
// conversation to follow: the rotation closes resumed, once, and tells nobody; nothing the old conversation does is evidence.
func TestRotationOfARoleBackOnItsOldConversationIsResumed(t *testing.T) {
	f, _ := restartBegun(t, 0)
	f.s.Restarts = nil // the beat that began it dies

	rs := f.s.State.Role("designer")
	rs.RestartGaveUp = "sid-designer"
	// The owner ends the stale down and starts the role on its old conversation.
	rs.Downing, rs.Pending = time.Time{}, nil
	require.NoError(t, f.s.UpWith("designer", UpOptions{Resume: "sid-designer"}))

	*f.now = f.now.Add(bootPause)
	f.rg.byPID[f.tm.up["designer"]] = &registry.Session{Status: "idle", StatusUpdatedAt: f.now.UnixMilli(), SessionID: "sid-designer"}
	f.rg.endedAt["sid-designer"] = f.now.Add(-time.Second)

	for range 2 {
		f.s.Tick()

		r := lastRotation(t, f, "designer")
		require.False(t, r.Open(), "%+v", r)
		assert.Equal(t, OutcomeResumed, r.Closed.Outcome)
		assert.Contains(t, r.Closed.Detail, "old conversation")
		assert.Equal(t, 1, phaseLines(f, "designer", r.ID, PhaseClosed))
		assert.Zero(t, rotationNotes(f), "resumed is silent: %v", f.s.State.Notes)
		assert.Equal(t, 1, countEvents(f, "designer", "rotation"))
	}

	// What the old conversation does is not the fresh one's: no evidence, not even a late accept.
	r := lastRotation(t, f, "designer")
	require.NoError(t, RecordRotationEvidence(f.s.Crew.Layout(), "designer", EvidenceAccepted, "T-1", "designer", "sid-designer", *f.now))
	assert.Empty(t, lastRotation(t, f, "designer").Late)
	assert.Empty(t, r.Evidence)
}

// A record that can be read and not written holds nothing: the rotation could neither close nor tell, so the acceptance alert is
// told as it always was, 5 min after the packet was sent.
func TestRotationThatCannotBeWrittenLeavesTheAcceptanceAlertToItself(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a file mode does not stop root")
	}

	f, _ := restartBegun(t, 0)
	freshSession(t, f, true)
	f.typedAfter("designer", "sid-new", 2)

	r := lastRotation(t, f, "designer")
	f.s.cardSeen("designer", r.Lines[0], SeenTranscript, *f.now)

	path := f.s.Crew.Layout().Rotations("designer")
	require.NoError(t, os.Chmod(path, 0o400))
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	sent := deliveryOf(t, f).SentAt
	*f.now = sent.Add(6 * time.Minute)
	f.idleSince(sent)
	f.s.Tick()

	assert.Equal(t, 1, countNotes(f, "designer: T-1 has no acceptance after 5 min"), "told as on any crew: %v", f.s.State.Notes)
}

// An open rotation holds the alert only for as long as it can be closing: past its own longest wait and the time the dispatcher
// is told it stays open, it owns nothing.
func TestRotationHoldsTheAcceptanceAlertOnlyForItsOwnLongestWait(t *testing.T) {
	f, _ := restartBegun(t, 0)
	freshSession(t, f, true)
	f.typedAfter("designer", "sid-new", 2)

	r := lastRotation(t, f, "designer")
	require.True(t, r.Open())

	tk, err := f.tk.Get("T-1")
	require.NoError(t, err)

	*f.now = r.Started.Add(rotationHoldMax)
	assert.True(t, f.s.rotationOwns(tk), "within it")

	*f.now = r.Started.Add(rotationHoldMax + time.Second)
	f.s.rotationBook = nil
	assert.False(t, f.s.rotationOwns(tk), "past it")
}

// A failure of the record is told once a day for a role, whichever process finds it: the ticker is a new one every beat.
func TestRotationFailureIsToldOncePerRoleADay(t *testing.T) {
	f := restarting(t, "", 100, time.Minute)
	path := f.s.Crew.Layout().Rotations("designer")
	cause := fmt.Errorf("disk full")

	beat := func() {
		rotationWarned.Clear() // another process
		f.s.rotationWarn("designer", path, "not written", cause)
	}

	for range 3 {
		beat()
		*f.now = f.now.Add(time.Minute)
	}

	assert.Equal(t, 1, countEvents(f, "designer", "rotation-failed"), "once, not at every beat")

	// Another role's failure is its own; the same role's again after a day.
	rotationWarned.Clear()
	f.s.rotationWarn("harvester", f.s.Crew.Layout().Rotations("harvester"), "not written", cause)
	assert.Equal(t, 1, countEvents(f, "harvester", "rotation-failed"))

	*f.now = f.now.Add(25 * time.Hour)
	beat()
	assert.Equal(t, 2, countEvents(f, "designer", "rotation-failed"), "a day later")
}

// An open rotation of a role that is not in the crew any more has no session to follow: it is closed lost, with no way back,
// no note, and no entry made for the role in the state.
func TestRotationOfARoleGoneFromTheCrewIsClosed(t *testing.T) {
	f := restarting(t, "", 100, time.Minute)
	path := f.s.Crew.Layout().Rotations("ghost")
	at := *f.now

	for _, l := range []rotationLine{
		{ID: "g1", At: at, Phase: PhasePrepared, Role: "ghost", Kind: KindRestartAt, Why: "context 300k over restart_at 250k",
			From: &RotationFrom{Session: "sid-ghost"}},
		{ID: "g1", At: at, Phase: PhaseSwitching, Claim: at},
		{ID: "g1", At: at, Phase: PhaseStarted},
	} {
		require.NoError(t, appendRotationLine(path, l))
	}

	for range 2 {
		f.s.Tick()

		r := lastRotation(t, f, "ghost")
		require.False(t, r.Open(), "%+v", r)
		assert.Equal(t, OutcomeLost, r.Closed.Outcome)
		assert.Equal(t, "the role is gone from the crew", r.Closed.Detail)
		assert.Empty(t, r.Closed.Back)
		assert.Equal(t, 1, phaseLines(f, "ghost", "g1", PhaseClosed))
		assert.Zero(t, rotationNotes(f))
		assert.NotContains(t, f.s.State.Roles, "ghost")
	}
}

// The beat reads the end of a record, as much of it as shows the newest rotation whole, and a record only once in a beat.
func TestRotationBeatReadsLittleOfARecordAndOnlyOnce(t *testing.T) {
	f := restarting(t, "", 100, time.Minute)
	path := f.s.Crew.Layout().Rotations("designer")
	at := *f.now

	// A long history of closed rotations, then an open one with more evidence than a first read holds.
	for i := range 3000 {
		id := fmt.Sprintf("h%04d", i)
		require.NoError(t, appendRotationLine(path, rotationLine{ID: id, At: at, Phase: PhasePrepared, Role: "designer", Why: strings.Repeat("w", 200)}))
		require.NoError(t, appendRotationLine(path, rotationLine{ID: id, At: at, Phase: PhaseClosed, Outcome: OutcomeResumed}))
	}

	st, err := os.Stat(path)
	require.NoError(t, err)
	require.Greater(t, st.Size(), int64(rotationBeatRead)*3)

	closed := readRotationTail(path)
	assert.Less(t, len(closed), 60, "a few KiB of a record whose newest rotation is closed, not all of it")
	assert.Equal(t, "h2999", foldRotations(closed)[len(foldRotations(closed))-1].ID)

	require.NoError(t, appendRotationLine(path, rotationLine{ID: "open", At: at, Phase: PhasePrepared, Role: "designer"}))

	for i := range 60 {
		require.NoError(t, appendRotationLine(path, rotationLine{ID: "open", At: at, Phase: phaseEvidence, What: EvidenceNoteFetched,
			Task: fmt.Sprintf("T-%d", i), Session: strings.Repeat("s", 200)}))
	}

	rots := foldRotations(readRotationTail(path))
	last := rots[len(rots)-1]
	assert.Equal(t, "open", last.ID, "its prepared line is in what was read, though it is further back than a first read goes")
	assert.True(t, last.Open())

	// Once in a beat: what the beat read stays until the beat ends or the shift appends.
	first := f.s.rotationsOf("designer")
	require.NoError(t, appendRotationLine(path, rotationLine{ID: "late", At: at, Phase: PhasePrepared, Role: "designer"}))
	assert.Equal(t, len(first), len(f.s.rotationsOf("designer")), "not read again in the same beat")

	f.s.Tick()
	assert.True(t, slices.ContainsFunc(f.s.rotationsOf("designer"), func(r Rotation) bool { return r.ID == "late" }), "the next beat reads it")
}

// A restart of a pane that was dead when it began has a down with no claim: the restart finds its rotation all the same, and
// writes its start.
func TestRotationOfADeadPaneIsFoundByItsRestart(t *testing.T) {
	f := restarting(t, "", 300_000, 10*time.Minute)
	f.note(t, "T-1", f.now.Add(-10*time.Minute))

	// The pane died between the gate that chose the restart and the down.
	f.tm.die("designer")
	f.s.beginRestart("designer", f.s.heldTasks("designer"), "context 300k over restart_at 250k", "")
	require.Len(t, f.s.Restarts, 1)

	r := lastRotation(t, f, "designer")
	require.Equal(t, PhaseSwitching, r.Phase)
	require.True(t, r.Claim.IsZero(), "a dead pane is no down: nothing to claim")

	f.finish(t)

	r = lastRotation(t, f, "designer")
	assert.Equal(t, PhaseStarted, r.Phase, "the restart that started it wrote that")
	assert.Len(t, r.Lines, 2)
	assert.Len(t, rotationsOn(t, f, "designer"), 1)
}

// A beat that died after it put the role down for a newer rotation and before it closed the older one leaves both open: the next
// beat closes the older one, superseded, with no way back and no note.
func TestRotationSupersededByTheBeatWhenTheNewerOnesBeatDied(t *testing.T) {
	f := restarting(t, "", 100, time.Minute)
	path := f.s.Crew.Layout().Rotations("designer")
	at := *f.now

	for _, l := range []rotationLine{
		{ID: "old1", At: at, Phase: PhasePrepared, Role: "designer", Kind: KindRestartAt, Why: "context 300k over restart_at 250k",
			From: &RotationFrom{Session: "sid-a"}},
		{ID: "old1", At: at, Phase: PhaseSwitching, Claim: at},
		{ID: "old1", At: at, Phase: PhaseStarted},
		{ID: "new2", At: at.Add(time.Hour), Phase: PhasePrepared, Role: "designer", Kind: KindRestartAt, Why: "context 310k over restart_at 250k",
			From: &RotationFrom{Session: "sid-b"}},
		{ID: "new2", At: at.Add(time.Hour), Phase: PhaseSwitching, Claim: at.Add(time.Hour)},
	} {
		require.NoError(t, appendRotationLine(path, l))
	}

	f.s.Tick()

	rots := rotationsOn(t, f, "designer")
	require.Len(t, rots, 2)
	require.False(t, rots[0].Open())
	assert.Equal(t, OutcomeLost, rots[0].Closed.Outcome)
	assert.Equal(t, "superseded by rotation new2", rots[0].Closed.Detail)
	assert.Empty(t, rots[0].Closed.Back)
	assert.Zero(t, rotationNotes(f), "%v", f.s.State.Notes)
}
