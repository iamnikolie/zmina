package shift

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/watch"
)

// spreadYAML is a crew over two nodes: dev and qa run on beta, the rest on home (alpha). ops and qa are kept up.
const spreadYAML = `namespace: /ns
claude: claude
home: alpha
nodes:
  alpha: {}
  beta: {}
roles:
  dispatcher:
    boot: read d and boot
  dev:
    place: beta
  ops:
    keep: always
  docs: {}
  qa:
    place: beta
    keep: always
`

// placedFixture is the fixture of a crew file, read for the node it names, whoever this machine is.
func placedFixture(t *testing.T, body, node string) (fix, *listingTmux) {
	t.Helper()

	f := fixtureWith(t, body)

	c, err := crew.LoadFor(f.s.Crew.Path, node)
	require.NoError(t, err)

	f.s.Crew, f.s.Node = c, node

	lt := &listingTmux{fakeTmux: f.tm, extra: map[string]bool{}}
	f.s.Tmux = lt

	return f, lt
}

// A start of a role the crew file places on another node is refused with one line that names the node, by every way
// that starts a role, and nothing is started or remembered.
func TestStartRefusesARolePlacedOnAnotherNode(t *testing.T) {
	f, _ := placedFixture(t, spreadYAML, "alpha")

	for _, c := range []struct {
		name, role string
		start      func(role string) error
	}{
		{"up", "dev", f.s.Up},
		{"upWith", "dev", func(r string) error { return f.s.UpWith(r, UpOptions{Fresh: true}) }},
		{"upFor", "dev", func(r string) error { return f.s.upFor(r, UpOptions{}) }},
		{"a copy", "dev#2", func(r string) error { return f.s.UpWith(r, UpOptions{}) }},
		{"an alarm's wake", "dev", func(r string) error { return f.s.UpWith(r, UpOptions{Auto: true}) }},
		{"a hand's up with its tasks", "dev", func(r string) error { return f.s.UpHanding(r, UpOptions{}) }},
	} {
		err := c.start(c.role)
		require.Error(t, err, c.name)
		assert.Equal(t, c.role+` is placed on beta, not on this node (alpha): run it there, or zmina tell beta "<text>" to its dispatcher`,
			err.Error(), c.name)
		assert.NotContains(t, err.Error(), "\n", c.name)
	}

	assert.Empty(t, f.tm.starts)
	assert.True(t, f.s.State.Role("dev").Started.IsZero(), "a refusal records no start")
	assert.False(t, f.s.State.Role("dev").Up)

	// The roles that are this node's start as they always did.
	require.NoError(t, f.s.Up("ops"))
	assert.Equal(t, []string{"ops"}, f.tm.starts)
}

func TestTheDispatcherIsRefusedWhereItDoesNotRun(t *testing.T) {
	f, _ := placedFixture(t, "home: alpha\nnodes: {alpha: {}, beta: {}, gamma: {}}\nroles:\n  dispatcher: {}\n  dev: {place: beta}\n", "gamma")

	err := f.s.Up("dispatcher")
	require.Error(t, err)
	assert.Equal(t, "dispatcher runs on alpha, beta (home and the nodes that hold a role), not on this node (gamma)", err.Error())
	assert.Empty(t, f.tm.starts)
}

// The ticker starts the keep: always roles of its own node and the dispatcher, and does nothing at all for a role placed
// elsewhere: no start, no state, no probe-driven bookkeeping.
func TestTickSkipsARolePlacedOnAnotherNode(t *testing.T) {
	f, _ := placedFixture(t, spreadYAML, "alpha")

	did := f.s.Tick()

	assert.ElementsMatch(t, []string{"dispatcher", "ops"}, f.tm.starts, "qa is keep: always but beta's")
	assert.NotContains(t, strings.Join(did, "\n"), "qa")
	assert.True(t, f.s.State.Role("qa").Started.IsZero(), "nothing of a role placed elsewhere is recorded")
	assert.False(t, f.s.State.Role("qa").Up)

	// And the same file on beta: its own two, the dispatcher too (a node that holds a role runs one).
	g, _ := placedFixture(t, spreadYAML, "beta")
	g.s.Tick()

	assert.ElementsMatch(t, []string{"dispatcher", "qa"}, g.tm.starts)
	assert.True(t, g.s.State.Role("ops").Started.IsZero())
}

