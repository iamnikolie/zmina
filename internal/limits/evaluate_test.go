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
	// resetAt is where the 5-hour windows of these tests reset: two hours after noon.
	resetAt = noon.Add(2 * time.Hour)
	// weekAt is where the 7-day window resets: three days on.
	weekAt = noon.Add(72 * time.Hour)
)

// claudeAt is a Claude report taken at noon with the 5-hour window at five and the 7-day window at seven percent.
func claudeAt(five, seven float64, fiveReset, sevenReset *time.Time) Report {
	return Report{Claude: &Claude{
		FiveHour: &Window{Used: five, Resets: fiveReset},
		SevenDay: &Window{Used: seven, Resets: sevenReset},
		At:       noon,
	}}
}

// readAt is the same report taken at another moment.
func readAt(r Report, at time.Time) Report {
	r.Claude.At = at

	return r
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

// holdsAlso is the default thresholds with a hold share on a window of a plan too.
func holdsAlso(plan Plan, window string, share Share) Thresholds {
	t := DefaultThresholds()

	if plan == PlanClaude {
		t.Claude.Hold[window] = share
	} else {
		t.Codex.Hold[window] = share
	}

	return t
}

func TestWarnIsToldOncePerPeriod(t *testing.T) {
	r := claudeAt(76, 10, at(resetAt), at(weekAt))

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

	assert.Empty(t, eval(claudeAt(74.9, 10, at(resetAt), at(weekAt)), Book{}, noon).Actions, "under warn")
	assert.Len(t, eval(claudeAt(75, 10, at(resetAt), at(weekAt)), Book{}, noon).Actions, 1, "at warn")
}

func TestCheckpointIsAskedOncePerPeriodAndCountsAsWarn(t *testing.T) {
	got := eval(claudeAt(91, 10, at(resetAt), at(weekAt)), Book{}, noon)
	assert.Equal(t, []Kind{KindCheckpoint}, kinds(got), "one action: a window at 91 does not also warn")

	later := eval(claudeAt(92, 10, at(resetAt), at(weekAt)), got.Book, noon.Add(time.Minute))
	assert.Empty(t, later.Actions, "asked once")

	// It warned first, climbs: the checkpoint comes once and the warn is not repeated.
	first := eval(claudeAt(80, 10, at(resetAt), at(weekAt)), Book{}, noon)
	assert.Equal(t, []Kind{KindWarn}, kinds(first))

	second := eval(claudeAt(90, 10, at(resetAt), at(weekAt)), first.Book, noon.Add(time.Minute))
	assert.Equal(t, []Kind{KindCheckpoint}, kinds(second))
	assert.Empty(t, eval(claudeAt(91, 10, at(resetAt), at(weekAt)), second.Book, noon.Add(2*time.Minute)).Actions)
}

func TestHoldStartsAtItsShareAndIsKeptInTheBook(t *testing.T) {
	got := eval(claudeAt(97, 10, at(resetAt), at(weekAt)), Book{}, noon)
	require.Equal(t, []Kind{KindHold}, kinds(got))

	h := got.Actions[0].Hold
	assert.Equal(t, PlanClaude, h.Plan)
	assert.Equal(t, WindowFiveHour, h.Window)
	assert.True(t, h.Reset.Equal(resetAt))
	assert.True(t, h.Since.Equal(noon))
	assert.Equal(t, 97.0, h.Used)
	assert.Equal(t, []Hold{h}, got.Book.Holds)

	// Held: nothing more is asked while it is on, and the lower shares were told with it.
	again := eval(claudeAt(98, 10, at(resetAt), at(weekAt)), got.Book, noon.Add(time.Minute))
	assert.Empty(t, again.Actions)
	assert.Equal(t, []Hold{h}, again.Book.Holds)
	require.Len(t, got.Book.Periods, 1)
	assert.False(t, got.Book.Periods[0].Warn.IsZero())
	assert.False(t, got.Book.Periods[0].Checkpoint.IsZero())
}

// By default only the short window is held: a week at 99% warns and asks for checkpoints, and holds nothing.
func TestOnlyTheShortWindowIsHeldByDefault(t *testing.T) {
	got := eval(claudeAt(10, 99, at(resetAt), at(weekAt)), Book{}, noon)
	assert.Equal(t, []Kind{KindCheckpoint}, kinds(got))
	assert.Equal(t, WindowSevenDay, got.Actions[0].Window)
	assert.Empty(t, got.Book.Holds)

	// A number for the long window opts it in, on its own share; the short one is held on its own.
	marks := holdsAlso(PlanClaude, WindowSevenDay, 98)

	opted := Evaluate(Input{Report: claudeAt(91, 98, at(resetAt), at(weekAt)), Thresholds: marks, Plans: bothPlans, Now: noon})
	assert.Equal(t, []Kind{KindCheckpoint, KindHold}, kinds(opted))
	require.Len(t, opted.Book.Holds, 1)
	assert.Equal(t, WindowSevenDay, opted.Book.Holds[0].Window)

	below := Evaluate(Input{Report: claudeAt(10, 97, at(resetAt), at(weekAt)), Thresholds: marks, Plans: bothPlans, Now: noon})
	assert.Equal(t, []Kind{KindCheckpoint}, kinds(below), "its own share, 98: 97 is under it")

	// A window with no hold share (the short one turned off) is never held.
	off := DefaultThresholds()
	off.Claude.Hold[WindowFiveHour] = 0

	none := Evaluate(Input{Report: claudeAt(99, 10, at(resetAt), at(weekAt)), Thresholds: off, Plans: bothPlans, Now: noon})
	assert.Equal(t, []Kind{KindCheckpoint}, kinds(none))
}

// A window found already at the hold share holds, and does not also warn and ask for a checkpoint nobody could take.
func TestAWindowFoundAtTheHoldShareOnlyHolds(t *testing.T) {
	got := eval(claudeAt(98, 10, at(resetAt), at(weekAt)), Book{}, noon)
	assert.Equal(t, []Kind{KindHold}, kinds(got))

	// If the hold ends under the share (turned off), the lower ones are not asked late for the same period.
	off := DefaultThresholds()
	off.Claude.Hold[WindowFiveHour] = 0

	after := Evaluate(Input{Report: claudeAt(98, 10, at(resetAt), at(weekAt)), Thresholds: off, Plans: bothPlans, Book: got.Book,
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
	// Read eight hours before: judged again at noon, a reading that old says nothing.
	old := readAt(claudeAt(99, 99, at(resetAt), at(weekAt)), noon.Add(-8*time.Hour))
	// ... and one whose reset passed since it was read.
	rolled := claudeAt(99, 10, at(noon.Add(-time.Minute)), at(weekAt))

	for name, r := range map[string]Report{"an old reading": old, "a reset that passed": rolled} {
		got := eval(r, Book{}, noon)
		assert.Empty(t, got.Actions, name)
		assert.Empty(t, got.Book.Holds, name)
		assert.False(t, got.Changed, name)
	}
}

func TestStaleDataReleasesNothingBeforeTheReset(t *testing.T) {
	held := eval(claudeAt(97, 10, at(resetAt), at(weekAt)), Book{}, noon).Book
	require.Len(t, held.Holds, 1)

	// Nothing read at all, or the reading old: the hold stays until its time.
	for name, r := range map[string]Report{
		"no reading": {},
		"an old one": readAt(claudeAt(10, 10, at(resetAt), at(weekAt)), noon.Add(-8*time.Hour)),
	} {
		got := eval(r, held, resetAt.Add(-time.Second))
		assert.Empty(t, got.Actions, name)
		assert.Equal(t, held.Holds, got.Book.Holds, name)
		assert.False(t, got.Changed, name)
	}
}

func TestAHoldEndsAtTheResetTime(t *testing.T) {
	held := eval(claudeAt(97, 10, at(resetAt), at(weekAt)), Book{}, noon).Book

	// No reading since: the clock ends it, and the data is said to be stale.
	got := eval(Report{}, held, resetAt)
	require.Equal(t, []Kind{KindRelease}, kinds(got))
	assert.Equal(t, ReasonReset, got.Actions[0].Reason)
	assert.False(t, got.Actions[0].Confirmed)
	assert.Empty(t, got.Book.Holds)
	assert.True(t, got.Changed)

	// A reading that is stale for its reset having passed is the same: the old period's figure confirms nothing.
	got = eval(claudeAt(97, 10, at(resetAt), at(weekAt)), held, resetAt.Add(time.Minute))
	assert.False(t, got.Actions[0].Confirmed)

	// A fresh reading of the next period confirms it, and the figure in the note is the new one.
	next := readAt(claudeAt(3, 10, at(resetAt.Add(5*time.Hour)), at(weekAt)), resetAt.Add(time.Minute))
	got = eval(next, held, resetAt.Add(2*time.Minute))
	require.Equal(t, []Kind{KindRelease}, kinds(got))
	assert.True(t, got.Actions[0].Confirmed)
	assert.Equal(t, 3.0, got.Actions[0].Used)
}

func TestAHoldEndsWhenAFreshReadingOfThePeriodShowsItUnderTheShare(t *testing.T) {
	held := eval(claudeAt(97, 10, at(resetAt), at(weekAt)), Book{}, noon).Book

	got := eval(readAt(claudeAt(96, 10, at(resetAt), at(weekAt)), noon.Add(time.Minute)), held, noon.Add(time.Minute))
	require.Equal(t, []Kind{KindRelease}, kinds(got))
	assert.Equal(t, ReasonBelow, got.Actions[0].Reason)
	assert.True(t, got.Actions[0].Confirmed)
	assert.Empty(t, got.Book.Holds)

	// A reading of the same period at or over the share keeps it.
	still := eval(readAt(claudeAt(97, 10, at(resetAt), at(weekAt)), noon.Add(time.Minute)), held, noon.Add(time.Minute))
	assert.Empty(t, still.Actions)

	// A reading of an earlier period (a status line drawn from old input) says nothing about this one.
	older := eval(readAt(claudeAt(5, 10, at(resetAt.Add(-time.Hour)), at(weekAt)), noon.Add(time.Minute)), held, noon.Add(time.Minute))
	assert.Empty(t, older.Actions)
}

// A fresh reading whose reset is later than the hold's: the window rolled over before the time it was held to.
func TestAHoldEndsWhenAFreshReadingShowsALaterPeriod(t *testing.T) {
	held := eval(claudeAt(97, 10, at(resetAt), at(weekAt)), Book{}, noon).Book

	got := eval(readAt(claudeAt(2, 10, at(resetAt.Add(2*time.Hour)), at(weekAt)), noon.Add(time.Minute)), held, noon.Add(time.Minute))
	require.Equal(t, []Kind{KindRelease}, kinds(got))
	assert.Equal(t, ReasonReset, got.Actions[0].Reason)
	assert.True(t, got.Actions[0].Confirmed)

	// A reset further off than the window is long is no later period: the reader dropped it, and it says nothing.
	far := eval(readAt(claudeAt(2, 10, at(resetAt.Add(5*time.Hour)), at(weekAt)), noon.Add(time.Minute)), held, noon.Add(time.Minute))
	assert.Empty(t, far.Actions)
}

func TestTwoWindowsAreJudgedEachOnItsOwn(t *testing.T) {
	marks := holdsAlso(PlanClaude, WindowSevenDay, 97)

	got := Evaluate(Input{Report: claudeAt(91, 97, at(resetAt), at(weekAt)), Thresholds: marks, Plans: bothPlans, Now: noon})
	require.Equal(t, []Kind{KindCheckpoint, KindHold}, kinds(got))
	assert.Equal(t, WindowFiveHour, got.Actions[0].Window)
	assert.Equal(t, WindowSevenDay, got.Actions[1].Window)
	require.Len(t, got.Book.Holds, 1)
	assert.Equal(t, WindowSevenDay, got.Book.Holds[0].Window)

	// The 5-hour window rolls over; the week stays held, and the new 5-hour period warns anew.
	next := readAt(claudeAt(80, 97, at(resetAt.Add(5*time.Hour)), at(weekAt)), resetAt.Add(time.Minute))
	got2 := Evaluate(Input{Report: next, Thresholds: marks, Plans: bothPlans, Book: got.Book, Now: resetAt.Add(2 * time.Minute)})
	assert.Equal(t, []Kind{KindWarn}, kinds(got2))
	assert.Equal(t, got.Book.Holds, got2.Book.Holds, "the week's hold is untouched")
}

// Two reset times less than two minutes apart are one period, and the later one is kept; further apart they are two.
func TestAMovedResetIsANewPeriodOnlyWhenItMovesFar(t *testing.T) {
	first := eval(claudeAt(76, 10, at(resetAt), at(weekAt)), Book{}, noon)
	require.Equal(t, []Kind{KindWarn}, kinds(first))

	jitter := eval(claudeAt(76, 10, at(resetAt.Add(90*time.Second)), at(weekAt)), first.Book, noon.Add(time.Minute))
	assert.Empty(t, jitter.Actions, "90 seconds is a rounding")
	require.Len(t, jitter.Book.Periods, 1)
	assert.True(t, jitter.Book.Periods[0].Reset.Equal(resetAt.Add(90*time.Second)), "the later reset is kept")

	// Another jitter, on the other side of a minute boundary, is still the same period: no flip.
	back := eval(claudeAt(76, 10, at(resetAt.Add(30*time.Second)), at(weekAt)), jitter.Book, noon.Add(2*time.Minute))
	assert.Empty(t, back.Actions)

	moved := eval(claudeAt(76, 10, at(resetAt.Add(3*time.Minute)), at(weekAt)), first.Book, noon.Add(3*time.Minute))
	assert.Equal(t, []Kind{KindWarn}, kinds(moved), "three minutes is another window")
	assert.Len(t, moved.Book.Periods, 2, "both periods are on the book until the old one is pruned")
}

func TestCodexWeekIsHeldOnlyWhenItOptsIn(t *testing.T) {
	r := Report{Codex: &Codex{Week: &Window{Used: 91, Resets: at(noon.Add(48 * time.Hour))}, At: noon, Home: "/h/a"}}

	got := eval(r, Book{}, noon)
	require.Equal(t, []Kind{KindCheckpoint}, kinds(got))
	assert.Equal(t, PlanCodex, got.Actions[0].Plan)
	assert.Equal(t, WindowWeek, got.Actions[0].Window)
	assert.Equal(t, "codex weekly window", got.Actions[0].Label())

	r.Codex.Week.Used = 99
	assert.Empty(t, eval(r, got.Book, noon.Add(time.Minute)).Actions, "its week is a long window: no hold by default")

	opted := Evaluate(Input{Report: r, Thresholds: holdsAlso(PlanCodex, WindowWeek, 97), Plans: bothPlans, Book: got.Book, Now: noon.Add(time.Minute)})
	require.Equal(t, []Kind{KindHold}, kinds(opted))
	assert.Equal(t, PlanCodex, opted.Book.Holds[0].Plan)
	assert.Equal(t, "/h/a", opted.Book.Holds[0].Home, "made by one home's reading")
}

// The seats of one Codex home are not held by another home's reading, and another home's reading does not end the hold.
func TestACodexHoldKeepsOnlyTheSeatsOfItsHome(t *testing.T) {
	marks := holdsAlso(PlanCodex, WindowWeek, 97)
	reset := noon.Add(48 * time.Hour)

	a := Report{Codex: &Codex{Week: &Window{Used: 98, Resets: &reset}, At: noon, Home: "/h/a"}}
	held := Evaluate(Input{Report: a, Thresholds: marks, Plans: bothPlans, Now: noon})
	require.Len(t, held.Book.Holds, 1)

	_, ok := held.Book.HeldUntil(PlanCodex, "/h/a", noon)
	assert.True(t, ok)
	_, ok = held.Book.HeldUntil(PlanCodex, "/h/b", noon)
	assert.False(t, ok, "another account")
	_, ok = held.Book.HeldUntil(PlanClaude, "", noon)
	assert.False(t, ok)

	// A fresh reading of the other home, low, says nothing of the held one.
	b := Report{Codex: &Codex{Week: &Window{Used: 5, Resets: &reset}, At: noon.Add(time.Minute), Home: "/h/b"}}
	still := Evaluate(Input{Report: b, Thresholds: marks, Plans: bothPlans, Book: held.Book, Now: noon.Add(time.Minute)})
	assert.Empty(t, still.Actions)
	assert.Len(t, still.Book.Holds, 1)
}

func TestAPlanWithNoRolesIsNotJudged(t *testing.T) {
	r := claudeAt(99, 99, at(resetAt), at(weekAt))

	got := Evaluate(Input{Report: r, Thresholds: DefaultThresholds(), Plans: []Plan{PlanCodex}, Now: noon})
	assert.Empty(t, got.Actions)
	assert.Empty(t, got.Book.Holds)
}

func TestAWindowWithNoResetTimeIsNeverHeld(t *testing.T) {
	r := claudeAt(98, 10, nil, at(weekAt))

	got := eval(r, Book{}, noon)
	assert.Equal(t, []Kind{KindCheckpoint}, kinds(got), "a hold needs a time to end at")
	assert.Empty(t, got.Book.Holds)

	// Its one period has no end: a fall under the share re-arms it.
	assert.Empty(t, eval(r, got.Book, noon.Add(time.Minute)).Actions)

	fallen := eval(claudeAt(10, 10, nil, at(weekAt)), got.Book, noon.Add(2*time.Minute))
	assert.Empty(t, fallen.Actions)

	climbed := eval(r, fallen.Book, noon.Add(3*time.Minute))
	assert.Equal(t, []Kind{KindCheckpoint}, kinds(climbed))
}

// A reset no window can have is never held: not one that does not lie ahead, not one further off than the window is long. A hold
// on the book with such a reset holds nothing and is ended.
func TestAResetThatCannotBeTheWindowsIsNeverHeld(t *testing.T) {
	// The reader drops it (Judge), so the window has no reset and is not held.
	got := eval(claudeAt(99, 10, at(noon.Add(400*24*time.Hour)), at(weekAt)), Book{}, noon)
	assert.Equal(t, []Kind{KindCheckpoint}, kinds(got))
	assert.Empty(t, got.Book.Holds)

	// A hold written with one anyway (an older build, a hand-edited file): not active, and ended with its own reason.
	bad := Hold{Plan: PlanClaude, Window: WindowFiveHour, Reset: noon.Add(400 * 24 * time.Hour), Since: noon.Add(-time.Hour), Used: 98}
	assert.False(t, bad.Active(noon))

	book := Book{Holds: []Hold{bad}}
	_, held := book.HeldUntil(PlanClaude, "", noon)
	assert.False(t, held, "the delivery's test says it is not held")

	ended := eval(Report{}, book, noon)
	require.Equal(t, []Kind{KindRelease}, kinds(ended))
	assert.Equal(t, ReasonBad, ended.Actions[0].Reason)
	assert.False(t, ended.Actions[0].Confirmed)
	assert.Empty(t, ended.Book.Holds)

	// A hold that is credible is active until its reset time, a week's worth for the week, the slack included.
	week := Hold{Plan: PlanClaude, Window: WindowSevenDay, Reset: noon.Add(7*24*time.Hour + 30*time.Minute), Since: noon}
	assert.True(t, week.Active(noon))
	assert.False(t, week.Active(week.Reset))

	// The one clock: judged at Now, a reading that has since gone stale holds nothing new even if it was fresh when it was read.
	r := claudeAt(98, 10, at(noon.Add(30*time.Second)), at(weekAt))
	assert.Empty(t, eval(r, Book{}, noon.Add(time.Minute)).Actions)
}

func TestPeriodsPastTheirResetByADayArePruned(t *testing.T) {
	first := eval(claudeAt(76, 10, at(resetAt), at(weekAt)), Book{}, noon)
	require.Len(t, first.Book.Periods, 1)

	kept := eval(Report{}, first.Book, resetAt.Add(23*time.Hour))
	assert.Len(t, kept.Book.Periods, 1)
	assert.False(t, kept.Changed)

	pruned := eval(Report{}, first.Book, resetAt.Add(25*time.Hour))
	assert.Empty(t, pruned.Book.Periods)
	assert.True(t, pruned.Changed)
}

func TestEvaluateDoesNotChangeTheBookOrTheReportItWasGiven(t *testing.T) {
	book := eval(claudeAt(97, 10, at(resetAt), at(weekAt)), Book{}, noon).Book
	before, err := json.Marshal(book)
	require.NoError(t, err)

	r := claudeAt(97, 10, at(resetAt), at(weekAt))
	eval(r, book, resetAt)

	after, err := json.Marshal(book)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after))
	assert.False(t, r.Claude.FiveHour.Stale, "the report is judged on a copy")
}

func TestBookKeepsItsTimesAcrossAFile(t *testing.T) {
	dir := t.TempDir()

	b, err := ReadBook(dir)
	require.NoError(t, err)
	assert.Empty(t, b.Holds, "no file: an empty book")

	want := eval(claudeAt(97, 10, at(resetAt), at(weekAt)), Book{}, noon).Book
	want.Waits = []Wait{{Role: "dev", Plan: PlanClaude, Window: WindowFiveHour, Until: noon.Add(time.Minute)}}
	require.NoError(t, WriteBook(dir, want))

	got, err := ReadBook(dir)
	require.NoError(t, err)
	require.Len(t, got.Holds, 1)
	assert.True(t, got.Holds[0].Reset.Equal(resetAt))
	assert.Equal(t, want.Periods, got.Periods)
	require.Len(t, got.Waits, 1)

	h, ok := got.HeldUntil(PlanClaude, "", noon.Add(time.Minute))
	require.True(t, ok)
	assert.Equal(t, WindowFiveHour, h.Window)

	_, ok = got.HeldUntil(PlanClaude, "", resetAt)
	assert.False(t, ok, "the reset time itself is no longer held")
	_, ok = got.HeldUntil(PlanCodex, "", noon)
	assert.False(t, ok)

	w, ok := got.WaitFor("dev", noon)
	require.True(t, ok)
	assert.Equal(t, WindowFiveHour, w.Window)
	_, ok = got.WaitFor("dev", noon.Add(time.Minute))
	assert.False(t, ok, "its turn has come")

	// An empty book removes the file.
	require.NoError(t, WriteBook(dir, Book{}))
	_, err = os.Stat(filepath.Join(dir, HoldsFile))
	assert.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, WriteBook(dir, Book{}), "again, nothing there")
}

