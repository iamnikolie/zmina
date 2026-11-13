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
	marks.Codex.Hold = 0

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
marks    claude: warn 75%, checkpoint 90%, hold 97% · codex: warn 75%, checkpoint 90%, hold off
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
	assert.Contains(t, out, "marks    claude: warn 75%, checkpoint 90%, hold 97% · codex: warn 75%, checkpoint 90%, hold 97%\n")
	assert.NotContains(t, out, "\nhold ")

	out, _, err = zm(t, crewFile, "limits", "--json")
	require.NoError(t, err)

	var got struct {
		Policy     map[string]map[string]any
		Thresholds map[string]map[string]any
		Holds      []map[string]any
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, map[string]any{"warn": 75.0, "checkpoint": 90.0, "hold": 97.0}, got.Thresholds["claude"])
	assert.Equal(t, map[string]any{"warn": 75.0, "checkpoint": 90.0, "hold": 97.0}, got.Thresholds["codex"])
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
	crewFile := testCrew(t, "limits:\n  claude: {warn: 60, hold: off}\n  codex: {checkpoint: off}\ntimezone: UTC\n")

	out, _, err := zm(t, crewFile, "limits")
	require.NoError(t, err)
	assert.Contains(t, out, "marks    claude: warn 60%, checkpoint 90%, hold off · codex: warn 75%, checkpoint off, hold 97%\n")

	out, _, err = zm(t, crewFile, "limits", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"hold": "off"`)
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
