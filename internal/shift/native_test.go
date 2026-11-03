package shift

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/peer"
	"github.com/iamnikolie/zmina/internal/registry"
)

// fakePeer is the inbox sockets of the fake sessions: what was posted to each, and which messages their
// conversations show.
type fakePeer struct {
	unusable bool
	err      error
	posted   map[string][]string // session id -> lines
	ids      map[string]string   // msg id -> session id
	shown    map[string]bool     // msg id
	n        int
}

func newFakePeer() *fakePeer {
	return &fakePeer{posted: map[string][]string{}, ids: map[string]string{}, shown: map[string]bool{}}
}

func (p *fakePeer) Usable(*registry.Session) bool { return !p.unusable }

func (p *fakePeer) Post(_ string, s *registry.Session, text string) (string, error) {
	if p.err != nil {
		return "", p.err
	}

	p.n++
	id := fmt.Sprintf("m-%d", p.n)
	p.posted[s.SessionID] = append(p.posted[s.SessionID], text)
	p.ids[id] = s.SessionID

	return id, nil
}

func (p *fakePeer) Delivered(_, sessionID, msgID string) (bool, error) {
	return p.shown[msgID] && p.ids[msgID] == sessionID, nil
}

// showAll marks every posted message as shown in its conversation.
func (p *fakePeer) showAll() {
	for id := range p.ids {
		p.shown[id] = true
	}
}

func nativeFixture(t *testing.T, body string) (fix, *fakePeer) {
	t.Helper()

	f := fixtureWith(t, body)
	p := newFakePeer()
	f.s.Peer = p

	return f, p
}

func TestNativeRoleStartsWithInboundSettings(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))

	assert.Contains(t, f.tm.argv["designer"], "--settings")
	assert.Contains(t, f.tm.argv["designer"], f.s.roleSettings("designer"))
	assert.True(t, f.s.State.Role("designer").Inbound)

	f.s.Crew.Delivery = crew.DeliveryTmux
	r, err := f.s.Role("designer")
	require.NoError(t, err)
	assert.NotContains(t, f.s.Command(r, ""), "--settings", "delivery tmux: no settings of zmina's")
}

func TestNativeInboxIsPostedNotTyped(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	f.tm.drafts["designer"] = "half a thought of the owner"
	*f.now = f.now.Add(3 * time.Minute)

	require.NoError(t, f.s.Queue("designer", "[dispatcher] first"))
	require.NoError(t, f.s.Queue("designer", "[dispatcher] second"))

	did := f.s.deliverInbox("designer", *f.now)
	assert.Equal(t, "posted to designer: [dispatcher] first", did, "a draft does not hold a posted line")
	assert.Empty(t, f.tm.typed["designer"], "nothing typed")
	assert.Equal(t, "half a thought of the owner", f.tm.drafts["designer"], "the draft stays as it was")
	assert.Equal(t, []string{"[dispatcher] first"}, p.posted["sid-designer"])
	assert.True(t, f.ev.has("designer", "posted"))

	*f.now = f.now.Add(time.Minute)
	assert.Empty(t, f.s.deliverInbox("designer", *f.now), "the next waits until the first shows")

	p.showAll()
	*f.now = f.now.Add(time.Minute)
	assert.Equal(t, "posted to designer: [dispatcher] second", f.s.deliverInbox("designer", *f.now))
	assert.True(t, f.ev.has("designer", "seen"))
}

func TestNativeWaitsForATurnToEnd(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "busy", *f.now)
	*f.now = f.now.Add(3 * time.Minute)
	require.NoError(t, f.s.Queue("designer", "[dispatcher] next"))

	assert.Empty(t, f.s.deliverInbox("designer", *f.now), "busy: waits")

	f.status("designer", "waiting", *f.now)
	assert.Empty(t, f.s.deliverInbox("designer", *f.now), "a dialog: waits")

	f.status("designer", "busy", *f.now)
	f.rg.ended = map[string]bool{"sid-designer": true}
	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now), "its turn is over, a background shell keeps it busy")
	assert.Len(t, p.posted["sid-designer"], 1)
}

func TestNativeSlashCommandIsTyped(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	*f.now = f.now.Add(3 * time.Minute)
	require.NoError(t, f.s.Queue("designer", "/compact"))

	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	assert.Equal(t, []string{"/compact"}, f.tm.typed["designer"])
	assert.Empty(t, p.posted)
}

func TestNativeUnseenInAnIdleSessionFails(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	*f.now = f.now.Add(3 * time.Minute)
	require.NoError(t, f.s.Queue("designer", "[dispatcher] held somewhere"))
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	*f.now = f.now.Add(postedWait)
	f.s.deliverInbox("designer", *f.now)

	rs := f.s.State.Role("designer")
	require.Len(t, rs.Failed, 1)
	assert.Equal(t, postedNever, rs.Failed[0].Error)
	assert.Empty(t, rs.Posted)
	require.NotEmpty(t, notesOf(f, "designer"))
	assert.Contains(t, notesOf(f, "designer")[0], "delivery failed")
}

func TestNativeLineToAnEndedConversationGoesBack(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	*f.now = f.now.Add(3 * time.Minute)
	require.NoError(t, f.s.Queue("designer", "[dispatcher] read this"))
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

	// The role restarted on a fresh conversation before it read the line.
	f.rg.byPID[f.tm.up["designer"]].SessionID = "sid-designer-2"
	*f.now = f.now.Add(time.Minute)
	assert.Equal(t, "posted to designer: [dispatcher] read this", f.s.deliverInbox("designer", *f.now))
	assert.Equal(t, []string{"[dispatcher] read this"}, p.posted["sid-designer-2"])
	assert.True(t, f.ev.has("designer", "requeued"))
}

