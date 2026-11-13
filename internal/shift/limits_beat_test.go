package shift

import (
	"fmt"
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
	"github.com/iamnikolie/zmina/internal/codex"
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

	return heldDesignerIn(t, limitsCrew)
}

// heldDesignerIn is heldDesigner in a crew of the given file.
func heldDesignerIn(t *testing.T, body string) (fix, *fakePeer) {
	t.Helper()

	f, p := nativeFixture(t, body)
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
	require.Len(t, f.book(t).Periods, 1)
	assert.False(t, f.book(t).Periods[0].Warn.IsZero())

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

func TestLimitsHoldEndsAtTheResetAndTheLinesGoInTurn(t *testing.T) {
	f, p := heldDesigner(t)
	f.live("designer")

	// The window rolled over and a status line since drew the new period: confirmed.
	*f.now = limitsReset5.Add(time.Minute)
	f.status("designer", "idle", *f.now)
	f.live("designer")
	f.claudeLimitsResetting(2, 10, limitsReset5.Add(5*time.Hour), limitsReset7)

	lines := f.s.planLines(f.s.PlanRoles(limits.PlanClaude))
	require.Positive(t, lines)

	did := f.s.limitsStep(*f.now)
	require.Len(t, did, 1)
	assert.Equal(t, "limits: claude: 5h window reset, "+strconv.Itoa(lines)+" lines go: the dispatcher now, the other 3 roles 3 at a time, a minute apart", did[0])
	assert.True(t, f.ev.has("", "limit-release"))
	assert.Empty(t, f.book(t).Holds)

	// The dispatcher goes at once; the roles wait their turn, a group a minute, the one that waited longest first.
	assert.NotContains(t, f.s.Waiting(), "designer", "its turn is a minute on")
	assert.Empty(t, f.s.TurnEnd("designer", "sid-designer"))
	assert.Empty(t, p.posted["sid-designer"])

	*f.now = f.now.Add(time.Minute)
	f.status("designer", "idle", *f.now)
	f.live("designer")
	assert.True(t, slicesContain(f.s.Waiting(), "designer"))

	// Its queue goes in its order on the normal path (the two lines as one post).
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
	assert.Contains(t, did[0], " go: the dispatcher and one role now, the other 2 roles when a fresh reading shows the window under its hold share, or in 10 minutes")
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

	_, held := f.s.planHold("designer", *f.now)
	assert.False(t, held)
	assert.True(t, f.s.planHeld("designer", *f.now), "its turn is a minute on")
	assert.False(t, f.s.planHeld("designer", f.now.Add(time.Minute)))
}

func TestLimitsTwoWindowsHoldTheLongerOne(t *testing.T) {
	// The week is held too only when the crew file opts it in.
	f, _ := heldDesignerIn(t, "timezone: UTC\nlimits:\n  claude: {hold: {seven_day: 97}}\n"+crewYAML+"  dev:\n  codex-dev:\n    runtime: codex\n")

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
	assert.Empty(t, f.book(t).Waits, "no role is paced while the week still holds the plan")

	*f.now = limitsReset7
	f.s.Limits = &limits.Report{}
	did = f.s.limitsStep(*f.now)
	require.Len(t, did, 1)
	assert.Contains(t, did[0], "claude: 7d window reset time 12:00 passed")
	assert.False(t, f.s.planHeld("designer", f.now.Add(11*time.Minute)), "its turn has come by the time the data is called stale")
}

func TestLimitsCodexPlanHoldsOnlyCodexRoles(t *testing.T) {
	f := fixtureWith(t, codexHoldCrew)

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

	// The hold ends: the role waits its turn, a minute, and then the run goes, once.
	*f.now = reset.Add(time.Minute)
	f.claudeLimitsResetting(2, 10, reset.Add(5*time.Hour), limitsReset7)
	f.s.Tick()
	assert.Empty(t, f.tk.list)

	*f.now = f.now.Add(2 * time.Minute)
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
	assert.Zero(t, compactCount(f.tm.typed["dev"]), "the hold is over, the role's turn is a minute on")

	*f.now = f.now.Add(2 * time.Minute)
	f.status("dev", "idle", f.now.Add(-10*time.Minute))
	f.claudeLimitsResetting(3, 10, reset.Add(5*time.Hour), limitsReset7)
	f.s.Tick()

	assert.Equal(t, 1, compactCount(f.tm.typed["dev"]), "once its turn has come")
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
	assert.False(t, f.ev.has("designer", "alarm"), "the hold is over, the role's turn is a minute on")

	*f.now = f.now.Add(2 * time.Minute)
	f.claudeLimitsResetting(2, 10, limitsReset5.Add(5*time.Hour), limitsReset7)
	f.s.Tick()
	assert.True(t, f.ev.has("designer", "alarm"), "and rings once its turn has come")
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

// A reader says nothing of a holds file that does not read: it holds nothing, and the beat is the one that moves it aside, once.
func TestLimitsAFileThatDoesNotReadHoldsNothingAndReadersAreSilent(t *testing.T) {
	f := fixtureWith(t, limitsCrew)
	require.NoError(t, os.MkdirAll(f.s.Crew.Layout().Limits(), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(f.s.Crew.Layout().Limits(), limits.HoldsFile), []byte("{nope"), 0o600))

	var warned []error

	f.s.Warn = func(err error) { warned = append(warned, err) }

	for i := 0; i < 5; i++ {
		assert.False(t, f.s.planHeld("designer", *f.now))
		f.s.limitBook = nil
	}

	assert.Empty(t, warned, "not on every reader call")
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
	assert.False(t, f.ev.has("designer", "restart-asked"), "the hold is over, the role's turn is a minute on")

	*f.now = f.now.Add(2 * time.Minute)
	f.claudeLimitsResetting(3, 10, reset.Add(5*time.Hour), limitsReset7)
	f.status("designer", "idle", f.now.Add(-6*time.Minute))
	f.rg.activity["sid-designer"] = registry.Activity{Prompt: f.now.Add(-7 * time.Minute)}

	f.s.Tick()
	assert.True(t, f.ev.has("designer", "restart-asked"), "asked once its turn has come")
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
	assert.Empty(t, f.tm.typed["designer"], "the hold is over, the role's turn is a minute on")

	*f.now = f.now.Add(2 * time.Minute)
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

	f.s.Crew.Limits = &crew.Limits{Claude: &crew.ClaudeLimits{Hold: &crew.HoldShares{Short: &crew.Share{Off: true}}}}
	f.s.Limits = &limits.Report{}
	*f.now = f.now.Add(time.Minute)

	did := f.s.limitsStep(*f.now)
	require.Len(t, did, 1)
	assert.Contains(t, did[0], "claude: 5h window hold is turned off in the crew file; ")

	_, held := f.s.planHold("designer", *f.now)
	assert.False(t, held)

	items, _, err := store.Scan()
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.False(t, items[0].Open())
}

// codexHoldCrew is the crew with its Codex seat's week opted in to a hold (a long window is not held by default).
const codexHoldCrew = "timezone: UTC\nlimits:\n  codex: {hold: {week: 97}}\n" + crewYAML + "  dev:\n  codex-dev:\n    runtime: codex\n"

// heldCodex is a crew whose Codex seat is up, past its boot and idle with two lines queued for it and its plan held. The seat takes its
// lines on its bridge's inbox when native, typed into its console when not.
func heldCodex(t *testing.T, native bool) (fix, *fakePeer) {
	t.Helper()

	f, p := nativeFixture(t, codexHoldCrew)
	if native {
		f.s.CodexInbox = p
	}

	require.NoError(t, f.s.Up("codex-dev"))

	r, err := f.s.Role("codex-dev")
	require.NoError(t, err)

	pane, err := f.s.probe("codex-dev")
	require.NoError(t, err)

	reg := &codex.Registry{Dir: filepath.Join(f.s.Crew.StateDir(), "codex")}
	require.NoError(t, reg.Save(codex.Record{Session: registry.Session{PID: pane.PID, Name: r.Session, SessionID: "thread", Status: "idle",
		StatusUpdatedAt: f.now.UnixMilli()}}))

	f.s.State.Role("codex-dev").Started = f.now.Add(-time.Hour)

	require.NoError(t, f.s.Queue("codex-dev", "[dispatcher] first"))
	require.NoError(t, f.s.Queue("codex-dev", "[dispatcher] second"))

	f.codexLimits(98, f.now.Add(48*time.Hour))
	require.NotEmpty(t, f.s.limitsStep(*f.now))
	require.True(t, f.s.planHeld("codex-dev", *f.now))

	return f, p
}

// ownerSays queues a line of the owner's own (Chat's send writes a receipt for it) behind what waits.
func ownerSays(t *testing.T, f fix, role, line string) {
	t.Helper()

	require.NoError(t, f.s.QueueOwner(role, line))

	rs := f.s.State.Role(role)
	rs.ChatReceipts = append(rs.ChatReceipts, chat.Receipt{ID: "c1", Line: line, At: *f.now})
}

// The owner's own line passes a plan hold for a Codex seat as it does for a Claude one, at every gate; what zmina queued waits.
func TestLimitsHoldLetsTheOwnersLineThroughToACodexSeat(t *testing.T) {
	const owner = "stop, look at this first"

	gates := map[string]func(f fix){
		"the delivery pass":   func(f fix) { _, err := f.s.DeliverPass(); require.NoError(t, err) },
		"the courier's pass":  func(f fix) { _, err := f.s.DeliverRoles([]string{"codex-dev"}); require.NoError(t, err) },
		"the beat's delivery": func(f fix) { f.s.DeliverTo("codex-dev") },
		"a flush":             func(f fix) { f.s.FlushInbox("codex-dev") },
		"a turn's end":        func(f fix) { f.s.TurnEnd("codex-dev", "thread") },
		"the whole beat":      func(f fix) { f.s.Tick() },
	}

	for _, native := range []bool{false, true} {
		for name, gate := range gates {
			if name == "a turn's end" && !native {
				continue // a turn's end is the bridge's hook: a seat that is typed to has none
			}

			t.Run(fmt.Sprintf("native=%v/%s", native, name), func(t *testing.T) {
				f, p := heldCodex(t, native)
				rs := f.s.State.Role("codex-dev")

				// Without the owner's line the seat is held, whichever gate asks.
				assert.False(t, slicesContain(f.s.Waiting(), "codex-dev"))
				gate(f)
				assert.Empty(t, f.tm.typed["codex-dev"])
				assert.Empty(t, p.posted["thread"])
				assert.Equal(t, []string{"[dispatcher] first", "[dispatcher] second"}, rs.Inbox)

				ownerSays(t, f, "codex-dev", owner)
				assert.True(t, slicesContain(f.s.Waiting(), "codex-dev"), "the owner's line is in the queue: the seat is asked")

				gate(f)

				if native {
					assert.Equal(t, []string{owner}, p.posted["thread"], "posted alone, not joined with the lines before it")
					assert.Empty(t, f.tm.typed["codex-dev"])
				} else {
					assert.Equal(t, []string{owner}, f.tm.typed["codex-dev"])
				}

				assert.Equal(t, []string{"[dispatcher] first", "[dispatcher] second"}, rs.Inbox, "what zmina queued waits, in its order")
			})
		}
	}
}

// A held Codex seat's full queue still takes the owner's message.
func TestLimitsHoldTakesTheOwnersLineForAFullCodexQueue(t *testing.T) {
	f, _ := heldCodex(t, false)
	rs := f.s.State.Role("codex-dev")

	for len(rs.Inbox) < inboxMax {
		rs.Inbox = append(rs.Inbox, "filler")
	}

	require.NoError(t, f.s.QueueOwner("codex-dev", "one more from the owner"))
	assert.Len(t, rs.Inbox, inboxMax+1)
}

// Putting down an idle role that holds no task asks a model for nothing: a hold does not stop it.
func TestLimitsHoldDoesNotStopAReleaseThatAsksNothing(t *testing.T) {
	f := releasing(t, "")
	f.tm.screen = "⏺ Done.\n"
	f.tm.suggestions["designer"] = "commit the logo"

	f.claudeLimitsResetting(97, 10, f.now.Add(time.Hour), limitsReset7)
	f.s.Tick()

	assert.True(t, f.ev.has("designer", "released"), "no task: it goes down at once, held or not")
	assert.Equal(t, []string{"/exit"}, f.tm.typed["designer"], "it is told to end, nothing a model answers")
}

// A checkpoint saved when the thresholds asked for one is not the note a restart is waiting for: "no restart follows" is true of the
// request, and a restart at restart_at asks for its own note.
func TestALimitCheckpointIsNotTheNoteARestartWaitsFor(t *testing.T) {
	f, p := turnEnding(t, "", 262_000)

	// The thresholds asked the plan's roles for a checkpoint a few minutes ago; the role saved one in its last turn.
	asked := f.now.Add(-5 * time.Minute)
	require.NoError(t, limits.WriteBook(f.s.Crew.Layout().Limits(), limits.Book{Periods: []limits.Period{
		{Plan: limits.PlanClaude, Window: limits.WindowFiveHour, Reset: f.now.Add(time.Hour), Warn: asked, Checkpoint: asked}}}))
	f.s.limitBook = nil // read afresh, as a shift of another process does
	f.note(t, "T-1", f.now.Add(-30*time.Second))

	did := f.s.TurnEnd("designer", "sid-designer")
	require.NotEmpty(t, did, "the restart asks for a note of its own")
	assert.Contains(t, did[0], "asked for its note to restart fresh")
	require.Len(t, p.posted["sid-designer"], 1)
	assert.True(t, strings.HasPrefix(p.posted["sid-designer"][0], "[zmina] restart: "))

	// A checkpoint saved before the thresholds asked is the role's own note as ever.
	g, _ := turnEnding(t, "", 262_000)
	require.NoError(t, limits.WriteBook(g.s.Crew.Layout().Limits(), limits.Book{Periods: []limits.Period{
		{Plan: limits.PlanClaude, Window: limits.WindowFiveHour, Reset: g.now.Add(time.Hour), Warn: g.now.Add(-10 * time.Second), Checkpoint: g.now.Add(-10 * time.Second)}}}))
	g.s.limitBook = nil
	g.note(t, "T-1", g.now.Add(-30*time.Second))
	assert.Empty(t, g.s.TurnEnd("designer", "sid-designer"), "the note is there: nothing to ask for")
}

// A postponed run is said once, however late the beat that first finds it is, and not again for as long as it is postponed; the next run
// postponed is said once too.
func TestLimitsHoldSaysAPostponedRunOnce(t *testing.T) {
	f := fixtureWith(t, "timezone: UTC\n"+crewYAML+"  nightly:\n    schedule: \"0 22 * * *\"\n    prompt: run the harvest\n")

	*f.now = time.Date(2026, 10, 24, 21, 58, 0, 0, time.UTC)
	f.s.Tick()
	require.Equal(t, time.Date(2026, 10, 24, 22, 0, 0, 0, time.UTC), f.s.State.Role("nightly").NextRun)

	count := func() int { return f.ev.count("nightly", "schedule-postponed") }

	// The ticker was down: the first beat to find the run is minutes late, and the hold is on.
	*f.now = time.Date(2026, 10, 24, 22, 5, 0, 0, time.UTC)
	f.claudeLimitsResetting(97, 10, time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC).Add(-time.Hour), limitsReset7)
	f.s.limitBook = nil

	for i := 0; i < 4; i++ {
		f.s.limitBook = nil // each beat is a process of its own
		f.s.Tick()

		*f.now = f.now.Add(time.Minute)
		f.claudeLimitsResetting(97, 10, time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC), limitsReset7)
	}

	assert.Equal(t, 1, count(), "said once")
	assert.Empty(t, f.tk.list)

	// The next day's run, postponed too, is one more.
	*f.now = time.Date(2026, 10, 25, 1, 59, 0, 0, time.UTC)
	f.claudeLimitsResetting(97, 10, time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC).Add(4*time.Hour), limitsReset7)
	f.s.State.Role("nightly").NextRun = time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)

	f.s.limitBook = nil
	f.s.Tick()
	f.s.limitBook = nil
	f.s.Tick()
	assert.Equal(t, 2, count(), "another run, another word")
}

