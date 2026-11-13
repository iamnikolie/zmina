package shift

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/chat"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/limits"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
)

// limitsCrew is the default crew on the crew's own clock (UTC) with a second Claude seat and a Codex seat: the plans have roles.
const limitsCrew = "timezone: UTC\n" + crewYAML + "  dev:\n  codex-dev:\n    runtime: codex\n"

// The fixture's clock is 2026-10-24 12:00 UTC. The 5-hour window resets at 13:12, the week three days on.
var (
	limitsReset5 = time.Date(2026, 10, 24, 13, 12, 0, 0, time.UTC)
	limitsReset7 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
)

// claudeLimits sets what the beat reads of the Claude plan: the windows at the given shares, read now.
func (f fix) claudeLimits(five, seven float64) {
	f.claudeLimitsResetting(five, seven, limitsReset5, limitsReset7)
}

func (f fix) claudeLimitsResetting(five, seven float64, r5, r7 time.Time) {
	f.s.Limits = &limits.Report{Claude: &limits.Claude{
		FiveHour: &limits.Window{Used: five, Resets: &r5},
		SevenDay: &limits.Window{Used: seven, Resets: &r7},
		At:       *f.now,
	}}
}

func (f fix) codexLimits(week float64, resets time.Time) {
	f.s.Limits = &limits.Report{Codex: &limits.Codex{Week: &limits.Window{Used: week, Resets: &resets}, At: *f.now}}
}

func (f fix) book(t *testing.T) limits.Book {
	t.Helper()

	b, err := limits.ReadBook(f.s.Crew.Layout().Limits())
	require.NoError(t, err)

	return b
}

// heldDesigner is a crew whose designer is up, past its boot and idle with two lines queued for it (a role that takes lines the
// way TestAFailedTurnHoldsTheQueueForAWhile's does), and whose 5-hour window is at 97%: the hold has begun.
func heldDesigner(t *testing.T) (fix, *fakePeer) {
	t.Helper()

	f, p := nativeFixture(t, limitsCrew)
	require.NoError(t, f.s.Up("designer"))

	start := *f.now
	f.status("designer", "idle", start)
	f.rg.endedAt["sid-designer"] = start.Add(time.Second)
	f.s.State.Role("designer").Booted = start

	require.NoError(t, f.s.Queue("designer", "[dispatcher] first"))
	require.NoError(t, f.s.Queue("designer", "[dispatcher] second"))

	f.claudeLimits(97, 10)
	require.NotEmpty(t, f.s.limitsStep(*f.now))
	require.Len(t, f.book(t).Holds, 1)

	return f, p
}

func TestLimitsWarnIsOneNoteOncePerPeriod(t *testing.T) {
	f := fixtureWith(t, limitsCrew)
	f.claudeLimits(76, 10)

	want := "claude 5h window at 76%, resets 13:12 (in 1h12m)"

	did := f.s.limitsStep(*f.now)
	assert.Equal(t, []string{"limits: " + want}, did)
	assert.Equal(t, []string{want}, f.s.State.Notes, "a note for the dispatcher")
	assert.True(t, f.ev.has("", "limit-warn"))
	assert.Equal(t, 1, f.book(t).Told[limits.Key(limits.PlanClaude, limits.WindowFiveHour, &limitsReset5)].Warn.Compare(time.Time{}))

	// The next beats of the period say nothing more, and the note once told and taken is not told again.
	f.s.State.Notes = nil
	*f.now = f.now.Add(time.Minute)
	f.claudeLimits(78, 10)
	assert.Empty(t, f.s.limitsStep(*f.now))
	assert.Empty(t, f.s.State.Notes)

	// The 7-day window is judged on its own: it warns when it gets there, in its own words.
	*f.now = f.now.Add(time.Minute)
	f.claudeLimits(78, 80)
	assert.Equal(t, []string{"limits: claude 7d window at 80%, resets Tue 12:00 (in 2d23h)"}, f.s.limitsStep(*f.now))
}

func TestLimitsWarnAgainWhenTheResetMoves(t *testing.T) {
	f := fixtureWith(t, limitsCrew)
	f.claudeLimits(76, 10)
	require.Len(t, f.s.limitsStep(*f.now), 1)

	*f.now = limitsReset5.Add(time.Minute)
	f.claudeLimitsResetting(76, 10, limitsReset5.Add(5*time.Hour), limitsReset7)
	did := f.s.limitsStep(*f.now)
	require.Len(t, did, 1, "a new period of the window warns again")
	assert.Contains(t, did[0], "claude 5h window at 76%, resets 18:12")
}