func TestHeldUntilIsTheLatestResetOfTheWindowsHeld(t *testing.T) {
	b := Book{Holds: []Hold{
		{Plan: PlanClaude, Window: WindowFiveHour, Reset: noon.Add(time.Hour), Since: noon},
		{Plan: PlanClaude, Window: WindowSevenDay, Reset: noon.Add(72 * time.Hour), Since: noon},
		{Plan: PlanCodex, Window: WindowWeek, Reset: noon.Add(100 * time.Hour), Since: noon},
	}}

	h, ok := b.HeldUntil(PlanClaude, "", noon)
	require.True(t, ok)
	assert.Equal(t, WindowSevenDay, h.Window)

	h, ok = b.HeldUntil(PlanClaude, "", noon.Add(2*time.Hour))
	require.True(t, ok)
	assert.Equal(t, WindowSevenDay, h.Window, "the 5-hour one is over, the week goes on")
	assert.Len(t, b.Active(noon.Add(2*time.Hour)), 2)
}

func TestAFileThatDoesNotReadHoldsNothing(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, HoldsFile), []byte("{nope"), 0o600))

	b, err := ReadBook(dir)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrBookFuture)
	assert.Empty(t, b.Holds)
}

// A file of a newer version is not read as an old one and not mistaken for a broken one: it is left alone.
func TestAFileOfANewerVersionIsNotReadAndIsToldApart(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, HoldsFile), []byte(`{"v":9,"holds":[{"plan":"claude","window":"five_hour","reset":"2099-01-01T00:00:00Z"}]}`), 0o600))

	b, err := ReadBook(dir)
	require.ErrorIs(t, err, ErrBookFuture)
	assert.Empty(t, b.Holds, "nothing is held by what this zmina cannot read")

	// The first version's file (periods kept as a map) reads, with its holds.
	require.NoError(t, os.WriteFile(filepath.Join(dir, HoldsFile),
		[]byte(`{"v":1,"told":{"claude|five_hour|1":{"warn":"2030-01-01T12:00:00Z"}},"holds":[{"plan":"claude","window":"five_hour","reset":"2030-01-01T14:00:00Z","since":"2030-01-01T12:00:00Z","used":97}]}`), 0o600))

	b, err = ReadBook(dir)
	require.NoError(t, err)
	assert.Len(t, b.Holds, 1)
	assert.Empty(t, b.Periods)
}