// manyRoles is a crew of a dispatcher and five seats on the Claude plan, none up: their queues are what is released.
const manyRoles = "timezone: UTC\nnamespace: /ns\nclaude: claude\nroles:\n  dispatcher:\n    boot: read d and boot\n" +
	"  dev1:\n  dev2:\n  dev3:\n  dev4:\n  dev5:\n  dev6:\n    keep: always\n"

// A release lets the dispatcher go at once and then three roles a minute, the role whose oldest line waited longest first; a role with
// nothing waiting goes last. The gate is the book's, so the courier, which reads it, keeps to the same turns.
func TestLimitsReleasePacesTheRolesOldestQueueFirst(t *testing.T) {
	f := fixtureWith(t, manyRoles)

	// dev3's line is the oldest, then dev1's, dev5's, dev2's; dev4 and dev6 have none.
	for _, name := range []string{"dev3", "dev1", "dev5", "dev2"} {
		require.NoError(t, f.s.Queue(name, "[dispatcher] for "+name))
		*f.now = f.now.Add(time.Minute)
	}

	f.s.FlushCards()

	reset := f.now.Add(time.Hour)
	f.claudeLimitsResetting(97, 10, reset, limitsReset7)
	require.NotEmpty(t, f.s.limitsStep(*f.now))

	*f.now = reset
	f.claudeLimitsResetting(2, 10, reset.Add(5*time.Hour), limitsReset7)
	did := f.s.limitsStep(*f.now)
	require.Len(t, did, 1)
	assert.Contains(t, did[0], "claude: 5h window reset, 5 lines go: the dispatcher now, the other 6 roles 3 at a time, a minute apart")

	turn := map[string]time.Duration{}
	for _, w := range f.book(t).Waits {
		turn[w.Role] = w.Until.Sub(reset)
	}

	assert.Equal(t, map[string]time.Duration{
		"dev3": time.Minute, "dev1": time.Minute, "dev5": time.Minute,
		"dev2": 2 * time.Minute, "dev4": 2 * time.Minute, "dev6": 2 * time.Minute,
	}, turn, "oldest queue first, those with nothing waiting last, three to a group")

	// The dispatcher is not held; the first group is, until its minute; a role with keep: always is not started before its turn.
	assert.False(t, f.s.planHeld("dispatcher", *f.now))
	assert.True(t, f.s.planHeld("dev3", *f.now))
	assert.True(t, f.s.planHeld("dev6", f.now.Add(90*time.Second)))
	assert.False(t, f.s.planHeld("dev3", f.now.Add(time.Minute)))
	assert.False(t, f.s.planHeld("dev6", f.now.Add(2*time.Minute)))

	// The courier's look is the same: a role up with a line, in its minute, is left out of what Waiting names, and named once it is over.
	for _, name := range []string{"dev3", "dev1"} {
		rs := f.s.State.Role(name)
		rs.Up = true
	}

	assert.NotContains(t, f.s.Waiting(), "dev3")

	*f.now = f.now.Add(time.Minute)
	f.claudeLimitsResetting(2, 10, reset.Add(5*time.Hour), limitsReset7)
	assert.Contains(t, f.s.Waiting(), "dev3")
	assert.Contains(t, f.s.Waiting(), "dev1")
	assert.NotContains(t, f.s.Waiting(), "dev2", "not up")

	// The ticker starts nothing of a role waiting its turn.
	f.s.Tick()
	assert.False(t, f.tm.Has("dev6"), "keep: always, and its turn is a minute on")

	*f.now = f.now.Add(2 * time.Minute)
	f.claudeLimitsResetting(2, 10, reset.Add(5*time.Hour), limitsReset7)
	f.s.Tick()
	assert.True(t, f.tm.Has("dev6"), "its turn has come")
	assert.Empty(t, f.book(t).Waits, "the book is clear once every turn has come")
}

