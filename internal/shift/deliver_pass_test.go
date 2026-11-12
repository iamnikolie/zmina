package shift

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/inbox"
	"github.com/iamnikolie/zmina/internal/peer"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// tally counts what a shift asks of tmux, the session registry and the inbox sockets.
type tally struct{ calls []string }

func (t *tally) add(call string) { t.calls = append(t.calls, call) }

type countTmux struct {
	Tmux
	t *tally
}

func (c countTmux) Probe(n string) (tmux.Pane, error) {
	c.t.add("tmux Probe")

	return c.Tmux.Probe(n)
}
func (c countTmux) Start(n, d string, e, a []string) error {
	c.t.add("tmux Start")

	return c.Tmux.Start(n, d, e, a)
}
func (c countTmux) Kill(n string) error {
	c.t.add("tmux Kill")

	return c.Tmux.Kill(n)
}
func (c countTmux) Type(n, x string) error {
	c.t.add("tmux Type")

	return c.Tmux.Type(n, x)
}
func (c countTmux) TypeLines(n, x string) error {
	c.t.add("tmux TypeLines")

	return c.Tmux.TypeLines(n, x)
}
func (c countTmux) Keys(n string, k ...string) error {
	c.t.add("tmux Keys")

	return c.Tmux.Keys(n, k...)
}
func (c countTmux) Capture(n string, l int) (string, error) {
	c.t.add("tmux Capture")

	return c.Tmux.Capture(n, l)
}
func (c countTmux) KeepScreens(n string) error {
	c.t.add("tmux KeepScreens")

	return c.Tmux.KeepScreens(n)
}
func (c countTmux) Attached(n string) (bool, error) {
	c.t.add("tmux Attached")

	return c.Tmux.Attached(n)
}
func (c countTmux) Prompt(n string) (string, bool, error) {
	c.t.add("tmux Prompt")

	return c.Tmux.Prompt(n)
}
func (c countTmux) Input(n string) (string, string, bool, error) {
	c.t.add("tmux Input")

	return c.Tmux.Input(n)
}

type countReg struct {
	*fakeReg
	t *tally
}

func (c countReg) ByPID(pid int) (*registry.Session, error) {
	c.t.add("registry ByPID")

	return c.fakeReg.ByPID(pid)
}
func (c countReg) LiveByName(n string) *registry.Session {
	c.t.add("registry LiveByName")

	return c.fakeReg.LiveByName(n)
}
func (c countReg) Last(id string) (registry.Turn, error) {
	c.t.add("registry Last")

	return c.fakeReg.Last(id)
}

type countPeer struct {
	*fakePeer
	t *tally
}

func (c countPeer) Usable(s *registry.Session) bool {
	c.t.add("peer Usable")

	return c.fakePeer.Usable(s)
}
func (c countPeer) Post(ns string, s *registry.Session, x string) (string, error) {
	c.t.add("peer Post")

	return c.fakePeer.Post(ns, s, x)
}
func (c countPeer) Delivered(ns, sid, id string) (bool, error) {
	c.t.add("peer Delivered")

	return c.fakePeer.Delivered(ns, sid, id)
}

// counted puts the counters between the shift and its fakes, from here on.
func counted(f fix, p *fakePeer) *tally {
	t := &tally{}
	f.s.Tmux, f.s.Reg = countTmux{f.tm, t}, countReg{f.rg, t}

	if p != nil {
		f.s.Peer = countPeer{p, t}
	}

	return t
}

func stateJSON(t *testing.T, s *Shift) string {
	t.Helper()

	raw, err := json.Marshal(s.State)
	require.NoError(t, err)

	return string(raw)
}

// typedFixture is a crew of roles typed into, the dev and the designer up, idle and past their boot.
func typedFixture(t *testing.T, body string, roles ...string) fix {
	t.Helper()

	f := fixtureWith(t, body)

	for _, name := range roles {
		require.NoError(t, f.s.Up(name))
	}

	*f.now = f.now.Add(bootPause)

	for _, name := range roles {
		f.status(name, "idle", *f.now)
	}

	return f
}

