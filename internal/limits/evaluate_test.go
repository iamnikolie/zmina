package limits

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	bothPlans = []Plan{PlanClaude, PlanCodex}
	// resetAt is where the windows of these tests reset: two hours after noon.
	resetAt = noon.Add(2 * time.Hour)
)

// claudeAt is a Claude report taken at noon with the 5-hour window at five and the 7-day window at seven percent.
func claudeAt(five, seven float64, fiveReset, sevenReset *time.Time) Report {
	return Report{Claude: &Claude{
		FiveHour: &Window{Used: five, Resets: fiveReset},
		SevenDay: &Window{Used: seven, Resets: sevenReset},
		At:       noon,
	}}
}

func eval(r Report, b Book, now time.Time) Result {
	return Evaluate(Input{Report: r, Thresholds: DefaultThresholds(), Plans: bothPlans, Book: b, Now: now})
}

func kinds(r Result) []Kind {
	var out []Kind
	for _, a := range r.Actions {
		out = append(out, a.Kind)
	}

	return out
}

func TestWarnIsToldOncePerPeriod(t *testing.T) {
	week := at(noon.Add(72 * time.Hour))
	r := claudeAt(76, 10, at(resetAt), week)

	got := eval(r, Book{}, noon)
	require.Len(t, got.Actions, 1)
	assert.Equal(t, KindWarn, got.Actions[0].Kind)
	assert.Equal(t, PlanClaude, got.Actions[0].Plan)
	assert.Equal(t, WindowFiveHour, got.Actions[0].Window)
	assert.Equal(t, 76.0, got.Actions[0].Used)
	assert.True(t, got.Actions[0].Reset.Equal(resetAt))
	assert.True(t, got.Changed)

	again := eval(r, got.Book, noon.Add(time.Minute))
	assert.Empty(t, again.Actions, "told once for this period")
	assert.False(t, again.Changed)

	assert.Empty(t, eval(claudeAt(74.9, 10, at(resetAt), week), Book{}, noon).Actions, "under warn")
	assert.Len(t, eval(claudeAt(75, 10, at(resetAt), week), Book{}, noon).Actions, 1, "at warn")
}

func TestCheckpointIsAskedOncePerPeriodAndCountsAsWarn(t *testing.T) {
	week := at(noon.Add(72 * time.Hour))

	got := eval(claudeAt(91, 10, at(resetAt), week), Book{}, noon)
	assert.Equal(t, []Kind{KindCheckpoint}, kinds(got), "one action: a window at 91 does not also warn")

	later := eval(claudeAt(92, 10, at(resetAt), week), got.Book, noon.Add(time.Minute))
	assert.Empty(t, later.Actions, "asked once")

	// It warned first, climbs: the checkpoint comes once and the warn is not repeated.
	first := eval(claudeAt(80, 10, at(resetAt), week), Book{}, noon)
	assert.Equal(t, []Kind{KindWarn}, kinds(first))

	second := eval(claudeAt(90, 10, at(resetAt), week), first.Book, noon.Add(time.Minute))
	assert.Equal(t, []Kind{KindCheckpoint}, kinds(second))
	assert.Empty(t, eval(claudeAt(91, 10, at(resetAt), week), second.Book, noon.Add(2*time.Minute)).Actions)
}

func TestHoldStartsAtItsShareAndIsKeptInTheBook(t *testing.T) {
	week := at(noon.Add(72 * time.Hour))

	got := eval(claudeAt(97, 10, at(resetAt), week), Book{}, noon)
	require.Equal(t, []Kind{KindHold}, kinds(got))

	h := got.Actions[0].Hold
	assert.Equal(t, PlanClaude, h.Plan)
	assert.Equal(t, WindowFiveHour, h.Window)
	assert.True(t, h.Reset.Equal(resetAt))
	assert.True(t, h.Since.Equal(noon))
	assert.Equal(t, 97.0, h.Used)
	assert.Equal(t, []Hold{h}, got.Book.Holds)

	// Held: nothing more is asked while it is on, and the lower shares were told with it.
	again := eval(claudeAt(98, 10, at(resetAt), week), got.Book, noon.Add(time.Minute))
	assert.Empty(t, again.Actions)
	assert.Equal(t, []Hold{h}, again.Book.Holds)
	assert.Contains(t, got.Book.Told, Key(PlanClaude, WindowFiveHour, at(resetAt)))
}

