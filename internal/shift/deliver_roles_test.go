package shift

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A pass over some roles tries those and no others: what waits for a role left out stays queued, and nothing is asked
// about it.
func TestDeliverRolesTriesOnlyTheRolesNamed(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer", "harvester")
	require.NoError(t, f.s.Queue("designer", "one"))
	require.NoError(t, f.s.Queue("harvester", "two"))

	got, err := f.s.DeliverRoles([]string{"harvester"})
	require.NoError(t, err)
	assert.Equal(t, []RoleDelivery{{Role: "harvester", Changed: true}}, untimed(got))
	assert.Equal(t, []string{"two"}, f.tm.typed["harvester"])
	assert.Empty(t, f.tm.typed["designer"])
	assert.Equal(t, []string{"one"}, f.s.State.Role("designer").Inbox, "left as it was")

	// An empty list is no pass: nothing asked, nothing reported.
	calls := counted(f, nil)
	before := stateJSON(t, f.s)

	got, err = f.s.DeliverRoles(nil)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Empty(t, calls.calls)
	assert.Equal(t, before, stateJSON(t, f.s))

	// The one left out goes when it is named, and the one that went is not given again.
	got, err = f.s.DeliverRoles([]string{"designer", "harvester"})
	require.NoError(t, err)
	assert.Equal(t, []RoleDelivery{{Role: "designer", Changed: true}}, untimed(got))
	assert.Equal(t, []string{"one"}, f.tm.typed["designer"])
	assert.Equal(t, []string{"two"}, f.tm.typed["harvester"])
}

// The report says which role changed the state: one that cannot take its line is asked about and changes nothing, the
// one after it that can goes. Each is compared against the state as it was just before it, not against the pass's start.
func TestDeliverRolesSaysWhichRoleChangedTheState(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	upNative(t, f, "harvester")
	f.status("designer", "busy", *f.now)
	require.NoError(t, f.s.Queue("designer", "[dev] one"))
	require.NoError(t, f.s.Queue("harvester", "[dev] two"))

	calls := counted(f, p)
	before := stateJSON(t, f.s)

	got, err := f.s.DeliverRoles([]string{"designer"})
	require.NoError(t, err)
	assert.Equal(t, []RoleDelivery{{Role: "designer", Changed: false}}, untimed(got))
	assert.NotEmpty(t, calls.calls, "the busy role was asked about")
	assert.Equal(t, before, stateJSON(t, f.s))
	assert.Empty(t, p.raw)

	got, err = f.s.DeliverRoles([]string{"harvester", "designer"})
	require.NoError(t, err)
	assert.Equal(t, []RoleDelivery{{Role: "designer", Changed: false}, {Role: "harvester", Changed: true}}, untimed(got),
		"Waiting's order, each against the state just before it")
	assert.Len(t, p.posted["sid-harvester"], 1)
	assert.Empty(t, p.posted["sid-designer"])

	// The blocked role frees up: it goes, and it is said to have changed the state.
	f.status("designer", "idle", *f.now)

	got, err = f.s.DeliverRoles([]string{"designer"})
	require.NoError(t, err)
	assert.Equal(t, []RoleDelivery{{Role: "designer", Changed: true}}, untimed(got))
	assert.Len(t, p.posted["sid-designer"], 1)
}

// Each role is compared with the state just before it: a role that changed nothing after one that did is not blamed for
// that one's change.
func TestDeliverRolesComparesEachRoleWithTheStateJustBeforeIt(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")
	upNative(t, f, "harvester")
	f.status("harvester", "busy", *f.now)
	require.NoError(t, f.s.Queue("designer", "[dev] one"))
	require.NoError(t, f.s.Queue("harvester", "[dev] two"))

	got, err := f.s.DeliverRoles(f.s.Waiting())
	require.NoError(t, err)
	assert.Equal(t, []RoleDelivery{{Role: "designer", Changed: true}, {Role: "harvester", Changed: false}}, untimed(got))
	assert.Len(t, p.posted["sid-designer"], 1)
	assert.Empty(t, p.posted["sid-harvester"])
}

// A change that is not in the role's own record counts too: the dispatcher's delivery takes the signals and notes of the
// state, and a try that failed is counted.
func TestDeliverRolesCountsAChangeOutsideTheRolesOwnRecord(t *testing.T) {
	t.Run("the dispatcher takes the signals", func(t *testing.T) {
		f, p := nativeFixture(t, crewYAML)
		upNative(t, f, "dispatcher")

		_, err := f.s.Note("dev", "T-1 result ready", false)
		require.NoError(t, err)
		require.NotEmpty(t, f.s.State.Signals)

		got, err := f.s.DeliverRoles([]string{"dispatcher"})
		require.NoError(t, err)
		assert.Equal(t, []RoleDelivery{{Role: "dispatcher", Changed: true}}, untimed(got))
		assert.Empty(t, f.s.State.Signals)
		assert.Len(t, p.posted["sid-dispatcher"], 1)
	})

	t.Run("a try that failed", func(t *testing.T) {
		f := typedFixture(t, crewYAML, "designer")
		require.NoError(t, f.s.Queue("designer", "hello"))
		f.tm.typeErr = errors.New("tmux: no server")

		got, err := f.s.DeliverRoles([]string{"designer"})
		require.NoError(t, err)
		assert.Equal(t, []RoleDelivery{{Role: "designer", Changed: true}}, untimed(got))
		assert.Equal(t, 1, f.s.State.Role("designer").Attempts)
	})
}