func TestLimitsCheckpointAsksTheRolesThatHoldATask(t *testing.T) {
	f := fixtureWith(t, limitsCrew)
	require.NoError(t, f.s.Up("designer"))
	require.NoError(t, f.s.Up("dev"))
	f.tm.up["codex-dev"] = 4242
	f.tk.list = []task.Task{
		{ID: "T-1", Role: "designer", State: task.Open, Title: "draw the logo"},
		{ID: "T-2", Role: "codex-dev", State: task.Open, Title: "wire the api"},
		{ID: "T-3", Role: "harvester", State: task.Open, Title: "sweep the notes"},
	}

	f.claudeLimits(91, 10)
	did := f.s.limitsStep(*f.now)

	note := "claude 5h window at 91%, resets 13:12 (in 1h12m): asked designer for a checkpoint now (each holds a task)"
	assert.Equal(t, []string{"limits: " + note}, did)
	assert.Equal(t, []string{note}, f.s.State.Notes)

	// The line is the restart's request: a short locator to one file of role-shaped checkpoint requests.
	require.Len(t, f.s.State.Role("designer").Inbox, 1)
	line := f.s.State.Role("designer").Inbox[0]
	assert.True(t, strings.HasPrefix(line, limitCheckpointOpening), line)
	assert.True(t, f.s.sessionBound("designer", line), "a request for this conversation only")

	paths, err := filepath.Glob(filepath.Join(f.s.Crew.StateDir(), "restarts", "*.json"))
	require.NoError(t, err)
	require.Len(t, paths, 1)
	assert.Contains(t, line, paths[0])

	raw, err := os.ReadFile(paths[0])
	require.NoError(t, err)
	assert.Contains(t, string(raw), "task checkpoint T-1")
	assert.Contains(t, string(raw), "Do not park the task")
	assert.Contains(t, string(raw), "no restart follows")

	assert.Empty(t, f.s.State.Role("dev").Inbox, "up, but holds no task")
	assert.Empty(t, f.s.State.Role("harvester").Inbox, "holds a task, but is not up")
	assert.Empty(t, f.s.State.Role("codex-dev").Inbox, "another plan")
	assert.True(t, f.ev.has("designer", "limit-checkpoint"))

	// Once a period: the next beat asks nobody and says nothing.
	f.s.State.Notes, f.s.State.Role("designer").Inbox = nil, nil
	*f.now = f.now.Add(time.Minute)
	f.claudeLimits(93, 10)
	assert.Empty(t, f.s.limitsStep(*f.now))
	assert.Empty(t, f.s.State.Role("designer").Inbox)
}

func TestLimitsCheckpointSaysSoWhenNobodyHoldsATask(t *testing.T) {
	f := fixtureWith(t, limitsCrew)
	require.NoError(t, f.s.Up("designer"))

	f.claudeLimits(90, 10)
	did := f.s.limitsStep(*f.now)

	require.Len(t, did, 1)
	assert.Contains(t, did[0], "claude 5h window at 90%")
	assert.Contains(t, did[0], "no role of the plan that is up holds a task, so nobody was asked")
	assert.Empty(t, f.s.State.Role("designer").Inbox)
}

func TestLimitsHoldKeepsEveryLineUntilTheReset(t *testing.T) {
	f, p := heldDesigner(t)

	hold := "claude: 5h window at 97%, holding its 4 roles until 13:12; lines wait, the owner's pass"
	assert.Contains(t, f.s.State.Notes, hold)
	assert.True(t, f.ev.has("", "limit-hold"))

	h := f.book(t).Holds[0]
	assert.Equal(t, limits.PlanClaude, h.Plan)
	assert.Equal(t, limits.WindowFiveHour, h.Window)
	assert.True(t, h.Reset.Equal(limitsReset5))

	queued := func() []string { return f.s.State.Role("designer").Inbox }
	waiting := func() bool { return slicesContain(f.s.Waiting(), "designer") }

	f.live("designer")

	start := *f.now

	for _, after := range []time.Duration{time.Second, time.Minute, 40 * time.Minute, 71 * time.Minute} {
		*f.now = start.Add(after)
		f.status("designer", "idle", start)

		assert.False(t, waiting(), "%v in: Waiting leaves the role out, so the courier does not ask it", after)

		tried, err := f.s.DeliverRoles([]string{"designer"})
		require.NoError(t, err)
		assert.Empty(t, tried, "the delivery pass tries nothing")

		changed, err := f.s.DeliverPass()
		require.NoError(t, err)
		assert.False(t, changed)

		assert.Empty(t, f.s.DeliverTo("designer"), "the beat's delivery")
		assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"), "a turn's end")
		assert.Empty(t, f.s.FlushInbox("designer"))
		assert.Empty(t, p.posted["sid-designer"])
		assert.Empty(t, f.tm.typed["designer"])
		assert.Equal(t, []string{"[dispatcher] first", "[dispatcher] second"}, queued())
		assert.Zero(t, f.s.State.Role("designer").Attempts, "no try is counted against a line")
	}

	// The whole beat, too: nothing typed or posted to anyone, and a role the ticker would start is not started.
	f.s.Tick()
	assert.Empty(t, f.tm.typed)
	assert.Empty(t, p.posted)
	assert.False(t, f.tm.Has("harvester"), "keep: always, but a start's boot line is a model call")
	assert.False(t, f.tm.Has("dispatcher"))
	assert.Equal(t, []string{"[dispatcher] first", "[dispatcher] second"}, queued())

	// The dispatcher's own signals and notes wait as well: its plan is the held one.
	assert.NotEmpty(t, f.s.State.Notes)
	assert.NotContains(t, f.s.Waiting(), "dispatcher")

	// The reset time passes: by the clock alone the roles are asked again, before any beat says so.
	*f.now = limitsReset5
	assert.True(t, waiting())
}