// A node the file does not list runs nothing: no role, no dispatcher, no detector, and the beat says once a day, in the log, why.
func TestANodeThatIsNotListedRunsNothingAndSaysOncePerDay(t *testing.T) {
	f, _ := placedFixture(t, spreadYAML+"watch:\n  ci-red:\n    kind: exec\n    every: 1m\n    run: [detector]\n    baseline: false\n", "gamma")

	did := f.s.Tick()

	require.Len(t, did, 1)
	assert.Contains(t, did[0], "this node (gamma) is not one of nodes: (alpha, beta)")
	assert.Contains(t, did[0], "runs nothing of this crew (not a role, the dispatcher or a job)")
	assert.Empty(t, f.tm.starts)
	assert.Equal(t, 1, eventsOf(f, "node-not-running"))
	assert.Equal(t, *f.now, f.s.State.LastTick, "the beat still shows that the ticker runs")
	assert.Empty(t, f.s.DueWatches(), "a detector is a job of the crew too")

	// The same watch on a node that is listed is due, so that the line above is the gate's and not an empty list.
	g, _ := placedFixture(t, spreadYAML+"watch:\n  ci-red:\n    kind: exec\n    every: 1m\n    run: [detector]\n    baseline: false\n", "alpha")
	assert.Len(t, g.s.DueWatches(), 1)

	// Said once a day, however many beats; again the next day, and the day before's key goes.
	for range 3 {
		*f.now = f.now.Add(10 * time.Minute)
		assert.Empty(t, f.s.Tick())
	}

	assert.Equal(t, 1, eventsOf(f, "node-not-running"))

	*f.now = f.now.Add(24 * time.Hour)
	assert.Len(t, f.s.Tick(), 1)
	assert.Equal(t, 2, eventsOf(f, "node-not-running"))

	n := 0

	for key := range f.s.State.RecordNotified {
		if strings.HasPrefix(key, notRunningPrefix) {
			n++
		}
	}

	assert.Equal(t, 1, n)

	// Whatever starts a role there says why not.
	assert.Contains(t, f.s.PlacedAway("ops"), "this node (gamma) is not one of nodes:")
	assert.Equal(t, "this node runs nothing of the crew (zmina doctor)", f.s.StartBlock("dev", *f.now))
}

// A machine whose node name cannot be read fails closed: it runs nothing, whatever the file says, where guessing that it is a
// node that runs everything would start every role a second time.
func TestAMachineWhoseNodeNameIsUnknownRunsNothing(t *testing.T) {
	f, _ := placedFixture(t, spreadYAML, "")

	did := f.s.Tick()

	require.Len(t, did, 1)
	assert.Contains(t, did[0], "node name cannot be read")
	assert.Contains(t, did[0], "zmina node init")
	assert.Empty(t, f.tm.starts, "no keep: always role, no dispatcher")
	assert.Empty(t, f.s.DueWatches())

	for _, name := range []string{"dispatcher", "ops", "dev", "qa"} {
		err := f.s.Up(name)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "node name cannot be read", name)
	}

	assert.Empty(t, f.tm.starts)
	assert.Contains(t, f.s.StartBlock("ops", *f.now), "runs nothing of the crew")
}

// A crew with no nodes: runs every role whoever the machine is: the beat is the one it always was.
func TestTickOfACrewThatIsNotSpreadIsUnchanged(t *testing.T) {
	s, tm, _, _ := fixture(t)

	did := s.Tick()

	assert.ElementsMatch(t, []string{"dispatcher", "harvester"}, tm.starts)
	assert.Contains(t, did, "started dispatcher")
	assert.True(t, s.Crew.Runs())

	// Reading the same file for a node of any name changes nothing.
	for _, node := range []string{"", "alpha"} {
		g, _ := placedFixture(t, crewYAML, node)
		assert.True(t, g.s.Crew.Runs(), node)
		g.s.Tick()
		assert.ElementsMatch(t, []string{"dispatcher", "harvester"}, g.tm.starts, node)
	}
}

// A role placed elsewhere that is up here is told to the dispatcher once a day, never taken down; its session is no orphan.
func TestARolePlacedElsewhereThatIsUpHereIsNotedOncePerDayAndKept(t *testing.T) {
	f, lt := placedFixture(t, "reconcile: {orphans: down}\n"+spreadYAML, "alpha")

	f.s.Tick()

	f.tm.up["dev"] = 4001
	f.rg.byPID[4001] = &registry.Session{PID: 4001, Status: "idle", StatusUpdatedAt: f.now.Add(-time.Hour).UnixMilli(), SessionID: "sid-dev"}

	for range 3 {
		*f.now = f.now.Add(10 * time.Minute)
		f.s.Tick()
	}

	assert.Equal(t, 1, eventsOf(f, "placed-elsewhere"), "once, however many beats")
	assert.True(t, f.tm.Has("dev"), "never taken down")
	assert.Contains(t, eventDetails(f, "placed-elsewhere"), "dev is up on this node (alpha), but the crew file places it on beta")

	// Past midnight of the crew's clock the note is told again.
	*f.now = f.now.Add(24 * time.Hour)
	f.s.Tick()
	assert.Equal(t, 2, eventsOf(f, "placed-elsewhere"))

	// The day's keys of days gone do not pile up.
	n := 0

	for key := range f.s.State.RecordNotified {
		if strings.HasPrefix(key, awayPrefix) {
			n++
		}
	}

	assert.Equal(t, 1, n)

	// Not an orphan: it is a role of the crew file, even though this node does not run it, and "orphans: down" took nothing down.
	sessions, err := lt.Sessions()
	require.NoError(t, err)
	assert.Empty(t, f.s.Orphans(sessions))
	assert.Zero(t, eventsOf(f, "orphan"))
	assert.Zero(t, eventsOf(f, "orphan-down"))
	assert.Contains(t, f.s.Names(), "dev", "Names is unchanged: its state is not pruned")
}

func eventDetails(f fix, event string) string {
	var out []string

	for _, e := range f.ev.list {
		if e.Event == event {
			out = append(out, e.Detail)
		}
	}

	return strings.Join(out, "\n")
}