// A pass gives a queued line to an idle role, as DeliverTo does, and says it changed the state.
func TestDeliverPassGivesAQueuedLineToAnIdleRole(t *testing.T) {
	t.Run("posted", func(t *testing.T) {
		f, p := nativeFixture(t, crewYAML)
		upNative(t, f, "designer")

		id, err := f.s.QueueTracked("designer", "[dev] look at T-1")
		require.NoError(t, err)

		changed, err := f.s.DeliverPass()
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, []string{"[dev] look at T-1 (zmina " + id + ")"}, p.posted["sid-designer"])
		assert.Empty(t, f.s.State.Role("designer").Inbox)
		assert.Len(t, f.s.State.Role("designer").Posted, 1, "followed until its conversation shows it")
	})

	t.Run("typed", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")
		require.NoError(t, f.s.Queue("designer", "hello"))

		changed, err := f.s.DeliverPass()
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, []string{"hello"}, f.tm.typed["designer"])
		assert.Empty(t, f.s.State.Role("designer").Inbox)
	})

	t.Run("the dispatcher its notes", func(t *testing.T) {
		f, p := nativeFixture(t, crewYAML)
		upNative(t, f, "dispatcher")
		upNative(t, f, "designer")

		_, err := f.s.Note("dev", "T-1 result ready", false)
		require.NoError(t, err)
		require.NoError(t, f.s.Queue("designer", "[dev] hi"))

		changed, err := f.s.DeliverPass()
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Len(t, p.posted["sid-dispatcher"], 1, "the dispatcher's signals")
		assert.Len(t, p.posted["sid-designer"], 1, "and a role's line, in the same pass")
		assert.Empty(t, f.s.State.Signals)
	})
}

// A pass that finds nothing waiting asks tmux, the registry and the sockets nothing, and leaves the state as it was,
// whatever lies in it: a line queued for a role that is down, a notice that is only an info line riding.
func TestDeliverPassWithNothingWaitingTouchesNothing(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	upNative(t, f, "designer")
	require.NoError(t, f.s.Queue("harvester", "[dev] for a role that is down"))
	f.s.State.Notes = []string{"dev: started again (keep: always)"}
	f.s.markInfo("dev: started again (keep: always)", *f.now)

	before := stateJSON(t, f.s)
	calls := counted(f, p)

	assert.Empty(t, f.s.Waiting())

	changed, err := f.s.DeliverPass()
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Empty(t, calls.calls, "no tmux, registry or socket call")
	assert.Equal(t, before, stateJSON(t, f.s))
	assert.Empty(t, p.raw)
}

// Waiting is a read of the state and the clock: the roles a pass could give something to, the dispatcher first.
func TestWaitingNamesTheRolesAPassCouldGiveTo(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")
	upNative(t, f, "designer")
	upNative(t, f, "harvester")

	calls := counted(f, p)

	// Every look below asks tmux, the registry and the sockets nothing (the queueing between them may).
	waiting := func() []string {
		calls.calls = nil
		before := stateJSON(t, f.s)
		got := f.s.Waiting()

		assert.Empty(t, calls.calls, "a read of the state: nothing was asked of tmux, the registry or a socket")
		assert.Equal(t, before, stateJSON(t, f.s), "and nothing was changed")

		return got
	}

	assert.Empty(t, waiting())

	require.NoError(t, f.s.Queue("harvester", "[dev] one"))
	require.NoError(t, f.s.Queue("designer", "[dev] two"))
	assert.Equal(t, []string{"designer", "harvester"}, waiting(), "the crew's order")

	_, err := f.s.Note("dev", "T-1 result ready", false)
	require.NoError(t, err)
	assert.Equal(t, []string{"dispatcher", "designer", "harvester"}, waiting(), "the dispatcher first")

	require.NoError(t, f.s.Queue("dispatcher", "[dev] its own"))
	f.s.State.Signals = nil
	assert.Equal(t, []string{"dispatcher", "designer", "harvester"}, waiting(), "its own queue counts too")

	// A role that is not up under zmina cannot be given anything: the beat starts it.
	f.s.State.Role("designer").Up = false
	assert.Equal(t, []string{"dispatcher", "harvester"}, waiting())
	f.s.State.Role("designer").Up = true

	// Nor one a slash command was just typed into, nor one whose line failed a moment ago.
	f.s.State.Role("harvester").SlashAt = *f.now
	assert.Equal(t, []string{"dispatcher", "designer"}, waiting())
	*f.now = f.now.Add(slashSettle)
	assert.Equal(t, []string{"dispatcher", "designer", "harvester"}, waiting())

	rs := f.s.State.Role("designer")
	rs.Attempts, rs.TriedAt = 1, *f.now
	assert.Equal(t, []string{"dispatcher", "harvester"}, waiting())
	*f.now = f.now.Add(countedGap)
	assert.Equal(t, []string{"dispatcher", "designer", "harvester"}, waiting())

	// A role the crew file does not have is not named, and looking makes no state for a role that has none.
	f.s.State.Roles["ghost"] = &RoleState{Up: true, Inbox: []string{"x"}}
	assert.NotContains(t, waiting(), "ghost")

	delete(f.s.State.Roles, "ghost")
	delete(f.s.State.Roles, "harvester")
	assert.Equal(t, []string{"dispatcher", "designer"}, waiting())
	assert.NotContains(t, f.s.State.Roles, "harvester")
}