func TestLimitsHoldLetsTheOwnersLineThrough(t *testing.T) {
	f, _ := heldDesigner(t)
	f.live("designer")

	*f.now = f.now.Add(time.Minute)
	f.status("designer", "idle", *f.now)

	const owner = "stop, look at this first"
	require.NoError(t, f.s.Queue("designer", owner))
	require.False(t, slicesContain(f.s.Waiting(), "designer"), "held while the next line is not the owner's")

	rs := f.s.State.Role("designer")
	rs.ChatReceipts = append(rs.ChatReceipts, chat.Receipt{ID: "c1", Line: owner, At: *f.now})

	assert.True(t, slicesContain(f.s.Waiting(), "designer"), "the owner's message is in the queue: the role is asked")
	assert.Equal(t, 2, f.s.ownerLine("designer"))
	assert.NotEmpty(t, f.s.DeliverTo("designer"), "and it goes")
	assert.Equal(t, []string{owner}, f.tm.typed["designer"])
	assert.Equal(t, []string{"[dispatcher] first", "[dispatcher] second"}, rs.Inbox, "what zmina queued waits, in its order")

	// A held role whose queue is full still takes the owner's message.
	for len(rs.Inbox) < inboxMax {
		rs.Inbox = append(rs.Inbox, "filler")
	}

	require.NoError(t, f.s.QueueOwner("designer", "one more from the owner"))
}

func TestLimitsHoldDoesNotInterruptABusyRole(t *testing.T) {
	f, p := nativeFixture(t, limitsCrew)
	require.NoError(t, f.s.Up("designer"))

	start := *f.now
	f.status("designer", "busy", start)
	f.rg.endedAt["sid-designer"] = start.Add(time.Second)
	f.s.State.Role("designer").Booted = start
	f.live("designer")

	f.claudeLimits(97, 10)
	require.NotEmpty(t, f.s.limitsStep(start))

	// It is not told, typed to, stopped or put down: it works on and finishes its turn.
	assert.Empty(t, f.tm.keys["designer"])
	assert.Empty(t, f.tm.typed["designer"])
	assert.True(t, f.tm.Has("designer"))
	assert.True(t, f.s.State.Role("designer").Up)

	// A line queued while it works waits behind the hold, and the turn that ends gets nothing more.
	require.NoError(t, f.s.Queue("designer", "[dispatcher] after you finish"))

	*f.now = f.now.Add(10 * time.Minute)
	f.status("designer", "idle", *f.now)
	f.rg.endedAt["sid-designer"] = f.now.Add(time.Second)

	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"))
	assert.Empty(t, f.s.DeliverTo("designer"))
	assert.Empty(t, p.posted["sid-designer"])
	assert.Empty(t, f.tm.typed["designer"])
	assert.Equal(t, []string{"[dispatcher] after you finish"}, f.s.State.Role("designer").Inbox)
}

func TestLimitsHoldEndsAtTheResetAndTheLinesGoInOrder(t *testing.T) {
	f, p := heldDesigner(t)
	f.live("designer")

	// The window rolled over and a status line since drew the new period: confirmed.
	*f.now = limitsReset5.Add(time.Minute)
	f.status("designer", "idle", *f.now)
	f.claudeLimitsResetting(2, 10, limitsReset5.Add(5*time.Hour), limitsReset7)

	lines := f.s.planLines(limits.PlanClaude)
	require.Positive(t, lines)

	did := f.s.limitsStep(*f.now)
	require.Len(t, did, 1)
	assert.Equal(t, "limits: claude: 5h window reset, "+strconv.Itoa(lines)+" lines go now", did[0])
	assert.True(t, f.ev.has("", "limit-release"))
	assert.Empty(t, f.book(t).Holds)

	// The queue goes in its order on the normal path (the two lines as one post).
	assert.True(t, slicesContain(f.s.Waiting(), "designer"))
	assert.Equal(t, []string{"posted to designer: 2 lines joined: 1. [dispatcher] first 2. [dispatcher] second"},
		f.s.TurnEnd("designer", "sid-designer"))
	assert.Equal(t, []string{"1. [dispatcher] first\n2. [dispatcher] second"}, p.posted["sid-designer"])
}