// With reconcile on, a seat placed elsewhere is never wanted up, never started for a task or a line, and the reason says
// where it runs.
func TestReconcileNeverStartsASeatPlacedElsewhere(t *testing.T) {
	f, _ := placedFixture(t, "reconcile: {restart: true, assign: true}\n"+spreadYAML, "alpha")
	f.tk.list = []task.Task{openTask("T-1", "dev"), openTask("T-2", "docs")}

	now := f.s.Now()
	assert.Equal(t, "it is placed on beta, not on this node", f.s.StartBlock("dev", now))
	assert.Empty(t, f.s.wantedUp("dev", f.s.Crew.Roles["dev"], now), "a task held does not want it up here")
	assert.Equal(t, "holds T-2", f.s.wantedUp("docs", f.s.Crew.Roles["docs"], now), "its own seat is wanted up as before")
	assert.Equal(t, keepAlwaysWhy, f.s.wantedUp("ops", f.s.Crew.Roles["ops"], now))
	assert.Empty(t, f.s.wantedUp("qa", f.s.Crew.Roles["qa"], now), "keep: always, and beta's")

	started, why := f.s.StartOnTask(f.tk.list[0])
	assert.False(t, started)
	assert.Equal(t, "it is placed on beta, not on this node", why)
	assert.Equal(t, "it is placed on beta, not on this node", f.s.StartForLine("dev"))

	f.s.Tick()
	f.s.Tick()
	assert.NotContains(t, f.tm.starts, "dev")
	assert.NotContains(t, f.tm.starts, "qa")
	assert.Contains(t, f.tm.starts, "ops")
	assert.Contains(t, f.tm.starts, "docs", "the seat that holds a task of its own is started, as with the key on anywhere")

	// The same with the key off: the four reasons it always had, and the placement first.
	g, _ := placedFixture(t, spreadYAML, "alpha")
	assert.Equal(t, "it is placed on beta, not on this node", g.s.StartBlock("dev", g.s.Now()))
	assert.Empty(t, g.s.StartBlock("ops", g.s.Now()))
}

func TestViewSaysWhereARoleIsPlaced(t *testing.T) {
	f, _ := placedFixture(t, spreadYAML, "alpha")

	by := map[string]View{}
	for _, v := range f.s.Views() {
		by[v.Role] = v
	}

	assert.Equal(t, "alpha", by["ops"].Place)
	assert.False(t, by["ops"].Away)
	assert.Equal(t, "beta", by["dev"].Place)
	assert.True(t, by["dev"].Away)
	assert.Empty(t, by["dispatcher"].Place, "the dispatcher is on every node that holds a role")
	assert.False(t, by["dispatcher"].Away)

	g, _ := placedFixture(t, "home: alpha\nnodes: {alpha: {}, beta: {}, gamma: {}}\nroles:\n  dispatcher: {}\n  dev: {place: beta}\n", "gamma")
	d := g.s.Views()[0]
	assert.Equal(t, "dispatcher", d.Role)
	assert.True(t, d.Away)
	assert.Equal(t, "alpha", d.Place, "home")
}

// A crew with no nodes: shows nothing of placement, in the view or in its JSON.
func TestViewOfACrewThatIsNotSpreadSaysNothingOfPlacement(t *testing.T) {
	s, _, _, _ := fixture(t)

	for _, v := range s.Views() {
		assert.Empty(t, v.Place)
		assert.False(t, v.Away)

		raw, err := json.Marshal(v)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), `"place"`)
		assert.NotContains(t, string(raw), `"away"`)
	}
}

// A role the beat skips is not reported as unanswered when tmux does not answer about it: the beat asks only for the note.
func TestTickDoesNotReportAnAwayRoleTmuxCannotAnswerFor(t *testing.T) {
	f, _ := placedFixture(t, spreadYAML, "alpha")
	f.tm.probeErr["dev"] = assert.AnError

	did := f.s.Tick()

	assert.NotContains(t, strings.Join(did, "\n"), "dev: left alone this beat")
	assert.Zero(t, eventsOf(f, "placed-elsewhere"))
}

// Lines that wait for a role placed elsewhere keep waiting, even with a session of it up here: the beat types into no
// session that is not its own.
func TestTickDeliversNothingToAnAwayRole(t *testing.T) {
	f, _ := placedFixture(t, spreadYAML, "alpha")
	f.s.Tick()

	f.tm.up["dev"] = 4001
	f.rg.byPID[4001] = &registry.Session{PID: 4001, Status: "idle", StatusUpdatedAt: f.now.Add(-time.Hour).UnixMilli(), SessionID: "sid-dev"}
	f.s.enqueue("dev", "a line queued long ago", Facts{From: "zmina"})

	// A session of it that zmina once started here: booted long ago, idle, with an input line.
	rs := f.s.State.Role("dev")
	rs.Up, rs.Started, rs.Booted = true, f.now.Add(-time.Hour), f.now.Add(-time.Hour)

	*f.now = f.now.Add(10 * time.Minute)
	f.s.Tick()

	assert.Empty(t, f.tm.typed["dev"], "nothing typed into it")
	assert.Equal(t, []string{"a line queued long ago"}, f.s.State.Role("dev").Inbox, "and the line waits")
}

