package route

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/limits"
)

func claude(name string, st State, tasks ...string) Seat {
	return seat(name, "claude", st, tasks...)
}

func codex(name string, st State, tasks ...string) Seat {
	return seat(name, "codex", st, tasks...)
}

func seat(name, runtime string, st State, tasks ...string) Seat {
	s := Seat{Name: name, Runtime: runtime, State: st, Tasks: tasks}
	if st == StateBusy {
		s.Note = "busy"
		if len(tasks) > 0 {
			s.Note += " on " + tasks[0]
		}
	}

	return s
}

func asleep(s Seat) Seat {
	s.State, s.Note = StateUnavailable, "asleep until Mon 09:00"

	return s
}

func withContext(s Seat, tokens, budget int) Seat {
	s.Context, s.ContextBudget = tokens, budget

	return s
}

// plans are the readings: claude's 7 days and codex's week, in percent; a negative one is unknown.
func plans(claudeUsed, codexUsed float64) limits.Report {
	r := limits.Report{Policy: limits.DefaultPolicy()}
	if claudeUsed >= 0 {
		r.Claude = &limits.Claude{SevenDay: &limits.Window{Used: claudeUsed}}
	}

	if codexUsed >= 0 {
		r.Codex = &limits.Codex{Week: &limits.Window{Used: codexUsed}}
	}

	return r
}

func staleClaude(used float64) limits.Report {
	r := plans(-1, 50)
	r.Claude = &limits.Claude{SevenDay: &limits.Window{Used: used, Stale: true}}

	return r
}

var table = Table{Rules: []Rule{
	{Kind: OtherCrew, Under: "/work/acme-lg", UnderText: "~/work/acme-lg", Tell: "box/acme"},
	{Kind: OtherCrew, Under: "/work/erp", UnderText: "~/work/erp", Tell: "box/erp"},
	{Kind: Continuation, FreshOver: 80},
	{Kind: Project, Owners: map[string]string{"app": "app-dev", "docs": "writer", "landing": "designer"}},
	{Kind: WorkKind, Owners: map[string]string{"design": "designer", "film": "director", "security": "sec"}},
	{Kind: Review, First: "codex-reviewer", Again: []string{"codex-dev", "dev", "dev2", "dev3"}},
	{Kind: Pool, Seats: []string{"dev", "dev2", "dev3", "codex-dev"}},
}}

// crewSeats are the seats of a quiet crew: everyone idle but the pool's three Claude seats, down.
func crewSeats() []Seat {
	return []Seat{
		claude("dispatcher", StateIdle), claude("dev", StateDown), claude("dev2", StateDown), claude("dev3", StateDown),
		codex("codex-dev", StateIdle), codex("codex-reviewer", StateIdle), claude("designer", StateIdle),
		claude("director", StateIdle), claude("writer", StateIdle), claude("app-dev", StateIdle), claude("sec", StateIdle),
	}
}

func with(seats []Seat, replace ...Seat) []Seat {
	out := append([]Seat{}, seats...)

	for _, r := range replace {
		found := false

		for i := range out {
			if out[i].Name == r.Name {
				out[i], found = r, true
			}
		}

		if !found {
			out = append(out, r)
		}
	}

	return out
}

func env(seats []Seat, l limits.Report) Env {
	return Env{Seats: seats, Limits: l, Dispatcher: "dispatcher", MaxCopy: 9}
}