// A hold never ends on stale data before its reset time; at the time it ends by the clock, and says so when no reading
// confirms the new period.
func TestLimitsHoldEndsByTheClockWhenNoReadingConfirms(t *testing.T) {
	f, _ := heldDesigner(t)

	// Nothing read, or a reading too old to say anything (stale): the hold stays.
	for _, r := range []*limits.Report{
		nil,
		{},
		{Claude: &limits.Claude{FiveHour: &limits.Window{Used: 3, Resets: &limitsReset5, Stale: true}, At: f.now.Add(-8 * time.Hour)}},
	} {
		f.s.Limits = r
		*f.now = limitsReset5.Add(-time.Minute)

		assert.Empty(t, f.s.limitsStep(*f.now))
		assert.Len(t, f.book(t).Holds, 1)
	}

	// At the reset time itself: released, with its data said to be stale.
	*f.now = limitsReset5
	f.s.Limits = &limits.Report{}

	did := f.s.limitsStep(*f.now)
	require.Len(t, did, 1)
	assert.Contains(t, did[0], "claude: 5h window reset time 13:12 passed and no fresh reading has come, so its data is stale; ")
	assert.Contains(t, did[0], " go now")
	assert.Empty(t, f.book(t).Holds)
}

func TestLimitsHoldEndsWhenAFreshReadingShowsTheWindowUnderTheShare(t *testing.T) {
	f, _ := heldDesigner(t)

	*f.now = f.now.Add(5 * time.Minute)
	f.claudeLimits(95, 10)

	did := f.s.limitsStep(*f.now)
	require.Len(t, did, 1)
	assert.Contains(t, did[0], "claude: 5h window is at 95%, under its hold share; ")
	assert.Empty(t, f.book(t).Holds)
	assert.False(t, f.s.planHeld("designer", *f.now))
}

func TestLimitsTwoWindowsHoldTheLongerOne(t *testing.T) {
	f, _ := heldDesigner(t)

	// The week reaches the share too: both are held, and the plan waits for the one that resets last.
	*f.now = f.now.Add(time.Minute)
	f.claudeLimits(97, 98)
	did := f.s.limitsStep(*f.now)
	require.Len(t, did, 1)
	assert.Contains(t, did[0], "claude: 7d window at 98%, holding its 4 roles until Tue 12:00")
	require.Len(t, f.book(t).Holds, 2)

	h, ok := f.s.planHold("designer", *f.now)
	require.True(t, ok)
	assert.Equal(t, limits.WindowSevenDay, h.Window)

	// The 5-hour window resets: the plan is held still, no line goes, and the note says what holds it.
	*f.now = limitsReset5.Add(time.Minute)
	f.claudeLimitsResetting(1, 98, limitsReset5.Add(5*time.Hour), limitsReset7)
	did = f.s.limitsStep(*f.now)
	require.Len(t, did, 1)
	assert.Contains(t, did[0], "claude: 5h window reset, but the plan is still held by its 7d window until Tue 12:00; lines wait")
	assert.True(t, f.s.planHeld("designer", *f.now))
	assert.NotContains(t, f.s.Waiting(), "designer")

	*f.now = limitsReset7
	f.s.Limits = &limits.Report{}
	did = f.s.limitsStep(*f.now)
	require.Len(t, did, 1)
	assert.Contains(t, did[0], "claude: 7d window reset time 12:00 passed")
	assert.False(t, f.s.planHeld("designer", *f.now))
}

func TestLimitsCodexPlanHoldsOnlyCodexRoles(t *testing.T) {
	f := fixtureWith(t, limitsCrew)

	for _, name := range []string{"designer", "codex-dev"} {
		rs := f.s.State.Role(name)
		rs.Up = true
		rs.Inbox = []string{"[dispatcher] hello"}
	}

	reset := f.now.Add(48 * time.Hour)
	f.codexLimits(97, reset)

	did := f.s.limitsStep(*f.now)
	require.Len(t, did, 1)
	assert.Equal(t, "limits: codex: weekly window at 97%, holding its 1 role until Mon 12:00; lines wait, the owner's pass", did[0])

	assert.Equal(t, []string{"designer"}, f.s.Waiting(), "the Claude seat is asked, the Codex seat is held")
	assert.True(t, f.s.planHeld("codex-dev", *f.now))
	assert.False(t, f.s.planHeld("designer", *f.now))

	// Its own shares: the Claude plan has no reading at all, and says nothing.
	assert.Equal(t, limits.PlanCodex, f.book(t).Holds[0].Plan)
}

