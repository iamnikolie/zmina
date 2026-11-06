package shift

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/task"
)

const trimCrew = `namespace: /ns
claude: claude
roles:
  dispatcher:
    boot: read d and boot
    context_max: 150k
  dev:
    boot: read dev and boot
    context_max: 100k
    context_action: clear
  designer:
`

func TestTrimCompactsAnIdleRolePastItsContext(t *testing.T) {
	f := fixtureWith(t, trimCrew)
	f.s.Tick()

	*f.now = f.now.Add(3 * time.Minute)
	f.rg.context["sid-dispatcher"] = 362_000

	f.status("dispatcher", "idle", f.now.Add(-time.Minute))
	f.s.Tick()
	assert.Empty(t, f.tm.typed["dispatcher"], "idle for a minute only: a reply may be on its way")

	f.status("dispatcher", "busy", f.now.Add(-10*time.Minute))
	f.s.Tick()
	assert.Empty(t, f.tm.typed["dispatcher"], "never while busy")

	f.s.State.Notes = []string{"designer: ended"}
	f.status("dispatcher", "idle", f.now.Add(-10*time.Minute))
	f.s.Tick()
	require.Equal(t, []string{"[zmina] designer: ended"}, f.tm.typed["dispatcher"], "notes go first, no trim that beat")

	f.s.Tick()
	assert.Len(t, f.tm.typed["dispatcher"], 1, "it took the notes: idle again only now")

	*f.now = f.now.Add(6 * time.Minute)
	did := f.s.Tick()
	assert.Equal(t, []string{"[zmina] designer: ended", "/compact"}, f.tm.typed["dispatcher"])
	assert.Contains(t, did, "dispatcher: /compact, context 362k over 150k")
	assert.True(t, f.ev.has("dispatcher", "compacted"))
	f.rg.context["sid-dispatcher"] = 0 // compacted: unknown until the next turn
	f.rg.compacted["sid-dispatcher"] = *f.now

	*f.now = f.now.Add(10 * time.Minute)
	f.s.Tick()
	assert.Equal(t, 1, compactCount(f.tm.typed["dispatcher"]), "once per pause, even before the next turn shows the new size")

	f.rg.context["sid-dispatcher"] = 40_000
	*f.now = f.now.Add(time.Hour)
	f.s.Tick()
	assert.Equal(t, 1, compactCount(f.tm.typed["dispatcher"]), "under the limit")
}

func TestTrimClearsAndBootsAgainUnlessATaskIsHeld(t *testing.T) {
	f := fixtureWith(t, trimCrew)
	require.NoError(t, f.s.Up("dev"))

	*f.now = f.now.Add(3 * time.Minute)
	f.rg.context["sid-dev"] = 120_000
	f.tk.list = []task.Task{{ID: "T-4", Role: "dev", State: task.Open}}
	f.status("dev", "idle", f.now.Add(-10*time.Minute))

	f.s.Tick()
	assert.Equal(t, []string{"/compact"}, f.tm.typed["dev"], "holding T-4: compacted, not cleared")

	*f.now = f.now.Add(time.Minute)
	f.rg.compacted["sid-dev"] = *f.now
	f.rg.context["sid-dev"] = 60_000
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	assert.Equal(t, 60_000, f.s.State.Role("dev").TrimBase, "the first size after the trim")

	f.tk.list[0].State = task.Done
	f.rg.context["sid-dev"] = 120_000
	*f.now = f.now.Add(time.Hour)
	f.status("dev", "idle", f.now.Add(-10*time.Minute))
	f.s.Tick()
	assert.Len(t, f.tm.typed["dev"], 1, "a clear waits idle_minutes, not five")

	f.status("dev", "idle", f.now.Add(-40*time.Minute))
	did := f.s.Tick()
	assert.Equal(t, []string{"/compact", "/clear"}, f.tm.typed["dev"])
	assert.Contains(t, did, "dev: /clear, context 120k over 100k")
	assert.Equal(t, []string{"read dev and boot"}, f.s.State.Role("dev").Inbox, "booted again on the next beat")

	f.s.Tick()
	assert.Equal(t, []string{"/compact", "/clear", "read dev and boot"}, f.tm.typed["dev"])
}

