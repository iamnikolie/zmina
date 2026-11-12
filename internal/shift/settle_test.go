package shift

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/peer"
)

// A pass that fails counts as one try; passes seconds apart (a command's delivery, then the beat) are the same try.
// Past retryGap the line is tried and counted again, and typeTries counted tries move it to the failed list.
func TestTypedTriesCountedAtLeastRetryGapApart(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	require.NoError(t, f.s.Queue("designer", "bad"))
	*f.now = f.now.Add(bootPause)
	f.status("designer", "idle", *f.now)
	f.tm.typeErr = errors.New("tmux refused")

	for range typeTries {
		assert.Empty(t, f.s.deliverInbox("designer", *f.now))
		*f.now = f.now.Add(time.Second)
	}

	rs := f.s.State.Role("designer")
	assert.Equal(t, []string{"bad"}, rs.Inbox, "three failing passes inside 3 s: still queued")
	assert.Equal(t, 1, rs.Attempts, "and one try counted")
	assert.Empty(t, rs.Failed)
	assert.Equal(t, 1, f.eventCount("type-failed"), "the line was tried once")

	*f.now = f.now.Add(retryGap)
	assert.Empty(t, f.s.deliverInbox("designer", *f.now))
	assert.Equal(t, 2, rs.Attempts, "a pass retryGap after the last try counts")

	*f.now = f.now.Add(retryGap)
	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	assert.Empty(t, rs.Inbox)
	require.Len(t, rs.Failed, 1)
	assert.Equal(t, "bad", rs.Failed[0].Line)
	assert.Zero(t, rs.Attempts)
	assert.True(t, rs.TriedAt.IsZero(), "a line that failed for good leaves no stamp")
}

// A line that goes after a failed try clears the try.
func TestTypedTryIsForgottenOnceTheLineGoes(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	require.NoError(t, f.s.Queue("designer", "once"))
	*f.now = f.now.Add(bootPause)
	f.status("designer", "idle", *f.now)

	f.tm.typeErr = errors.New("tmux refused")
	assert.Empty(t, f.s.deliverInbox("designer", *f.now))
	require.Equal(t, 1, f.s.State.Role("designer").Attempts)

	f.tm.typeErr = nil
	*f.now = f.now.Add(retryGap)
	assert.Equal(t, "typed into designer: once", f.s.deliverInbox("designer", *f.now))

	rs := f.s.State.Role("designer")
	assert.Zero(t, rs.Attempts)
	assert.True(t, rs.TriedAt.IsZero())
}

// A post that never started is held to the same gap.
func TestPostTriesCountedAtLeastRetryGapApart(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	*f.now = f.now.Add(3 * time.Minute)
	require.NoError(t, f.s.Queue("designer", "[dispatcher] one"))

	p.err = fmt.Errorf("%w: dial: no such file", peer.ErrUnavailable)

	for range typeTries {
		assert.Empty(t, f.s.deliverInbox("designer", *f.now))
		*f.now = f.now.Add(time.Second)
	}

	rs := f.s.State.Role("designer")
	assert.Equal(t, []string{"[dispatcher] one"}, rs.Inbox, "three failing passes inside 3 s: still queued")
	assert.Equal(t, 1, rs.Attempts)
	assert.Empty(t, rs.Failed)
	assert.Equal(t, 1, f.eventCount("post-failed"))

	p.err = nil
	assert.Empty(t, f.s.deliverInbox("designer", *f.now), "not before retryGap, though the post would now work")
	assert.Empty(t, p.raw)

	*f.now = f.now.Add(retryGap)
	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	assert.Len(t, p.raw, 1)
	assert.Zero(t, rs.Attempts)
}

// A slash command is typed and leaves no Sent record: the line after it waits slashSettle, then goes.
func TestSlashCommandSettlesBeforeTheNextLine(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	require.NoError(t, f.s.Queue("designer", "/compact"))
	require.NoError(t, f.s.Queue("designer", "next line"))
	*f.now = f.now.Add(bootPause)
	f.status("designer", "idle", *f.now)

	assert.Equal(t, "typed into designer: /compact", f.s.deliverInbox("designer", *f.now))
	require.Nil(t, f.s.State.Role("designer").Sent, "a slash command leaves no record to wait for")
	assert.Equal(t, *f.now, f.s.State.Role("designer").SlashAt)

	*f.now = f.now.Add(time.Second)
	assert.Empty(t, f.s.deliverInbox("designer", *f.now), "a pass 1 s after the command delivers nothing")
	assert.Equal(t, []string{"/compact"}, f.tm.typed["designer"])

	*f.now = f.now.Add(slashSettle)
	assert.Equal(t, "typed into designer: next line", f.s.deliverInbox("designer", *f.now))
	assert.Equal(t, []string{"/compact", "next line"}, f.tm.typed["designer"])
}

