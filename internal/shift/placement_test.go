package shift

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
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

// A node that holds nothing runs no dispatcher either.
func TestANodeThatHoldsNothingRunsNothing(t *testing.T) {
	f, _ := placedFixture(t, spreadYAML, "gamma")

	assert.Empty(t, f.s.Tick())
	assert.Empty(t, f.tm.starts)
}

// A machine that is no node, and a crew with no nodes:, run every role: the beat is the one it always was.
func TestTickOfACrewThatIsNotSpreadIsUnchanged(t *testing.T) {
	s, tm, _, _ := fixture(t)

	did := s.Tick()

	assert.ElementsMatch(t, []string{"dispatcher", "harvester"}, tm.starts)
	assert.Contains(t, did, "started dispatcher")

	g, _ := placedFixture(t, spreadYAML, "")
	g.s.Tick()
	assert.ElementsMatch(t, []string{"dispatcher", "ops", "qa"}, g.tm.starts, "no node: every keep: always role")
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

	// Even a machine that names itself a node: the crew file declares none.
	s.Node = "alpha"
	assert.Empty(t, s.PlacedAway("harvester"))
}