// A crew that declares no nodes: starts every role, asks no placement question, and StartBlock has no word of it.
func TestStartOfACrewThatIsNotSpreadIsUnchanged(t *testing.T) {
	s, tm, _, _ := fixture(t)

	for _, name := range s.Names() {
		assert.Empty(t, s.PlacedAway(name), name)
		assert.Empty(t, s.StartBlock(name, s.Now()), name)
	}

	require.NoError(t, s.Up("designer"))
	assert.Equal(t, []string{"designer"}, tm.starts)

	// Read for a node of any name, or none, the file that declares no nodes places every role here (Shift.Node is not what decides).
	for _, node := range []string{"", "alpha"} {
		g, _ := placedFixture(t, crewYAML, node)

		for _, name := range g.s.Names() {
			assert.Empty(t, g.s.PlacedAway(name), node+" "+name)
			assert.Empty(t, g.s.StartBlock(name, g.s.Now()), node+" "+name)
		}
	}
}

// A role up here that another node lately told up too is one role on two nodes: an event and one note to the dispatcher a day,
// nothing taken down. A role up on one node only, the dispatcher, and a role a peer does not hold are not.
func TestARoleUpOnTwoNodesIsAnEventAndOneNoteADay(t *testing.T) {
	f, _ := placedFixture(t, spreadYAML, "alpha")
	f.s.Tick()

	// ops is home's and up here; beta tells it up too, and the dispatcher, and docs, which is not up here.
	f.tm.up["ops"] = 4002
	f.rg.byPID[4002] = &registry.Session{PID: 4002, Status: "idle", StatusUpdatedAt: f.now.Add(-time.Hour).UnixMilli(), SessionID: "sid-ops"}

	asked := 0
	f.s.PeerHeld = func() map[string][]string {
		asked++

		return map[string][]string{"ops": {"beta"}, "dispatcher": {"beta"}, "docs": {"beta"}}
	}

	for range 3 {
		*f.now = f.now.Add(10 * time.Minute)
		f.s.Tick()
	}

	assert.Equal(t, 1, eventsOf(f, "role-twice"), "once a day, however many beats")
	assert.Equal(t, "ops is up on this node (alpha) and on beta: one role on two nodes; zmina down ops on the one the crew file does not place it on (it places it on alpha)",
		eventDetails(f, "role-twice"))
	assert.True(t, f.tm.Has("ops"), "nothing is taken down")
	assert.True(t, f.s.State.RecordNotified[twicePrefix+"ops/beta/"+f.now.Format(time.DateOnly)])
	assert.Zero(t, eventsOf(f, "orphan-down"))

	// Past the day it is told again, and the old day's key goes.
	*f.now = f.now.Add(24 * time.Hour)
	f.s.Tick()
	assert.Equal(t, 2, eventsOf(f, "role-twice"))

	n := 0

	for key := range f.s.State.RecordNotified {
		if strings.HasPrefix(key, twicePrefix) {
			n++
		}
	}

	assert.Equal(t, 1, n)

	// Once the other node says it no longer holds it, there is nothing to tell.
	f.s.PeerHeld = func() map[string][]string { return nil }
	*f.now = f.now.Add(24 * time.Hour)
	f.s.Tick()
	assert.Equal(t, 2, eventsOf(f, "role-twice"))
}

// Only a node the crew file lists is a witness of a role up there: a peer of the mesh that is none of nodes: may say what it likes, and
// what it says neither makes a note nor uses up the day's key for a real duplicate later on.
func TestRoleTwiceCountsOnlyNodesTheFileLists(t *testing.T) {
	f, _ := placedFixture(t, spreadYAML, "alpha")
	f.s.Tick()

	f.tm.up["ops"] = 4002
	f.rg.byPID[4002] = &registry.Session{PID: 4002, Status: "idle", StatusUpdatedAt: f.now.Add(-time.Hour).UnixMilli(), SessionID: "sid-ops"}

	// A peer that lists every role as up, and is no node of the crew.
	f.s.PeerHeld = func() map[string][]string {
		return map[string][]string{"ops": {"mallory"}, "docs": {"mallory"}, "dev": {"mallory"}}
	}

	*f.now = f.now.Add(10 * time.Minute)
	f.s.Tick()
	assert.Zero(t, eventsOf(f, "role-twice"), "a peer that is no node of the crew is no evidence")

	for key := range f.s.State.RecordNotified {
		assert.False(t, strings.HasPrefix(key, twicePrefix), "no key was made: %s", key)
	}

	// The same day, the liar still saying it, a real duplicate on beta: it is told.
	f.s.PeerHeld = func() map[string][]string { return map[string][]string{"ops": {"mallory", "beta"}} }

	*f.now = f.now.Add(10 * time.Minute)
	f.s.Tick()
	assert.Equal(t, 1, eventsOf(f, "role-twice"))
	assert.Contains(t, eventDetails(f, "role-twice"), "and on beta:", "only the node the file lists is named")
	assert.NotContains(t, eventDetails(f, "role-twice"), "mallory")

	// And this node itself is no other node.
	f.s.PeerHeld = func() map[string][]string { return map[string][]string{"docs": {"alpha"}} }

	f.tm.up["docs"] = 4005
	f.rg.byPID[4005] = &registry.Session{PID: 4005, Status: "idle", SessionID: "sid-docs"}

	*f.now = f.now.Add(10 * time.Minute)
	f.s.Tick()
	assert.Equal(t, 1, eventsOf(f, "role-twice"))
}