func TestAFileThatDoesNotReadIsMovedAsideKeepingThree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, HoldsFile)

	to, err := MoveAside(dir, noon)
	require.NoError(t, err)
	assert.Empty(t, to, "no file, nothing moved")

	for i := 0; i < 5; i++ {
		require.NoError(t, os.WriteFile(path, []byte("{bad"), 0o600))

		to, err = MoveAside(dir, noon.Add(time.Duration(i)*time.Second))
		require.NoError(t, err)
		assert.Contains(t, to, "holds.json.bad-")
	}

	assert.NoFileExists(t, path)

	kept, err := filepath.Glob(path + ".bad-*")
	require.NoError(t, err)
	assert.Len(t, kept, 3, "the newest three")
	assert.Equal(t, path+".bad-20300101-120004", kept[2])
}

func TestSharesAreNumbersOrOff(t *testing.T) {
	raw, err := json.Marshal(Shares{Warn: 75, Checkpoint: 0, Hold: map[string]Share{WindowFiveHour: 97, WindowSevenDay: 0}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"warn":75,"checkpoint":"off","hold":{"five_hour":97,"seven_day":"off"}}`, string(raw))

	var back Shares
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, Shares{Warn: 75, Hold: map[string]Share{WindowFiveHour: 97, WindowSevenDay: 0}}, back)
	assert.Error(t, json.Unmarshal([]byte(`{"warn":"sometimes"}`), &back))

	assert.Equal(t, "warn 75%, checkpoint off, hold 5h 97%, 7d off", Shares{Warn: 75, Hold: map[string]Share{WindowFiveHour: 97, WindowSevenDay: 0}}.String())
	assert.Equal(t, "warn off, checkpoint off, hold off", Shares{}.String())
	assert.Equal(t, "claude: warn 75%, checkpoint 90%, hold 5h 97%, 7d off · codex: warn 75%, checkpoint 90%, hold week off", DefaultThresholds().String())
	assert.Equal(t, Shares{}, Thresholds{}.For("gemini"))
	assert.False(t, Thresholds{}.Any())
	assert.True(t, DefaultThresholds().Any())
	assert.Equal(t, 97, int(DefaultThresholds().Claude.HoldOf(WindowFiveHour)))
	assert.Zero(t, DefaultThresholds().Claude.HoldOf(WindowSevenDay))
	assert.Zero(t, DefaultThresholds().Codex.HoldOf(WindowWeek), "Codex has no short window: nothing is held by default")
	assert.Equal(t, []string{WindowFiveHour, WindowSevenDay}, PlanWindows(PlanClaude))
	assert.Equal(t, []string{WindowWeek}, PlanWindows(PlanCodex))
}

func TestSharesActInOrder(t *testing.T) {
	assert.NoError(t, DefaultThresholds().Claude.Order())
	assert.NoError(t, Shares{Warn: 90, Checkpoint: 90, Hold: map[string]Share{WindowFiveHour: 90}}.Order(), "equal is in order")
	assert.NoError(t, Shares{Warn: 95, Hold: map[string]Share{WindowFiveHour: 97}}.Order(), "a share that is off is skipped")
	assert.NoError(t, Shares{}.Order())

	assert.ErrorContains(t, Shares{Warn: 95, Checkpoint: 90}.Order(), "warn 95 is above checkpoint 90")
	assert.ErrorContains(t, Shares{Checkpoint: 98, Hold: map[string]Share{WindowFiveHour: 97}}.Order(), "checkpoint 98 is above the hold of the 5h window 97")
	assert.ErrorContains(t, Shares{Warn: 99, Hold: map[string]Share{WindowSevenDay: 97}}.Order(), "warn 99 is above the hold of the 7d window 97")
}

// roles are a dispatcher and these others, oldest line first.
func roles(others ...string) func(Plan, string) Roles {
	return func(Plan, string) Roles { return Roles{Dispatcher: "dispatcher", Others: others} }
}

func waits(b Book) map[string]time.Duration {
	out := map[string]time.Duration{}

	for _, w := range b.Waits {
		out[w.Role] = w.Until.Sub(resetAt)
	}

	return out
}

// A release lets the dispatcher go at once and the roles three a minute, the role whose line waited longest first.
func TestAReleaseLetsTheRolesGoInTurn(t *testing.T) {
	held := eval(claudeAt(97, 10, at(resetAt), at(weekAt)), Book{}, noon).Book

	next := readAt(claudeAt(2, 10, at(resetAt.Add(5*time.Hour)), at(weekAt)), resetAt)
	got := Evaluate(Input{Report: next, Thresholds: DefaultThresholds(), Plans: bothPlans, Book: held, Now: resetAt,
		Order: roles("a", "b", "c", "d", "e", "f", "g")})

	require.Equal(t, []Kind{KindRelease}, kinds(got))
	assert.True(t, got.Actions[0].Confirmed)
	assert.Equal(t, map[string]time.Duration{
		"a": time.Minute, "b": time.Minute, "c": time.Minute,
		"d": 2 * time.Minute, "e": 2 * time.Minute, "f": 2 * time.Minute,
		"g": 3 * time.Minute,
	}, waits(got.Book), "the dispatcher has no wait: it goes at the release")

	// The turns come round with the clock, and a role whose turn has come is no longer on the book.
	later := Evaluate(Input{Report: next, Thresholds: DefaultThresholds(), Plans: bothPlans, Book: got.Book, Now: resetAt.Add(90 * time.Second)})
	assert.Equal(t, []string{"d", "e", "f", "g"}, []string{later.Book.Waits[0].Role, later.Book.Waits[1].Role, later.Book.Waits[2].Role, later.Book.Waits[3].Role})
	assert.Empty(t, Evaluate(Input{Report: next, Thresholds: DefaultThresholds(), Plans: bothPlans, Book: got.Book, Now: resetAt.Add(4 * time.Minute)}).Book.Waits)

	_, waiting := got.Book.WaitFor("a", resetAt.Add(30*time.Second))
	assert.True(t, waiting)
	_, waiting = got.Book.WaitFor("dispatcher", resetAt)
	assert.False(t, waiting)
}

// A hold that ended by the clock alone, with no fresh reading, lets the dispatcher and one role go; the rest follow a fresh reading
// under the share, or ten minutes.
func TestAnUnconfirmedReleaseLetsOneRoleGoFirst(t *testing.T) {
	held := eval(claudeAt(97, 10, at(resetAt), at(weekAt)), Book{}, noon).Book

	got := Evaluate(Input{Report: Report{}, Thresholds: DefaultThresholds(), Plans: bothPlans, Book: held, Now: resetAt,
		Order: roles("a", "b", "c", "d", "e", "f", "g", "h")})
	require.Equal(t, []Kind{KindRelease}, kinds(got))
	assert.False(t, got.Actions[0].Confirmed)

	ten := 10 * time.Minute
	assert.Equal(t, map[string]time.Duration{"b": ten, "c": ten, "d": ten, "e": ten, "f": ten, "g": ten, "h": ten}, waits(got.Book),
		"a goes with the dispatcher; the others wait")

	for _, w := range got.Book.Waits {
		assert.True(t, w.Pending, w.Role)
	}

	// No reading: nothing moves until the ten minutes are up.
	idle := Evaluate(Input{Report: Report{}, Thresholds: DefaultThresholds(), Plans: bothPlans, Book: got.Book, Now: resetAt.Add(5 * time.Minute)})
	assert.Equal(t, got.Book.Waits, idle.Book.Waits)
	assert.False(t, idle.Changed)

	// A fresh reading of the window under its share: they go on in groups, from now.
	fresh := readAt(claudeAt(4, 10, at(resetAt.Add(5*time.Hour)), at(weekAt)), resetAt.Add(5*time.Minute))
	moved := Evaluate(Input{Report: fresh, Thresholds: DefaultThresholds(), Plans: bothPlans, Book: got.Book, Now: resetAt.Add(5 * time.Minute)})
	assert.Equal(t, map[string]time.Duration{"e": 6 * time.Minute, "f": 6 * time.Minute, "g": 6 * time.Minute, "h": 7 * time.Minute}, waits(moved.Book),
		"b, c and d go now; the others three a minute after")
	for _, w := range moved.Book.Waits {
		assert.False(t, w.Pending, w.Role)
	}

	// A fresh reading still at the share changes nothing.
	still := readAt(claudeAt(98, 10, at(resetAt.Add(5*time.Hour)), at(weekAt)), resetAt.Add(5*time.Minute))
	assert.Equal(t, got.Book.Waits, Evaluate(Input{Report: still, Thresholds: DefaultThresholds(), Plans: bothPlans, Book: got.Book,
		Now: resetAt.Add(5 * time.Minute)}).Book.Waits)

	// Ten minutes without one: they go, three a minute from then.
	late := Evaluate(Input{Report: Report{}, Thresholds: DefaultThresholds(), Plans: bothPlans, Book: got.Book, Now: resetAt.Add(10 * time.Minute)})
	assert.Empty(t, late.Book.Waits)
}

// While another window of the plan still holds it, no role is let go: the pacing comes with the last release.
func TestNoRoleIsLetGoWhileAnotherWindowHoldsThePlan(t *testing.T) {
	marks := holdsAlso(PlanClaude, WindowSevenDay, 97)
	held := Evaluate(Input{Report: claudeAt(97, 98, at(resetAt), at(weekAt)), Thresholds: marks, Plans: bothPlans, Now: noon}).Book
	require.Len(t, held.Holds, 2)

	next := readAt(claudeAt(1, 98, at(resetAt.Add(5*time.Hour)), at(weekAt)), resetAt)
	got := Evaluate(Input{Report: next, Thresholds: marks, Plans: bothPlans, Book: held, Now: resetAt, Order: roles("a", "b")})
	require.Equal(t, []Kind{KindRelease}, kinds(got))
	assert.Empty(t, got.Book.Waits)
	require.Len(t, got.Book.Holds, 1)
}

// Lifting a hold by hand ends it, marks its period lifted so that it is not held again, and lets the roles go in turn.
func TestLiftingAHoldByHand(t *testing.T) {
	r := claudeAt(98, 10, at(resetAt), at(weekAt))
	held := eval(r, Book{}, noon).Book
	require.Len(t, held.Holds, 1)

	book, lifted := Lift(held, PlanClaude, "", noon.Add(time.Minute), roles("a", "b", "c", "d"))
	require.Len(t, lifted.Actions, 1)
	assert.Equal(t, ReasonHand, lifted.Actions[0].Reason)
	assert.True(t, lifted.Actions[0].Confirmed)
	assert.Equal(t, KindRelease, lifted.Actions[0].Kind)
	assert.Empty(t, book.Holds)
	assert.Len(t, book.Waits, 4)

	// Still at 98% in the same period: not held again. A later period may be.
	again := eval(readAt(r, noon.Add(2*time.Minute)), book, noon.Add(2*time.Minute))
	assert.Empty(t, again.Actions)
	assert.Empty(t, again.Book.Holds)

	next := readAt(claudeAt(99, 10, at(resetAt.Add(5*time.Hour)), at(weekAt)), resetAt.Add(time.Minute))
	assert.Equal(t, []Kind{KindHold}, kinds(eval(next, again.Book, resetAt.Add(time.Minute))), "the next period holds again")

	// A lifted period survives a jitter of its reset time.
	jitter := eval(readAt(claudeAt(98, 10, at(resetAt.Add(time.Minute)), at(weekAt)), noon.Add(2*time.Minute)), book, noon.Add(2*time.Minute))
	assert.Empty(t, jitter.Actions)
}

func TestLiftingNamesTheWindowAndLeavesTheOthers(t *testing.T) {
	marks := holdsAlso(PlanClaude, WindowSevenDay, 97)
	held := Evaluate(Input{Report: claudeAt(97, 98, at(resetAt), at(weekAt)), Thresholds: marks, Plans: bothPlans, Now: noon}).Book
	require.Len(t, held.Holds, 2)

	book, lifted := Lift(held, PlanClaude, WindowSevenDay, noon, roles("a"))
	require.Len(t, lifted.Actions, 1)
	assert.Equal(t, WindowSevenDay, lifted.Actions[0].Window)
	require.Len(t, book.Holds, 1)
	assert.Equal(t, WindowFiveHour, book.Holds[0].Window)
	assert.Empty(t, book.Waits, "the plan is held still by its other window")

	_, nothing := Lift(book, PlanCodex, "", noon, nil)
	assert.Empty(t, nothing.Actions, "nothing is lifted that is not held")
	_, nothing = Lift(book, PlanClaude, WindowSevenDay, noon, nil)
	assert.Empty(t, nothing.Actions)
}

// A person who lifts a plan's holds also lets go the roles still waiting their turn after an earlier release.
func TestLiftingAlsoLetsGoTheRolesStillWaitingTheirTurn(t *testing.T) {
	held := eval(claudeAt(97, 10, at(resetAt), at(weekAt)), Book{}, noon).Book
	released := Evaluate(Input{Report: Report{}, Thresholds: DefaultThresholds(), Plans: bothPlans, Book: held, Now: resetAt, Order: roles("a", "b", "c")}).Book
	require.Len(t, released.Waits, 2)

	book, lifted := Lift(released, PlanClaude, "", resetAt.Add(time.Minute), roles("a", "b", "c"))
	assert.Empty(t, lifted.Actions, "no hold is on")
	assert.Equal(t, 2, lifted.Waits)
	assert.Empty(t, book.Waits, "they go at once: the person who lifts has said go")
}
