package shift

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/task"
)

// What zmina send --when-idle does: queue a tracked line, then the pass for that one role. An idle role has it
// as the command ends.
func TestDeliverToPostsToAnIdleRole(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	id, err := f.s.QueueTracked("designer", "[dev] look at T-1")
	require.NoError(t, err)

	line := f.s.DeliverTo("designer")
	assert.Equal(t, "posted to designer: [dev] look at T-1 (zmina "+id+")", line)
	assert.Equal(t, []string{"[dev] look at T-1 (zmina " + id + ")"}, p.posted["sid-designer"])
	assert.Empty(t, f.s.State.Role("designer").Inbox)
	assert.Len(t, f.s.State.Role("designer").Posted, 1, "followed until its conversation shows it")
}

// A role that is typed into gets its line typed, as the beat would type it.
func TestDeliverToTypesIntoAnIdleTypedRole(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	*f.now = f.now.Add(bootPause)
	f.status("designer", "idle", *f.now)
	require.NoError(t, f.s.Queue("designer", "hello"))

	assert.Equal(t, "typed into designer: hello", f.s.DeliverTo("designer"))
	assert.Equal(t, []string{"hello"}, f.tm.typed["designer"])
	assert.Empty(t, f.s.State.Role("designer").Inbox)
}

// A busy role keeps the line; the next beat delivers it, once, when the role is between turns.
func TestDeliverToLeavesABusyRoleToTheBeat(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	f.status("designer", "busy", *f.now)

	_, err := f.s.QueueTracked("designer", "[dev] look at T-1")
	require.NoError(t, err)

	assert.Empty(t, f.s.DeliverTo("designer"))
	assert.Empty(t, p.posted["sid-designer"])
	assert.Len(t, f.s.State.Role("designer").Inbox, 1, "still queued")

	f.status("designer", "idle", *f.now)
	*f.now = f.now.Add(time.Minute)
	did := f.s.Tick()
	assert.Contains(t, did, "posted to designer: [dev] look at T-1 (zmina S-1)")
	assert.Len(t, p.posted["sid-designer"], 1)

	p.showAll()
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	assert.Len(t, p.posted["sid-designer"], 1, "once")
	assert.Empty(t, f.s.State.Role("designer").Inbox)
}

// The guards are the beat's: a dialog, a draft in a typed role's input line, a role not booted, one being put down.
func TestDeliverToKeepsTheBeatsGuards(t *testing.T) {
	t.Run("dialog", func(t *testing.T) {
		f, p := nativeFixture(t, crewYAML)
		upNative(t, f, "designer")
		f.status("designer", "waiting", *f.now)
		require.NoError(t, f.s.Queue("designer", "[dev] hi"))

		assert.Empty(t, f.s.DeliverTo("designer"))
		assert.Empty(t, p.raw)
	})

	t.Run("a draft in a typed role's input line", func(t *testing.T) {
		f := fixtureWith(t, crewYAML)
		require.NoError(t, f.s.Up("designer"))
		*f.now = f.now.Add(bootPause)
		f.status("designer", "idle", *f.now)
		f.tm.drafts["designer"] = "half a sentence"
		require.NoError(t, f.s.Queue("designer", "hello"))

		assert.Empty(t, f.s.DeliverTo("designer"))
		assert.Empty(t, f.tm.typed["designer"])
		assert.Equal(t, "half a sentence", f.tm.drafts["designer"])
	})

	t.Run("not booted", func(t *testing.T) {
		f, p := nativeFixture(t, crewYAML)
		require.NoError(t, f.s.Up("designer"))
		f.status("designer", "idle", *f.now)
		require.NoError(t, f.s.Queue("designer", "[dev] hi"))

		assert.Empty(t, f.s.DeliverTo("designer"), "its boot is not over")
		assert.Empty(t, p.raw)
	})

	t.Run("the line before it not shown yet", func(t *testing.T) {
		f, p := nativeFixture(t, crewYAML)
		upNative(t, f, "designer")
		require.NoError(t, f.s.Queue("designer", "[dev] one"))

		assert.NotEmpty(t, f.s.DeliverTo("designer"))
		require.NoError(t, f.s.Queue("designer", "[dev] two"))
		assert.Empty(t, f.s.DeliverTo("designer"), "the next waits for the first to show")
		assert.Len(t, p.raw, 1)

		p.showAll()
		assert.NotEmpty(t, f.s.DeliverTo("designer"))
		assert.Len(t, p.raw, 2)
	})

	t.Run("a role being put down", func(t *testing.T) {
		f, p := nativeFixture(t, crewYAML)
		upNative(t, f, "designer")
		f.tm.exitSlow = true
		_, err := f.s.BeginDown("designer", DownOptions{})
		require.NoError(t, err)
		require.NoError(t, f.s.Queue("designer", "[dev] hi"))

		assert.Empty(t, f.s.DeliverTo("designer"))
		assert.Empty(t, p.raw)
		assert.Equal(t, []string{"/exit"}, f.tm.typed["designer"])
	})

	t.Run("a role that is not in the crew file", func(t *testing.T) {
		f, _ := nativeFixture(t, crewYAML)

		assert.Empty(t, f.s.DeliverTo("nobody"))
		_, known := f.s.State.Roles["nobody"]
		assert.False(t, known, "no state made up for it")
	})
}

