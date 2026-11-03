package shift

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// At a turn's end what waits is posted as a beat would post it, and only that: the session the hook ran for, past
// its boot, idle, each line once the one before it showed; nothing typed.
func TestTurnEndPostsWhatABeatWould(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	start := *f.now
	f.status("designer", "idle", start)
	require.NoError(t, f.s.Queue("designer", "[dispatcher] first"))
	require.NoError(t, f.s.Queue("designer", "[dispatcher] second"))

	*f.now = start.Add(10 * time.Second)
	assert.False(t, f.s.TurnOver("designer", "sid-designer"), "idle, but the transcript does not show the turn closed yet")
	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"), "the boot is not over")

	f.rg.endedAt["sid-designer"] = start.Add(8 * time.Second)
	assert.True(t, f.s.TurnOver("designer", "sid-designer"))
	assert.False(t, f.s.TurnOver("designer", "sid-other"))
	assert.Empty(t, f.s.TurnEnd("designer", "sid-other"), "a session the hook did not run for")

	assert.Equal(t, []string{"posted to designer: [dispatcher] first"}, f.s.TurnEnd("designer", "sid-designer"))
	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"), "the next waits until the first shows")

	p.showAll()
	assert.Equal(t, []string{"posted to designer: [dispatcher] second"}, f.s.TurnEnd("designer", "sid-designer"))
	assert.Equal(t, []string{"[dispatcher] first", "[dispatcher] second"}, p.posted["sid-designer"])

	p.showAll()
	*f.now = start.Add(3 * time.Minute)
	f.status("designer", "busy", *f.now)
	delete(f.rg.endedAt, "sid-designer")
	require.NoError(t, f.s.Queue("designer", "[dispatcher] third"))
	assert.False(t, f.s.TurnOver("designer", "sid-designer"))
	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"), "a turn already running")

	f.status("designer", "idle", *f.now)
	f.s.State.Role("designer").Inbox = []string{"/compact"}
	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"), "a slash command is typed by the beat")
	assert.Empty(t, f.tm.typed["designer"], "nothing typed")
}

// An obsolete task packet pruned from the head never leaves a slash command for the turn's end to type.
func TestTurnEndTypesNoSlashCommandBehindAPrunedPacket(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	start := *f.now
	f.status("designer", "idle", start)
	f.rg.endedAt["sid-designer"] = start.Add(5 * time.Second)
	*f.now = start.Add(10 * time.Second)

	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Done}}
	rs := f.s.State.Role("designer")
	rs.Inbox = []string{"[zmina T-1] Read handoff packet t-1.json; before work run: zmina task accept T-1", "/compact"}

	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"))
	assert.Empty(t, f.tm.typed["designer"], "nothing typed")
	assert.Empty(t, p.posted["sid-designer"])
	assert.Equal(t, []string{"/compact"}, rs.Inbox, "the slash command waits for the beat")
}

// A role whose lines are typed gets nothing at a turn's end: typing waits for the beat.
func TestTurnEndTypesNothing(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	f.s.Crew.Delivery = crew.DeliveryTmux
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	require.NoError(t, f.s.Queue("designer", "[dispatcher] first"))
	*f.now = f.now.Add(3 * time.Minute)

	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"))
	assert.Empty(t, f.tm.typed["designer"])
	assert.Empty(t, p.posted["sid-designer"])
}

// The dispatcher's signals go at the end of its turn, before its own queue.
func TestTurnEndTellsTheDispatcher(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("dispatcher"))
	f.status("dispatcher", "idle", *f.now)
	*f.now = f.now.Add(3 * time.Minute)

	_, err := f.s.Note("mrs", "acme/site!12 approved", false)
	require.NoError(t, err)

	assert.Equal(t, []string{"told the dispatcher: [zmina] S-1 mrs: acme/site!12 approved (zmina S-1)"},
		f.s.TurnEnd("dispatcher", "sid-dispatcher"))
	assert.Len(t, p.posted["sid-dispatcher"], 1)
}

// The Stop hook is in the settings of a role on its own channel: silent, exit 0 always, a short timeout.
func TestTurnEndHookInSettings(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	f.s.Self = "/opt/zmina"

	var got struct {
		Hooks struct {
			Stop []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
					Timeout int    `json:"timeout"`
				} `json:"hooks"`
			}
		} `json:"hooks"`
	}

	require.NoError(t, json.Unmarshal([]byte(f.s.roleSettings("designer")), &got))
	require.Len(t, got.Hooks.Stop, 1)
	require.Len(t, got.Hooks.Stop[0].Hooks, 1)
	h := got.Hooks.Stop[0].Hooks[0]
	assert.Equal(t, "command", h.Type)
	assert.Equal(t, "'/opt/zmina' --crew '"+f.s.Crew.Path+"' _turn-end --role 'designer' >/dev/null 2>&1 || true", h.Command)
	assert.Equal(t, 5, h.Timeout)
}