// A hold that ended by the clock with no fresh reading lets the dispatcher and one role go; the others wait for a reading under the share,
// or ten minutes.
func TestLimitsUnconfirmedReleaseLetsOneRoleGoFirst(t *testing.T) {
	f := fixtureWith(t, manyRoles)

	for _, name := range []string{"dev3", "dev1", "dev5"} {
		require.NoError(t, f.s.Queue(name, "[dispatcher] for "+name))
		*f.now = f.now.Add(time.Minute)
	}

	f.s.FlushCards()

	reset := f.now.Add(time.Hour)
	f.claudeLimitsResetting(97, 10, reset, limitsReset7)
	require.NotEmpty(t, f.s.limitsStep(*f.now))

	*f.now = reset
	f.s.Limits = &limits.Report{}

	did := f.s.limitsStep(*f.now)
	require.Len(t, did, 1)
	assert.Contains(t, did[0], "so its data is stale; 4 lines go: the dispatcher and one role now, the other 5 roles when a fresh reading shows the window under its hold share, or in 10 minutes")

	assert.False(t, f.s.planHeld("dev3", *f.now), "the one whose line waited longest goes with the dispatcher")

	for _, name := range []string{"dev1", "dev5", "dev2", "dev4", "dev6"} {
		assert.True(t, f.s.planHeld(name, f.now.Add(9*time.Minute)), name)
	}

	// A fresh reading under the share, five minutes on: the rest go, in groups, from now.
	*f.now = f.now.Add(5 * time.Minute)
	f.claudeLimitsResetting(3, 10, reset.Add(5*time.Hour), limitsReset7)
	f.s.limitsStep(*f.now)

	assert.False(t, f.s.planHeld("dev1", *f.now))
	assert.False(t, f.s.planHeld("dev5", *f.now))
	assert.False(t, f.s.planHeld("dev2", *f.now))
	assert.True(t, f.s.planHeld("dev4", *f.now), "the second group is a minute on")
	assert.False(t, f.s.planHeld("dev4", f.now.Add(time.Minute)))

	// No reading: ten minutes after the release they all go by the clock.
	g := fixtureWith(t, manyRoles)
	require.NoError(t, g.s.Queue("dev1", "[dispatcher] one"))
	g.claudeLimitsResetting(97, 10, g.now.Add(time.Hour), limitsReset7)
	require.NotEmpty(t, g.s.limitsStep(*g.now))

	*g.now = g.now.Add(time.Hour)
	g.s.Limits = &limits.Report{}
	g.s.limitsStep(*g.now)
	assert.True(t, g.s.planHeld("dev2", g.now.Add(9*time.Minute)))

	*g.now = g.now.Add(10 * time.Minute)
	g.s.limitsStep(*g.now)
	assert.False(t, g.s.planHeld("dev2", *g.now))
	assert.Empty(t, g.book(t).Waits)
}