func TestLimitsHoldOfTheDispatchersPlanTellsTheOwnerAndClosesTheItem(t *testing.T) {
	f := fixtureWith(t, limitsCrew)
	store := &asks.Store{Dir: f.s.Crew.Layout().Asks()}
	f.s.Asks = store

	// Another plan held: the dispatcher reads the note, the owner needs no item.
	f.codexLimits(97, f.now.Add(48*time.Hour))
	require.NotEmpty(t, f.s.limitsStep(*f.now))

	items, _, err := store.Scan()
	require.NoError(t, err)
	assert.Empty(t, items)

	// Its own plan: it cannot read the note, so the owner is told through the todo.
	f.claudeLimits(97, 10)
	require.NotEmpty(t, f.s.limitsStep(*f.now))

	items, _, err = store.Scan()
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "dispatcher", items[0].Blocks, "it holds the dispatcher up")
	assert.Equal(t, asks.Check, items[0].Kind)
	assert.True(t, items[0].Open())
	assert.Contains(t, items[0].Title, "claude 5h window at 97%")
	assert.Contains(t, items[0].Text, "resets at 13:12")

	var claude limits.Hold

	for _, h := range f.book(t).Holds {
		if h.Plan == limits.PlanClaude {
			claude = h
		}
	}

	assert.Equal(t, items[0].ID, claude.Ask)

	// The note is queued for the dispatcher all the same, and goes when the hold ends.
	assert.True(t, slicesContain(f.s.State.Notes, "claude: 5h window at 97%, holding its 4 roles until 13:12; lines wait, the owner's pass"))

	*f.now = limitsReset5
	f.s.Limits = &limits.Report{}
	require.NotEmpty(t, f.s.limitsStep(*f.now))

	got, err := store.Get(items[0].ID)
	require.NoError(t, err)
	assert.False(t, got.Open(), "the item closes with the hold")
}

func TestLimitsWithoutAReadingJudgesNothingAndHoldsWhatIsOnTheBook(t *testing.T) {
	f, _ := heldDesigner(t)

	f.s.Limits = nil
	f.s.limitBook = nil // a shift that reads the file afresh, as a command or a courier pass does

	assert.Empty(t, f.s.limitsStep(*f.now))
	assert.True(t, f.s.planHeld("designer", *f.now), "the book alone holds, by the clock")
	assert.False(t, f.s.planHeld("designer", limitsReset5))
}

func TestLimitsPlanWithNoRolesIsNotJudged(t *testing.T) {
	f := fixtureWith(t, "timezone: UTC\n"+crewYAML)

	f.codexLimits(99, f.now.Add(48*time.Hour))
	assert.Empty(t, f.s.limitsStep(*f.now), "no Codex seat: nothing to say of the Codex plan")
	assert.Empty(t, f.book(t).Holds)
}

func TestLimitsOffSharesDoNothing(t *testing.T) {
	f := fixtureWith(t, "timezone: UTC\nlimits:\n  claude: {warn: off, checkpoint: off, hold: off}\n"+crewYAML)

	f.claudeLimits(99, 99)
	assert.Empty(t, f.s.limitsStep(*f.now))
	assert.Empty(t, f.s.State.Notes)
	assert.False(t, f.s.planHeld("designer", *f.now))
}

