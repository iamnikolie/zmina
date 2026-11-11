package shift

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/registry"
)

// shortAges shortens the age limits of the sweep for one test.
func shortAges(t *testing.T, inFlight, failed time.Duration) {
	t.Helper()

	was1, was2 := inFlightMaxAge, failedMaxAge
	inFlightMaxAge, failedMaxAge = inFlight, failed

	t.Cleanup(func() { inFlightMaxAge, failedMaxAge = was1, was2 })
}

// A posted line, a typed line and a failed line past their age limits go in one beat: one event and one FYI line name
// how many, of which kinds and which ids; the next beats say nothing more.
func TestAgeSweepDropsOldRecordsAndTellsOnce(t *testing.T) {
	shortAges(t, time.Hour, 3*time.Hour)

	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	upNative(t, f, "harvester")
	_, postedID := postedTo(t, f, "designer", "[dev] look at T-1")
	f.status("designer", "busy", *f.now) // never quiet: the posted line stays posted

	t0 := *f.now
	h := f.s.State.Role("harvester")
	h.Sent = &Sent{Line: "[dev] look at T-2", At: t0, IDs: []string{"S-2"}}
	h.Failed = []Failed{{Line: "[dev] look at T-3", Error: "tmux refused", At: t0, IDs: []string{"S-3"}},
		{Line: "[dev] look at T-4", Error: "tmux refused", At: t0.Add(150 * time.Minute)}}
	f.s.State.Acks["S-3"] = &Ack{ID: "S-3", Role: "harvester", Text: "[dev] look at T-3"}
	f.s.State.Acks["S-2"] = &Ack{ID: "S-2", Role: "harvester", Text: "[dev] look at T-2", First: t0}

	// Nothing is old enough yet.
	assert.Nil(t, f.s.sweepDelivery(t0))
	require.Len(t, f.s.State.Role("designer").Posted, 1)

	*f.now = t0.Add(4 * time.Hour)
	f.status("designer", "busy", *f.now)
	f.s.Tick()

	assert.Empty(t, f.s.State.Role("designer").Posted)
	assert.Nil(t, h.Sent)
	require.Len(t, h.Failed, 1, "the failed line under its limit stays")
	assert.Equal(t, "[dev] look at T-4", h.Failed[0].Line)
	assert.Equal(t, 1, f.eventCount("delivery-aged"))
	assert.NotNil(t, f.s.State.Acks[postedID], "a delivered line's ack stays")
	assert.NotNil(t, f.s.State.Acks["S-2"])
	assert.Nil(t, f.s.State.Acks["S-3"], "a line never delivered loses its ack with its record")

	var note string

	for _, n := range f.s.State.Notes {
		if strings.HasPrefix(n, "delivery: dropped") {
			require.Empty(t, note, "one line")

			note = n
		}
	}

	require.NotEmpty(t, note)
	assert.Contains(t, note, "dropped 3 records")
	assert.Contains(t, note, "1 posted, 1 sent, 1 failed")
	assert.Contains(t, note, "designer, harvester")
	assert.Contains(t, note, postedID)
	assert.Contains(t, note, "S-2")
	assert.True(t, f.s.isInfo(note), "an FYI line: no id, no ack")

	for range 3 {
		*f.now = f.now.Add(time.Minute)
		f.s.Tick()
	}

	assert.Equal(t, 1, f.eventCount("delivery-aged"), "not every beat")
	assert.Equal(t, 1, strings.Count(strings.Join(f.s.State.Notes, "|"), "delivery: dropped"))
	assert.Nil(t, f.s.sweepDelivery(*f.now))
}

// The ids an event and a line of the sweep name are clipped.
func TestAgeSweepClipsTheIDs(t *testing.T) {
	shortAges(t, time.Hour, time.Hour)

	f, _ := nativeFixture(t, crewYAML)
	rs := f.s.State.Role("designer")

	for i := range 12 {
		rs.Failed = append(rs.Failed, Failed{Line: fmt.Sprint("line ", i), At: f.now.Add(-2 * time.Hour),
			IDs: []string{fmt.Sprint("S-", i+1)}})
	}

	out := f.s.sweepDelivery(*f.now)
	require.Len(t, out, 1)
	assert.Contains(t, out[0], "dropped 12 records")
	assert.Contains(t, out[0], "S-1 S-2 S-3 S-4 S-5 S-6 S-7 S-8 and 4 more")
	assert.NotContains(t, out[0], "S-9")
}

// Lines waiting in the inbox of a role that is simply down are not expired, however old: they wait for it.
func TestAgeSweepLeavesWaitingInboxLinesAlone(t *testing.T) {
	shortAges(t, time.Minute, time.Minute)

	f, _ := nativeFixture(t, crewYAML)
	id, err := f.s.QueueTracked("designer", "[dev] look at T-1")
	require.NoError(t, err)
	require.NoError(t, f.s.Queue("designer", "[dev] and T-2"))

	rs := f.s.State.Role("designer")
	rs.Held = true
	f.s.State.Acks[id].First = *f.now // an old delivered ack of the same role

	*f.now = f.now.Add(1000 * 24 * time.Hour)

	for range 3 {
		f.s.Tick()
	}

	assert.Len(t, rs.Inbox, 2)
	assert.Equal(t, []string{id}, rs.QueuedIDs[rs.Inbox[0]])
	assert.NotNil(t, f.s.State.Acks[id])
	assert.Zero(t, f.eventCount("delivery-aged"))
	assert.NotContains(t, strings.Join(f.s.State.Notes, "|"), "delivery: dropped")
}