func TestEachRuleKindAnswers(t *testing.T) {
	quiet := crewSeats()

	cases := []struct {
		name   string
		table  Table
		in     Input
		seats  []Seat
		limits limits.Report
		to     string
		action Action
		rule   int
		why    string
	}{
		{name: "rule 0: a repo under another crew's directory is told to it", table: table,
			in: Input{Repo: "/work/acme-lg/shop"}, seats: quiet, limits: plans(41, 50),
			to: "box/acme", action: ActionTell, rule: 0, why: "repo under ~/work/acme-lg"},
		{name: "rule 0: the second other_crew row", table: table,
			in: Input{Repo: "/work/erp"}, seats: quiet, limits: plans(41, 50),
			to: "box/erp", action: ActionTell, rule: 0, why: "repo under ~/work/erp"},
		{name: "rule 2: the seat that did T-12 goes on", table: table,
			in:    Input{After: &Task{ID: "T-12", Seat: "dev"}},
			seats: with(quiet, withContext(claude("dev", StateIdle), 40_000, 100_000)), limits: plans(41, 50),
			to: "dev", action: ActionTask, rule: 2, why: "goes on from T-12, done by dev, context 40%"},
		{name: "rule 2: its context over 80% takes a fresh copy", table: table,
			in:    Input{After: &Task{ID: "T-12", Seat: "dev"}},
			seats: with(quiet, withContext(claude("dev", StateIdle), 85_000, 100_000)), limits: plans(41, 50),
			to: "dev#2", action: ActionCopy, rule: 2, why: "context 85% is over 80%: a fresh copy"},
		{name: "rule 2: exactly 80% is not over", table: table,
			in:    Input{After: &Task{ID: "T-12", Seat: "dev"}},
			seats: with(quiet, withContext(claude("dev", StateIdle), 80_000, 100_000)), limits: plans(41, 50),
			to: "dev", action: ActionTask, rule: 2},
		{name: "rule 2: the next free copy number", table: table,
			in: Input{After: &Task{ID: "T-12", Seat: "dev#2"}},
			seats: with(quiet, withContext(claude("dev#2", StateIdle), 90_000, 100_000), claude("dev", StateIdle),
				claude("dev#3", StateIdle)), limits: plans(41, 50),
			to: "dev#4", action: ActionCopy, rule: 2},
		{name: "rule 2: a context not known is not judged", table: table,
			in: Input{After: &Task{ID: "T-12", Seat: "dev"}}, seats: with(quiet, claude("dev", StateIdle)), limits: plans(41, 50),
			to: "dev", action: ActionTask, rule: 2},
		{name: "rule 2: a seat that is down is started on the task", table: table,
			in: Input{After: &Task{ID: "T-12", Seat: "dev"}}, seats: quiet, limits: plans(41, 50),
			to: "dev", action: ActionUpTask, rule: 2},
		{name: "rule 2: a copy that is gone goes back to its role", table: table,
			in: Input{After: &Task{ID: "T-12", Seat: "dev#3"}}, seats: with(quiet, claude("dev", StateIdle)), limits: plans(41, 50),
			to: "dev", action: ActionTask, rule: 2, why: "done by dev"},
		{name: "rule 3: the project's owner", table: table,
			in: Input{Project: "app"}, seats: quiet, limits: plans(41, 50),
			to: "app-dev", action: ActionTask, rule: 3, why: "project app is owned by app-dev"},
		{name: "rule 3: the project is the repo's directory when none is named", table: table,
			in: Input{Repo: "/work/docs"}, seats: quiet, limits: plans(41, 50),
			to: "writer", action: ActionTask, rule: 3},
		{name: "rule 3: a project named wins over the repo's directory", table: table,
			in: Input{Repo: "/work/docs", Project: "landing"}, seats: quiet, limits: plans(41, 50),
			to: "designer", action: ActionTask, rule: 3},
		{name: "rule 4: a kind's seat", table: table,
			in: Input{Kind: "film"}, seats: quiet, limits: plans(41, 50),
			to: "director", action: ActionTask, rule: 4, why: "kind film goes to director"},
		{name: "rule 5: the first review goes to the first seat", table: table,
			in:    Input{Kind: "review", Of: &Task{ID: "T-14", Seat: "dev"}},
			seats: quiet, limits: plans(41, 50),
			to: "codex-reviewer", action: ActionTask, rule: 5, why: "T-14 by dev (claude), first review"},
		{name: "rule 6: the pool, by the emptier plan", table: table,
			in: Input{Kind: "code", Repo: "/work/site"}, seats: with(quiet, claude("dev2", StateIdle)), limits: plans(41, 50),
			to: "dev2", action: ActionTask, rule: 6, why: "claude 7d 41% < codex week 50%"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Decide(c.table, c.in, env(c.seats, c.limits))

			assert.Equal(t, c.to, d.To)
			assert.Equal(t, c.action, d.Action)
			assert.Equal(t, c.rule, d.Rule)
			assert.Contains(t, d.Why, c.why)
			assert.True(t, d.Matched())
		})
	}
}