// With a limits directory that cannot be written the book is never kept, so nothing is held or told, no todo item is opened for the owner
// every beat, and the one event an hour says why.
func TestLimitsUnwritableBookDoesNothingElseAndSaysWhyOnceAnHour(t *testing.T) {
	f := fixtureWith(t, limitsCrew)
	store := &asks.Store{Dir: f.s.Crew.Layout().Asks()}
	f.s.Asks = store

	require.NoError(t, os.WriteFile(f.s.Crew.Layout().Limits(), []byte("a file where the directory goes"), 0o600))

	for i := 0; i < 5; i++ {
		f.claudeLimits(97, 10)
		assert.Empty(t, f.s.limitsStep(*f.now), "beat %d", i)
		*f.now = f.now.Add(10 * time.Minute)
	}

	items, _, err := store.Scan()
	require.NoError(t, err)
	assert.Empty(t, items, "no todo item for a hold that could not be kept")
	assert.Empty(t, f.s.State.Notes)
	assert.False(t, f.s.planHeld("dispatcher", *f.now))
	assert.Equal(t, 1, f.ev.count("", "limits-problem"), "fifty minutes: said once")

	*f.now = f.now.Add(20 * time.Minute)
	f.claudeLimits(97, 10)
	f.s.limitsStep(*f.now)
	assert.Equal(t, 2, f.ev.count("", "limits-problem"), "an hour on, said again")
}