// The note is made once a day for a role and the nodes it is up on: another set of nodes is another duplicate, and is told, the order in
// which the peers are listed is none.
func TestRoleTwiceNoteIsKeyedByTheNodeSet(t *testing.T) {
	body := strings.Replace(spreadYAML, "  beta: {}\n", "  beta: {}\n  gamma: {}\n", 1)
	f, _ := placedFixture(t, body, "alpha")
	f.s.Tick()

	f.tm.up["ops"] = 4002
	f.rg.byPID[4002] = &registry.Session{PID: 4002, Status: "idle", StatusUpdatedAt: f.now.Add(-time.Hour).UnixMilli(), SessionID: "sid-ops"}

	held := map[string][]string{"ops": {"beta"}}
	f.s.PeerHeld = func() map[string][]string { return held }

	*f.now = f.now.Add(10 * time.Minute)
	f.s.Tick()
	assert.Equal(t, 1, eventsOf(f, "role-twice"))

	// Gamma too: a different set, told.
	held = map[string][]string{"ops": {"gamma", "beta"}}

	*f.now = f.now.Add(10 * time.Minute)
	f.s.Tick()
	assert.Equal(t, 2, eventsOf(f, "role-twice"))
	assert.Contains(t, eventDetails(f, "role-twice"), "and on beta, gamma")

	// The same set in another order is not a new duplicate, and neither is a peer listed twice.
	held = map[string][]string{"ops": {"beta", "gamma", "beta"}}

	*f.now = f.now.Add(10 * time.Minute)
	f.s.Tick()
	assert.Equal(t, 2, eventsOf(f, "role-twice"))

	day := f.now.Format(time.DateOnly)
	assert.True(t, f.s.State.RecordNotified[twicePrefix+"ops/beta,gamma/"+day])
	assert.True(t, f.s.State.RecordNotified[twicePrefix+"ops/beta/"+day])
}

func TestRoleTwiceIsQuietWithoutPeersOrNodes(t *testing.T) {
	// A crew that declares no nodes asks nobody, and the seam is not set for it.
	s, tm, rg, _ := fixture(t)
	require.Nil(t, s.PeerHeld)

	s.PeerHeld = func() map[string][]string { t.Fatal("asked"); return nil }

	tm.up["designer"] = 4003
	rg.byPID[4003] = &registry.Session{PID: 4003, Status: "idle", SessionID: "sid-designer"}
	s.Tick()

	// A spread crew with no seam (a machine with no address book) says nothing.
	f, _ := placedFixture(t, spreadYAML, "alpha")
	f.tm.up["ops"] = 4004
	f.s.Tick()
	assert.Zero(t, eventsOf(f, "role-twice"))
}

// The state of a role placed elsewhere follows the probe, both ways, because what the hello tells as held is made from it: a
// session up here is told, and a session that ended is no longer told (a stale "up" would make a double run that is not one).
func TestUpOfARolePlacedElsewhereFollowsTheProbeBothWays(t *testing.T) {
	f, _ := placedFixture(t, spreadYAML, "alpha")
	f.s.Tick()
	assert.False(t, f.s.State.Role("dev").Up)

	// Up here, never started by this node's ticker: told.
	f.tm.up["dev"] = 4010
	f.rg.byPID[4010] = &registry.Session{PID: 4010, Status: "idle", StatusUpdatedAt: f.now.Add(-time.Hour).UnixMilli(), SessionID: "sid-dev"}
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	assert.True(t, f.s.State.Role("dev").Up, "up here, so held")

	// Its session ends on its own: no longer told, and nothing of the beat's ended-role handling ran for it.
	delete(f.tm.up, "dev")
	*f.now = f.now.Add(time.Minute)
	did := f.s.Tick()
	assert.False(t, f.s.State.Role("dev").Up, "gone, so not held any more")
	assert.NotContains(t, strings.Join(did, "\n"), "dev: ended")

	// A role that was in the state as up, and is not up, is not held; one that is up and was never in it is.
	f.s.State.Role("qa").Up = true
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	assert.False(t, f.s.State.Role("qa").Up)

	// A tmux that does not answer changes nothing.
	f.s.State.Role("qa").Up = true
	f.tm.probeErr["qa"] = assert.AnError
	*f.now = f.now.Add(time.Minute)
	f.s.Tick()
	assert.True(t, f.s.State.Role("qa").Up)
}