func TestTheFirstRuleThatYieldsASeatWins(t *testing.T) {
	seats := with(crewSeats(), withContext(claude("dev", StateIdle), 10_000, 100_000))

	// Every rule has something to say: the order of the table decides.
	in := Input{
		Repo: "/work/acme-lg/shop", Project: "app", Kind: "film",
		After: &Task{ID: "T-12", Seat: "dev"}, Of: &Task{ID: "T-14", Seat: "dev"},
	}

	assert.Equal(t, "box/acme", Decide(table, in, env(seats, plans(41, 50))).To, "rule 0 first")

	in.Repo = "/work/site"
	assert.Equal(t, "dev", Decide(table, in, env(seats, plans(41, 50))).To, "then the continuation")

	in.After = nil
	assert.Equal(t, "app-dev", Decide(table, in, env(seats, plans(41, 50))).To, "then the project")

	in.Project = ""
	assert.Equal(t, "director", Decide(table, in, env(seats, plans(41, 50))).To, "then the kind")

	in.Kind = "review"
	assert.Equal(t, "codex-reviewer", Decide(table, in, env(seats, plans(41, 50))).To, "then the review")

	in.Kind = "code"
	assert.Equal(t, "dev", Decide(table, in, env(seats, plans(41, 50))).To, "the pool last")

	// The same table in another order answers another way.
	swapped := Table{Rules: []Rule{table.Rules[3], table.Rules[0]}}
	in = Input{Repo: "/work/acme-lg/docs"}
	assert.Equal(t, "writer", Decide(swapped, in, env(seats, plans(41, 50))).To, "project docs before the other crew")
}

func TestARepoOnlyMatchesAUnderItsDirectory(t *testing.T) {
	assert.True(t, Within("/work/acme-lg", "/work/acme-lg"))
	assert.True(t, Within("/work/acme-lg/a/b", "/work/acme-lg"))
	assert.True(t, Within("/work/acme-lg/a/../b", "/work/acme-lg/"))
	assert.False(t, Within("/work/acme-lg2/a", "/work/acme-lg"), "a longer name is not below it")
	assert.False(t, Within("/work", "/work/acme-lg"))
	assert.False(t, Within("/other/acme-lg", "/work/acme-lg"))
}

func TestARuleThatMatchesButYieldsNoSeatLetsTheNextOneAnswer(t *testing.T) {
	seats := with(crewSeats(), asleep(claude("app-dev", StateIdle)), claude("dev", StateIdle))
	d := Decide(table, Input{Project: "app"}, env(seats, plans(41, 50)))

	assert.Equal(t, "dev", d.To, "the project's owner is asleep: the pool answers")
	assert.Equal(t, 6, d.Rule)

	var project Step

	for _, s := range d.Trace {
		if s.Rule == 3 {
			project = s
		}
	}

	assert.False(t, project.Matched)
	assert.Equal(t, "project app: app-dev is asleep until Mon 09:00", project.Why)

	// A seat the crew does not have is the same.
	d = Decide(table, Input{After: &Task{ID: "T-1", Seat: "gone"}, Project: "docs"}, env(crewSeats(), plans(41, 50)))
	assert.Equal(t, "writer", d.To)
	assert.Contains(t, d.Trace[len(d.Trace)-2].Why, "T-1 was done by gone, which is not a seat of this crew")
}