// The todo item of a hold is made after the book stands, and its id goes in with a second write.
func TestLimitsHoldOfTheDispatchersPlanKeepsTheItemIdInTheBook(t *testing.T) {
	f := fixtureWith(t, limitsCrew)
	f.s.Asks = &asks.Store{Dir: f.s.Crew.Layout().Asks()}

	f.claudeLimits(97, 10)
	require.NotEmpty(t, f.s.limitsStep(*f.now))

	require.Len(t, f.book(t).Holds, 1)
	assert.Equal(t, "A-1", f.book(t).Holds[0].Ask)

	// A beat later nothing is made again.
	*f.now = f.now.Add(time.Minute)
	f.claudeLimits(97, 10)
	f.s.limitsStep(*f.now)

	items, _, err := f.s.Asks.(*asks.Store).Scan()
	require.NoError(t, err)
	assert.Len(t, items, 1)
}

// A holds file that does not read is moved aside, once, and a new one is written; readers say nothing.
func TestLimitsBeatMovesACorruptHoldsFileAsideAndWritesANewOne(t *testing.T) {
	f := fixtureWith(t, limitsCrew)
	dir := f.s.Crew.Layout().Limits()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, limits.HoldsFile), []byte("{nope"), 0o600))

	f.claudeLimits(97, 10)
	require.NotEmpty(t, f.s.limitsStep(*f.now))

	aside, err := filepath.Glob(filepath.Join(dir, limits.HoldsFile+".bad-*"))
	require.NoError(t, err)
	require.Len(t, aside, 1)

	raw, err := os.ReadFile(aside[0])
	require.NoError(t, err)
	assert.Equal(t, "{nope", string(raw))

	assert.Len(t, f.book(t).Holds, 1, "the new book holds")
	assert.Equal(t, 1, f.ev.count("", "limits-file-bad"))

	*f.now = f.now.Add(time.Minute)
	f.claudeLimits(97, 10)
	f.s.limitsStep(*f.now)
	assert.Equal(t, 1, f.ev.count("", "limits-file-bad"), "once")
}