// Info lines alone wait for a line to go with, as deliver holds them; once they have waited out infoRide they go.
func TestWaitingHoldsInfoLinesAsDeliverDoes(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	f.s.State.Notes = []string{"dev: started again (keep: always)"}
	f.s.markInfo("dev: started again (keep: always)", *f.now)
	assert.Empty(t, f.s.Waiting())

	*f.now = f.now.Add(infoRide)
	assert.Equal(t, []string{"dispatcher"}, f.s.Waiting())

	changed, err := f.s.DeliverPass()
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Len(t, p.posted["sid-dispatcher"], 1)

	// A signal to act on goes at once, with the info line.
	f.s.State.Notes = []string{"dev: started again (keep: always)"}
	f.s.State.Info = nil
	f.s.markInfo("dev: started again (keep: always)", *f.now)
	assert.Empty(t, f.s.Waiting())

	_, err = f.s.Note("dev", "T-2 needs a decision", false)
	require.NoError(t, err)
	assert.Equal(t, []string{"dispatcher"}, f.s.Waiting())
}

// A pass is a delivery and nothing else: no beat is written, no role is started, none is trimmed, no record is
// swept.
func TestDeliverPassIsNoBeat(t *testing.T) {
	f := typedFixture(t, trimCrew, "dev")
	f.status("dev", "idle", f.now.Add(-time.Hour))
	f.rg.context["sid-dev"] = 120_000 // far over its context_max: a beat would clear it
	require.NoError(t, f.s.Queue("dev", "hello"))

	beat := f.now.Add(-30 * time.Second)
	f.s.State.LastTick = beat
	rs := f.s.State.Role("dev")
	started := rs.Started

	// A failed line that is past its age limit: the beat's sweep would drop it and tell the dispatcher.
	old := f.now.Add(-failedMaxAge - time.Hour)
	rs.Failed = []Failed{{Line: "an old one", Error: "typed, never taken", At: old}}

	// A role that is down stays down and keeps its line, a dispatcher that is down is not started.
	require.NoError(t, f.s.Queue("designer", "[dev] hi"))
	require.NoError(t, f.s.Queue("dispatcher", "[dev] hi"))

	changed, err := f.s.DeliverPass()
	require.NoError(t, err)
	assert.True(t, changed)

	assert.Equal(t, []string{"hello"}, f.tm.typed["dev"], "the line, and no /clear")
	assert.Equal(t, []string{"dev"}, f.tm.starts, "no role started but the one started above")
	assert.Equal(t, beat, f.s.State.LastTick, "not a beat")
	assert.True(t, f.s.lastBeat.IsZero())
	assert.True(t, rs.Up)
	assert.Equal(t, started, rs.Started)
	assert.True(t, rs.Trimmed.IsZero(), "nothing trimmed")
	assert.Equal(t, []Failed{{Line: "an old one", Error: "typed, never taken", At: old}}, rs.Failed, "nothing swept")
	assert.Empty(t, f.s.State.Notes, "nobody told of a sweep, a start or a trim")
	assert.Empty(t, f.s.State.Signals)

	assert.False(t, f.s.State.Role("designer").Up, "Up is the beat's to write")
	assert.Equal(t, []string{"[dev] hi"}, f.s.State.Role("designer").Inbox)
	assert.Equal(t, []string{"[dev] hi"}, f.s.State.Role("dispatcher").Inbox)
}

