package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/limits"
	"github.com/iamnikolie/zmina/internal/shift"
)

// holdBook writes the holds file of a crew: a hold of the Claude 5-hour window at 97%, resetting at reset.
func holdBook(t *testing.T, c *crew.Crew, reset time.Time, ask string) {
	t.Helper()

	require.NoError(t, limits.WriteBook(c.Layout().Limits(), limits.Book{Holds: []limits.Hold{
		{Plan: limits.PlanClaude, Window: limits.WindowFiveHour, Reset: reset, Since: reset.Add(-3 * time.Hour), Used: 97, Ask: ask}}}))
}

func TestLimitsTextShowsTheMarksAndTheHolds(t *testing.T) {
	marks := limits.DefaultThresholds()
	marks.Codex.Hold[limits.WindowWeek] = 97

	r := limits.Report{
		Policy:     limits.DefaultPolicy(),
		Thresholds: &marks,
		Holds: []limits.HoldInfo{{
			Hold:  limits.Hold{Plan: limits.PlanClaude, Window: limits.WindowFiveHour, Reset: limitsNow.Add(42 * time.Minute), Used: 97},
			Roles: []string{"dispatcher", "dev"}, Lines: 14,
		}},
	}

	var out strings.Builder
	require.NoError(t, writeLimits(&out, r, limitsNow))

	assert.Equal(t, `claude   unknown (no status line has drawn the limits yet)
codex    unknown (no Codex rollout with the limits found)
policy   claude: pinned work only from 80%, nothing from 99% · codex: pinned work only from 80%
marks    claude: warn 75%, checkpoint 90%, hold 5h 97%, 7d off · codex: warn 75%, checkpoint 90%, hold week 97%
hold     claude 5h window at 97%, until 21:40 (2 roles, 14 lines waiting); the owner's own line passes
`, out.String())

	out.Reset()

	off := limits.Thresholds{}
	r.Thresholds, r.Holds = &off, nil
	require.NoError(t, writeLimits(&out, r, limitsNow))
	assert.Contains(t, out.String(), "marks    off\n")
}

func TestLimitsFlowShowsTheMarksAndTheHolds(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n  codex-dev:\n    runtime: codex\ntimezone: UTC\n")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	// The defaults are on, with no key in the crew file.
	out, _, err := zm(t, crewFile, "limits")
	require.NoError(t, err)
	assert.Contains(t, out, "marks    claude: warn 75%, checkpoint 90%, hold 5h 97%, 7d off · codex: warn 75%, checkpoint 90%, hold week off\n")
	assert.NotContains(t, out, "\nhold ")

	out, _, err = zm(t, crewFile, "limits", "--json")
	require.NoError(t, err)

	var got struct {
		Policy     map[string]map[string]any
		Thresholds map[string]map[string]any
		Holds      []map[string]any
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, map[string]any{"warn": 75.0, "checkpoint": 90.0, "hold": map[string]any{"five_hour": 97.0, "seven_day": "off"}}, got.Thresholds["claude"])
	assert.Equal(t, map[string]any{"warn": 75.0, "checkpoint": 90.0, "hold": map[string]any{"week": "off"}}, got.Thresholds["codex"])
	assert.Equal(t, map[string]any{"pinned": 80.0, "stop": 99.0}, got.Policy["claude"], "routing is as it was")
	assert.Empty(t, got.Holds)

	// A hold on the book: the window, its share, until when, the roles of the plan and the lines that wait for them.
	reset := time.Now().UTC().Add(90 * time.Minute).Truncate(time.Minute)
	holdBook(t, c, reset, "")

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)
	l.Role("dev").Inbox = []string{"[dispatcher] look at T-1", "[dispatcher] and T-2"}
	l.Role("codex-dev").Inbox = []string{"[dispatcher] for codex"}
	require.NoError(t, l.Save())

	out, _, err = zm(t, crewFile, "limits")
	require.NoError(t, err)
	assert.Contains(t, out, fmt.Sprintf("hold     claude 5h window at 97%%, until %s (2 roles, 2 lines waiting); the owner's own line passes\n",
		reset.Format("15:04")), out)

	out, _, err = zm(t, crewFile, "limits", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Holds, 1)
	assert.Equal(t, "claude", got.Holds[0]["plan"])
	assert.Equal(t, "five_hour", got.Holds[0]["window"])
	assert.Equal(t, 97.0, got.Holds[0]["used"])
	assert.Equal(t, []any{"dispatcher", "dev"}, got.Holds[0]["roles"])
	assert.Equal(t, 2.0, got.Holds[0]["lines"])
	assert.Equal(t, reset.Format(time.RFC3339), got.Holds[0]["reset"])

	// One whose reset has passed is no hold: the clock ends it.
	holdBook(t, c, time.Now().Add(-time.Minute), "")

	out, _, err = zm(t, crewFile, "limits")
	require.NoError(t, err)
	assert.NotContains(t, out, "\nhold ")
}