// A holds file of a newer zmina is left alone: nothing is held by it, nothing is written over it, and the one event says so.
func TestLimitsBeatLeavesAHoldsFileOfANewerZminaAlone(t *testing.T) {
	f := fixtureWith(t, limitsCrew)
	dir := f.s.Crew.Layout().Limits()
	require.NoError(t, os.MkdirAll(dir, 0o700))

	const newer = `{"v":9,"holds":[{"plan":"claude","window":"five_hour","reset":"2099-01-01T00:00:00Z"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, limits.HoldsFile), []byte(newer), 0o600))

	for i := 0; i < 3; i++ {
		f.claudeLimits(97, 10)
		assert.Empty(t, f.s.limitsStep(*f.now))
		f.s.limitBook = nil
		*f.now = f.now.Add(time.Minute)
	}

	assert.False(t, f.s.planHeld("designer", *f.now), "nothing is held by what this zmina cannot read")

	raw, err := os.ReadFile(filepath.Join(dir, limits.HoldsFile))
	require.NoError(t, err)
	assert.Equal(t, newer, string(raw), "left as it was")

	matches, _ := filepath.Glob(filepath.Join(dir, limits.HoldsFile+".bad-*"))
	assert.Empty(t, matches, "not moved aside either")
	assert.Equal(t, 1, f.ev.count("", "limits-problem"))
}

// If holds.json is lost, the todo item of the hold is closed by the next beat.
func TestLimitsBeatClosesAnOrphanedHoldItem(t *testing.T) {
	f := fixtureWith(t, limitsCrew)
	store := &asks.Store{Dir: f.s.Crew.Layout().Asks()}
	f.s.Asks = store

	f.claudeLimits(97, 10)
	require.NotEmpty(t, f.s.limitsStep(*f.now))

	items, _, err := store.Scan()
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.True(t, items[0].Open())

	// Another item, the owner's own, stays.
	other, err := store.Add(asks.Ask{Kind: asks.Check, Title: "the palette", Role: "designer"}, *f.now)
	require.NoError(t, err)

	require.NoError(t, os.Remove(filepath.Join(f.s.Crew.Layout().Limits(), limits.HoldsFile)))
	f.s.limitBook = nil

	// The next beat reads the 5-hour window at 94%: no hold stands, and the item has nothing to answer for.
	*f.now = f.now.Add(time.Minute)
	f.claudeLimits(94, 10)
	f.s.limitsStep(*f.now)

	got, err := store.Get(items[0].ID)
	require.NoError(t, err)
	assert.False(t, got.Open(), "closed")

	still, err := store.Get(other.ID)
	require.NoError(t, err)
	assert.True(t, still.Open(), "an item that is not a hold's is none of its business")
}

// A checkpoint asked and not yet read is dropped when the hold begins and when it ends, so it is not read after the reset.
func TestLimitsDropsAStaleCheckpointAskAtTheHoldAndAtTheRelease(t *testing.T) {
	for _, when := range []string{"at the hold", "at the release"} {
		t.Run(when, func(t *testing.T) {
			f := fixtureWith(t, limitsCrew)
			require.NoError(t, f.s.Up("designer"))
			f.tk.list = []task.Task{{ID: "T-1", Role: "designer", State: task.Open, Title: "draw the logo"}}

			// At 91% the designer is asked and has not read the line: it waits in its queue behind another.
			require.NoError(t, f.s.Queue("designer", "[dispatcher] first"))
			f.claudeLimits(91, 10)
			require.NotEmpty(t, f.s.limitsStep(*f.now))

			rs := f.s.State.Role("designer")
			require.Len(t, rs.Inbox, 2)
			assert.True(t, strings.HasPrefix(rs.Inbox[1], limitCheckpointOpening))

			*f.now = f.now.Add(time.Minute)

			if when == "at the hold" {
				f.claudeLimits(97, 10)
				require.NotEmpty(t, f.s.limitsStep(*f.now))
				assert.Equal(t, []string{"[dispatcher] first"}, rs.Inbox, "dropped, what else waits stays")

				return
			}

			// Without the hold ever dropping it (a hold the checkpoint was queued during), the release does: put one back first.
			f.claudeLimits(97, 10)
			require.NotEmpty(t, f.s.limitsStep(*f.now))
			rs.Inbox = append(rs.Inbox, limitCheckpointOpening+"the 5h window of your plan is at 97%. Read \"/x\", save each checkpoint")

			*f.now = limitsReset5
			f.s.Limits = &limits.Report{}
			require.NotEmpty(t, f.s.limitsStep(*f.now))

			assert.Equal(t, []string{"[dispatcher] first"}, rs.Inbox)
		})
	}
}

// Seats of another Codex home bill another account: a hold one home's reading made does not keep them.
func TestLimitsCodexHoldKeepsOnlyTheSeatsOfTheHomeThatMadeIt(t *testing.T) {
	f := fixtureWith(t, "timezone: UTC\nlimits:\n  codex: {hold: {week: 97}}\n"+crewYAML+"  cx-a:\n    runtime: codex\n  cx-b:\n    runtime: codex\n    codex_home: /home/alex/.codex-b\n")

	homeA := f.s.Crew.CodexHomeOf(f.s.Crew.Roles["cx-a"])
	homeB := f.s.Crew.CodexHomeOf(f.s.Crew.Roles["cx-b"])
	require.NotEqual(t, homeA, homeB)
	assert.Equal(t, "/home/alex/.codex-b", homeB)
	assert.Empty(t, f.s.Crew.CodexHomeOf(f.s.Crew.Roles["designer"]), "a Claude seat has none")

	reset := f.now.Add(48 * time.Hour)
	f.s.Limits = &limits.Report{Codex: &limits.Codex{Week: &limits.Window{Used: 98, Resets: &reset}, At: *f.now, Home: homeB}}

	did := f.s.limitsStep(*f.now)
	require.Len(t, did, 1)
	assert.Contains(t, did[0], "codex: weekly window at 98%, holding its 1 role until")

	assert.True(t, f.s.planHeld("cx-b", *f.now), "its own home's reading")
	assert.False(t, f.s.planHeld("cx-a", *f.now), "another account")
	assert.False(t, f.s.planHeld("designer", *f.now))
	assert.Equal(t, homeB, f.book(t).Holds[0].Home)
}

// While the dispatcher's plan is held, nothing reads what waits for it for as long as the hold lasts (a week at the longest): its queue of
// signals takes up to a hundred, one note says how many more were refused, and the note of the hold is never pushed out by the health notes.
func TestLimitsHoldOfTheDispatchersPlanTakesMoreSignalsAndKeepsItsNote(t *testing.T) {
	f := fixtureWith(t, limitsCrew)

	// Not held: the cap is as it always was.
	for i := 0; i < SignalsMax; i++ {
		_, err := f.s.Note("dev", fmt.Sprintf("result %d", i), false)
		require.NoError(t, err)
	}

	_, err := f.s.Note("dev", "one too many", false)
	require.Error(t, err)
	assert.Empty(t, f.s.State.Notes, "no summary while nothing is held")

	f.s.State.Signals, f.s.State.SignalCounts = nil, nil

	f.claudeLimits(97, 10)
	require.NotEmpty(t, f.s.limitsStep(*f.now))
	require.True(t, f.s.planHeld("dispatcher", *f.now))

	hold := "claude: 5h window at 97%, holding its 4 roles until 13:12; lines wait, the owner's pass"
	require.Contains(t, f.s.State.Notes, hold)

	for i := 0; i < signalsHeldMax; i++ {
		_, err := f.s.Note("dev", fmt.Sprintf("result %d", i), false)
		require.NoError(t, err, "signal %d", i)
	}

	assert.Len(t, f.s.State.Signals, signalsHeldMax)

	// Past it a signal is refused, and one note says how many, as many times as it comes.
	for i := 0; i < 3; i++ {
		_, err := f.s.Note("dev", fmt.Sprintf("too many %d", i), false)
		require.Error(t, err)
	}

	assert.Len(t, f.s.State.Signals, signalsHeldMax)

	var summaries []string

	for _, n := range f.s.State.Notes {
		if strings.HasPrefix(n, signalsRefusedNote) {
			summaries = append(summaries, n)
		}
	}

	require.Len(t, summaries, 1, "one note, not one for each")
	assert.Equal(t, signalsRefusedNote+"3 signals from the roles were refused while the dispatcher was held (it takes 100): zmina queue ls, zmina note", summaries[0])

	// The health notes pile up behind it: the oldest of them go, the hold's note and the summary do not.
	for i := 0; i < 2*notesMax; i++ {
		f.s.addNotes([]string{fmt.Sprintf("dev%d: busy for 95 min", i)})
	}

	assert.Contains(t, f.s.State.Notes, hold)
	assert.Contains(t, f.s.State.Notes, summaries[0])
	assert.LessOrEqual(t, len(f.s.State.Notes), notesMax+2)
	assert.Contains(t, f.s.State.Notes, fmt.Sprintf("dev%d: busy for 95 min", 2*notesMax-1), "the newest health notes stay")
	assert.NotContains(t, f.s.State.Notes, "dev0: busy for 95 min", "the oldest go")
}

// zmina send without --when-idle types into the role's prompt at once, which a hold would let through: from a role (the dispatcher
// included) the line is queued instead and the caller is told; the owner's own send goes.
func TestLimitsHoldQueuesASendThatIsNotTheOwners(t *testing.T) {
	f, p := heldDesigner(t)
	f.status("designer", "idle", *f.now)
	f.live("designer")

	rs := f.s.State.Role("designer")
	before := len(rs.Inbox)

	for _, by := range []string{"dispatcher", "dev"} {
		f.s.By = by

		err := f.s.Send("designer", "[dispatcher] do this now")

		var queued *SendQueued
		require.ErrorAs(t, err, &queued, by)
		assert.Equal(t, "claude 5h window", queued.What)
		assert.True(t, queued.Until.Equal(limitsReset5))
		assert.Equal(t, "queued: claude 5h window held until 13:12", queued.Say(time.UTC))
	}

	assert.Empty(t, f.tm.typed["designer"], "nothing typed")
	assert.Empty(t, p.posted["sid-designer"], "nothing posted")
	assert.Len(t, rs.Inbox, before+2, "the lines are in the queue, behind the others")

	// The owner's own send goes.
	f.s.By = "hand"
	require.NoError(t, f.s.Send("designer", "look at this first"))
	assert.Equal(t, []string{"look at this first"}, p.posted["sid-designer"])
}