// A pass leaves a busy role's line queued and says nothing changed: the state is what it was.
func TestDeliverPassLeavesABusyRoleItsLine(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	f.status("designer", "busy", *f.now)
	require.NoError(t, f.s.Queue("designer", "[dev] hi"))

	before := stateJSON(t, f.s)

	changed, err := f.s.DeliverPass()
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, before, stateJSON(t, f.s))
	assert.Empty(t, p.raw)
}

// A line tmux refuses is tried again only after countedGap, not by the next pass: the pass names no role for it, so it
// asks tmux nothing either.
func TestDeliverPassHonoursTheCountedGap(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer")
	require.NoError(t, f.s.Queue("designer", "hello"))
	f.tm.typeErr = errNotTyped("tmux: no server")
	rs := f.s.State.Role("designer")

	changed, err := f.s.DeliverPass()
	require.NoError(t, err)
	assert.True(t, changed, "a try counted is a change")
	assert.Equal(t, 1, rs.Attempts)
	assert.Equal(t, *f.now, rs.TriedAt)

	// Seconds later: neither tried nor counted again, and nothing asked of tmux.
	*f.now = f.now.Add(countedGap - time.Second)
	calls := counted(f, nil)
	before := stateJSON(t, f.s)

	assert.Empty(t, f.s.Waiting())

	changed, err = f.s.DeliverPass()
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, 1, rs.Attempts)
	assert.Empty(t, calls.calls)
	assert.Equal(t, before, stateJSON(t, f.s))

	// Past the gap it is tried again, and goes.
	*f.now = f.now.Add(time.Second)
	f.tm.typeErr = nil

	changed, err = f.s.DeliverPass()
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, []string{"hello"}, f.tm.typed["designer"])
	assert.Zero(t, rs.Attempts)
	assert.Empty(t, rs.Inbox)
}

// What waits behind a slash command waits slashSettle; a pass in between asks nothing and changes nothing.
func TestDeliverPassHonoursTheSlashSettle(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer")
	require.NoError(t, f.s.Queue("designer", "/compact"))
	require.NoError(t, f.s.Queue("designer", "hello"))
	rs := f.s.State.Role("designer")

	changed, err := f.s.DeliverPass()
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, []string{"/compact"}, f.tm.typed["designer"])
	assert.Equal(t, *f.now, rs.SlashAt)

	*f.now = f.now.Add(slashSettle - time.Second)
	calls := counted(f, nil)
	before := stateJSON(t, f.s)

	assert.Empty(t, f.s.Waiting())

	changed, err = f.s.DeliverPass()
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Empty(t, calls.calls)
	assert.Equal(t, before, stateJSON(t, f.s))
	assert.Equal(t, []string{"/compact"}, f.tm.typed["designer"])

	*f.now = f.now.Add(time.Second)
	f.status("designer", "idle", *f.now)

	changed, err = f.s.DeliverPass()
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, []string{"/compact", "hello"}, f.tm.typed["designer"])
}

// A state that cannot be written after a line went stops the pass: the roles after it are given nothing, and the
// error is the caller's to fail on.
func TestDeliverPassStopsWhenTheStateCannotBeSaved(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer", "harvester")
	require.NoError(t, f.s.Queue("designer", "one"))
	require.NoError(t, f.s.Queue("harvester", "two"))

	saves := 0
	f.s.Flush = func() error { saves++; return errors.New("disk full") }

	changed, err := f.s.DeliverPass()
	require.ErrorContains(t, err, "disk full")
	assert.True(t, changed)
	assert.Equal(t, 1, saves)
	assert.Equal(t, []string{"one"}, f.tm.typed["designer"])
	assert.Empty(t, f.tm.typed["harvester"])
	assert.Equal(t, []string{"two"}, f.s.State.Role("harvester").Inbox)
}