// A window found already at the hold share holds, and does not also warn and ask for a checkpoint nobody could take.
func TestAWindowFoundAtTheHoldShareOnlyHolds(t *testing.T) {
	got := eval(claudeAt(98, 10, at(resetAt), at(noon.Add(72*time.Hour))), Book{}, noon)
	assert.Equal(t, []Kind{KindHold}, kinds(got))

	// If the hold ends under the share (turned off), the lower ones are not asked late for the same period.
	off := Thresholds{Claude: Shares{Warn: 75, Checkpoint: 90}, Codex: DefaultShares()}
	after := Evaluate(Input{Report: claudeAt(98, 10, at(resetAt), at(noon.Add(72*time.Hour))), Thresholds: off, Plans: bothPlans, Book: got.Book,
		Now: noon.Add(time.Minute)})
	assert.Equal(t, []Kind{KindRelease}, kinds(after))
	assert.Equal(t, ReasonOff, after.Actions[0].Reason, "turned off: no reading is needed to end it")
	assert.Empty(t, after.Book.Holds)

	// With nothing read at all, too.
	none := Evaluate(Input{Report: Report{}, Thresholds: off, Plans: bothPlans, Book: got.Book, Now: noon.Add(time.Minute)})
	assert.Equal(t, []Kind{KindRelease}, kinds(none))
	assert.Equal(t, ReasonOff, none.Actions[0].Reason)
}

func TestStaleDataTriggersNothingNew(t *testing.T) {
	r := claudeAt(99, 99, at(resetAt), at(noon.Add(72*time.Hour)))
	r.Claude.FiveHour.Stale = true
	r.Claude.SevenDay.Stale = true

	got := eval(r, Book{}, noon)
	assert.Empty(t, got.Actions)
	assert.Empty(t, got.Book.Holds)
	assert.False(t, got.Changed)

	// Judged by Collect: a reading older than six hours is stale, and so is a window whose reset passed.
	cl := &Claude{FiveHour: &Window{Used: 99, Resets: at(noon.Add(time.Hour))}, At: noon.Add(-7 * time.Hour)}
	cl.Judge(noon)
	assert.Empty(t, eval(Report{Claude: cl}, Book{}, noon).Actions)
}

func TestStaleDataReleasesNothingBeforeTheReset(t *testing.T) {
	held := eval(claudeAt(97, 10, at(resetAt), at(noon.Add(72*time.Hour))), Book{}, noon).Book
	require.Len(t, held.Holds, 1)

	// Nothing read at all, or the reading stale (old): the hold stays until its time.
	for name, r := range map[string]Report{
		"no reading": {},
		"an old one": {Claude: &Claude{FiveHour: &Window{Used: 10, Resets: at(resetAt), Stale: true}, At: noon.Add(-8 * time.Hour)}},
	} {
		got := eval(r, held, resetAt.Add(-time.Second))
		assert.Empty(t, got.Actions, name)
		assert.Equal(t, held.Holds, got.Book.Holds, name)
		assert.False(t, got.Changed, name)
	}
}

func TestAHoldEndsAtTheResetTime(t *testing.T) {
	held := eval(claudeAt(97, 10, at(resetAt), at(noon.Add(72*time.Hour))), Book{}, noon).Book

	// No reading since: the clock ends it, and the data is said to be stale.
	got := eval(Report{}, held, resetAt)
	require.Equal(t, []Kind{KindRelease}, kinds(got))
	assert.Equal(t, ReasonReset, got.Actions[0].Reason)
	assert.False(t, got.Actions[0].Confirmed)
	assert.Empty(t, got.Book.Holds)
	assert.True(t, got.Changed)

	// A reading that is stale for its reset having passed is the same: the old period's figure confirms nothing.
	old := claudeAt(97, 10, at(resetAt), at(noon.Add(72*time.Hour)))
	old.Claude.FiveHour.Stale = true
	got = eval(old, held, resetAt.Add(time.Minute))
	assert.False(t, got.Actions[0].Confirmed)

	// A fresh reading of the next period confirms it, and the figure in the note is the new one.
	next := Report{Claude: &Claude{FiveHour: &Window{Used: 3, Resets: at(resetAt.Add(5 * time.Hour))}, At: resetAt.Add(time.Minute)}}
	got = eval(next, held, resetAt.Add(2*time.Minute))
	require.Equal(t, []Kind{KindRelease}, kinds(got))
	assert.True(t, got.Actions[0].Confirmed)
	assert.Equal(t, 3.0, got.Actions[0].Used)
}