func TestBusySeatsGiveWayToACopyOrAFreeSeatOfTheSamePlan(t *testing.T) {
	continuation := Input{After: &Task{ID: "T-12", Seat: "dev"}}
	busyDev := claude("dev", StateBusy, "T-12")

	cases := []struct {
		name    string
		policy  BusyPolicy
		seats   []Seat
		in      Input
		to      string
		action  Action
		why     string
		altsHas string
	}{
		{name: "a new copy", seats: with(crewSeats(), busyDev), in: continuation,
			to: "dev#2", action: ActionCopy, why: "dev is busy on T-12; a new copy dev#2 takes it", altsHas: "dev"},
		{name: "an idle copy first", seats: with(crewSeats(), busyDev, claude("dev#2", StateBusy, "T-4"), claude("dev#3", StateIdle)),
			in: continuation, to: "dev#3", action: ActionTask, why: "dev#3 is idle: its copy takes it"},
		{name: "the copy policy is the default", policy: CopyOrPool, seats: with(crewSeats(), busyDev), in: continuation,
			to: "dev#2", action: ActionCopy},
		{name: "pool only: a free seat of the same plan", policy: PoolOnly, seats: with(crewSeats(), busyDev, claude("dev2", StateIdle)),
			in: continuation, to: "dev2", action: ActionTask, why: "dev2 is idle, the same plan"},
		{name: "pool only: a down seat of the same plan is started", policy: PoolOnly, seats: with(crewSeats(), busyDev),
			in: continuation, to: "dev2", action: ActionUpTask, why: "dev2 is down, the same plan"},
		{name: "pool only never a codex seat for a claude one", policy: PoolOnly,
			seats: with(crewSeats(), busyDev, claude("dev2", StateBusy), claude("dev3", StateBusy)),
			in:    continuation, to: "dev", action: ActionTask, why: "no copy or free seat of its plan: it queues behind its work"},
		{name: "no copy number left", seats: with(crewSeats(), busyDev), in: continuation,
			to: "dev", action: ActionTask, why: "queues behind its work"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tb := table
			tb.Busy = c.policy

			e := env(c.seats, plans(41, 50))
			if c.name == "no copy number left" {
				e.MaxCopy = 1
				// The same plan has nothing free either.
				e.Seats = with(e.Seats, claude("dev2", StateBusy), claude("dev3", StateBusy))
			}

			d := Decide(tb, c.in, e)

			assert.Equal(t, c.to, d.To)
			assert.Equal(t, c.action, d.Action)
			assert.Equal(t, 2, d.Rule, "the rule that picked the seat stays the rule")
			assert.Contains(t, d.Why, c.why)

			if c.altsHas != "" {
				assert.Contains(t, d.Alternatives, Alternative{To: c.altsHas, Why: "busy on T-12"})
			}
		})
	}
}

func TestTheDispatcherHasNoCopies(t *testing.T) {
	tb := Table{Rules: []Rule{{Kind: WorkKind, Owners: map[string]string{"triage": "dispatcher"}}}}
	d := Decide(tb, Input{Kind: "triage"}, env(with(crewSeats(), claude("dispatcher", StateBusy, "T-1")), plans(41, 50)))

	assert.Equal(t, "dispatcher", d.To)
	assert.Equal(t, ActionTask, d.Action)
	assert.Contains(t, d.Why, "queues behind its work")
}