func TestTrimNeverFeedsItself(t *testing.T) {
	f := fixtureWith(t, trimCrew)
	require.NoError(t, f.s.Up("dev"))

	*f.now = f.now.Add(3 * time.Minute)
	f.rg.context["sid-dev"] = 120_000
	f.status("dev", "idle", f.now.Add(-time.Hour))
	f.s.Tick()
	f.s.Tick()
	require.Equal(t, []string{"/clear", "read dev and boot"}, f.tm.typed["dev"])

	// The clear started another conversation, and its boot alone fills more than context_max.
	sess := f.rg.byPID[f.tm.up["dev"]]
	sess.SessionID = "sid-dev-2"
	f.rg.context["sid-dev-2"] = 110_000
	*f.now = f.now.Add(time.Minute)
	did := f.s.Tick()
	assert.Contains(t, did, "dev: fills 110k right after a trim, over its context_max 100k; not trimmed again in "+
		"this conversation — raise context_max")

	*f.now = f.now.Add(3 * time.Hour)
	sess.StatusUpdatedAt = f.now.Add(-time.Hour).UnixMilli()
	f.s.Tick()
	assert.Len(t, f.tm.typed["dev"], 2, "no clear loop")
}

func TestTrimWaitsForAnAttachedTerminalAndForTheBootLine(t *testing.T) {
	f := fixtureWith(t, trimCrew)
	f.s.Tick()

	*f.now = f.now.Add(time.Hour)
	f.rg.context["sid-dispatcher"] = 362_000
	f.status("dispatcher", "idle", f.now.Add(-time.Hour))
	f.tm.drafts["dispatcher"] = "half a thought"
	f.s.Tick()
	assert.Empty(t, f.tm.typed["dispatcher"], "a half-typed line would swallow /compact")

	f.tm.drafts["dispatcher"] = "-"
	f.s.Tick()
	assert.Empty(t, f.tm.typed["dispatcher"], "no input line: a dialog is open")

	f.tm.drafts["dispatcher"] = ""
	f.tm.attached["dispatcher"] = true
	f.s.State.Role("dispatcher").Inbox = []string{"read d and boot"}
	f.s.State.Signals = []string{"mr-watch: !35 failed"}
	f.s.Tick()
	assert.Equal(t, []string{"read d and boot"}, f.tm.typed["dispatcher"], "its own line before the signals")

	f.s.Tick()
	assert.Equal(t, []string{"read d and boot", "[zmina] mr-watch: !35 failed"}, f.tm.typed["dispatcher"])

	f.status("dispatcher", "idle", f.now.Add(-time.Hour))
	f.s.Tick()
	assert.Equal(t, "/compact", f.tm.typed["dispatcher"][2], "an attached terminal does not hold a compact back")
}

func TestTrimClearWaitsForNoTerminal(t *testing.T) {
	f := fixtureWith(t, trimCrew)
	require.NoError(t, f.s.Up("dev"))

	*f.now = f.now.Add(2 * time.Hour)
	f.rg.context["sid-dev"] = 120_000
	f.status("dev", "idle", f.now.Add(-time.Hour))
	f.tm.attached["dev"] = true
	f.s.Tick()
	assert.Empty(t, f.tm.typed["dev"], "someone is looking at it")

	f.tm.attached["dev"] = false
	f.s.Tick()
	assert.Equal(t, []string{"/clear"}, f.tm.typed["dev"])
}

func TestTrimLeavesRolesWithoutALimit(t *testing.T) {
	f := fixtureWith(t, trimCrew)
	require.NoError(t, f.s.Up("designer"))

	*f.now = f.now.Add(time.Hour)
	f.rg.context["sid-designer"] = 900_000
	f.status("designer", "idle", f.now.Add(-time.Hour))
	f.s.Tick()

	assert.Empty(t, f.tm.typed["designer"])
}

func TestTokens(t *testing.T) {
	assert.Equal(t, "362k", Tokens(362_286))
	assert.Equal(t, "1m", Tokens(1_000_000))
	assert.Equal(t, "1.2m", Tokens(1_200_000))
	assert.Equal(t, "900", Tokens(900))
}

func compactCount(lines []string) int {
	n := 0
	for _, line := range lines {
		if line == "/compact" {
			n++
		}
	}
	return n
}