func TestAHoldEndsWhenAFreshReadingOfThePeriodShowsItUnderTheShare(t *testing.T) {
	held := eval(claudeAt(97, 10, at(resetAt), at(noon.Add(72*time.Hour))), Book{}, noon).Book

	got := eval(claudeAt(96, 10, at(resetAt), at(noon.Add(72*time.Hour))), held, noon.Add(time.Minute))
	require.Equal(t, []Kind{KindRelease}, kinds(got))
	assert.Equal(t, ReasonBelow, got.Actions[0].Reason)
	assert.True(t, got.Actions[0].Confirmed)
	assert.Empty(t, got.Book.Holds)

	// A reading of the same period at or over the share keeps it.
	still := eval(claudeAt(97, 10, at(resetAt), at(noon.Add(72*time.Hour))), held, noon.Add(time.Minute))
	assert.Empty(t, still.Actions)

	// A reading of an earlier period (a status line drawn from old input) says nothing about this one.
	older := eval(claudeAt(5, 10, at(resetAt.Add(-5*time.Hour)), at(noon.Add(72*time.Hour))), held, noon.Add(time.Minute))
	assert.Empty(t, older.Actions)
}

// A fresh reading whose reset is later than the hold's: the window rolled over before the time it was held to.
func TestAHoldEndsWhenAFreshReadingShowsALaterPeriod(t *testing.T) {
	held := eval(claudeAt(97, 10, at(resetAt), at(noon.Add(72*time.Hour))), Book{}, noon).Book

	got := eval(claudeAt(2, 10, at(resetAt.Add(5*time.Hour)), at(noon.Add(72*time.Hour))), held, noon.Add(time.Minute))
	require.Equal(t, []Kind{KindRelease}, kinds(got))
	assert.Equal(t, ReasonReset, got.Actions[0].Reason)
	assert.True(t, got.Actions[0].Confirmed)
}

func TestTwoWindowsAreJudgedEachOnItsOwn(t *testing.T) {
	week := noon.Add(72 * time.Hour)

	got := eval(claudeAt(91, 97, at(resetAt), at(week)), Book{}, noon)
	require.Equal(t, []Kind{KindCheckpoint, KindHold}, kinds(got))
	assert.Equal(t, WindowFiveHour, got.Actions[0].Window)
	assert.Equal(t, WindowSevenDay, got.Actions[1].Window)
	require.Len(t, got.Book.Holds, 1)
	assert.Equal(t, WindowSevenDay, got.Book.Holds[0].Window)

	// The 5-hour window rolls over; the week stays held, and the new 5-hour period warns anew.
	next := claudeAt(80, 97, at(resetAt.Add(5*time.Hour)), at(week))
	next.Claude.At = resetAt.Add(time.Minute)
	got2 := eval(next, got.Book, resetAt.Add(2*time.Minute))
	assert.Equal(t, []Kind{KindWarn}, kinds(got2))
	assert.Equal(t, got.Book.Holds, got2.Book.Holds, "the week's hold is untouched")
}

func TestAMovedResetIsANewPeriod(t *testing.T) {
	week := at(noon.Add(72 * time.Hour))

	first := eval(claudeAt(76, 10, at(resetAt), week), Book{}, noon)
	require.Equal(t, []Kind{KindWarn}, kinds(first))

	// The reset moved by seconds: the same period. By hours: a new one, which warns again.
	jitter := eval(claudeAt(76, 10, at(resetAt.Add(20*time.Second)), week), first.Book, noon.Add(time.Minute))
	assert.Empty(t, jitter.Actions)

	moved := eval(claudeAt(76, 10, at(resetAt.Add(5*time.Hour)), week), first.Book, noon.Add(2*time.Minute))
	assert.Equal(t, []Kind{KindWarn}, kinds(moved))
	assert.Len(t, moved.Book.Told, 2, "both periods are on the book until the old one is pruned")
}