func TestReviews(t *testing.T) {
	again := Input{Kind: "review", Of: &Task{ID: "T-14", Seat: "dev"}, Rereview: true}

	cases := []struct {
		name   string
		in     Input
		seats  []Seat
		to     string
		action Action
		why    string
		alts   []Alternative
	}{
		{name: "a first review by the first seat", in: Input{Kind: "review", Of: &Task{ID: "T-14", Seat: "dev"}},
			seats: crewSeats(), to: "codex-reviewer", action: ActionTask, why: "T-14 by dev (claude), first review"},
		{name: "a re-review is never the author's", in: again,
			seats: with(crewSeats(), codex("codex-dev", StateBusy, "T-9"), claude("dev", StateIdle), claude("dev2", StateBusy, "T-3"),
				claude("dev3", StateIdle)),
			to: "dev3", action: ActionTask,
			why:  "re-review, T-14 by dev (claude); codex-dev busy on T-9; dev2 busy on T-3; dev3 idle, not the author",
			alts: []Alternative{{To: "codex-dev", Why: "busy on T-9"}, {To: "dev", Why: "the author"}, {To: "dev2", Why: "busy on T-3"}}},
		{name: "a re-review takes an idle seat before a down one", in: again,
			seats: with(crewSeats(), codex("codex-dev", StateDown), claude("dev2", StateIdle)),
			to:    "dev2", action: ActionTask},
		{name: "a re-review takes a down seat before a busy one", in: again,
			seats: with(crewSeats(), codex("codex-dev", StateBusy, "T-9"), claude("dev2", StateBusy), claude("dev3", StateDown)),
			to:    "dev3", action: ActionUpTask},
		{name: "a copy of the author is the author too", in: Input{Kind: "review", Of: &Task{ID: "T-14", Seat: "dev#2"}, Rereview: true},
			seats: with(crewSeats(), codex("codex-dev", StateBusy, "T-9"), claude("dev", StateIdle), claude("dev#2", StateBusy, "T-14"),
				claude("dev2", StateIdle)),
			to: "dev2", action: ActionTask},
		{name: "the first review's seat is the author: the others", in: Input{Kind: "review", Of: &Task{ID: "T-5", Seat: "codex-reviewer"}},
			seats: with(crewSeats(), codex("codex-dev", StateIdle)), to: "codex-dev", action: ActionTask,
			why: "review, T-5 by codex-reviewer (codex); codex-dev idle, not the author"},
		{name: "every seat of a re-review busy: its copy", in: again,
			seats: with(crewSeats(), codex("codex-dev", StateBusy, "T-9"), claude("dev2", StateBusy), claude("dev3", StateBusy)),
			to:    "codex-dev#2", action: ActionCopy},
		{name: "a seat asleep is not asked", in: Input{Kind: "review", Of: &Task{ID: "T-14", Seat: "dev"}},
			seats: with(crewSeats(), asleep(codex("codex-reviewer", StateIdle)), codex("codex-dev", StateIdle)),
			to:    "codex-dev", action: ActionTask,
			alts: []Alternative{{To: "codex-reviewer", Why: "codex-reviewer is asleep until Mon 09:00"}, {To: "dev", Why: "the author"}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Decide(table, c.in, env(c.seats, plans(41, 50)))

			assert.Equal(t, c.to, d.To)
			assert.Equal(t, c.action, d.Action)
			assert.Equal(t, 5, d.Rule)
			assert.Contains(t, d.Why, c.why)

			if c.alts != nil {
				assert.Subset(t, d.Alternatives, c.alts)
			}

			assert.NotEqual(t, Base(c.in.Of.Seat), Base(d.To), "the author never reviews")
		})
	}

	t.Run("a review with no author is not routed", func(t *testing.T) {
		d := Decide(table, Input{Kind: "review"}, env(crewSeats(), plans(41, 50)))

		// The pool does not take a review either: a review is not "the rest".
		assert.Contains(t, d.Trace[len(d.Trace)-2].Why, "a review needs --of")
	})

	t.Run("a table with a first seat only re-reviews with it", func(t *testing.T) {
		tb := Table{Rules: []Rule{{Kind: Review, First: "codex-reviewer"}}}
		d := Decide(tb, again.withOf("T-14", "dev"), env(crewSeats(), plans(41, 50)))

		assert.Equal(t, "codex-reviewer", d.To)
	})

	t.Run("a review is not the kind rule's", func(t *testing.T) {
		d := Decide(table, Input{Kind: "review", Of: &Task{ID: "T-1", Seat: "dev"}}, env(crewSeats(), plans(41, 50)))
		assert.Equal(t, 5, d.Rule)

		for _, s := range d.Trace {
			if s.Rule == 4 {
				assert.Equal(t, "a review is the review rule's", s.Why)
			}
		}
	})
}