func TestNativeFallsBackToTyping(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	*f.now = f.now.Add(3 * time.Minute)

	p.unusable = true
	require.NoError(t, f.s.Queue("designer", "[dispatcher] no socket"))
	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	assert.Equal(t, []string{"[dispatcher] no socket"}, f.tm.typed["designer"], "no inbox socket: typed")

	// A role started before zmina passed its settings holds posted lines behind a dialog: it is typed into.
	p.unusable = false
	f.s.State.Role("designer").Inbound = false
	f.s.State.Role("designer").Sent = nil
	require.NoError(t, f.s.Queue("designer", "[dispatcher] old role"))
	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	assert.Empty(t, p.posted)
}

func TestNativePostErrors(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	*f.now = f.now.Add(3 * time.Minute)
	require.NoError(t, f.s.Queue("designer", "[dispatcher] one"))

	p.err = fmt.Errorf("%w: dial: no such file", peer.ErrUnavailable)
	for range typeTries - 1 {
		assert.Empty(t, f.s.deliverInbox("designer", *f.now), "a post that never started is tried again")
	}

	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	assert.Len(t, f.s.State.Role("designer").Failed, 1, "then kept as failed")

	p.err = &peer.Committed{Err: fmt.Errorf("broken pipe")}
	require.NoError(t, f.s.Queue("designer", "[dispatcher] two"))
	assert.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	assert.Len(t, f.s.State.Role("designer").Failed, 2, "a broken write is kept as failed at once, never retried")
}

func TestNativeDispatcherSignals(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("dispatcher"))
	f.status("dispatcher", "idle", *f.now)
	f.tm.drafts["dispatcher"] = "owner typing"
	*f.now = f.now.Add(3 * time.Minute)

	_, err := f.s.Note("mrs", "acme/site!12 approved", false)
	require.NoError(t, err)

	line := f.s.deliver(*f.now)
	assert.Equal(t, "[zmina] S-1 mrs: acme/site!12 approved (zmina S-1)", line)
	assert.Equal(t, []string{line}, p.posted["sid-dispatcher"])
	assert.Empty(t, f.tm.typed["dispatcher"])
	assert.Empty(t, f.s.State.Signals)

	_, err = f.s.Note("mrs", "acme/site!13 approved", false)
	require.NoError(t, err)
	assert.Empty(t, f.s.deliver(*f.now), "waits until the line before shows")

	p.showAll()
	assert.NotEmpty(t, f.s.deliver(*f.now))
}

func TestNativeDirectSend(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "busy", *f.now)
	*f.now = f.now.Add(3 * time.Minute)

	require.NoError(t, f.s.Send("designer", "look at T-1"))
	assert.Equal(t, []string{"look at T-1"}, p.posted["sid-designer"], "posted even while busy")
	assert.Empty(t, f.tm.typed["designer"])

	require.NoError(t, f.s.Send("designer", "/compact"))
	assert.Equal(t, []string{"/compact"}, f.tm.typed["designer"], "a slash command is typed")
}

func TestNativeCodexGoesToItsBridgeInbox(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML+"  dev:\n    runtime: codex\n")
	cp := newFakePeer()
	f.s.CodexInbox = cp

	r, err := f.s.Role("dev")
	require.NoError(t, err)
	assert.Equal(t, crew.DeliveryNative, f.s.Crew.DeliveryFor(r))
	assert.NotContains(t, f.s.Command(r, ""), "--settings", "a bridge takes no Claude settings")

	_, dir := f.s.channel(r)
	assert.Equal(t, codex.InboxDir(filepath.Join(f.s.Crew.StateDir(), "codex"), r.Session), dir)

	r = &crew.Role{Args: []string{"--settings", "/x.json"}}
	assert.Equal(t, crew.DeliveryTmux, f.s.Crew.DeliveryFor(r), "a Claude role with its own --settings: typed")
}

func TestNativeCodexInboxDelivery(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML+"  dev:\n    runtime: codex\n")
	cp := newFakePeer()
	f.s.CodexInbox = cp

	require.NoError(t, f.s.Up("dev"))
	assert.True(t, f.s.State.Role("dev").Inbound)

	// A Codex role's status comes from its bridge's own record.
	reg := &codex.Registry{Dir: filepath.Join(f.s.Crew.StateDir(), "codex")}
	require.NoError(t, reg.Save(codex.Record{Session: registry.Session{PID: f.tm.up["dev"], SessionID: "thread-1",
		Status: "idle", StatusUpdatedAt: f.now.UnixMilli(), Inbox: "/x"}}))
	*f.now = f.now.Add(3 * time.Minute)

	id, err := f.s.QueueTracked("dev", "[dispatcher] look at T-1")
	require.NoError(t, err)
	assert.Equal(t, "S-1", id)

	assert.Equal(t, "posted to dev: [dispatcher] look at T-1 (zmina S-1)", f.s.deliverInbox("dev", *f.now))
	assert.Equal(t, []string{"[dispatcher] look at T-1 (zmina S-1)"}, cp.posted["thread-1"])
	assert.Empty(t, f.tm.typed["dev"], "nothing typed into the bridge's console")
}
