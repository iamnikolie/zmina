package shift

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upNative starts a role with socket delivery, idle and past its boot grace.
func upNative(t *testing.T, f fix, name string) {
	t.Helper()

	require.NoError(t, f.s.Up(name))
	f.status(name, "idle", *f.now)
	*f.now = f.now.Add(3 * time.Minute)
}

func TestAckSignalsCarryIDs(t *testing.T) {
	f, p := nativeFixture(t, watchYAML)
	upNative(t, f, "dispatcher")

	_, err := f.s.Note("dev", "T-5 result ready", false)
	require.NoError(t, err)
	_, err = f.s.Note("ci-red", "pipeline red", false) // a watch: its own ack
	require.NoError(t, err)

	line := f.s.deliver(*f.now)
	assert.Equal(t, "[zmina] S-1 dev: T-5 result ready · ci-red: pipeline red (zmina S-1)", line)
	assert.Equal(t, []string{line}, p.posted["sid-dispatcher"])

	pend := f.s.Pending("dispatcher")
	require.Len(t, pend, 1)
	assert.Equal(t, "dev: T-5 result ready", pend[0].Text)
	assert.Equal(t, *f.now, pend[0].First)
}

func TestAckRenotifiesUntilAcked(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	since := f.s.clock(*f.now)

	_, err := f.s.Note("dev", "T-5 result ready", false)
	require.NoError(t, err)
	require.NotEmpty(t, f.s.deliver(*f.now))
	p.showAll() // it reached the conversation, then a compaction lost it
	f.s.deliverInbox("dispatcher", *f.now)

	*f.now = f.now.Add(29 * time.Minute)
	f.s.renotify(*f.now)
	assert.Empty(t, f.s.State.Signals, "inside renotify")

	*f.now = f.now.Add(time.Minute)
	f.s.renotify(*f.now)
	assert.Equal(t, []string{"S-1 (still pending since " + since + ") dev: T-5 result ready"}, f.s.State.Signals)

	f.s.renotify(*f.now)
	assert.Len(t, f.s.State.Signals, 1, "queued already: not twice")

	line := f.s.deliver(*f.now)
	assert.Equal(t, "[zmina] S-1 (still pending since "+since+") dev: T-5 result ready (zmina S-1)", line)
	assert.Len(t, f.s.State.Acks, 1, "the same id, no new one")
	p.showAll()
	f.s.deliverInbox("dispatcher", *f.now)

	for range 5 {
		*f.now = f.now.Add(30 * time.Minute)
		f.s.renotify(*f.now)
		f.s.deliver(*f.now)
		p.showAll()
		f.s.deliverInbox("dispatcher", *f.now)
	}

	assert.Equal(t, renotesMax, f.s.State.Acks["S-1"].Renotes, "said again at most renotesMax times")
	assert.Len(t, f.s.Pending("dispatcher"), 1, "still listed")

	acked, err := f.s.AckLines([]string{"s-1"}, false, "dispatcher")
	require.NoError(t, err)
	assert.Equal(t, []string{"S-1"}, acked)
	assert.Empty(t, f.s.Pending(""))
	assert.True(t, f.ev.has("dispatcher", "acked"))
}

func TestAckLinesResolvesAllFirst(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	id, err := f.s.QueueTracked("designer", "[dispatcher] look at T-1")
	require.NoError(t, err)
	assert.Equal(t, "S-1", id)

	_, err = f.s.AckLines([]string{"S-1", "S-9"}, false, "")
	require.Error(t, err)
	assert.Len(t, f.s.Pending(""), 1, "an unknown id acks nothing")

	_, err = f.s.QueueTracked("designer", "[dispatcher] and T-2")
	require.NoError(t, err)

	acked, err := f.s.AckLines(nil, true, "designer")
	require.NoError(t, err)
	assert.Equal(t, []string{"S-1", "S-2"}, acked)
}