// A post that was in fact shown, found by a beat after a long gap (the machine slept), is closed the normal way, seen,
// with its tell stage: the sweep looks before it judges, and does not report it dropped by age.
func TestAgeSweepClosesWhatWasShownBeforeItJudges(t *testing.T) {
	shortAges(t, time.Hour, time.Hour)

	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	_, id, _, err := f.s.NoteSentAs("remote box/ops", tellText, *f.now)
	require.NoError(t, err)
	f.s.BindMessage(TellPrefix+"e-1", id)
	require.NotEmpty(t, f.s.deliver(*f.now))
	p.showAll()

	*f.now = f.now.Add(5 * time.Hour)
	f.status("dispatcher", "idle", *f.now)
	f.s.Tick()

	for _, p := range f.s.State.Role("dispatcher").Posted {
		assert.True(t, p.At.After(f.now.Add(-time.Hour)), "the old post is closed; this one is the beat's own: %s", p.Line)
	}

	assert.True(t, f.ev.has("dispatcher", "seen"))
	assert.Contains(t, f.s.State.MessageStages[TellPrefix+"e-1"], "submitted")
	assert.Zero(t, f.eventCount("delivery-aged"), "it was shown: not aged")
	assert.NotContains(t, strings.Join(f.s.State.Notes, "|"), "delivery: dropped")
}

// A typed line that landed lingers as a record until the next delivery looks at it: a beat past the age limit settles it
// as landed, and does not report it dropped.
func TestAgeSweepClosesATypedLineThatLandedBeforeItJudges(t *testing.T) {
	shortAges(t, time.Hour, time.Hour)

	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	*f.now = f.now.Add(3 * time.Minute)

	require.NoError(t, f.s.Queue("designer", "[dev] look at T-1"))
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	require.NotNil(t, f.s.State.Role("designer").Sent)

	*f.now = f.now.Add(5 * time.Hour)
	f.s.Tick()

	assert.Nil(t, f.s.State.Role("designer").Sent)
	assert.Empty(t, f.s.State.Role("designer").Failed)
	assert.Zero(t, f.eventCount("delivery-aged"))
}

// A line posted to a Codex role's bridge inbox that nothing has taken yet is waiting for the bridge, like an inbox line
// of a role that is down: not aged out however long the bridge stays busy, nor after it died.
func TestAgeSweepLeavesACodexLineWaitingInItsFile(t *testing.T) {
	shortAges(t, time.Hour, time.Hour)

	f, dir := codexFixture(t)
	bridge(t, f, dir, "thread-1")

	_, err := f.s.QueueTracked("dev", "[dispatcher] look at T-1")
	require.NoError(t, err)
	require.NotEmpty(t, f.s.deliverInbox("dev", *f.now))

	// The bridge is busy with a long turn and takes nothing for a day.
	reg := &codex.Registry{Dir: filepath.Join(f.s.Crew.StateDir(), "codex")}
	require.NoError(t, reg.Save(codex.Record{Session: registry.Session{PID: f.tm.up["dev"], SessionID: "thread-1",
		Status: "busy", StatusUpdatedAt: f.now.UnixMilli(), Inbox: dir}}))

	*f.now = f.now.Add(5 * time.Hour)
	f.s.Tick()

	assert.Len(t, f.s.State.Role("dev").Posted, 1)
	assert.Len(t, waitingFiles(t, dir), 1)
	assert.Zero(t, f.eventCount("delivery-aged"))

	// Then it dies: the record is settleEnded's, which keeps it while the file waits.
	delete(f.tm.up, "dev")

	for range 3 {
		*f.now = f.now.Add(5 * time.Hour)
		f.s.Tick()
	}

	assert.Len(t, f.s.State.Role("dev").Posted, 1)
	assert.Zero(t, f.eventCount("delivery-aged"))
}

// The sweep judges the in-flight records of a role that is up this beat, and no other: one that is not up is the
// business of settleEnded, which gives a line back or drops it by its kind. A ticker that was stopped for a day finds a
// role that died meanwhile: that beat sees it end (and settles nothing, as one beat is not proof), and the sweep behind
// it must not report as dropped what the next beat gives back.
func TestAgeSweepLeavesARoleThatIsNotUpToItsSettling(t *testing.T) {
	shortAges(t, time.Hour, time.Hour)

	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	line, _ := postedTo(t, f, "designer", "[dev] look at T-1")

	rs := f.s.State.Role("designer")
	rs.Sent = &Sent{Line: "typed text with no id", At: *f.now}

	delete(f.tm.up, "designer")
	*f.now = f.now.Add(25 * time.Hour)
	f.s.Tick()

	assert.Len(t, rs.Posted, 1, "not judged: the role is not up")
	assert.NotNil(t, rs.Sent)
	assert.Zero(t, f.eventCount("delivery-aged"))
	assert.NotContains(t, strings.Join(f.s.State.Notes, "|"), "delivery: dropped")

	*f.now = f.now.Add(time.Minute)
	f.s.Tick()

	assert.Empty(t, rs.Posted)
	assert.Equal(t, []string{line}, rs.Inbox, "given back for the next session")
	assert.Nil(t, rs.Sent)
	assert.Equal(t, 1, f.eventCount("requeued"))
	assert.Zero(t, f.eventCount("delivery-aged"), "never reported dropped by age")
}