// A line given is saved at once, as in the beat, and the pass says it changed: the caller's save is then the second.
func TestDeliverPassFlushesWhatWentAtOnce(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer")
	require.NoError(t, f.s.Queue("designer", "one"))

	saves := 0
	f.s.Flush = func() error { saves++; return nil }

	_, err := f.s.DeliverPass()
	require.NoError(t, err)
	assert.Equal(t, 1, saves)
}

// count is how many times a tally saw a call.
func (t *tally) count(call string) int {
	n := 0

	for _, c := range t.calls {
		if c == call {
			n++
		}
	}

	return n
}

// A post to the dispatcher that fails is tried again only after retryGap, not by the next pass: the pass names no role
// for it, asks nothing, logs nothing and says nothing changed. Nothing is counted against the line: the dispatcher's
// signals stay queued however long it takes, and the beat, a minute later, tries again.
func TestDeliverPassHonoursTheRetryGapOfAFailedPostToTheDispatcher(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	_, err := f.s.Note("dev", "T-1 needs a decision", false)
	require.NoError(t, err)

	p.err = errors.New("connection refused")
	calls := counted(f, p)
	rs := f.s.State.Role("dispatcher")
	first := *f.now

	changed, err := f.s.DeliverPass()
	require.NoError(t, err)
	assert.True(t, changed, "a try that failed is a change: the next pass must wait for it")
	assert.Equal(t, 1, calls.count("peer Post"))
	assert.Equal(t, first, rs.DispatchTriedAt)

	// Pass after pass for a second and more: no post, no event, no change, nothing asked.
	calls.calls = nil
	before := stateJSON(t, f.s)

	for range 5 {
		*f.now = f.now.Add(200 * time.Millisecond)
		assert.Empty(t, f.s.Waiting())

		changed, err = f.s.DeliverPass()
		require.NoError(t, err)
		assert.False(t, changed)
	}

	assert.Empty(t, calls.calls)
	assert.Equal(t, 1, f.eventCount("post-failed"))
	assert.Equal(t, before, stateJSON(t, f.s))

	// Past the gap it is tried again.
	*f.now = first.Add(retryGap)
	assert.Equal(t, []string{"dispatcher"}, f.s.Waiting())

	_, err = f.s.DeliverPass()
	require.NoError(t, err)
	assert.Equal(t, 1, calls.count("peer Post"))
	assert.Equal(t, 2, f.eventCount("post-failed"))

	// Nothing is counted: it is no line's failed try, and no line moves to the failed list however often it fails.
	assert.Zero(t, rs.Attempts)
	assert.Empty(t, rs.Failed)
	assert.Len(t, f.s.State.Signals, 1)

	// The beat comes a minute after the try before it: that never waits.
	*f.now = f.now.Add(time.Minute)
	calls.calls = nil
	f.s.Deliver()
	assert.Equal(t, 1, calls.count("peer Post"))

	// The dispatcher takes it again: it goes, and the stamp goes with it.
	*f.now = f.now.Add(time.Minute)
	p.err = nil

	changed, err = f.s.DeliverPass()
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Len(t, p.posted["sid-dispatcher"], 1)
	assert.Empty(t, f.s.State.Signals)
	assert.True(t, rs.TriedAt.IsZero())
}

// A line typed into the dispatcher that tmux refuses is held back the same way.
func TestDeliverPassHonoursTheRetryGapOfAFailedTypeToTheDispatcher(t *testing.T) {
	f := typedFixture(t, crewYAML, "dispatcher")

	_, err := f.s.Note("dev", "T-1 needs a decision", false)
	require.NoError(t, err)

	f.tm.typeErr = errNotTyped("tmux: no server")
	calls := counted(f, nil)
	first := *f.now

	changed, err := f.s.DeliverPass()
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, 1, calls.count("tmux Type"))

	calls.calls = nil
	before := stateJSON(t, f.s)

	for range 5 {
		*f.now = f.now.Add(200 * time.Millisecond)

		changed, err = f.s.DeliverPass()
		require.NoError(t, err)
		assert.False(t, changed)
	}

	assert.Empty(t, calls.calls)
	assert.Equal(t, before, stateJSON(t, f.s))

	*f.now = first.Add(retryGap)
	f.tm.typeErr = nil

	changed, err = f.s.DeliverPass()
	require.NoError(t, err)
	assert.True(t, changed)
	require.Len(t, f.tm.typed["dispatcher"], 1)
	assert.Zero(t, f.s.State.Role("dispatcher").Attempts)
	assert.Empty(t, f.s.State.Role("dispatcher").Failed)
}

