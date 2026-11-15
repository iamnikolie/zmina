package route

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// away puts a seat on another node: its crew's address, as the caller names it.
func away(s Seat, address string) Seat {
	s.Away = address
	s.State, s.Note = StateUnavailable, "placed on "+address

	return s
}

// A rule that names a seat the crew file places on another node answers tell, with that crew: the seat is not this
// node's to give work to.
func TestARuleNamingAnAwaySeatAnswersTell(t *testing.T) {
	seats := with(crewSeats(), away(claude("designer", StateDown), "beta/acme"), away(claude("app-dev", StateDown), "beta/acme"),
		away(claude("sec", StateDown), "gamma"))

	for name, c := range map[string]struct {
		in   Input
		rule int
		to   string
		why  string
	}{
		"project": {Input{Project: "app"}, 3, "beta/acme", "project app is owned by app-dev; app-dev is placed on beta/acme, not on this node"},
		"kind":    {Input{Kind: "design"}, 4, "beta/acme", "kind design goes to designer; designer is placed on beta/acme, not on this node"},
		"a node of the default crew": {Input{Kind: "security"}, 4, "gamma",
			"kind security goes to sec; sec is placed on gamma, not on this node"},
	} {
		t.Run(name, func(t *testing.T) {
			d := Decide(table, c.in, env(seats, plans(10, 10)))

			require.True(t, d.Matched())
			assert.Equal(t, ActionTell, d.Action)
			assert.Equal(t, c.to, d.To)
			assert.Equal(t, c.rule, d.Rule)
			assert.Equal(t, c.why, d.Why)
		})
	}
}

func TestContinuationOfAnAwaySeatAnswersTell(t *testing.T) {
	seats := with(crewSeats(), away(claude("dev", StateDown), "beta/acme"))
	in := Input{After: &Task{ID: "T-7", Seat: "dev"}}

	d := Decide(table, in, env(seats, plans(10, 10)))

	require.True(t, d.Matched())
	assert.Equal(t, ActionTell, d.Action)
	assert.Equal(t, "beta/acme", d.To)
	assert.Equal(t, 2, d.Rule)
	assert.Equal(t, "goes on from T-7, done by dev; dev is placed on beta/acme, not on this node", d.Why)
}

// The pool never picks an away seat, however empty its plan is, and says why it passed it over; with every seat away it
// answers nothing.
func TestThePoolSkipsAwaySeats(t *testing.T) {
	seats := with(crewSeats(), away(claude("dev", StateDown), "beta/acme"), away(claude("dev2", StateDown), "beta/acme"))

	d := Decide(table, Input{Kind: "code"}, env(seats, plans(5, 90)))

	require.True(t, d.Matched())
	assert.Equal(t, "dev3", d.To, "the emptier plan is claude's, and its only seat left is dev3")
	assert.Equal(t, ActionUpTask, d.Action)
	assert.Contains(t, d.Alternatives, Alternative{To: "dev", Why: "placed on beta/acme, not on this node"})
	assert.Contains(t, d.Alternatives, Alternative{To: "dev2", Why: "placed on beta/acme, not on this node"})

	all := with(seats, away(claude("dev3", StateDown), "beta/acme"), away(codex("codex-dev", StateIdle), "beta/acme"))
	d = Decide(table, Input{Kind: "code"}, env(all, plans(5, 5)))
	assert.False(t, d.Matched())
	assert.Contains(t, d.Why, "no seat of the pool can take work now")
	assert.Contains(t, d.Why, "dev: placed on beta/acme, not on this node")
}

// A busy seat does not give way to an away seat of the same plan.
func TestBusySeatDoesNotGiveWayToAnAwaySeat(t *testing.T) {
	seats := with(crewSeats(), claude("designer", StateBusy, "T-1"), away(claude("dev", StateDown), "beta/acme"),
		away(claude("dev2", StateDown), "beta/acme"), away(claude("dev3", StateDown), "beta/acme"))

	d := Decide(table, Input{Kind: "design"}, env(seats, plans(10, 10)))

	require.True(t, d.Matched())
	assert.Equal(t, "designer#2", d.To, "its own copy, as before; never an away seat of the pool")
	assert.Equal(t, ActionCopy, d.Action)

	noCopy := Table{Busy: PoolOnly, Rules: table.Rules}
	d = Decide(noCopy, Input{Kind: "design"}, env(seats, plans(10, 10)))
	require.True(t, d.Matched())
	assert.Equal(t, "designer", d.To, "the pool's seats are away: it queues behind its work")
}

func TestAFirstReviewByAnAwaySeatAnswersTellAndARereviewSkipsIt(t *testing.T) {
	seats := with(crewSeats(), away(codex("codex-reviewer", StateDown), "beta/acme"))
	in := Input{Kind: ReviewKind, Of: &Task{ID: "T-3", Seat: "dev"}}

	d := Decide(table, in, env(seats, plans(10, 10)))

	require.True(t, d.Matched())
	assert.Equal(t, ActionTell, d.Action)
	assert.Equal(t, "beta/acme", d.To)
	assert.Equal(t, 5, d.Rule)

	in.Rereview = true
	d = Decide(table, in, env(with(seats, away(claude("dev2", StateDown), "beta/acme")), plans(10, 10)))

	require.True(t, d.Matched())
	assert.Equal(t, ActionTask, d.Action)
	assert.NotEqual(t, "dev2", d.To)
}

// With no seat away the answer is the one it always was: Away has no part in it.
func TestNoAwaySeatLeavesTheAnswerAsItWas(t *testing.T) {
	d := Decide(table, Input{Project: "app"}, env(crewSeats(), plans(10, 10)))

	require.True(t, d.Matched())
	assert.Equal(t, ActionTask, d.Action)
	assert.Equal(t, "app-dev", d.To)
}