func TestAckQueuedLineToARole(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	since := f.s.clock(*f.now)

	_, err := f.s.QueueTracked("designer", "[dispatcher] look at T-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"[dispatcher] look at T-1 (zmina S-1)"},
		f.s.State.Role("designer").Inbox)

	f.s.renotify(f.now.Add(time.Hour))
	assert.Len(t, f.s.State.Role("designer").Inbox, 1, "not delivered yet: not said again")

	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	p.showAll()
	f.s.deliverInbox("designer", *f.now) // settles the posted line

	*f.now = f.now.Add(30 * time.Minute)
	f.s.renotify(*f.now)
	assert.Equal(t, []string{"[dispatcher] look at T-1 (zmina S-1, still pending since " + since +
		")"}, f.s.State.Role("designer").Inbox)

	assert.Contains(t, f.s.PendingBrief("designer"), "S-1 (since "+since+") [dispatcher] look at T-1")
	assert.Empty(t, f.s.PendingBrief("dispatcher"))
}

func TestAckOldRoleIsLeftAsItWas(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	f.s.State.Role("designer").Inbound = false // started by an earlier zmina

	id, err := f.s.QueueTracked("designer", "[dispatcher] plain")
	require.NoError(t, err)
	assert.Empty(t, id)
	assert.Equal(t, []string{"[dispatcher] plain"}, f.s.State.Role("designer").Inbox)
	assert.Empty(t, f.s.State.Acks)

	g := fixtureWith(t, crewYAML) // no inbox socket at all: signals go as before
	require.NoError(t, g.s.Up("dispatcher"))
	g.status("dispatcher", "idle", *g.now)
	*g.now = g.now.Add(3 * time.Minute)
	_, err = g.s.Note("dev", "T-5 result ready", false)
	require.NoError(t, err)
	assert.Equal(t, "[zmina] dev: T-5 result ready", g.s.deliver(*g.now))
}

func TestAckFailedDeliveryLeavesNoID(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	p.err = assert.AnError

	_, err := f.s.Note("dev", "T-5 result ready", false)
	require.NoError(t, err)
	assert.Empty(t, f.s.deliver(*f.now))
	assert.Empty(t, f.s.State.Acks, "an id is kept only for a delivered line")
	assert.Equal(t, []string{"dev: T-5 result ready"}, f.s.State.Signals)
}

func TestAckHookInSettings(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)

	var plain map[string]any
	require.NoError(t, json.Unmarshal([]byte(f.s.roleSettings("designer")), &plain))
	assert.Equal(t, map[string]any{"crossSessionInbound": "accept"}, plain, "no executable known: no hook")

	f.s.Self = "/opt/zmina"
	raw := f.s.roleSettings("designer")

	var got struct {
		Inbound string `json:"crossSessionInbound"`
		Hooks   struct {
			SessionStart []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type, Command string
				} `json:"hooks"`
			}
		} `json:"hooks"`
	}

	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	assert.Equal(t, "accept", got.Inbound)
	require.Len(t, got.Hooks.SessionStart, 2)
	assert.Equal(t, "compact", got.Hooks.SessionStart[0].Matcher)
	assert.Equal(t, "clear", got.Hooks.SessionStart[1].Matcher)
	assert.Equal(t, "'/opt/zmina' --crew '"+f.s.Crew.Path+"' pending --role 'designer' --brief",
		got.Hooks.SessionStart[0].Hooks[0].Command)
}

func TestAckInRecoveryPackAndBootRule(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	_, err := f.s.Note("dev", "T-5 result ready", false)
	require.NoError(t, err)
	require.NotEmpty(t, f.s.deliver(*f.now))

	_, p, err := f.s.RecoveryPack()
	require.NoError(t, err)
	require.Len(t, p.Entries["unacked"], 1)
	assert.Equal(t, "zmina ack S-1", p.Entries["unacked"][0].Ref)

	r, err := f.s.Role("dispatcher")
	require.NoError(t, err)
	assert.Contains(t, f.s.BootText(r), "run `zmina ack S-12 S-14`")
	assert.Contains(t, f.s.BootText(r), "said again after 30m")
}
