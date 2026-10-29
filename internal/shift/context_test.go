package shift

import (
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

	*f.now = f.now.Add(10 * time.Minute)
	f.s.Tick()
	assert.Len(t, f.tm.typed["dispatcher"], 2, "once per pause, even before the next turn shows the new size")

	f.rg.context["sid-dispatcher"] = 40_000
	*f.now = f.now.Add(time.Hour)
	f.s.Tick()
	assert.Len(t, f.tm.typed["dispatcher"], 2, "under the limit")
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

	// The boot alone fills more than context_max.
	f.rg.context["sid-dev"] = 110_000
	*f.now = f.now.Add(time.Minute)
	did := f.s.Tick()
	assert.Contains(t, did, "dev: fills 110k right after a trim, over its context_max 100k; not trimmed again in "+
		"this conversation — raise context_max")

	*f.now = f.now.Add(3 * time.Hour)
	f.status("dev", "idle", f.now.Add(-time.Hour))
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