func TestLimitsMarksAreTheCrewFilesAndOffIsOff(t *testing.T) {
	crewFile := testCrew(t, "limits:\n  claude: {warn: 60, hold: {five_hour: off, seven_day: 98}}\n  codex: {checkpoint: off, hold: {week: 99}}\ntimezone: UTC\n")

	out, _, err := zm(t, crewFile, "limits")
	require.NoError(t, err)
	assert.Contains(t, out, "marks    claude: warn 60%, checkpoint 90%, hold 5h off, 7d 98% · codex: warn 75%, checkpoint off, hold week 99%\n")

	out, _, err = zm(t, crewFile, "limits", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"five_hour": "off"`)
	assert.Contains(t, out, `"seven_day": 98`)
}

func TestCrewCheckRefusesThresholdsOutOfOrder(t *testing.T) {
	crewFile := testCrew(t, "limits:\n  claude: {warn: 95}\ntimezone: UTC\n")

	_, _, err := zm(t, crewFile, "ls")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "limits.claude: warn 95 is above checkpoint 90")

	crewFile = testCrew(t, "limits:\n  codex: {hold: 101}\n")

	_, _, err = zm(t, crewFile, "ls")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "limits.codex.hold is a share of the window, 1 to 100, or off, not 101")
}

func TestLsAndQueueAndExplainSayThePlanHold(t *testing.T) {
	r := newExplainRig(t)
	r.session("idle", "")

	c, err := crew.Load(r.crewFile)
	require.NoError(t, err)

	// Nothing held: nothing said.
	for _, args := range [][]string{{"ls"}, {"ls", "--wide"}, {"explain", "dev"}} {
		assert.NotContains(t, r.mustRun(args...), "hold", args)
	}

	reset := r.now.Add(90 * time.Minute)
	holdBook(t, c, reset, "")

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)
	l.Role("dev").Inbox = []string{"[dispatcher] look at T-1"}
	require.NoError(t, l.Save())

	foot := "hold: claude 5h window at 97%, until 13:30 (2 roles, 1 line waiting); the owner's own line passes (zmina limits)"
	assert.Contains(t, r.mustRun("ls"), foot)
	assert.Contains(t, r.mustRun("ls", "--wide"), foot)
	assert.Contains(t, r.mustRun("ls", "dev"), foot)

	var views []shift.View

	require.NoError(t, json.Unmarshal([]byte(r.mustRun("ls", "--json")), &views))
	require.NotEmpty(t, views)

	for _, v := range views {
		assert.Equal(t, "claude 5h window", v.PlanHeld, v.Role)
		assert.True(t, v.PlanHeldUntil.Equal(reset), v.Role)
	}

	q := r.mustRun("queue", "ls")
	assert.Contains(t, q, "dev:\n  held: the claude 5h window is at its hold share, nothing is delivered until Sat 13:30 UTC (the owner's own line goes through)\n")
	assert.Contains(t, q, "inbox    [dispatcher] look at T-1")

	var qs []shift.QueueView

	require.NoError(t, json.Unmarshal([]byte(r.mustRun("queue", "ls", "--json")), &qs))
	require.NotEmpty(t, qs)
	assert.Equal(t, "claude 5h window", qs[0].PlanHeld)

	ex := r.mustRun("explain", "dev")
	assert.Contains(t, ex, "queue: held for the claude 5h window at 97% until Sat 13:30 UTC: nothing is delivered to the plan's roles until it resets; "+
		"the owner's own line goes through; 1 line waiting")

	out := r.explainJSON()
	require.NotNil(t, out.Queue.PlanHold)
	assert.Equal(t, "claude 5h window", out.Queue.PlanHold.Window)
	assert.Equal(t, 97.0, out.Queue.PlanHold.Used)
	assert.True(t, out.Queue.PlanHold.Until.Equal(reset))
}

// The hold of a plan supersedes a failed turn's in what the owner reads while it lasts; the one that ends later is the one said.
func TestPlanHoldSupersedesAFailedTurnsHoldForDisplay(t *testing.T) {
	r := newExplainRig(t)
	r.session("idle", "")
	r.now = r.now.Add(4 * time.Minute)

	c, err := crew.Load(r.crewFile)
	require.NoError(t, err)

	_, err = shift.RecordFailure(crewLayout(t, r), "dev", []byte(rigFailed), r.now.Add(-4*time.Minute))
	require.NoError(t, err)

	// The failed turn's hold runs to 15:00 UTC. A plan hold that ends sooner is the lesser: the failed turn's is still said.
	holdBook(t, c, r.now.Add(time.Hour), "")

	shows, _ := r.lsCells()
	assert.Contains(t, shows, "queue held until 15:00 UTC")
	assert.NotContains(t, shows, "held for the claude")
	assert.Contains(t, r.mustRun("explain", "dev"), "queue: held for the claude 5h window at 97% until Sat 13:04 UTC")
	assert.Contains(t, r.mustRun("explain", "dev"), "held for a failed turn (rate_limit")

	// One that ends later is the one said, and the failed turn's line is gone from the explanation.
	holdBook(t, c, r.now.Add(5*time.Hour), "")

	shows, _ = r.lsCells()
	assert.Contains(t, shows, "queue held for the claude 5h window until 17:04 UTC")
	assert.NotContains(t, r.mustRun("explain", "dev"), "held for a failed turn (rate_limit")
}

func TestDoctorSaysThePlanHoldAndWhoItKeeps(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n  codex-dev:\n    runtime: codex\ntimezone: UTC\n")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	holdBook(t, c, now.Add(90*time.Minute), "A-4")

	st := shift.NewState()
	st.Role("dev").Inbox = []string{"[dispatcher] look at T-1", "[dispatcher] and T-2"}
	st.Notes = []string{"a note"}

	var got []check

	checkPlanHolds(c, st, now, func(name, level, format string, args ...any) {
		got = append(got, check{Name: name, Level: level, Detail: fmt.Sprintf(format, args...)})
	})

	require.Len(t, got, 1)
	assert.Equal(t, "holds", got[0].Name)
	assert.Equal(t, "warn", got[0].Level)
	assert.Equal(t, "claude 5h window: nothing is delivered to dispatcher, dev until Sat 13:30 UTC; 3 lines wait, the owner's own line passes "+
		"(zmina limits); the dispatcher is held too and cannot tell you, so this is the owner's to look at (todo A-4)", got[0].Detail)

	// A hold that is over is not said.
	got = nil

	checkPlanHolds(c, st, now.Add(2*time.Hour), func(name, level, format string, args ...any) {
		got = append(got, check{Name: name, Level: level, Detail: fmt.Sprintf(format, args...)})
	})
	assert.Empty(t, got)

	// A Codex dispatcher is held by the Codex plan alone.
	cx := testCrew(t, "  dev:\ntimezone: UTC\n")
	cc, err := crew.Load(cx)
	require.NoError(t, err)
	require.NoError(t, limits.WriteBook(cc.Layout().Limits(), limits.Book{Holds: []limits.Hold{
		{Plan: limits.PlanCodex, Window: limits.WindowWeek, Reset: now.Add(48 * time.Hour), Since: now, Used: 98}}}))

	checkPlanHolds(cc, nil, now, func(name, level, format string, args ...any) {
		got = append(got, check{Name: name, Level: level, Detail: fmt.Sprintf(format, args...)})
	})
	require.Len(t, got, 1)
	assert.NotContains(t, got[0].Detail, "dispatcher is held too", "no Codex role: nobody is held, and the dispatcher is Claude's")
}

func TestDoctorLeavesOutAFailedTurnsHoldThePlanHoldCovers(t *testing.T) {
	crewFile := testCrew(t, "  dev:\ntimezone: UTC\n")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	hold := []shift.FailureHold{{Role: "dev", Text: "limit", Until: now.Add(time.Hour)}}

	// No plan hold: the failed turn's stays.
	assert.Equal(t, hold, supersededFailureHolds(c, nil, hold, now))

	holdBook(t, c, now.Add(2*time.Hour), "")
	assert.Empty(t, supersededFailureHolds(c, nil, hold, now), "the plan's hold ends later: it is the one doctor says")

	holdBook(t, c, now.Add(30*time.Minute), "")
	assert.Equal(t, hold, supersededFailureHolds(c, nil, hold, now), "the failed turn's ends later: it is still said")
}

func TestLimitsSnapshotReadsOnlyThePlansTheCrewBills(t *testing.T) {
	// A crew with no Codex seat never reads ~/.codex, whatever is in it.
	crewFile := testCrew(t, "  dev:\ntimezone: UTC\n")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	now := time.Now()
	limitsRollout(t, filepath.Join(os.Getenv("HOME"), ".codex"), now.Add(-time.Minute), 99, now.Add(24*time.Hour))

	_, err = limits.Tap(c.Layout().Limits(), []byte(fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":97,"resets_at":%d}}}`,
		now.Add(time.Hour).Unix())), now.Add(-time.Minute))
	require.NoError(t, err)

	a := &app{out: &strings.Builder{}, err: &strings.Builder{}, crew: crewFile}

	snap := a.limitsSnapshot(now)
	require.NotNil(t, snap)
	require.NotNil(t, snap.Claude)
	assert.Equal(t, 97.0, snap.Claude.FiveHour.Used)
	assert.Nil(t, snap.Codex, "no role of the crew bills the Codex plan")

	// With a Codex seat it is read.
	crewFile = testCrew(t, "  codex-dev:\n    runtime: codex\ntimezone: UTC\n")
	limitsRollout(t, filepath.Join(os.Getenv("HOME"), ".codex"), now.Add(-time.Minute), 99, now.Add(24*time.Hour))

	snap = (&app{out: &strings.Builder{}, err: &strings.Builder{}, crew: crewFile}).limitsSnapshot(now)
	require.NotNil(t, snap)
	require.NotNil(t, snap.Codex)
	assert.Equal(t, 99.0, snap.Codex.Week.Used)

	// Every threshold off: nothing is judged, so nothing is read; the empty report still lets the beat end a hold on the book.
	crewFile = testCrew(t, "limits:\n  claude: {warn: off, checkpoint: off, hold: off}\n  codex: {warn: off, checkpoint: off, hold: off}\n")
	snap = (&app{out: &strings.Builder{}, err: &strings.Builder{}, crew: crewFile}).limitsSnapshot(now)
	require.NotNil(t, snap)
	assert.Nil(t, snap.Claude)
	assert.Nil(t, snap.Codex)

	// A crew file that does not load: none either, and no error of its own.
	assert.Nil(t, (&app{out: &strings.Builder{}, err: &strings.Builder{}, crew: filepath.Join(t.TempDir(), "none.yaml")}).limitsSnapshot(now))
}

