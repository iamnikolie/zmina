package shift

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/ledger"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/task"
)

// heldAsHandOut is acceptedByOld with the generation the old conversation's hand-out had.
func heldAsHandOut(t *testing.T, f fix, gen int) task.Delivery {
	t.Helper()

	d := acceptedByOld(t, f)

	tk, err := f.tk.Get("T-1")
	require.NoError(t, err)

	tk.Delivery.Gen, d.Gen = gen, gen
	require.NoError(t, f.tk.Put(tk))

	return d
}

// who is the standing of a session reporting as the designer on T-1, as the commands that report ask it.
func (f fix) who(t *testing.T, session string) ledger.Standing {
	t.Helper()

	tk, err := f.tk.Get("T-1")
	require.NoError(t, err)

	l := &ledger.Ledger{Dispatcher: "dispatcher"}

	return l.Current(tk, ledger.Caller{By: "designer", Session: session, Live: f.s.LiveSession})
}

// A task handed to a role the first time is hand-out 1; the same packet handed again is the same hand-out, and a packet
// that changed, a role started since, or another role is the next one.
func TestHandTaskNumbersTheHandOuts(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	*f.now = f.now.Add(bootPause)
	f.status("designer", "idle", *f.now)
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open, Updated: f.now.UTC()}}

	gen := func() int { return deliveryOf(t, f).Gen }

	_, err := f.s.HandTask("designer", f.tk.list[0])
	require.NoError(t, err)
	assert.Equal(t, 1, gen(), "the first hand-out")

	_, err = f.s.HandTask("designer", f.tk.list[0])
	require.NoError(t, err)
	assert.Equal(t, 1, gen(), "the same packet to the same session is the same hand-out")

	// The session the line was typed to holds it.
	require.NotEmpty(t, f.s.FlushInbox("designer"))
	require.Equal(t, "sid-designer", deliveryOf(t, f).Session)
	assert.False(t, f.who(t, "sid-designer").Late)
	assert.True(t, f.who(t, "sid-other").Late)

	// The task changed: another packet, the next hand-out; the session that holds it is the same one.
	*f.now = f.now.Add(time.Minute)
	tk := f.tk.list[0]
	tk.Note, tk.Updated = "a new note", f.now.UTC()
	f.tk.list[0] = tk

	_, err = f.s.HandTask("designer", tk)
	require.NoError(t, err)
	assert.Equal(t, 2, gen(), "a packet that changed")
	assert.False(t, f.who(t, "sid-designer").Late, "the live session still holds it before the line is typed again")
	assert.True(t, f.who(t, "sid-other").Late)

	// The role was started since the delivery: the one it was handed to is gone.
	require.NoError(t, f.s.Down("designer", DownOptions{}))
	*f.now = f.now.Add(time.Minute)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	f.rg.byPID[f.tm.up["designer"]].SessionID = "sid-after"

	_, err = f.s.HandTask("designer", tk)
	require.NoError(t, err)
	assert.Equal(t, 3, gen(), "a role started since")
	assert.True(t, f.who(t, "sid-designer").Late, "the session that was is not the live one")
	assert.False(t, f.who(t, "sid-after").Late)

	// Another role: the next hand-out, for it.
	tk = f.tk.list[0]
	tk.Role = "harvester"
	f.tk.list[0] = tk

	_, err = f.s.HandTask("harvester", tk)
	require.NoError(t, err)
	assert.Equal(t, 4, gen(), "another role")
	assert.Equal(t, "harvester", deliveryOf(t, f).Role)
}

// A task handed out before hand-outs were counted is hand-out 1 when it is handed next: the number never goes back to 0.
func TestHandOutOfAnOldDeliveryStartsTheCount(t *testing.T) {
	f := settled(t)
	acceptedByOld(t, f)
	require.Zero(t, deliveryOf(t, f).Gen)

	_, err := f.restartByHand(t, UpOptions{Fresh: true})
	require.NoError(t, err)
	assert.Equal(t, 1, deliveryOf(t, f).Gen)
}

// A restart of the ticker hands out the next number to the session it starts; one whose fresh start fails puts the delivery
// back as it was, number and session: the old conversation holds the task again.
func TestTickerRestartHandsOutTheNextNumberAndRollbackPutsItBack(t *testing.T) {
	for _, fail := range []int{0, 1} {
		f, _ := restartBegun(t, fail)
		old := heldAsHandOut(t, f, 2)

		f.finish(t)

		if fail == 1 {
			assert.Equal(t, old, *deliveryOf(t, f), "back on its conversation: its delivery as it was")
			assert.False(t, f.who(t, "sid-designer").Late)
			assert.True(t, f.who(t, "sid-new").Late)

			continue
		}

		d := deliveryOf(t, f)
		assert.Equal(t, 3, d.Gen, "the fresh session's hand-out")
		assert.Empty(t, d.Session, "not typed to it yet")

		f.newSession("designer", "sid-new")
		assert.True(t, f.who(t, "sid-designer").Late, "the conversation it was taken from")
		assert.False(t, f.who(t, "sid-new").Late)

		// Typed to the fresh session, it is that session's by name.
		require.Len(t, f.typedAfter("designer", "sid-new", 2), 2)
		assert.Equal(t, "sid-new", deliveryOf(t, f).Session)
		assert.Equal(t, 3, deliveryOf(t, f).Gen, "typing it is no new hand-out")
		assert.True(t, f.who(t, "sid-designer").Late)
		assert.False(t, f.who(t, "sid-new").Late)
	}
}