// A role placed elsewhere that still holds open work or queued lines on this node's ledger is told once: the beat does nothing
// for it here. The note names zmina move for the next time. It is made again only after the role has held nothing.
func TestARolePlacedElsewhereThatHoldsWorkIsNotedOnce(t *testing.T) {
	f, _ := placedFixture(t, spreadYAML, "alpha")
	f.tk.list = []task.Task{openTask("T-14", "dev"), openTask("T-15", "docs")}
	f.s.enqueue("dev", "a line queued earlier", Facts{From: "zmina"})
	f.s.enqueue("qa", "another", Facts{From: "zmina"})
	f.s.enqueue("qa", "and another", Facts{From: "zmina"})

	for range 3 {
		*f.now = f.now.Add(10 * time.Minute)
		f.s.Tick()
	}

	assert.Equal(t, 2, eventsOf(f, "placed-elsewhere-work"), "dev and qa, once each however many beats; docs is this node's")

	text := eventDetails(f, "placed-elsewhere-work")
	assert.Contains(t, text, "dev holds T-14 and 1 queued line on this node (alpha), but the crew file places it on beta, so nothing here works on it: "+
		"park what it holds (zmina task park <id>) and give the work to beta (zmina tell beta")
	assert.Contains(t, text, "qa holds 2 queued lines on this node (alpha)")
	assert.Contains(t, text, "A role moved with zmina move takes its work along")
	assert.NotContains(t, text, "docs holds")

	// It held something and holds nothing now: the key goes, and holding again is told again.
	f.tk.list = []task.Task{openTask("T-15", "docs")}
	f.s.State.Role("dev").Inbox = nil
	*f.now = f.now.Add(10 * time.Minute)
	f.s.Tick()
	assert.False(t, f.s.State.RecordNotified[workPrefix+"dev"])

	f.tk.list = []task.Task{openTask("T-6", "dev")}
	*f.now = f.now.Add(10 * time.Minute)
	f.s.Tick()
	assert.Equal(t, 3, eventsOf(f, "placed-elsewhere-work"))
}