func (in Input) withOf(id, seat string) Input {
	in.Of = &Task{ID: id, Seat: seat}

	return in
}

func TestThePoolPicksTheEmptierPlan(t *testing.T) {
	idle := []Seat{claude("dev", StateIdle), claude("dev2", StateIdle), claude("dev3", StateIdle), codex("codex-dev", StateIdle)}

	cases := []struct {
		name   string
		seats  []Seat
		limits limits.Report
		to     string
		action Action
		why    string
		stale  bool
		alts   []Alternative
		busy   BusyPolicy
	}{
		{name: "claude is emptier", seats: idle, limits: plans(41, 50), to: "dev", action: ActionTask,
			why: "claude 7d 41% < codex week 50%", alts: []Alternative{{To: "codex-dev", Why: "codex week 50%"}}},
		{name: "codex is emptier", seats: idle, limits: plans(60, 20), to: "codex-dev", action: ActionTask,
			why: "codex week 20% < claude 7d 60%"},
		{name: "equal plans: the order of the pool", seats: idle, limits: plans(50, 50), to: "dev", action: ActionTask,
			why: "claude 7d 50% = codex week 50%"},
		{name: "a plan at its pinned share takes only pinned work", seats: idle, limits: plans(80, 70), to: "codex-dev", action: ActionTask,
			why: "claude 7d 80% is pinned work only"},
		{name: "claude at 79% is still open", seats: idle, limits: plans(79, 95), to: "dev", action: ActionTask},
		{name: "claude at its stop share takes nothing", seats: idle, limits: plans(99, 70), to: "codex-dev", action: ActionTask,
			why: "claude 7d 99% is stopped"},
		{name: "idle before down, then the order of the pool", limits: plans(41, 50),
			seats: []Seat{claude("dev", StateBusy, "T-12"), claude("dev2", StateIdle), claude("dev3", StateDown), codex("codex-dev", StateIdle)},
			to:    "dev2", action: ActionTask, why: "claude 7d 41% < codex week 50%; dev is busy on T-12",
			alts: []Alternative{{To: "dev3", Why: "down"}, {To: "codex-dev", Why: "codex week 50%"}}},
		{name: "a down seat is started", limits: plans(41, 50),
			seats: []Seat{claude("dev", StateBusy, "T-12"), claude("dev2", StateDown), claude("dev3", StateDown), codex("codex-dev", StateIdle)},
			to:    "dev2", action: ActionUpTask},
		{name: "the emptier plan's busy seat gives way to a copy, not to the other plan", limits: plans(41, 50),
			seats: []Seat{claude("dev", StateBusy, "T-12"), claude("dev2", StateBusy, "T-4"), claude("dev3", StateBusy, "T-14", "T-15"),
				codex("codex-dev", StateIdle)},
			to: "dev#2", action: ActionCopy, why: "dev is busy on T-12"},
		{name: "limits unknown: fewest open tasks", limits: plans(-1, -1), stale: true, busy: PoolOnly,
			seats: []Seat{claude("dev", StateBusy, "T-12", "T-4"), claude("dev2", StateBusy, "T-14"), claude("dev3", StateBusy, "T-15", "T-6", "T-7"),
				codex("codex-dev", StateBusy, "T-8", "T-9")},
			to: "dev2", action: ActionTask, why: "limits unknown: fewest open tasks"},
		{name: "a stale reading is unknown", limits: staleClaude(10), stale: true, seats: idle,
			to: "dev", action: ActionTask, why: "limits unknown: fewest open tasks"},
		{name: "unknown limits still answer by how soon a seat starts", limits: plans(-1, 30), stale: true,
			seats: []Seat{claude("dev", StateDown), claude("dev2", StateDown), claude("dev3", StateDown), codex("codex-dev", StateIdle)},
			to:    "codex-dev", action: ActionTask, why: "limits unknown"},
		{name: "only one plan known and both plans in the pool: not compared", limits: plans(90, -1), stale: true,
			seats: idle, to: "codex-dev", action: ActionTask},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Decide(Table{Rules: []Rule{table.Rules[6]}, Busy: c.busy}, Input{Kind: "code"}, env(c.seats, c.limits))

			assert.Equal(t, c.to, d.To)
			assert.Equal(t, c.action, d.Action)
			assert.Equal(t, 6, d.Rule)
			assert.Equal(t, "pool by limits", d.Name)
			assert.Contains(t, d.Why, c.why)
			assert.Equal(t, c.stale, d.Stale)

			if c.alts != nil {
				assert.Subset(t, d.Alternatives, c.alts)
			}
		})
	}
}