// A hold postpones a scheduled run instead of skipping it: the run goes once, when the hold ends.
func TestLimitsHoldPostponesAScheduledRun(t *testing.T) {
	f := fixtureWith(t, "timezone: UTC\n"+crewYAML+"  nightly:\n    schedule: \"0 22 * * *\"\n    prompt: run the harvest\n")

	*f.now = time.Date(2026, 10, 24, 21, 58, 0, 0, time.UTC)
	f.s.Tick()
	require.Equal(t, time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC), f.s.State.Role("nightly").NextRun)

	reset := time.Date(2026, 10, 24, 23, 0, 0, 0, time.UTC)

	*f.now = f.now.Add(3 * time.Minute)
	f.claudeLimitsResetting(97, 10, reset, limitsReset7)
	f.s.Tick()

	assert.Empty(t, f.tk.list, "no task made")
	assert.False(t, f.tm.Has("nightly"), "no role started")
	assert.Equal(t, time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC), f.s.State.Role("nightly").NextRun, "the run is still due")
	assert.True(t, f.ev.has("nightly", "schedule-postponed"))
	assert.False(t, f.ev.has("nightly", "schedule-skipped"))

	// Later beats of the hold do not say it again.
	before := len(f.ev.list)
	*f.now = f.now.Add(10 * time.Minute)
	f.s.Tick()
	assert.Empty(t, f.tk.list)

	for _, e := range f.ev.list[before:] {
		assert.NotEqual(t, "schedule-postponed", e.Event)
	}

	// The hold ends: the run goes, once.
	*f.now = reset.Add(time.Minute)
	f.claudeLimitsResetting(2, 10, reset.Add(5*time.Hour), limitsReset7)
	f.s.Tick()

	require.Len(t, f.tk.list, 1)
	assert.Equal(t, "schedule", f.tk.list[0].By)
	assert.True(t, f.tm.Has("nightly"))
	assert.Equal(t, time.Date(2026, 10, 25, 22, 0, 0, 0, time.UTC), f.s.State.Role("nightly").NextRun)
}

func TestLimitsHoldPostponesATrim(t *testing.T) {
	f := fixtureWith(t, "timezone: UTC\n"+crewYAML+"  dev:\n    context_max: 100k\n")
	require.NoError(t, f.s.Up("dev"))

	*f.now = f.now.Add(10 * time.Minute)
	f.rg.context["sid-dev"] = 120_000
	f.status("dev", "idle", f.now.Add(-10*time.Minute))

	// Without a hold the role would be compacted: it is idle, over its limit, with nothing waiting.
	reset := f.now.Add(2 * time.Hour)
	f.claudeLimitsResetting(97, 10, reset, limitsReset7)
	f.s.Tick()
	assert.Zero(t, compactCount(f.tm.typed["dev"]), "a /compact is a model call")

	*f.now = reset.Add(time.Minute)
	f.status("dev", "idle", f.now.Add(-10*time.Minute))
	f.claudeLimitsResetting(3, 10, reset.Add(5*time.Hour), limitsReset7)
	f.s.Tick()

	assert.Equal(t, 1, compactCount(f.tm.typed["dev"]), "once the hold is over")
}

func TestLimitsHoldPostponesAnAlarmThatWakesARole(t *testing.T) {
	f := fixtureWith(t, limitsCrew)
	al := &alarm.Store{Dir: f.s.Crew.Layout().Alarms()}
	f.s.Alarms = al

	_, err := al.Add(alarm.Alarm{Kind: alarm.Remind, Role: "designer", At: f.now.Add(time.Minute), Text: "check the logo"}, *f.now)
	require.NoError(t, err)

	*f.now = f.now.Add(time.Minute)
	f.claudeLimits(97, 10)
	did := f.s.Tick()

	assert.Empty(t, f.s.State.Role("designer").Inbox, "the wake waits")
	assert.False(t, f.tm.Has("designer"), "and the role is not started for it")
	assert.False(t, f.ev.has("designer", "alarm"))

	for _, line := range did {
		assert.NotContains(t, line, "A-1")
	}

	*f.now = limitsReset5.Add(time.Minute)
	f.claudeLimitsResetting(2, 10, limitsReset5.Add(5*time.Hour), limitsReset7)
	f.s.Tick()
	assert.True(t, f.ev.has("designer", "alarm"), "and rings once the hold is over")
	assert.NotEmpty(t, f.s.State.Role("designer").Inbox)
}

// The queue waits for whichever hold ends later: the plan's, or the one a failed turn left.
func TestLimitsHoldAndAFailedTurnsHoldEndLater(t *testing.T) {
	f, _ := heldDesigner(t)
	f.live("designer")
	start := *f.now

	// An account error 60 minutes in holds the queue 30 minutes: past the plan's reset, 72 minutes in.
	failedAt := start.Add(60 * time.Minute)
	*f.now = failedAt
	f.status("designer", "idle", failedAt)
	f.live("designer")
	f.failed(t, "designer", failedInput, failedAt)

	assert.False(t, slicesContain(f.s.Waiting(), "designer"))

	*f.now = limitsReset5.Add(time.Minute)
	f.status("designer", "idle", failedAt)
	f.live("designer")
	assert.False(t, f.s.planHeld("designer", *f.now), "the plan's hold is over")
	assert.True(t, f.s.failureHeld("designer", *f.now), "the failed turn's is on still")
	assert.False(t, slicesContain(f.s.Waiting(), "designer"), "so the line waits")

	*f.now = failedAt.Add(AccountHold + time.Minute)
	assert.True(t, slicesContain(f.s.Waiting(), "designer"))
}