func TestCodexWeekHasItsOwnThresholds(t *testing.T) {
	r := Report{Codex: &Codex{Week: &Window{Used: 91, Resets: at(noon.Add(48 * time.Hour))}, At: noon}}

	got := eval(r, Book{}, noon)
	require.Equal(t, []Kind{KindCheckpoint}, kinds(got))
	assert.Equal(t, PlanCodex, got.Actions[0].Plan)
	assert.Equal(t, WindowWeek, got.Actions[0].Window)
	assert.Equal(t, "codex weekly window", got.Actions[0].Label())

	r.Codex.Week.Used = 97
	got = eval(r, got.Book, noon.Add(time.Minute))
	require.Equal(t, []Kind{KindHold}, kinds(got))
	assert.Equal(t, PlanCodex, got.Book.Holds[0].Plan)

	// Its own shares: Codex off for hold does not touch Claude's.
	marks := Thresholds{Claude: DefaultShares(), Codex: Shares{Warn: 75, Checkpoint: 90}}
	assert.Empty(t, Evaluate(Input{Report: Report{Codex: &Codex{Week: &Window{Used: 99, Resets: at(noon.Add(time.Hour))}, At: noon}},
		Thresholds: Thresholds{Codex: Shares{}}, Plans: bothPlans, Now: noon}).Actions, "all off")
	assert.Equal(t, []Kind{KindCheckpoint}, kinds(Evaluate(Input{Report: Report{Codex: &Codex{Week: &Window{Used: 99, Resets: at(noon.Add(time.Hour))}, At: noon}},
		Thresholds: marks, Plans: bothPlans, Now: noon})), "hold off: the highest that is on")
}

func TestAPlanWithNoRolesIsNotJudged(t *testing.T) {
	r := claudeAt(99, 99, at(resetAt), at(noon.Add(72*time.Hour)))

	got := Evaluate(Input{Report: r, Thresholds: DefaultThresholds(), Plans: []Plan{PlanCodex}, Now: noon})
	assert.Empty(t, got.Actions)
	assert.Empty(t, got.Book.Holds)
}

func TestAWindowWithNoResetTimeIsNeverHeld(t *testing.T) {
	r := claudeAt(98, 10, nil, at(noon.Add(72*time.Hour)))

	got := eval(r, Book{}, noon)
	assert.Equal(t, []Kind{KindCheckpoint}, kinds(got), "a hold needs a time to end at")
	assert.Empty(t, got.Book.Holds)

	// Its one period has no end: a fall under the share re-arms it.
	assert.Empty(t, eval(r, got.Book, noon.Add(time.Minute)).Actions)

	fallen := eval(claudeAt(10, 10, nil, at(noon.Add(72*time.Hour))), got.Book, noon.Add(2*time.Minute))
	assert.Empty(t, fallen.Actions)

	climbed := eval(r, fallen.Book, noon.Add(3*time.Minute))
	assert.Equal(t, []Kind{KindCheckpoint}, kinds(climbed))
}

func TestPeriodsPastTheirResetByADayArePruned(t *testing.T) {
	week := at(noon.Add(72 * time.Hour))

	first := eval(claudeAt(76, 10, at(resetAt), week), Book{}, noon)
	require.Len(t, first.Book.Told, 1)

	kept := eval(Report{}, first.Book, resetAt.Add(23*time.Hour))
	assert.Len(t, kept.Book.Told, 1)
	assert.False(t, kept.Changed)

	pruned := eval(Report{}, first.Book, resetAt.Add(25*time.Hour))
	assert.Empty(t, pruned.Book.Told)
	assert.True(t, pruned.Changed)
}

func TestEvaluateDoesNotChangeTheBookItWasGiven(t *testing.T) {
	week := at(noon.Add(72 * time.Hour))
	book := eval(claudeAt(97, 10, at(resetAt), week), Book{}, noon).Book
	before, err := json.Marshal(book)
	require.NoError(t, err)

	eval(Report{}, book, resetAt)

	after, err := json.Marshal(book)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after))
}