// The beat judges the thresholds: a Claude plan found at the hold share holds, tells, and starts nothing the ticker keeps up.
func TestTickBeginsAHoldAndStartsNothing(t *testing.T) {
	crewFile := testCrew(t, "timezone: UTC\n")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	now := time.Now()
	_, err = limits.Tap(c.Layout().Limits(), []byte(fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":98,"resets_at":%d},`+
		`"seven_day":{"used_percentage":30,"resets_at":%d}}}`, now.Add(time.Hour).Unix(), now.Add(72*time.Hour).Unix())), now)
	require.NoError(t, err)

	out, _, err := zm(t, crewFile, "tick")
	require.NoError(t, err)
	assert.Contains(t, out, "limits: claude: 5h window at 98%, holding its 1 role until ")
	assert.NotContains(t, out, "started dispatcher", "a start's boot line is a model call")

	book, err := limits.ReadBook(c.Layout().Limits())
	require.NoError(t, err)
	require.Len(t, book.Holds, 1)
	assert.Equal(t, limits.WindowFiveHour, book.Holds[0].Window)

	st, err := shift.Read(c.StateDir())
	require.NoError(t, err)
	require.NotEmpty(t, st.Notes)
	assert.Contains(t, strings.Join(st.Notes, "|"), "claude: 5h window at 98%, holding its 1 role until ")

	// The next beat of the same period says nothing new.
	out, _, err = zm(t, crewFile, "tick")
	require.NoError(t, err)
	assert.NotContains(t, out, "limits:")
}