// The dispatcher's own queued line keeps its count as it had: one try a beat, the third failed one moves it to the
// failed list, whatever the dispatcher's post before it did to the stamp.
func TestFailedPostsToTheDispatcherStillCountItsOwnQueuedLine(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	_, err := f.s.Note("dev", "T-1 needs a decision", false)
	require.NoError(t, err)
	require.NoError(t, f.s.Queue("dispatcher", "[dev] its own line"))

	p.err = peer.ErrUnavailable
	rs := f.s.State.Role("dispatcher")

	for beat := 1; beat <= typeTries; beat++ {
		f.s.DeliverTo("dispatcher")

		if beat < typeTries {
			assert.Equal(t, beat, rs.Attempts, "beat %d", beat)
			assert.Empty(t, rs.Failed)
		}

		*f.now = f.now.Add(time.Minute)
	}

	require.Len(t, rs.Failed, 1, "the third try failed it")
	assert.Equal(t, "[dev] its own line", rs.Failed[0].Line)
	assert.Empty(t, rs.Inbox)
}

// A queued line over the post limit is stored whole before it is posted; a post that fails and is tried again finds
// that entry, as it must not add one for each try. The one stored is the one the post that goes carries.
func TestFailedPostsOfAnOversizedLineStoreItOnce(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	long := "[dev] " + strings.Repeat("x", postMax+100)
	require.NoError(t, f.s.Queue("dispatcher", long))

	p.err = peer.ErrUnavailable // nothing reached the session: the line stays queued, tried again
	calls := counted(f, p)

	for range 2 {
		_, err := f.s.DeliverPass()
		require.NoError(t, err)

		*f.now = f.now.Add(time.Minute)
	}

	assert.GreaterOrEqual(t, calls.count("peer Post"), 2, "tried each time")
	require.Len(t, f.s.State.Role("dispatcher").Inbox, 1, "still queued")

	store := &inbox.Store{Dir: f.s.Crew.Layout().Inbox()}
	stored := func() []string {
		t.Helper()

		entries, err := store.List()
		require.NoError(t, err)

		var ids []string
		for _, e := range entries {
			ids = append(ids, e.ID)
		}

		return ids
	}

	ids := stored()
	require.Len(t, ids, 1, "stored once")

	e, err := store.Get(ids[0])
	require.NoError(t, err)
	assert.Equal(t, long, e.Body)

	p.err = nil

	_, err = f.s.DeliverPass()
	require.NoError(t, err)
	require.Len(t, p.raw, 1)
	assert.Contains(t, p.raw[0], ids[0], "the post that goes points at it")
	assert.Empty(t, f.s.State.Role("dispatcher").Inbox)
	assert.Equal(t, ids, stored())
}

// A pass notes each role it delivers to in the lock holder record, as the beat does, and a pass that finds nothing
// waiting notes nothing.
func TestDeliverPassMarksTheRoleItDeliversTo(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer", "harvester")

	var marks []string
	f.s.Mark = func(phase, detail string) { marks = append(marks, phase+" "+detail) }

	_, err := f.s.DeliverPass()
	require.NoError(t, err)
	assert.Empty(t, marks, "nothing waiting")

	require.NoError(t, f.s.Queue("harvester", "one"))
	require.NoError(t, f.s.Queue("designer", "two"))

	_, err = f.s.DeliverPass()
	require.NoError(t, err)
	assert.Equal(t, []string{"deliver designer", "deliver harvester"}, marks, "the crew's order")
}