func TestBookKeepsItsTimesAcrossAFile(t *testing.T) {
	dir := t.TempDir()

	b, err := ReadBook(dir)
	require.NoError(t, err)
	assert.Empty(t, b.Holds, "no file: an empty book")

	week := at(noon.Add(72 * time.Hour))
	want := eval(claudeAt(97, 10, at(resetAt), week), Book{}, noon).Book
	require.NoError(t, WriteBook(dir, want))

	got, err := ReadBook(dir)
	require.NoError(t, err)
	require.Len(t, got.Holds, 1)
	assert.True(t, got.Holds[0].Reset.Equal(resetAt))
	assert.Equal(t, want.Told, got.Told)

	h, ok := got.HeldUntil(PlanClaude, noon.Add(time.Minute))
	require.True(t, ok)
	assert.Equal(t, WindowFiveHour, h.Window)

	_, ok = got.HeldUntil(PlanClaude, resetAt)
	assert.False(t, ok, "the reset time itself is no longer held")
	_, ok = got.HeldUntil(PlanCodex, noon)
	assert.False(t, ok)

	// An empty book removes the file.
	require.NoError(t, WriteBook(dir, Book{}))
	_, err = os.Stat(filepath.Join(dir, HoldsFile))
	assert.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, WriteBook(dir, Book{}), "again, nothing there")
}

func TestHeldUntilIsTheLatestResetOfTheWindowsHeld(t *testing.T) {
	b := Book{Holds: []Hold{
		{Plan: PlanClaude, Window: WindowFiveHour, Reset: noon.Add(time.Hour)},
		{Plan: PlanClaude, Window: WindowSevenDay, Reset: noon.Add(72 * time.Hour)},
		{Plan: PlanCodex, Window: WindowWeek, Reset: noon.Add(200 * time.Hour)},
	}}

	h, ok := b.HeldUntil(PlanClaude, noon)
	require.True(t, ok)
	assert.Equal(t, WindowSevenDay, h.Window)

	h, ok = b.HeldUntil(PlanClaude, noon.Add(2*time.Hour))
	require.True(t, ok)
	assert.Equal(t, WindowSevenDay, h.Window, "the 5-hour one is over, the week goes on")
	assert.Len(t, b.Active(noon.Add(2*time.Hour)), 2)
}

func TestAFileThatDoesNotReadHoldsNothing(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, HoldsFile), []byte("{not json"), 0o600))

	b, err := ReadBook(dir)
	require.Error(t, err)
	assert.Empty(t, b.Holds)
}

func TestSharesAreNumbersOrOff(t *testing.T) {
	raw, err := json.Marshal(Shares{Warn: 75, Checkpoint: 0, Hold: 97})
	require.NoError(t, err)
	assert.JSONEq(t, `{"warn":75,"checkpoint":"off","hold":97}`, string(raw))

	var back Shares
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, Shares{Warn: 75, Hold: 97}, back)
	assert.Error(t, json.Unmarshal([]byte(`{"warn":"sometimes"}`), &back))

	assert.Equal(t, "warn 75%, checkpoint off, hold 97%", Shares{Warn: 75, Hold: 97}.String())
	assert.Equal(t, "claude: warn 75%, checkpoint 90%, hold 97% · codex: warn 75%, checkpoint 90%, hold 97%", DefaultThresholds().String())
	assert.Equal(t, Shares{}, Thresholds{}.For("gemini"))
	assert.False(t, Thresholds{}.Any())
	assert.True(t, DefaultThresholds().Any())
}

func TestSharesActInOrder(t *testing.T) {
	assert.NoError(t, DefaultShares().Order())
	assert.NoError(t, Shares{Warn: 90, Checkpoint: 90, Hold: 90}.Order(), "equal is in order")
	assert.NoError(t, Shares{Warn: 95, Hold: 97}.Order(), "a share that is off is skipped")
	assert.NoError(t, Shares{Warn: 95, Checkpoint: 0, Hold: 0}.Order())
	assert.NoError(t, Shares{}.Order())

	assert.ErrorContains(t, Shares{Warn: 95, Checkpoint: 90, Hold: 97}.Order(), "warn 95 is above checkpoint 90")
	assert.ErrorContains(t, Shares{Warn: 75, Checkpoint: 98, Hold: 97}.Order(), "checkpoint 98 is above hold 97")
	assert.ErrorContains(t, Shares{Warn: 99, Hold: 97}.Order(), "warn 99 is above hold 97")
}