// The pass is a delivery and nothing else: it starts no role, trims nothing, and leaves the beat's own marks alone.
func TestDeliverToStartsTrimsAndMarksNothing(t *testing.T) {
	f := fixtureWith(t, trimCrew)
	require.NoError(t, f.s.Up("dev"))
	*f.now = f.now.Add(bootPause)
	f.status("dev", "idle", f.now.Add(-time.Hour))
	f.rg.context["sid-dev"] = 120_000 // far over its context_max: a beat would clear it
	require.NoError(t, f.s.Queue("dev", "hello"))

	beat := f.now.Add(-30 * time.Second)
	f.s.State.LastTick = beat
	rs := f.s.State.Role("dev")
	rs.Up = true
	started := rs.Started

	assert.Equal(t, "typed into dev: hello", f.s.DeliverTo("dev"))

	assert.Equal(t, beat, f.s.State.LastTick, "not a beat")
	assert.True(t, rs.Up)
	assert.Equal(t, started, rs.Started)
	assert.True(t, rs.Trimmed.IsZero(), "nothing trimmed")
	assert.Equal(t, []string{"hello"}, f.tm.typed["dev"], "no /clear typed")
	assert.Equal(t, []string{"dev"}, f.tm.starts, "no role started but the one started above")

	// A role that is down stays down, even one the beat would start (keep: always), and keeps its line.
	require.NoError(t, f.s.Queue("designer", "[dev] hi"))
	assert.Empty(t, f.s.DeliverTo("designer"))
	assert.Empty(t, f.s.DeliverTo("dispatcher"), "a dispatcher that is down is not started either")
	assert.Equal(t, []string{"dev"}, f.tm.starts)
	assert.False(t, f.s.State.Role("designer").Up, "Up is the beat's to write")
	assert.Equal(t, beat, f.s.State.LastTick)
	assert.Equal(t, []string{"[dev] hi"}, f.s.State.Role("designer").Inbox)
}

// The dispatcher's pass is deliver: its signals and notes, and its own queue when it is typed into.
func TestDeliverToTheDispatcher(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	_, err := f.s.Note("dev", "T-1 result ready", false)
	require.NoError(t, err)

	line := f.s.DeliverTo("dispatcher")
	assert.Equal(t, "told the dispatcher: [zmina] S-1 dev: T-1 result ready (zmina S-1)", line)
	assert.Len(t, p.posted["sid-dispatcher"], 1)
	assert.Empty(t, f.s.State.Signals)

	// Typed, a line queued for it goes through its inbox, as the beat does after the queue of signals.
	g := fixtureWith(t, crewYAML)
	require.NoError(t, g.s.Up("dispatcher"))
	*g.now = g.now.Add(bootPause)
	g.status("dispatcher", "idle", *g.now)
	require.NoError(t, g.s.Queue("dispatcher", "[dev] look at T-2"))

	assert.Equal(t, "typed into dispatcher: [dev] look at T-2", g.s.DeliverTo("dispatcher"))
}

// A handoff to an idle role posts the packet line once: not again by the next beat, nor by a second hand-off while
// the first is on its way.
func TestHandoffToAnIdleRoleIsPostedOnce(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open, Updated: f.now.UTC()}}

	_, err := f.s.HandTask("designer", f.tk.list[0])
	require.NoError(t, err)
	require.Len(t, f.s.State.Role("designer").Inbox, 1, "queued")

	assert.NotEmpty(t, f.s.DeliverTo("designer"))
	require.Len(t, p.posted["sid-designer"], 1)
	assert.Contains(t, p.posted["sid-designer"][0], "[zmina T-1] Read handoff packet ")
	assert.Empty(t, f.s.State.Role("designer").Inbox)
	assert.False(t, f.tk.list[0].Delivery.SentAt.IsZero(), "submitted")

	// Handed again while the first is on its way (task handoff --queue run twice): nothing new is queued.
	_, err = f.s.HandTask("designer", f.tk.list[0])
	require.NoError(t, err)
	assert.Empty(t, f.s.State.Role("designer").Inbox)
	assert.Empty(t, f.s.DeliverTo("designer"))

	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	p.showAll()
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()

	assert.Len(t, p.posted["sid-designer"], 1, "no second post at the beat")
	assert.Empty(t, f.s.State.Role("designer").Posted, "seen: done")
}

// A role that restarted gets the same packet again: only a line on its way to the session it has now is not queued twice.
func TestHandoffAfterARestartQueuesAgain(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open, Updated: f.now.UTC()}}

	_, err := f.s.HandTask("designer", f.tk.list[0])
	require.NoError(t, err)
	require.NotEmpty(t, f.s.DeliverTo("designer"))

	*f.now = f.now.Add(time.Hour)
	f.s.State.Role("designer").Started = *f.now

	_, err = f.s.HandTask("designer", f.tk.list[0])
	require.NoError(t, err)
	assert.Len(t, f.s.State.Role("designer").Inbox, 1)
}

// A handoff to a role that is busy waits for the beat, and goes once.
func TestHandoffToABusyRoleWaitsForTheBeat(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	f.status("designer", "busy", *f.now)
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open, Updated: f.now.UTC()}}

	_, err := f.s.HandTask("designer", f.tk.list[0])
	require.NoError(t, err)
	assert.Empty(t, f.s.DeliverTo("designer"))
	assert.Len(t, f.s.State.Role("designer").Inbox, 1)

	f.status("designer", "idle", *f.now)
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	assert.Len(t, p.posted["sid-designer"], 1)
}