// zmina restart --fresh, zmina up of a role holding tasks and the reopen of a keep: always role hand the task to the fresh
// session as the ticker's restart does: the next number, and the session it was taken from is late.
func TestRotationPathsHandOutTheNextNumber(t *testing.T) {
	t.Run("restart by hand", func(t *testing.T) {
		f := settled(t)
		heldAsHandOut(t, f, 4)

		_, err := f.restartByHand(t, UpOptions{Fresh: true})
		require.NoError(t, err)
		assert.Equal(t, 5, deliveryOf(t, f).Gen)

		bootedFresh(f)
		assert.True(t, f.who(t, "sid-designer").Late)
		assert.False(t, f.who(t, "sid-new").Late)
	})

	t.Run("up of a role holding a task", func(t *testing.T) {
		f := restarting(t, "", 100_000, 10*time.Minute)
		f.s.Tick()
		require.NoError(t, f.s.Down("designer", DownOptions{}))
		heldAsHandOut(t, f, 4)

		require.NoError(t, f.s.UpHanding("designer", UpOptions{}))
		assert.Equal(t, 5, deliveryOf(t, f).Gen)

		bootedFresh(f)
		assert.True(t, f.who(t, "sid-designer").Late)
		assert.False(t, f.who(t, "sid-new").Late)
	})

	t.Run("crash reopen", func(t *testing.T) {
		f := crashed(t, "")
		heldAsHandOut(t, f, 4)

		f.s.Tick()
		assert.Equal(t, 5, deliveryOf(t, f).Gen)

		f.newSession("designer", "sid-new")
		assert.True(t, f.who(t, "sid-designer").Late)
		assert.False(t, f.who(t, "sid-new").Late)
	})
}

// zmina rotation retry hands the packet to the fresh session again: a hand-out of its own (the next number), whose session is
// the same one, so that the session that was taken from stays late and the fresh one stays current.
func TestRetryHandsOutAgainToTheSameFreshSession(t *testing.T) {
	f := settled(t)
	heldAsHandOut(t, f, 2)

	_, err := f.restartByHand(t, UpOptions{Fresh: true})
	require.NoError(t, err)
	require.Equal(t, 3, deliveryOf(t, f).Gen)

	bootedFresh(f)
	f.s.State.Role("designer").Inbox = nil // typed, landed, and not accepted

	require.Contains(t, retried(t, f), "T-1: its packet is queued again")
	assert.Equal(t, 4, deliveryOf(t, f).Gen)
	assert.True(t, f.who(t, "sid-designer").Late)
	assert.False(t, f.who(t, "sid-new").Late, "the same fresh session, before and after the retry")
}

// An abort puts the delivery back as the hand-out found it: the conversation the role is back on holds the task again, as
// hand-out 2, and the fresh session that held 3 does not.
func TestAbortPutsTheOldSessionBackAsTheOneThatHoldsTheTask(t *testing.T) {
	f := settled(t)
	old := heldAsHandOut(t, f, 2)

	_, err := f.restartByHand(t, UpOptions{Fresh: true})
	require.NoError(t, err)
	require.Equal(t, 3, deliveryOf(t, f).Gen)

	bootedFresh(f)
	require.Len(t, f.typedAfter("designer", "sid-new", 2), 2)
	require.Equal(t, "sid-new", deliveryOf(t, f).Session)
	assert.False(t, f.who(t, "sid-new").Late, "while the fresh session holds it")
	assert.True(t, f.who(t, "sid-designer").Late)

	require.NoError(t, abort(t, f))
	f.newSession("designer", "sid-designer")

	assert.Equal(t, old, *deliveryOf(t, f))
	assert.False(t, f.who(t, "sid-designer").Late, "the old conversation holds it again")
	assert.True(t, f.who(t, "sid-new").Late, "and the fresh one does not")
	assert.Equal(t, 2, f.who(t, "sid-new").Gen)

	// The number the aborted hand-out had is not given to the next session's.
	tk, err := f.tk.Get("T-1")
	require.NoError(t, err)
	tk.Result = &record.Ref{Revision: "ab", Gen: 3}
	assert.Equal(t, 4, tk.NextGen())
}

// The packet is what a session is handed, and the late reports of the task are no part of it: a task with some has the
// packet it had, so that it is not handed out again for them.
func TestPacketIsTheSameWhateverLateReportsTheTaskHas(t *testing.T) {
	f := settled(t)

	tk, err := f.tk.Get("T-1")
	require.NoError(t, err)

	_, id, _, err := f.s.packetFile("designer", tk)
	require.NoError(t, err)

	tk.Late = []task.Late{{What: "result", Revision: "ab", By: "designer", Session: "sid-x", Gen: 1}}

	_, again, _, err := f.s.packetFile("designer", tk)
	require.NoError(t, err)
	assert.Equal(t, id, again)
}