func TestLimitsViewsAndExplainSayTheHold(t *testing.T) {
	f, _ := heldDesigner(t)
	f.live("designer")
	*f.now = f.now.Add(time.Minute)

	var designer, codex *View

	views := f.s.Views()
	for i := range views {
		switch views[i].Role {
		case "designer":
			designer = &views[i]
		case "codex-dev":
			codex = &views[i]
		}
	}

	require.NotNil(t, designer)
	assert.Equal(t, "claude 5h window", designer.PlanHeld)
	assert.True(t, designer.PlanHeldUntil.Equal(limitsReset5))
	require.NotNil(t, codex)
	assert.Empty(t, codex.PlanHeld)

	qs := f.s.Queues()
	require.NotEmpty(t, qs)

	for _, q := range qs {
		if q.Role == "designer" {
			assert.Equal(t, "claude 5h window", q.PlanHeld)
			assert.True(t, q.PlanHeldUntil.Equal(limitsReset5))
		}
	}

	v, r, ok := f.s.Look("designer")
	require.True(t, ok)

	e := f.s.Explain(v, r)
	require.NotNil(t, e.Queue.PlanHold)
	assert.Equal(t, "claude 5h window", e.Queue.PlanHold.Window)
	assert.Equal(t, 97.0, e.Queue.PlanHold.Used)
	assert.True(t, e.Queue.PlanHold.Until.Equal(limitsReset5))

	holds := f.s.PlanHolds(*f.now)
	require.Len(t, holds, 1)
	assert.Equal(t, []string{"dispatcher", "designer", "dev", "harvester"}, holds[0].Roles)
	assert.Equal(t, "claude 5h window at 97%, until 13:12", PlanHoldWords(holds[0].Hold, *f.now, time.UTC))

	// After the reset the views say nothing.
	*f.now = limitsReset5
	f.s.limitBook = nil

	for _, v := range f.s.Views() {
		assert.Empty(t, v.PlanHeld, v.Role)
	}
}

func TestLimitsAFileThatDoesNotReadHoldsNothingAndIsWarnedOf(t *testing.T) {
	f := fixtureWith(t, limitsCrew)
	require.NoError(t, os.MkdirAll(f.s.Crew.Layout().Limits(), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(f.s.Crew.Layout().Limits(), limits.HoldsFile), []byte("{nope"), 0o600))

	var warned []error

	f.s.Warn = func(err error) { warned = append(warned, err) }

	assert.False(t, f.s.planHeld("designer", *f.now))
	require.Len(t, warned, 1)
	assert.Contains(t, warned[0].Error(), "holds.json")
}

func TestPlanHoldWordsAndClocks(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	assert.Equal(t, "13:12", limitClock(limitsReset5, now, time.UTC))
	assert.Equal(t, "Tue 12:00", limitClock(limitsReset7, now, time.UTC))
	assert.Equal(t, "7 Nov 12:00", limitClock(now.Add(14*24*time.Hour), now, time.UTC))

	assert.Equal(t, "1h12m", limitIn(72*time.Minute))
	assert.Equal(t, "45m", limitIn(45*time.Minute))
	assert.Equal(t, "3d0h", limitIn(72*time.Hour))
	assert.Equal(t, "0m", limitIn(-time.Minute))
}

// A restart that asks the role for its note does not begin while the plan is held: the request would wait in a queue nothing
// delivers, and the ten minutes the note is given would run out unread.
func TestLimitsHoldPostponesARestartsRequest(t *testing.T) {
	f := restarting(t, "", 262_000, 6*time.Minute)

	reset := f.now.Add(time.Hour)
	f.claudeLimitsResetting(97, 10, reset, limitsReset7)

	did := f.s.Tick()
	assert.NotContains(t, strings.Join(did, "|"), "asked for its note")
	assert.True(t, f.s.State.Role("designer").RestartAsked.IsZero())
	assert.False(t, f.ev.has("designer", "restart-asked"))

	*f.now = reset.Add(time.Minute)
	f.claudeLimitsResetting(3, 10, reset.Add(5*time.Hour), limitsReset7)
	f.status("designer", "idle", f.now.Add(-6*time.Minute))
	f.rg.activity["sid-designer"] = registry.Activity{Prompt: f.now.Add(-7 * time.Minute)}

	f.s.Tick()
	assert.True(t, f.ev.has("designer", "restart-asked"), "asked once the hold is over")
}