// The same holds for a role its lines are posted to, and for the one-line-per-pass of the beat at its own cadence:
// a minute after the command nothing waits any more.
func TestSlashCommandSettleHoldsPostsAndLeavesTheBeatAlone(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	require.NoError(t, f.s.Queue("designer", "/rename new-name"))
	require.NoError(t, f.s.Queue("designer", "[dispatcher] after the rename"))

	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	assert.Equal(t, []string{"/rename new-name"}, f.tm.typed["designer"], "a slash command is typed")

	*f.now = f.now.Add(2 * time.Second)
	assert.Empty(t, f.s.deliverInbox("designer", *f.now))
	assert.Empty(t, p.raw, "nothing posted into the settling role")

	*f.now = f.now.Add(time.Minute - 2*time.Second)
	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now), "the next beat delivers as before")
	assert.Len(t, p.raw, 1)
}

// A trim types /compact or /clear too: the boot or recovery line it queued waits the same time.
func TestTrimSettlesBeforeALineIsDelivered(t *testing.T) {
	f := fixtureWith(t, trimCrew)
	require.NoError(t, f.s.Up("dev"))
	*f.now = f.now.Add(3 * time.Minute)
	f.rg.context["sid-dev"] = 120_000
	f.status("dev", "idle", f.now.Add(-time.Hour))

	f.s.Tick()
	require.Equal(t, []string{"/clear"}, f.tm.typed["dev"])
	require.Len(t, f.s.State.Role("dev").Inbox, 1, "its boot waits")

	*f.now = f.now.Add(time.Second)
	assert.Empty(t, f.s.deliverInbox("dev", *f.now))
	assert.Equal(t, []string{"/clear"}, f.tm.typed["dev"])

	*f.now = f.now.Add(slashSettle)
	assert.NotEmpty(t, f.s.deliverInbox("dev", *f.now))
	assert.Len(t, f.tm.typed["dev"], 2)
}

// The dispatcher is a role too: its signals wait out a slash command typed into it.
func TestSlashCommandSettleHoldsTheDispatcherQueue(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("dispatcher"))
	*f.now = f.now.Add(bootPause)
	f.status("dispatcher", "idle", *f.now)
	f.s.State.Role("dispatcher").SlashAt = *f.now

	_, err := f.s.Note("dev", "T-1 result ready", false)
	require.NoError(t, err)

	*f.now = f.now.Add(time.Second)
	assert.Empty(t, f.s.Deliver())

	*f.now = f.now.Add(slashSettle)
	assert.NotEmpty(t, f.s.Deliver())
}

// A stamp after now (the clock went back) holds nothing: a role is never held longer than the stamps' spans.
func TestStampsHoldOnlyForTheirSpan(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	assert.False(t, within(now, time.Time{}, time.Minute), "no stamp")
	assert.True(t, within(now, now, time.Minute))
	assert.True(t, within(now, now.Add(-time.Minute+time.Nanosecond), time.Minute))
	assert.False(t, within(now, now.Add(-time.Minute), time.Minute))
	assert.False(t, within(now, now.Add(time.Hour), time.Minute), "a stamp in the future")

	rs := &RoleState{TriedAt: now}
	assert.False(t, rs.triedLately(now), "a stamp with no counted try means nothing")
	rs.Attempts = 1
	assert.True(t, rs.triedLately(now.Add(retryGap-time.Second)))
	assert.False(t, rs.triedLately(now.Add(retryGap)))

	assert.Less(t, retryGap, time.Minute, "the beat's own cadence must change nothing")
	assert.Less(t, slashSettle, time.Minute, "the beat's own cadence must change nothing")
}

// The stamps are optional in state.json: a state written before them reads, and they are not written while unset.
func TestSettleStampsAreOptionalInState(t *testing.T) {
	st := NewState()
	st.Role("dev").Inbox = []string{"x"}

	raw, err := json.Marshal(st)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "tried_at")
	assert.NotContains(t, string(raw), "slash_at")

	back, _, err := decode(raw)
	require.NoError(t, err)
	assert.True(t, back.Roles["dev"].TriedAt.IsZero())
	assert.True(t, back.Roles["dev"].SlashAt.IsZero())

	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	st.Role("dev").Attempts, st.Role("dev").TriedAt, st.Role("dev").SlashAt = 1, at, at

	raw, err = json.Marshal(st)
	require.NoError(t, err)

	back, _, err = decode(raw)
	require.NoError(t, err)
	assert.Equal(t, at, back.Roles["dev"].TriedAt)
	assert.Equal(t, at, back.Roles["dev"].SlashAt)
}

// The hook that runs once a compaction is over says the command is done: what waits goes at once, even when the
// compaction was quicker than slashSettle.
func TestTurnEndAfterACompactionDoesNotWaitOutTheSettleTime(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	start := *f.now
	f.status("designer", "idle", start)
	f.rg.endedAt["sid-designer"] = start.Add(5 * time.Second)
	*f.now = start.Add(10 * time.Second)

	rs := f.s.State.Role("designer")
	rs.SlashAt = *f.now
	require.NoError(t, f.s.Queue("designer", "[dispatcher] after the compaction"))

	*f.now = f.now.Add(time.Second)
	assert.Empty(t, f.s.deliverInbox("designer", *f.now), "a pass of the beat's kind still waits")

	assert.Equal(t, []string{"posted to designer: [dispatcher] after the compaction"}, f.s.TurnEnd("designer", "sid-designer"))
	assert.Equal(t, []string{"[dispatcher] after the compaction"}, p.posted["sid-designer"])
	assert.True(t, rs.SlashAt.IsZero())
}