// zmina up is the owner's own act: it goes on during a plan hold, and says on stderr that the role's plan is held.
func TestUpDuringAPlanHoldGoesOnAndSaysSo(t *testing.T) {
	crewFile := testCrew(t, "  dev:\ntimezone: UTC\n")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	reset := time.Now().UTC().Add(time.Hour).Truncate(time.Minute)
	holdBook(t, c, reset, "")

	out, errOut, err := zm(t, crewFile, "up", "dev")
	require.NoError(t, err, errOut)
	assert.Contains(t, out, "dev up as ")
	assert.Contains(t, errOut, "zmina: warning: dev: its plan is held (claude 5h window until "+reset.Format("15:04")+")")
	assert.Contains(t, errOut, "nothing is delivered to it but your own line")

	// A role of a plan that is not held starts without a word.
	free := testCrew(t, "  dev:\ntimezone: UTC\n")
	_, errOut, err = zm(t, free, "up", "dev")
	require.NoError(t, err, errOut)
	assert.NotContains(t, errOut, "plan is held")
}

// zmina limits release lifts a plan's hold by hand: the owner and the dispatcher may; the period is recorded as lifted, the roles go in turn,
// its todo item closes and the event says by hand.
func TestLimitsReleaseLiftsAHoldByHand(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n  codex-dev:\n    runtime: codex\ntimezone: UTC\n")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	reset := time.Now().UTC().Add(90 * time.Minute).Truncate(time.Minute)

	item, err := askStore(c).Add(asks.Ask{Kind: asks.Check, Title: "claude 5h window held", Role: "ticker", By: "limits-hold", Blocks: "dispatcher"}, time.Now())
	require.NoError(t, err)
	holdBook(t, c, reset, item.ID)

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)
	l.Role("dev").Inbox = []string{"[dispatcher] look at T-1"}
	require.NoError(t, l.Save())

	// Nothing is held for Codex: said, and no failure.
	out, _, err := zm(t, crewFile, "limits", "release", "codex")
	require.NoError(t, err)
	assert.Contains(t, out, "nothing is held for codex")

	// A role may not lift it; the dispatcher may.
	t.Setenv("ZMINA_ROLE", "dev")

	_, _, err = zm(t, crewFile, "limits", "release", "claude")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only the owner or the dispatcher can lift a hold")

	book, err := limits.ReadBook(c.Layout().Limits())
	require.NoError(t, err)
	assert.Len(t, book.Holds, 1, "refused: it is still on")

	t.Setenv("ZMINA_ROLE", "dispatcher")

	out, _, err = zm(t, crewFile, "limits", "release", "claude", "five_hour")
	require.NoError(t, err)
	assert.Contains(t, out, "lifted: claude 5h window, held at 97% since ")
	assert.Contains(t, out, "the dispatcher goes now, the other 1 role in turn")

	book, err = limits.ReadBook(c.Layout().Limits())
	require.NoError(t, err)
	assert.Empty(t, book.Holds)
	require.Len(t, book.Periods, 1)
	assert.False(t, book.Periods[0].Lifted.IsZero(), "the period is lifted: it is not held again")
	require.Len(t, book.Waits, 1)
	assert.Equal(t, "dev", book.Waits[0].Role)

	got, err := askStore(c).Get(item.ID)
	require.NoError(t, err)
	assert.False(t, got.Open(), "its todo item closes")

	events, err := os.ReadFile(c.Layout().Events())
	require.NoError(t, err)
	assert.Contains(t, string(events), `"event":"limit-release"`)
	assert.Contains(t, string(events), "hold lifted by hand")

	st, err := shift.Read(c.StateDir())
	require.NoError(t, err)
	assert.Contains(t, strings.Join(st.Notes, "|"), "claude: 5h window hold lifted by hand")

	// Again: the role still waiting its turn is let go with it (the owner's word); and once more, nothing is held.
	out, _, err = zm(t, crewFile, "limits", "release", "claude")
	require.NoError(t, err)
	assert.Contains(t, out, "1 role waiting their turn after an earlier release let go")

	out, _, err = zm(t, crewFile, "limits", "release", "claude")
	require.NoError(t, err)
	assert.Contains(t, out, "nothing is held for claude")

	// A window or a plan that is no name of either is an error.
	t.Setenv("ZMINA_ROLE", "")

	_, _, err = zm(t, crewFile, "limits", "release", "gemini")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "claude or codex")

	_, _, err = zm(t, crewFile, "limits", "release", "claude", "weekly")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "five_hour, seven_day")
}