// A restart, an own restart and a sleep of a role placed elsewhere that is up here are refused before anything is put down: the
// start that follows would be refused, and the role left down. down itself works on it.
func TestRestartOfARolePlacedElsewhereIsRefusedUpFrontAndDownWorks(t *testing.T) {
	f, _ := placedFixture(t, spreadYAML, "alpha")
	f.tm.up["dev"] = 4011
	f.rg.byPID[4011] = &registry.Session{PID: 4011, Status: "idle", StatusUpdatedAt: f.now.Add(-time.Hour).UnixMilli(), SessionID: "sid-dev"}

	_, _, err := f.s.BeginRotationDown("dev", KindManual, DownOptions{Force: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dev is placed on beta, not on this node (alpha)")
	assert.True(t, f.tm.Has("dev"), "nothing was put down")
	assert.True(t, f.s.State.Role("dev").Downing.IsZero(), "no down was begun")

	_, _, _, err = f.s.ClaimOwnRestart("dev", Own{}, false, false, false)
	require.Error(t, err)

	f.s.Alarms = &alarm.Store{Dir: f.s.Crew.Layout().Alarms()}

	_, _, err = f.s.Sleep("dev", *f.now, "1h", "", DownOptions{Force: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dev is placed on beta")
	assert.True(t, f.tm.Has("dev"))

	// The owner's way out of a double run is a plain down, and it works.
	require.NoError(t, f.s.Down("dev", DownOptions{Force: true}))
	assert.False(t, f.tm.Has("dev"))
}

// A detector runs on one node of a crew over nodes: the beat begins it on home (or the node it names) and on no other, a
// run by hand elsewhere is refused with the node's name, and the other node does not show a next run for it.
func TestAWatchRunsOnTheNodeItIsPlacedOn(t *testing.T) {
	body := spreadYAML + "watch:\n  ci-red:\n    kind: exec\n    every: 1m\n    run: [detector]\n" +
		"  cards:\n    kind: exec\n    every: 1m\n    run: [detector]\n    place: beta\n"

	a, _ := placedFixture(t, body, "alpha")
	b, _ := placedFixture(t, body, "beta")

	due := func(f fix) []string {
		var names []string
		for _, r := range f.s.DueWatches() {
			names = append(names, r.Watch.Name)
		}

		return names
	}

	assert.Equal(t, []string{"ci-red"}, due(a))
	assert.Equal(t, []string{"cards"}, due(b))

	_, err := a.s.BeginWatch("cards")
	require.EqualError(t, err, "watch cards is placed on beta, not on this node (alpha): run it there")

	_, err = b.s.BeginWatch("ci-red")
	require.EqualError(t, err, "watch ci-red is placed on alpha, not on this node (beta): run it there")
	assert.Nil(t, b.s.State.Watch["ci-red"], "a refused run leaves no state")

	views := map[string]WatchView{}
	for _, v := range b.s.WatchViews() {
		views[v.Name] = v
	}

	assert.True(t, views["ci-red"].Away)
	assert.Equal(t, "alpha", views["ci-red"].Place)
	assert.True(t, views["ci-red"].NextRun.IsZero())
	assert.False(t, views["cards"].Away)
	assert.Equal(t, "beta", views["cards"].Place)

	// A dry run runs the source too, and forgetting here would change nothing where the watch runs.
	_, err = a.s.PeekWatch("cards")
	require.EqualError(t, err, "watch cards is placed on beta, not on this node (alpha): run it there")

	_, err = a.s.ForgetWatch("cards", "")
	require.EqualError(t, err, "watch cards is placed on beta, not on this node (alpha): run it there")

	// A machine that is no node of the file runs none, by hand either, even when a watch names it.
	g, _ := placedFixture(t, body+"  lost:\n    kind: exec\n    every: 1m\n    run: [detector]\n    place: gamma\n", "gamma")
	for _, name := range []string{"ci-red", "lost"} {
		_, err = g.s.BeginWatch(name)
		require.ErrorContains(t, err, "this machine is no node of the crew file", name)
	}

	assert.Empty(t, g.s.DueWatches())
}

// A watch placed on a node that holds no role has no dispatcher to hear it: it runs on no node, and both say why. The reason is that
// node's, not the file's: a watch on home and one on a node that holds a role are heard, are not warned of, and run there.
func TestAWatchOnANodeWithNoDispatcherRunsNowhere(t *testing.T) {
	body := "namespace: /ns\nclaude: claude\nhome: alpha\nnodes:\n  alpha: {}\n  beta: {}\n  gamma: {}\nroles:\n  dispatcher:\n    boot: b\n" +
		"  ops: {}\n  dev:\n    place: gamma\n" +
		"watch:\n  cards:\n    kind: exec\n    every: 1m\n    run: [detector]\n    place: beta\n" +
		"  mrs:\n    kind: exec\n    every: 1m\n    run: [detector]\n    place: gamma\n" +
		"  ci-red:\n    kind: exec\n    every: 1m\n    run: [detector]\n"

	due := func(f fix) []string {
		var names []string
		for _, r := range f.s.DueWatches() {
			names = append(names, r.Watch.Name)
		}

		return names
	}

	for node, want := range map[string][]string{"alpha": {"ci-red"}, "beta": nil, "gamma": {"mrs"}} {
		f, _ := placedFixture(t, body, node)
		warnings := strings.Join(f.s.Crew.Warnings, "\n")

		assert.False(t, f.s.Crew.Holds("beta"), "beta holds no role: ops is on home, dev on gamma")
		assert.True(t, f.s.Crew.Holds("gamma"), node)
		assert.Equal(t, want, due(f), node)

		assert.Contains(t, warnings,
			"watch cards: place beta holds no role of the crew, so no dispatcher runs there to hear it; the watch runs on no node", node)
		assert.Equal(t, 1, strings.Count(warnings, "holds no role"), "only the watch on beta is warned of: %s", warnings)

		_, err := f.s.BeginWatch("cards")
		require.ErrorContains(t, err, "no dispatcher runs there: it runs on no node", node)
	}
}

// What a node kept of a watch goes when the file places the watch on another node, but for the keys that still wait on
// an ack: those stay, reminded of and ackable here, and the rest goes with the last of them. A result that comes in
// after the watch was placed elsewhere is not applied.
func TestTheNodeAWatchLeftKeepsOnlyWhatWaitsOnAnAck(t *testing.T) {
	here := "watch:\n  ci-red:\n    kind: exec\n    every: 1m\n    run: [detector]\n    baseline: false\n" +
		"  quiet:\n    kind: exec\n    every: 1m\n    run: [detector]\n    baseline: false\n"
	away := "watch:\n  ci-red:\n    kind: exec\n    every: 1m\n    run: [detector]\n    baseline: false\n    place: beta\n" +
		"  quiet:\n    kind: exec\n    every: 1m\n    run: [detector]\n    baseline: false\n    place: beta\n"

	f, _ := placedFixture(t, spreadYAML+here, "alpha")

	for _, name := range []string{"ci-red", "quiet"} {
		run, err := f.s.BeginWatch(name)
		require.NoError(t, err)
		f.s.FinishWatch(run, watch.Result{Events: []watch.Event{{Key: "k1", Fingerprint: "a", Text: "red"}}}, false)
	}

	_, err := f.s.AckWatch("quiet", nil, true, "dispatcher")
	require.NoError(t, err)
	require.Len(t, f.s.WaitingKeys(), 1)

	f.s.State.Watch["ci-red"].Failures, f.s.State.Watch["ci-red"].LastError = 4, "boom"

	// A run begun before the file changed ends after it.
	run, err := f.s.BeginWatch("ci-red")
	require.NoError(t, err)

	c, err := crew.LoadFor(rewrite(t, f, spreadYAML+away), "alpha")
	require.NoError(t, err)

	f.s.Crew = c

	did := f.s.FinishWatch(run, watch.Result{Events: []watch.Event{{Key: "k2", Fingerprint: "b", Text: "new"}}}, false)
	require.Len(t, did, 1)
	assert.Contains(t, did[0], "placed on another node while it ran; result discarded")

	f.s.PruneWatches()

	assert.Nil(t, f.s.State.Watch["quiet"], "nothing of it waited: all of it goes")
	require.NotNil(t, f.s.State.Watch["ci-red"])
	assert.Zero(t, f.s.State.Watch["ci-red"].Failures)
	assert.Empty(t, f.s.State.Watch["ci-red"].LastError)

	waits := f.s.WaitingKeys()
	require.Len(t, waits, 1)
	assert.Equal(t, "k1", waits[0].Key)

	_, err = f.s.AckWatch("ci-red", nil, true, "dispatcher")
	require.NoError(t, err)

	f.s.PruneWatches()
	assert.Empty(t, f.s.State.Watch)

	// A machine that is no node of the file drops nothing: its name may only be unreadable for a moment.
	g, _ := placedFixture(t, spreadYAML+here, "alpha")
	run, err = g.s.BeginWatch("quiet")
	require.NoError(t, err)
	g.s.FinishWatch(run, watch.Result{Events: []watch.Event{{Key: "k1", Fingerprint: "a", Text: "red"}}}, false)
	_, err = g.s.AckWatch("quiet", nil, true, "dispatcher")
	require.NoError(t, err)

	c, err = crew.LoadFor(rewrite(t, g, spreadYAML+away), "")
	require.NoError(t, err)

	g.s.Crew = c
	g.s.PruneWatches()
	assert.NotNil(t, g.s.State.Watch["quiet"])
}

// A key whose note a task carries is acked, and the node a watch left records that when it prunes: the run that would have recorded
// it is another node's now. So the state goes once nothing waits, though a key a task carries is still waiting in the raw state, and
// what is not carried stays.
func TestTheNodeAWatchLeftRecordsTheAckOfATaskAndDropsItsState(t *testing.T) {
	here := "watch:\n  ci-red:\n    kind: exec\n    every: 1m\n    run: [detector]\n    baseline: false\n"
	away := here + "    place: beta\n"

	f, _ := placedFixture(t, spreadYAML+here, "alpha")

	run, err := f.s.BeginWatch("ci-red")
	require.NoError(t, err)
	f.s.FinishWatch(run, watch.Result{Events: []watch.Event{
		{Key: "ci-red/k1", Fingerprint: "a", Text: "red"}, {Key: "ci-red/k2", Fingerprint: "a", Text: "red again"},
	}}, false)
	require.Len(t, f.s.WaitingKeys(), 2)

	// The dispatcher turned the first note into a task, which acks it; nothing has run since to record that.
	f.tk.list = []task.Task{{ID: "T-1", Role: "dev", State: task.Open, Created: f.now.Add(time.Minute),
		Asked: "the red build (watch key ci-red/ci-red/k1)"}}
	require.Len(t, f.s.WaitingKeys(), 1, "a key a task carries is not waiting for the dispatcher")
	require.Len(t, f.s.State.Watch["ci-red"].Waiting(), 2, "though the state still says it waits")

	c, err := crew.LoadFor(rewrite(t, f, spreadYAML+away), "alpha")
	require.NoError(t, err)

	f.s.Crew = c
	f.s.PruneWatches()

	st := f.s.State.Watch["ci-red"]
	require.NotNil(t, st, "k2 still waits: its state stays")
	assert.Equal(t, []string{"ci-red/k2"}, st.Waiting())
	assert.Equal(t, "task T-1", st.Keys["ci-red/k1"].AckedBy, "the ack of the task is kept")
	assert.True(t, f.ev.has("watch:ci-red", "watch-ack"))

	// The last that waited is acked by hand: the state goes.
	_, err = f.s.AckWatch("ci-red", nil, true, "dispatcher")
	require.NoError(t, err)

	f.s.PruneWatches()
	assert.Empty(t, f.s.State.Watch)

	// Nothing waits but a key a task carries: the state goes at the first prune, with nobody to ack anything.
	g, _ := placedFixture(t, spreadYAML+here, "alpha")

	run, err = g.s.BeginWatch("ci-red")
	require.NoError(t, err)
	g.s.FinishWatch(run, watch.Result{Events: []watch.Event{{Key: "ci-red/k1", Fingerprint: "a", Text: "red"}}}, false)
	g.tk.list = []task.Task{{ID: "T-1", Role: "dev", State: task.Open, Created: g.now.Add(time.Minute),
		Asked: "the red build (watch key ci-red/ci-red/k1)"}}

	c, err = crew.LoadFor(rewrite(t, g, spreadYAML+away), "alpha")
	require.NoError(t, err)

	g.s.Crew = c
	g.s.PruneWatches()
	assert.NotContains(t, g.s.State.Watch, "ci-red", "a watch that returns later starts from nothing, not from a wait that was over")

	// A machine that is no node of the file drops and acks nothing.
	h, _ := placedFixture(t, spreadYAML+here, "alpha")

	run, err = h.s.BeginWatch("ci-red")
	require.NoError(t, err)
	h.s.FinishWatch(run, watch.Result{Events: []watch.Event{{Key: "ci-red/k1", Fingerprint: "a", Text: "red"}}}, false)
	h.tk.list = []task.Task{{ID: "T-1", Role: "dev", State: task.Open, Created: h.now.Add(time.Minute),
		Asked: "the red build (watch key ci-red/ci-red/k1)"}}

	c, err = crew.LoadFor(rewrite(t, h, spreadYAML+away), "")
	require.NoError(t, err)

	h.s.Crew = c
	h.s.PruneWatches()
	require.Contains(t, h.s.State.Watch, "ci-red")
	assert.Equal(t, []string{"ci-red/k1"}, h.s.State.Watch["ci-red"].Waiting())
}

// A crew that is not spread runs its watches as before, and its views say nothing of nodes.
func TestAWatchOfACrewThatIsNotSpreadIsUnchanged(t *testing.T) {
	f := fixtureWith(t, watchYAML)

	require.Len(t, f.s.DueWatches(), 1)

	for _, v := range f.s.WatchViews() {
		assert.False(t, v.Away)
		assert.Empty(t, v.Place)
	}
}

// rewrite puts body in place of the fixture's crew file and returns its path.
func rewrite(t *testing.T, f fix, body string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(f.s.Crew.Path, []byte(body), 0o600))

	return f.s.Crew.Path
}