// A role asked for its note before the hold began gets its ten minutes again when the hold ends: they ran out unread.
func TestLimitsReleaseGivesAnAskedRoleItsMinutesAgain(t *testing.T) {
	f := restarting(t, "", 262_000, 6*time.Minute)

	f.s.Tick()
	rs := f.s.State.Role("designer")
	require.False(t, rs.RestartAsked.IsZero())

	*f.now = f.now.Add(time.Minute)
	reset := f.now.Add(time.Hour)
	f.claudeLimitsResetting(97, 10, reset, limitsReset7)
	f.s.Tick()

	*f.now = f.now.Add(30 * time.Minute)
	f.s.Tick()
	assert.False(t, f.ev.has("designer", "restart-off"), "no minutes run while nothing is delivered")
	assert.False(t, rs.RestartAsked.IsZero())

	*f.now = reset.Add(time.Minute)
	f.claudeLimitsResetting(3, 10, reset.Add(5*time.Hour), limitsReset7)
	f.s.Tick()

	assert.True(t, rs.RestartAsked.Equal(*f.now), "its ten minutes start again")
	assert.False(t, f.ev.has("designer", "restart-off"))
	assert.False(t, f.ev.has("designer", "restart-failed"))
}

func TestLimitsHoldDoesNotBeginARelease(t *testing.T) {
	f := releasing(t, "")
	f.tm.screen = "⏺ Drawing the second variant\n"
	f.tm.suggestions["designer"] = "commit the logo"
	f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open, Title: "draw the logo"}}

	reset := f.now.Add(time.Hour)
	f.claudeLimitsResetting(97, 10, reset, limitsReset7)
	f.s.Tick()

	assert.Empty(t, f.tm.typed["designer"], "the release line asks a model for something")
	assert.True(t, f.s.State.Role("designer").Releasing.IsZero())

	*f.now = reset.Add(time.Minute)
	f.status("designer", "idle", f.now.Add(-31*time.Minute))
	f.claudeLimitsResetting(3, 10, reset.Add(5*time.Hour), limitsReset7)
	f.s.Tick()
	require.Len(t, f.tm.typed["designer"], 1)
	assert.Contains(t, f.tm.typed["designer"][0], "[zmina release]")
}

func TestLimitsHoldPostponesTheRestartAtATurnsEnd(t *testing.T) {
	f, p := turnEnding(t, "", 262_000)

	f.claudeLimitsResetting(97, 10, f.now.Add(time.Hour), limitsReset7)
	require.NotEmpty(t, f.s.limitsStep(*f.now))

	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"), "a request for its note would wait in a queue nothing delivers")
	assert.Empty(t, p.posted["sid-designer"])
	assert.Empty(t, f.s.State.Role("designer").RestartDue)
	assert.False(t, f.ev.has("designer", "restart-due"))
}

// A line said again for want of an ack is not said while the role's plan is held: it would only pile up in a queue.
func TestLimitsHoldPostponesASaidAgain(t *testing.T) {
	f, p := nativeFixture(t, crewYAML)
	upNative(t, f, "designer")

	_, err := f.s.QueueTracked("designer", "[dispatcher] look at T-1")
	require.NoError(t, err)
	require.NotEmpty(t, f.s.deliverInbox("designer", *f.now))
	p.showAll()
	f.s.deliverInbox("designer", *f.now)

	f.claudeLimitsResetting(97, 10, f.now.Add(2*time.Hour), limitsReset7)
	require.NotEmpty(t, f.s.limitsStep(*f.now))

	rs := f.s.State.Role("designer")

	f.s.renotify(f.now.Add(40 * time.Minute))
	assert.Empty(t, rs.Inbox, "held: not said again")

	f.s.renotify(f.now.Add(3 * time.Hour))
	assert.Len(t, rs.Inbox, 1, "said again once the hold is over")
}

// A hold the owner turns off in the crew file ends at the next beat, with no reading needed, and its item on the todo closes.
func TestLimitsHoldEndsWhenTheCrewFileTurnsItOff(t *testing.T) {
	f := fixtureWith(t, limitsCrew)
	store := &asks.Store{Dir: f.s.Crew.Layout().Asks()}
	f.s.Asks = store

	f.claudeLimits(97, 10)
	require.NotEmpty(t, f.s.limitsStep(*f.now))
	require.True(t, f.s.planHeld("designer", *f.now))

	f.s.Crew.Limits = &crew.Limits{Claude: &crew.ClaudeLimits{Hold: &crew.Share{Off: true}}}
	f.s.Limits = &limits.Report{}
	*f.now = f.now.Add(time.Minute)

	did := f.s.limitsStep(*f.now)
	require.Len(t, did, 1)
	assert.Contains(t, did[0], "claude: 5h window hold is turned off in the crew file; ")
	assert.False(t, f.s.planHeld("designer", *f.now))

	items, _, err := store.Scan()
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.False(t, items[0].Open())
}