func TestThePoolWithNoSeatToTakeWorkIsNoMatch(t *testing.T) {
	pool := Table{Rules: []Rule{table.Rules[6]}}

	d := Decide(pool, Input{}, env([]Seat{claude("dev", StateIdle), codex("codex-dev", StateIdle)}, plans(85, 90)))

	assert.Equal(t, ActionNone, d.Action)
	assert.Equal(t, -1, d.Rule)
	assert.Contains(t, d.Why, "no seat of the pool can take work now")
	assert.Contains(t, d.Why, "dev: claude 7d 85% is pinned work only")
	assert.Contains(t, d.Why, "codex-dev: codex week 90% is pinned work only")
	assert.Contains(t, d.Why, ": pick by hand")
}

func TestNoRuleMatched(t *testing.T) {
	noPool := Table{Rules: table.Rules[:6]}

	t.Run("a kind with no seat", func(t *testing.T) {
		d := Decide(noPool, Input{Kind: "movie"}, env(crewSeats(), plans(41, 50)))

		assert.Equal(t, ActionNone, d.Action)
		assert.Empty(t, d.To)
		assert.Equal(t, "no rule matched: kind movie has no seat in routes (design, film, security): pick by hand", d.Headline())
		assert.Len(t, d.Trace, 5, "every rule was asked: the two other_crew rows are one")
	})

	t.Run("nothing to route by", func(t *testing.T) {
		d := Decide(noPool, Input{}, env(crewSeats(), plans(41, 50)))

		assert.Equal(t, "no rule matched: nothing to route by: give --repo, --project, --kind, --after or --of: pick by hand", d.Headline())
	})

	t.Run("no routes", func(t *testing.T) {
		d := Decide(Table{}, Input{Kind: "code"}, env(crewSeats(), plans(41, 50)))

		assert.Equal(t, ActionNone, d.Action)
		assert.Equal(t, "no routes in the crew file", d.Headline())
		assert.Empty(t, d.Trace)
	})
}

func TestExplainListsEveryRuleUpToTheAnswer(t *testing.T) {
	seats := []Seat{claude("dev", StateBusy, "T-12"), claude("dev2", StateIdle), claude("dev3", StateDown), codex("codex-dev", StateIdle)}
	d := Decide(table, Input{Kind: "code", Repo: "/work/site"}, env(seats, plans(41, 50)))

	var out stringsBuilder
	require.NoError(t, d.WriteExplain(&out))

	assert.Equal(t, `0 other crew       no    repo not under ~/work/acme-lg, ~/work/erp
2 continuation     no    no --after
3 project          no    project site has no owner
4 kind             no    kind code has no seat in routes (design, film, security)
5 review           no    not a review
6 pool by limits   yes   dev2 (claude 7d 41% < codex week 50%; dev is busy on T-12)
`, out.String())
}

type stringsBuilder struct{ b []byte }