func TestDoctorSaysAHoldsFileThatDoesNotReadOrIsNewer(t *testing.T) {
	crewFile := testCrew(t, "  dev:\ntimezone: UTC\n")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(c.Layout().Limits(), 0o700))

	lines := func() []check {
		var got []check

		checkPlanHolds(c, nil, time.Now(), func(name, level, format string, args ...any) {
			got = append(got, check{Name: name, Level: level, Detail: fmt.Sprintf(format, args...)})
		})

		return got
	}

	assert.Empty(t, lines())

	path := filepath.Join(c.Layout().Limits(), limits.HoldsFile)
	require.NoError(t, os.WriteFile(path, []byte("{nope"), 0o600))

	got := lines()
	require.Len(t, got, 1)
	assert.Equal(t, "warn", got[0].Level)
	assert.Contains(t, got[0].Detail, "the holds file does not read")
	assert.Contains(t, got[0].Detail, "the next beat moves it aside")

	require.NoError(t, os.WriteFile(path, []byte(`{"v":9}`), 0o600))

	got = lines()
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Detail, "nothing is held by it here, and nothing is written over it; upgrade zmina")
}

func TestLsAndDoctorSayTheRolesWaitingTheirTurnAfterARelease(t *testing.T) {
	r := newExplainRig(t)
	r.session("idle", "")

	c, err := crew.Load(r.crewFile)
	require.NoError(t, err)

	require.NoError(t, limits.WriteBook(c.Layout().Limits(), limits.Book{Waits: []limits.Wait{
		{Role: "dev", Plan: limits.PlanClaude, Window: limits.WindowFiveHour, Until: r.now.Add(2 * time.Minute)},
		{Role: "dispatcher", Plan: limits.PlanClaude, Window: limits.WindowFiveHour, Until: r.now.Add(time.Minute)},
	}}))

	assert.Contains(t, r.mustRun("ls"), "release: claude plan: 2 roles let go in turn, the last at 12:02 (3 at a time, a minute apart)")

	var views []shift.View

	require.NoError(t, json.Unmarshal([]byte(r.mustRun("ls", "--json")), &views))

	for _, v := range views {
		if v.Role == "dev" {
			assert.Equal(t, "release of the claude plan", v.PlanHeld)
			assert.True(t, v.PlanHeldUntil.Equal(r.now.Add(2*time.Minute)))
		}
	}

	ex := r.mustRun("explain", "dev")
	assert.Contains(t, ex, "queue: waiting its turn in the release of the claude plan until Sat 12:02 UTC")

	var got []check

	st, err := shift.Read(c.StateDir())
	require.NoError(t, err)

	checkPlanHolds(c, st, r.now, func(name, level, format string, args ...any) {
		got = append(got, check{Name: name, Level: level, Detail: fmt.Sprintf(format, args...)})
	})
	require.Len(t, got, 1)
	assert.Equal(t, "ok", got[0].Level)
	assert.Contains(t, got[0].Detail, "release: claude plan: 2 roles let go in turn")
}