// A line waiting for its ack past the crew's renotify is the beat's to say again, not a pass's: what a pass gives is
// what waits, and the dispatcher's signals stay as they are.
func TestDeliverPassDoesNotRenotify(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	_, err := f.s.Note("dev", "T-5 result ready", false)
	require.NoError(t, err)
	require.NotEmpty(t, f.s.deliver(*f.now))
	p.showAll()
	f.s.deliverInbox("dispatcher", *f.now) // settles the posted line

	*f.now = f.now.Add(f.s.Crew.RenotifyD() + time.Minute)

	// Something else waits, so the pass has work for the dispatcher.
	_, err = f.s.Note("dev", "T-6 result ready", false)
	require.NoError(t, err)

	changed, err := f.s.DeliverPass()
	require.NoError(t, err)
	assert.True(t, changed)
	require.Len(t, p.posted["sid-dispatcher"], 2)
	assert.NotContains(t, p.posted["sid-dispatcher"][1], "T-5", "the first is not said again")
	assert.Zero(t, f.s.State.Acks["S-1"].Renotes)
	assert.Zero(t, f.eventCount("renoted"))

	// The beat does say it again.
	p.showAll()
	f.s.deliverInbox("dispatcher", *f.now)
	f.s.renotify(*f.now)
	assert.Equal(t, 1, f.s.State.Acks["S-1"].Renotes)
}

// A line in flight (posted and not yet shown, typed and not yet landed) with nothing else queued is the beat's to
// follow: a pass names no such role, asks nothing about it and changes nothing, however long the line has been in
// flight.
func TestDeliverPassLeavesALineInFlightAlone(t *testing.T) {
	t.Run("posted", func(t *testing.T) {
		f, p := nativeFixture(t, crewYAML)
		upNative(t, f, "designer")

		_, err := f.s.QueueTracked("designer", "[dev] look at T-1")
		require.NoError(t, err)
		require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

		rs := f.s.State.Role("designer")
		require.Len(t, rs.Posted, 1)
		require.Empty(t, rs.Inbox)

		*f.now = f.now.Add(postedWait + time.Minute) // the beat would give it up as never shown
		before := stateJSON(t, f.s)
		calls := counted(f, p)

		assert.Empty(t, f.s.Waiting())

		changed, err := f.s.DeliverPass()
		require.NoError(t, err)
		assert.False(t, changed)
		assert.Empty(t, calls.calls)
		assert.Equal(t, before, stateJSON(t, f.s))
		assert.Empty(t, rs.Failed)
	})

	t.Run("typed", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")
		require.NoError(t, f.s.Queue("designer", "hello"))
		require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))

		rs := f.s.State.Role("designer")
		require.NotNil(t, rs.Sent)
		require.Empty(t, rs.Inbox)

		*f.now = f.now.Add(confirmAfter + time.Minute) // the beat would press Enter again or give it up
		before := stateJSON(t, f.s)
		calls := counted(f, nil)

		assert.Empty(t, f.s.Waiting())

		changed, err := f.s.DeliverPass()
		require.NoError(t, err)
		assert.False(t, changed)
		assert.Empty(t, calls.calls)
		assert.Equal(t, before, stateJSON(t, f.s))
		assert.Empty(t, rs.Failed)
		assert.Empty(t, f.tm.keys["designer"], "no Enter pressed again")
	})
}

// The gap after a failed post to the dispatcher is its own: it holds also while a queued line of the dispatcher's
// already counts tries, and the count does not move for it.
func TestAFailedPostToTheDispatcherIsHeldWhileItsOwnLineCountsTries(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "dispatcher")

	_, err := f.s.Note("dev", "T-1 needs a decision", false)
	require.NoError(t, err)
	require.NoError(t, f.s.Queue("dispatcher", "[dev] its own line"))

	rs := f.s.State.Role("dispatcher")
	tried := f.now.Add(-countedGap - time.Second)
	rs.Attempts, rs.TriedAt = 1, tried

	f.status("dispatcher", "busy", *f.now)
	f.rg.endedAt = map[string]time.Time{} // mid-turn: its own line is not tried

	p.err = errors.New("connection refused")
	calls := counted(f, p)

	for range 10 {
		*f.now = f.now.Add(500 * time.Millisecond)

		_, err := f.s.DeliverPass()
		require.NoError(t, err)
	}

	assert.Equal(t, 1, calls.count("peer Post"))
	assert.Equal(t, 1, rs.Attempts)
	assert.Equal(t, tried, rs.TriedAt)
}