// A /compact takes a minute or more: until its compaction shows, the last turn is the one before it, over the limit,
// and is no size after a trim.
func TestTrimBaseWaitsForTheCompaction(t *testing.T) {
	f := fixtureWith(t, trimCrew)
	f.s.Tick()

	*f.now = f.now.Add(10 * time.Minute)
	f.rg.context["sid-dispatcher"] = 479_281
	f.status("dispatcher", "idle", f.now.Add(-10*time.Minute))
	f.s.Tick()
	require.Equal(t, 1, compactCount(f.tm.typed["dispatcher"]))

	*f.now = f.now.Add(time.Minute)
	f.rg.compacted["sid-dispatcher"] = f.now.Add(-time.Hour) // an earlier compaction of this conversation
	did := f.s.Tick()
	assert.NotContains(t, strings.Join(did, "\n"), "right after a trim", "still compacting: the old turn is no new size")
	assert.True(t, f.s.State.Role("dispatcher").TrimPending)

	*f.now = f.now.Add(time.Minute)
	f.rg.compacted["sid-dispatcher"] = *f.now
	f.rg.context["sid-dispatcher"] = 0
	f.s.Tick()
	assert.True(t, f.s.State.Role("dispatcher").TrimPending, "compacted: the size is known after the next turn")

	f.rg.context["sid-dispatcher"] = 43_600
	*f.now = f.now.Add(time.Minute)
	did = f.s.Tick()
	assert.NotContains(t, strings.Join(did, "\n"), "right after a trim")
	rs := f.s.State.Role("dispatcher")
	assert.False(t, rs.TrimPending)
	assert.Equal(t, 43_600, rs.TrimBase)

	f.rg.context["sid-dispatcher"] = 200_000
	*f.now = f.now.Add(time.Hour)
	f.status("dispatcher", "idle", f.now.Add(-10*time.Minute))
	f.s.Tick()
	assert.Equal(t, 2, compactCount(f.tm.typed["dispatcher"]), "trimmed again once it grows: auto-trim goes on")
}

// A trim nothing came of (the compaction failed, the line was lost) is let go after the pause, and tried again.
func TestTrimUnseenIsLetGo(t *testing.T) {
	f := fixtureWith(t, trimCrew)
	f.s.Tick()

	*f.now = f.now.Add(10 * time.Minute)
	f.rg.context["sid-dispatcher"] = 200_000
	f.status("dispatcher", "idle", f.now.Add(-10*time.Minute))
	f.s.Tick()
	require.Equal(t, 1, compactCount(f.tm.typed["dispatcher"]))

	*f.now = f.now.Add(10 * time.Minute)
	f.s.Tick()
	assert.True(t, f.s.State.Role("dispatcher").TrimPending)

	*f.now = f.now.Add(25 * time.Minute)
	f.status("dispatcher", "idle", f.now.Add(-10*time.Minute))
	f.s.Tick()
	assert.True(t, f.ev.has("dispatcher", "trim-unseen"))
	f.s.Tick()
	assert.Equal(t, 2, compactCount(f.tm.typed["dispatcher"]))
}

// A compaction seen once is remembered for this trim: a long first turn after it may push its boundary out of the
// part of the transcript read, and its size is still the trim's base — over the limit, the conversation is left be.
func TestTrimRemembersItsCompaction(t *testing.T) {
	f := fixtureWith(t, trimCrew)
	f.s.Tick()

	*f.now = f.now.Add(10 * time.Minute)
	f.rg.context["sid-dispatcher"] = 200_000
	f.status("dispatcher", "idle", f.now.Add(-10*time.Minute))
	f.s.Tick()
	require.Equal(t, 1, compactCount(f.tm.typed["dispatcher"]))

	*f.now = f.now.Add(time.Minute)
	f.rg.compacted["sid-dispatcher"] = *f.now
	f.rg.context["sid-dispatcher"] = 0
	f.s.Tick()
	assert.True(t, f.s.State.Role("dispatcher").TrimTook)

	delete(f.rg.compacted, "sid-dispatcher") // out of the tail read
	f.rg.context["sid-dispatcher"] = 180_000
	*f.now = f.now.Add(time.Minute)
	did := f.s.Tick()
	assert.Contains(t, strings.Join(did, "\n"), "dispatcher: fills 180k right after a trim")
	rs := f.s.State.Role("dispatcher")
	assert.False(t, rs.TrimPending)
	assert.Equal(t, 180_000, rs.TrimBase)

	*f.now = f.now.Add(35 * time.Minute)
	f.status("dispatcher", "idle", f.now.Add(-10*time.Minute))
	f.s.Tick()
	assert.Equal(t, 1, compactCount(f.tm.typed["dispatcher"]), "not trimmed again in this conversation")
	assert.False(t, f.ev.has("dispatcher", "trim-unseen"))
}