// A name Waiting does not name is skipped, whatever the caller meant: a role that is down, one a failed line holds
// back, one the crew file has not got. Nothing is asked and nothing is reported for them.
func TestDeliverRolesSkipsWhatWaitingDoesNotName(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer", "harvester")
	require.NoError(t, f.s.Queue("designer", "one"))
	require.NoError(t, f.s.Queue("harvester", "two"))

	f.s.State.Role("designer").Up = false
	rs := f.s.State.Role("harvester")
	rs.Attempts, rs.TriedAt = 1, *f.now
	f.s.State.Roles["ghost"] = &RoleState{Up: true, Inbox: []string{"x"}}

	calls := counted(f, nil)
	before := stateJSON(t, f.s)

	assert.Empty(t, f.s.Waiting())

	got, err := f.s.DeliverRoles([]string{"designer", "harvester", "ghost", "nobody"})
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Empty(t, calls.calls)
	assert.Equal(t, before, stateJSON(t, f.s))
}

// Over every role Waiting names it is the pass DeliverPass is: the same lines go and the state ends the same.
func TestDeliverRolesOverEveryWaitingRoleIsTheDeliverPass(t *testing.T) {
	setup := func() fix {
		f, _ := nativeFixture(t, crewYAML)
		upNative(t, f, "dispatcher")
		upNative(t, f, "designer")
		upNative(t, f, "harvester")
		f.status("harvester", "busy", *f.now)

		// A start's tag is random: the two states are compared whole.
		for _, name := range []string{"dispatcher", "designer", "harvester"} {
			f.s.State.Role(name).Tag = "abcdefghijkl"
		}

		_, err := f.s.Note("dev", "T-1 result ready", false)
		require.NoError(t, err)
		require.NoError(t, f.s.Queue("designer", "[dev] one"))
		require.NoError(t, f.s.Queue("harvester", "[dev] two"))

		return f
	}

	pass, roles := setup(), setup()

	changed, err := pass.s.DeliverPass()
	require.NoError(t, err)
	assert.True(t, changed)

	got, err := roles.s.DeliverRoles(roles.s.Waiting())
	require.NoError(t, err)
	assert.Equal(t, []RoleDelivery{{Role: "dispatcher", Changed: true}, {Role: "designer", Changed: true},
		{Role: "harvester", Changed: false}}, untimed(got))
	assert.Equal(t, stateJSON(t, pass.s), stateJSON(t, roles.s))
}

// A state that cannot be saved after a line went stops it as it stops DeliverPass: the roles after are not tried and
// not reported, the role it came on is.
func TestDeliverRolesStopsWhenTheStateCannotBeSaved(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer", "harvester")
	require.NoError(t, f.s.Queue("designer", "one"))
	require.NoError(t, f.s.Queue("harvester", "two"))

	f.s.Flush = func() error { return errors.New("disk full") }

	got, err := f.s.DeliverRoles(f.s.Waiting())
	require.ErrorContains(t, err, "disk full")
	assert.Equal(t, []RoleDelivery{{Role: "designer", Changed: true}}, untimed(got))
	assert.Empty(t, f.tm.typed["harvester"])
	assert.Equal(t, []string{"two"}, f.s.State.Role("harvester").Inbox)
}

// Like the pass it marks the role it delivers to in the lock holder record, and starts, trims and sweeps nothing.
func TestDeliverRolesMarksTheRoleAndMakesNoBeat(t *testing.T) {
	f := typedFixture(t, trimCrew, "dev")
	f.status("dev", "idle", f.now.Add(-time.Hour))
	f.rg.context["sid-dev"] = 120_000 // far over its context_max: a beat would clear it
	require.NoError(t, f.s.Queue("dev", "hello"))
	require.NoError(t, f.s.Queue("designer", "[dev] hi"))

	var marks []string
	f.s.Mark = func(phase, detail string) { marks = append(marks, phase+" "+detail) }

	beat := f.now.Add(-30 * time.Second)
	f.s.State.LastTick = beat

	got, err := f.s.DeliverRoles([]string{"dev", "designer"})
	require.NoError(t, err)
	assert.Equal(t, []RoleDelivery{{Role: "dev", Changed: true}}, untimed(got))
	assert.Equal(t, []string{"deliver dev"}, marks)
	assert.Equal(t, []string{"hello"}, f.tm.typed["dev"], "the line, and no /clear")
	assert.Equal(t, []string{"dev"}, f.tm.starts, "no role started but the one started above")
	assert.Equal(t, beat, f.s.State.LastTick)
	assert.False(t, f.s.State.Role("designer").Up)
}

// untimed is the deliveries without how long each took.
func untimed(in []RoleDelivery) []RoleDelivery {
	out := make([]RoleDelivery, len(in))
	for i, d := range in {
		out[i] = RoleDelivery{Role: d.Role, Changed: d.Changed}
	}

	return out
}

// A try says how long it took, and one that took over slowTry ends the pass: the roles after it are the next pass's,
// so a tmux that answers slowly does not hold the state lock for one role after another.
func TestDeliverRolesEndsThePassAfterASlowTry(t *testing.T) {
	f := typedFixture(t, crewYAML, "designer", "harvester")
	require.NoError(t, f.s.Queue("designer", "one"))
	require.NoError(t, f.s.Queue("harvester", "two"))

	// Each look at the wall clock finds it 3 s on: every try takes 3 s.
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	wallNow = func() time.Time { at = at.Add(3 * time.Second); return at }

	t.Cleanup(func() { wallNow = time.Now })

	got, err := f.s.DeliverRoles([]string{"designer", "harvester"})
	require.NoError(t, err)
	assert.Equal(t, []RoleDelivery{{Role: "designer", Changed: true, Took: 3 * time.Second}}, got)
	assert.Len(t, f.s.State.Role("harvester").Inbox, 1, "left for the next pass")
}