func (s *stringsBuilder) Write(p []byte) (int, error) { s.b = append(s.b, p...); return len(p), nil }
func (s *stringsBuilder) String() string              { return string(s.b) }

func TestTheLimitsAreInTheAnswer(t *testing.T) {
	d := Decide(table, Input{Kind: "film"}, env(crewSeats(), plans(41.4, 50.6)))

	require.NotNil(t, d.Limits.Claude)
	require.NotNil(t, d.Limits.Codex)
	assert.Equal(t, 41, *d.Limits.Claude)
	assert.Equal(t, 51, *d.Limits.Codex)
	assert.False(t, d.Stale)

	d = Decide(table, Input{Kind: "film"}, env(crewSeats(), staleClaude(41)))
	assert.Nil(t, d.Limits.Claude, "a stale reading is not a figure")
	assert.True(t, d.Stale)
	assert.Equal(t, "director", d.To, "a rule that does not use the limits answers all the same")
}

func TestTheSameInputsGiveTheSameAnswer(t *testing.T) {
	seats := []Seat{claude("dev", StateBusy, "T-12"), claude("dev2", StateIdle), claude("dev3", StateIdle), codex("codex-dev", StateIdle)}
	in := Input{Kind: "movie", Repo: "/work/site"}
	tb := Table{Rules: []Rule{
		{Kind: WorkKind, Owners: map[string]string{"a": "dev", "b": "dev2", "c": "dev3", "d": "codex-dev", "e": "dev", "f": "dev2", "g": "dev3"}},
		table.Rules[6],
	}}

	first := Decide(tb, in, env(seats, plans(30, 30)))

	for range 50 {
		assert.Equal(t, first, Decide(tb, in, env(seats, plans(30, 30))))
	}

	assert.Equal(t, "dev2", first.To, "equal plans, equal state: the order of the pool")
	assert.Contains(t, first.Trace[0].Why, "(a, b, c, d, e, f, …)", "names are listed sorted, six at most")
}

func TestJSONHasTheSpecsShape(t *testing.T) {
	d := Decide(table, Input{Kind: "code"}, env([]Seat{claude("dev", StateBusy, "T-12"), claude("dev2", StateIdle), claude("dev3", StateDown),
		codex("codex-dev", StateIdle)}, plans(41, 50)))

	got := d.JSON(false)
	assert.Equal(t, map[string]any{
		"to": "dev2", "action": "task", "rule": 6, "name": "pool by limits",
		"why": "claude 7d 41% < codex week 50%; dev is busy on T-12",
		"alternatives": []any{map[string]any{"to": "dev3", "why": "down"}, map[string]any{"to": "dev", "why": "busy on T-12"},
			map[string]any{"to": "codex-dev", "why": "codex week 50%"}},
		"limits": map[string]any{"claude": 41, "codex": 50},
		"stale":  false,
	}, roundTrip(t, got))

	none := Decide(Table{}, Input{}, env(nil, plans(-1, -1))).JSON(false)
	m := roundTrip(t, none)
	assert.Nil(t, m["rule"], "none has no rule: zero is rule 0")
	assert.Equal(t, "none", m["action"])
	assert.Equal(t, []any{}, m["alternatives"])
	assert.Equal(t, map[string]any{"claude": nil, "codex": nil}, m["limits"])
	assert.Equal(t, true, m["stale"])

	explained := roundTrip(t, d.JSON(true))
	assert.Len(t, explained["explain"], len(d.Trace))
}

func roundTrip(t *testing.T, v any) map[string]any {
	t.Helper()

	raw, err := json.Marshal(v)
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))

	// encoding/json reads numbers as float64: say the integers the way the answer does.
	for k, val := range out {
		out[k] = integers(val)
	}

	return out
}

func integers(v any) any {
	switch x := v.(type) {
	case float64:
		return int(x)
	case map[string]any:
		for k, val := range x {
			x[k] = integers(val)
		}
	case []any:
		for i, val := range x {
			x[i] = integers(val)
		}
	}

	return v
}
