package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/task"
)

// parked puts an item on the crew's list as if it had been made age ago; mut changes it first.
func parked(t *testing.T, file, text string, age time.Duration, mut func(*later.Item)) string {
	t.Helper()

	c, err := crew.Load(file)
	require.NoError(t, err)

	it := later.Item{Text: text}
	if mut != nil {
		mut(&it)
	}

	it, err = laterStore(c).Add(it, time.Now().Add(-age))
	require.NoError(t, err)

	return it.ID
}

const day = 24 * time.Hour

// staleCrew is a crew with a parked list of every kind of item a sweep may meet: L-1 and L-2 are stale (L-2 came from a
// task), the rest are not: L-3 young, L-4 kept lately, L-5 waiting on a day, L-6 back on the owner's todo, L-7 closed.
func staleCrew(t *testing.T, extra string) string {
	t.Helper()

	file := testCrew(t, "  dev:\n"+extra)

	parked(t, file, "rotate the deploy key", 40*day, func(it *later.Item) { it.Note = "all repos" })
	parked(t, file, "disk ticket", 30*day, func(it *later.Item) { it.From, it.Role = "T-9", "dev" })
	parked(t, file, "young idea", 2*day, nil)

	c, err := crew.Load(file)
	require.NoError(t, err)

	kept := parked(t, file, "kept lately", 40*day, nil)
	_, err = laterStore(c).Keep(kept, "still the plan", time.Now().Add(-3*day))
	require.NoError(t, err)

	waiting := parked(t, file, "waits for the first of the month", 40*day, nil)
	_, err = alarmStore(c).Add(alarm.Alarm{Kind: alarm.Later, At: time.Now().Add(10 * day), When: "+10d", Later: waiting,
		Text: "waits"}, time.Now())
	require.NoError(t, err)

	parked(t, file, "brought back by its alarm", 40*day, func(it *later.Item) { it.Back = time.Now().Add(-day) })

	closed := parked(t, file, "already closed", 40*day, nil)
	_, err = laterStore(c).Close(closed, later.Done, "", time.Now())
	require.NoError(t, err)

	return file
}

func TestLaterKeepStampsTheItemAndNeedsAReason(t *testing.T) {
	file := testCrew(t, "  dev:\n")

	_, _, err := zm(t, file, "later", "add", "park the pager rota", "--note", "from the offsite")
	require.NoError(t, err)

	_, _, err = zm(t, file, "later", "keep", "1")
	require.ErrorContains(t, err, "later keep needs --note: say why L-1 still stands")

	_, _, err = zm(t, file, "later", "keep", "1", "--note", "   ")
	require.ErrorContains(t, err, "needs --note")

	before := time.Now()
	out, _, err := zm(t, file, "later", "keep", "l-1", "--note", "the rota is not agreed yet")
	require.NoError(t, err)
	assert.Equal(t, "L-1 · kept\n", out)

	out, _, err = zm(t, file, "later", "ls", "--json")
	require.NoError(t, err)

	var items []later.Item
	require.NoError(t, json.Unmarshal([]byte(out), &items))
	require.Len(t, items, 1)
	assert.Equal(t, later.Parked, items[0].State, "kept is still parked")
	assert.Equal(t, "the rota is not agreed yet", items[0].Note, "the reason replaces the note, as done's does")
	assert.False(t, items[0].Reviewed.Before(before.Add(-time.Second)), "the review time is in the JSON")
	assert.Contains(t, out, `"reviewed"`)

	out, _, err = zm(t, file, "later", "ls")
	require.NoError(t, err)
	assert.Regexp(t, `^L-1\s+0m\s+park the pager rota · the rota is not agreed yet\n$`, out, "the age is still the item's own")

	out, _, err = zm(t, file, "log", "--task", "L-1")
	require.NoError(t, err)
	assert.Contains(t, out, "kept: the rota is not agreed yet")

	_, _, err = zm(t, file, "later", "done", "1")
	require.NoError(t, err)

	_, _, err = zm(t, file, "later", "keep", "1", "--note", "x")
	require.ErrorContains(t, err, "L-1 is already done")

	_, _, err = zm(t, file, "later", "keep", "9", "--note", "x")
	require.ErrorContains(t, err, "no item L-9")
}

// An item file written before reviewed existed reads, lists and is kept like any other.
func TestLaterKeepReadsAnOldItemFile(t *testing.T) {
	file := testCrew(t, "  dev:\n")

	c, err := crew.Load(file)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(c.Layout().Later(), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(c.Layout().Later(), "L-1.json"),
		[]byte(`{"id":"L-1","text":"an idea from last year","state":"parked","created":"2025-10-01T10:00:00Z","updated":"2025-10-01T10:00:00Z"}`),
		0o600))

	out, _, err := zm(t, file, "later", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "an idea from last year")

	_, _, err = zm(t, file, "later", "keep", "L-1", "--note", "still wanted")
	require.NoError(t, err)

	out, _, err = zm(t, file, "later", "ls", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"reviewed"`)
	assert.Contains(t, out, `"created": "2025-10-01T10:00:00Z"`)
}

func TestLaterSweepDryRunSaysWhichItemsAndMakesNothing(t *testing.T) {
	file := staleCrew(t, "")

	out, _, err := zm(t, file, "later", "sweep", "dev", "--dry-run")
	require.NoError(t, err)

	assert.Contains(t, out, "would sweep 2 of 2 stale items for dev (older than 14d, not kept within 14d):\n")
	assert.Regexp(t, `L-1\s+40d\s+rotate the deploy key · all repos\n`, out)
	assert.Regexp(t, `L-2\s+30d\s+disk ticket\n`, out)

	for _, not := range []string{"L-3", "L-4", "L-5", "L-6", "L-7"} {
		assert.NotContains(t, out, not, "%s is not stale", not)
	}

	out, _, err = zm(t, file, "task", "ls", "--all")
	require.NoError(t, err)
	assert.NotContains(t, out, "T-1", "a dry run makes no task")

	out, _, err = zm(t, file, "later", "sweep", "dev", "--dry-run", "--max", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "would sweep 1 of 2 stale items")
	assert.NotContains(t, out, "L-2")
}

func TestLaterSweepMakesOneTaskThatCarriesEveryItem(t *testing.T) {
	file := staleCrew(t, "")

	out, _, err := zm(t, file, "later", "sweep", "dev")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev · sweep of 2 parked items (L-1, L-2)\n", out)

	rec := readTaskFile(t, file, "T-1")
	assert.Equal(t, []any{"L-1", "L-2"}, rec["sweep"])
	assert.Equal(t, "dev", rec["role"])
	assert.Equal(t, "open", rec["state"])
	assert.Equal(t, "Sweep of 2 parked items", rec["title"])

	shown, _, err := zm(t, file, "task", "show", "T-1", "--full")
	require.NoError(t, err)
	assert.Contains(t, shown, "check: zmina later swept T-1 · closing it as done waits on it; 2 of 2 parked items have no verdict")

	for _, want := range []string{
		"Sweep of the parked list: 2 items over 14d old that were not kept in that time.",
		"never answer from its text alone",
		`zmina later done L-n --note "<evidence>"`,
		`zmina later drop L-n --note "<why>"`,
		"handing out is the dispatcher's: zmina note",
		`zmina todo add "<the question>" --rec "<your answer>" --reason "<why>"`,
		`zmina later keep L-n --note "asked: A-n"`,
		`zmina later keep L-n --note "<why>"`,
		"refused without --note",
		"zmina later swept T-1 exits 0",
		"zmina task done T-1",
		"zmina later ls --full",
		"L-1 · 40d · rotate the deploy key · note: all repos",
		"L-2 · 30d · from T-9 (dev) · disk ticket",
	} {
		assert.Contains(t, shown, want)
	}

	assert.NotContains(t, shown, sweepSelf, "the task's own id is in its text")
	assert.NotContains(t, shown, "young idea")
	assert.NotContains(t, shown, "later take L-n", "dev is no dispatcher: it proposes, the dispatcher takes")

	evs, err := eventLog(mustCrew(t, file)).Read(events.Filter{Task: "T-1"})
	require.NoError(t, err)

	var swept []string

	for _, e := range evs {
		if e.Event == "later-sweep" {
			swept = append(swept, e.Detail)
		}
	}

	assert.Equal(t, []string{"2 of 2 stale parked items (older than 14d): L-1, L-2"}, swept)
}

// The dispatcher hands work out itself, so its sweep says take; any other seat is told to propose it.
func TestSweepForTheDispatcherSaysTakeAndOthersPropose(t *testing.T) {
	file := staleCrew(t, "")

	_, _, err := zm(t, file, "later", "sweep", "dispatcher")
	require.NoError(t, err)

	shown, _, err := zm(t, file, "task", "show", "T-1", "--full")
	require.NoError(t, err)
	assert.Contains(t, shown, "- real work, ready: zmina later take L-n <role> [--repo r --worktree]")
	assert.Contains(t, shown, "Related leftovers go in one task: take the main item")
	assert.NotContains(t, shown, "zmina note")
}

func TestLaterSweepTakesTheOldestFirstAndSaysWhatWaits(t *testing.T) {
	file := testCrew(t, "  dev:\n")

	for i := range 5 {
		parked(t, file, "item "+string(rune('a'+i)), time.Duration(50-i)*day, nil)
	}

	_, _, err := zm(t, file, "later", "sweep", "dev", "--max", "2")
	require.NoError(t, err)

	rec := readTaskFile(t, file, "T-1")
	assert.Equal(t, []any{"L-1", "L-2"}, rec["sweep"], "the oldest")
	assert.Contains(t, rec["asked"], "(the 2 oldest of 5; the rest wait for the next sweep)")

	_, _, err = zm(t, file, "later", "sweep", "dev", "--older", "60d")
	require.ErrorContains(t, err, "T-1 is an open sweep", "an open sweep is finished first")
}

func TestLaterSweepWithNothingStaleSaysSoAndMakesNothing(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	parked(t, file, "young idea", day, nil)

	out, errOut, err := zm(t, file, "later", "sweep", "dev")
	require.NoError(t, err, "exit 0")
	assert.Equal(t, "nothing to sweep: no parked item is older than 14d, not kept within 14d and not waiting on a day to come back\n",
		out)
	assert.Empty(t, errOut)

	out, _, err = zm(t, file, "later", "sweep", "dev", "--older", "1h", "--dry-run")
	require.NoError(t, err)
	assert.Contains(t, out, "would sweep 1 of 1 stale items")

	all, _, err := zm(t, file, "task", "ls", "--all")
	require.NoError(t, err)
	assert.NotContains(t, all, "T-1")
}

func TestLaterSweepRefusesWhatItCannotDo(t *testing.T) {
	file := staleCrew(t, "")

	_, _, err := zm(t, file, "later", "sweep", "ghost")
	require.Error(t, err, "a role the crew does not have")

	_, _, err = zm(t, file, "later", "sweep", "dev", "--older", "soon")
	require.ErrorContains(t, err, `--older "soon"`)

	_, _, err = zm(t, file, "later", "sweep", "dev", "--older", "0d")
	require.ErrorContains(t, err, "--older")

	for _, bad := range []string{"0", "26", "-3"} {
		_, _, err = zm(t, file, "later", "sweep", "dev", "--max", bad)
		require.ErrorContains(t, err, "--max is a number of items from 1 to 25", bad)
	}

	out, _, err := zm(t, file, "task", "ls", "--all")
	require.NoError(t, err)
	assert.NotContains(t, out, "T-1")
}

func TestOpenSweepRefusesASecondOneAndNamesIt(t *testing.T) {
	file := staleCrew(t, "")

	_, _, err := zm(t, file, "later", "sweep", "dev")
	require.NoError(t, err)

	for _, args := range [][]string{{"later", "sweep", "dev"}, {"later", "sweep", "dispatcher"}, {"later", "sweep", "dev", "--dry-run"}} {
		_, _, err = zm(t, file, args...)
		require.ErrorContains(t, err, "T-1 is an open sweep (2 of 2 parked items without a verdict): finish it first "+
			"(zmina later swept T-1)", args)
	}

	out, _, err := zm(t, file, "later", "ls")
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(out), "\n")
	assert.Equal(t, "sweep T-1 open: 2 of 2 without a verdict (zmina later swept T-1)", lines[len(lines)-1])

	_, _, err = zm(t, file, "later", "keep", "L-1", "--note", "still wanted")
	require.NoError(t, err)

	out, _, err = zm(t, file, "later", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "sweep T-1 open: 1 of 2 without a verdict")

	out, _, err = zm(t, file, "later", "ls", "--json")
	require.NoError(t, err)
	assert.NotContains(t, out, "sweep T-1", "JSON stays the items alone")
}

// Closing an item a sweep lists asks for the evidence; an item outside a sweep is closed as it always was.
func TestSweptItemsAreClosedOnEvidence(t *testing.T) {
	file := staleCrew(t, "")

	_, _, err := zm(t, file, "later", "sweep", "dev")
	require.NoError(t, err)

	_, _, err = zm(t, file, "later", "done", "L-1")
	require.ErrorContains(t, err, "L-1 is listed in the open sweep T-1: closing it as done needs --note, saying what shows it is "+
		"resolved (a commit, file:line, a task id, command output)")

	_, _, err = zm(t, file, "later", "drop", "2", "--note", " ")
	require.ErrorContains(t, err, "L-2 is listed in the open sweep T-1: closing it as dropped needs --note, saying why it is no "+
		"longer wanted")

	out, _, err := zm(t, file, "later", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "L-1", "a refused close leaves it on the list")
	assert.Contains(t, out, "L-2")

	// An item the sweep does not list is not held to it.
	out, _, err = zm(t, file, "later", "done", "L-3")
	require.NoError(t, err)
	assert.Equal(t, "L-3 · done\n", out)

	out, _, err = zm(t, file, "later", "done", "L-1", "--note", "fixed in commit 4f2a9c1")
	require.NoError(t, err)
	assert.Equal(t, "L-1 · done\n", out)

	// A closed item says it is closed, whatever the sweep.
	_, _, err = zm(t, file, "later", "done", "L-1")
	require.ErrorContains(t, err, "L-1 is already done")

	_, _, err = zm(t, file, "later", "drop", "L-2", "--note", "the product is gone")
	require.NoError(t, err)

	_, _, err = zm(t, file, "task", "done", "T-1")
	require.NoError(t, err)

	// The sweep is over: no evidence is asked of an item it listed or of any other.
	extra := parked(t, file, "another old one", 30*day, nil)

	_, _, err = zm(t, file, "later", "drop", extra)
	require.NoError(t, err, "no open sweep, no rule")
}

func TestLaterSweptListsWhatHasNoVerdictAndExitsOneUntilNone(t *testing.T) {
	file := staleCrew(t, "")

	_, _, err := zm(t, file, "later", "swept", "T-1")
	require.ErrorContains(t, err, "no task T-1")

	plain, _, err := zm(t, file, "task", "new", "dev", "an ordinary task")
	require.NoError(t, err)
	assert.Contains(t, plain, "T-1")

	_, _, err = zm(t, file, "later", "swept", "T-1")
	require.ErrorContains(t, err, "T-1 is no sweep task")

	_, _, err = zm(t, file, "later", "sweep", "dev")
	require.NoError(t, err)

	out, _, err := zm(t, file, "later", "swept", "T-2")
	require.Error(t, err)
	assert.Equal(t, 1, exitCode(err))
	assert.Empty(t, out)
	assert.Contains(t, err.Error(), "T-2: 2 of 2 parked items have no verdict: L-1, L-2")

	_, _, err = zm(t, file, "later", "done", "L-1", "--note", "fixed in commit 4f2a9c1")
	require.NoError(t, err)

	_, _, err = zm(t, file, "later", "swept", "2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1 of 2 parked items have no verdict: L-2")
	assert.NotContains(t, err.Error(), "L-1")

	_, _, err = zm(t, file, "later", "keep", "L-2", "--note", "waiting on the vendor")
	require.NoError(t, err)

	out, _, err = zm(t, file, "later", "swept", "2")
	require.NoError(t, err)
	assert.Equal(t, "T-2 · all 2 parked items have a verdict\n", out)
}

// The sweep has no worktree for a check to run in, so closing it as done asks zmina later swept itself.
func TestSweepTaskDoesNotCloseUntilSwept(t *testing.T) {
	file := staleCrew(t, "")

	_, _, err := zm(t, file, "later", "sweep", "dev")
	require.NoError(t, err)

	for _, args := range [][]string{{"task", "done", "T-1"}, {"task", "set", "T-1", "done"}} {
		_, _, err = zm(t, file, args...)
		require.ErrorContains(t, err, "T-1: has parked items without a verdict: 2 of 2 (L-1, L-2); give each a verdict, "+
			"zmina later swept T-1 says which are left", args)
	}

	out, _, err := zm(t, file, "task", "show", "T-1")
	require.NoError(t, err)
	assert.Contains(t, out, "T-1 · dev · open")

	_, _, err = zm(t, file, "later", "keep", "L-1", "--note", "still wanted")
	require.NoError(t, err)
	_, _, err = zm(t, file, "later", "keep", "L-2", "--note", "still wanted too")
	require.NoError(t, err)

	_, _, err = zm(t, file, "task", "done", "T-1")
	require.NoError(t, err)

	out, _, err = zm(t, file, "task", "show", "T-1")
	require.NoError(t, err)
	assert.Contains(t, out, "T-1 · dev · done")
	assert.NotContains(t, out, "unchecked:")
}

// The owner (and the dispatcher) may close a sweep early, and it is on the record; a worker may not.
func TestSweepTaskForcedClosedIsRecordedAsUnchecked(t *testing.T) {
	file := staleCrew(t, "")

	_, _, err := zm(t, file, "later", "sweep", "dev")
	require.NoError(t, err)

	t.Setenv("ZMINA_ROLE", "dev")

	_, _, err = zm(t, file, "task", "done", "T-1", "--force")
	require.Error(t, err)
	assert.ErrorContains(t, err, "only the owner or the dispatcher closes a task past its check")

	got, err := taskStore(mustCrew(t, file)).Get("T-1")
	require.NoError(t, err)
	assert.Equal(t, task.Open, got.State)

	t.Setenv("ZMINA_ROLE", "")

	_, _, err = zm(t, file, "task", "done", "T-1", "--force")
	require.NoError(t, err)

	out, _, err := zm(t, file, "task", "show", "T-1")
	require.NoError(t, err)
	assert.Contains(t, out, "unchecked: closed by hand past its check (zmina later swept T-1), which was not run")
}

// A crew whose routes say another seat, and which wants a reason for a hand pick, is not asked for one: the sweep's role is
// chosen by the owner or the crew file, and the pick records that.
func TestSweepIsNotHeldToRouteOverride(t *testing.T) {
	file := routeCrew(t, "route_override: required\n")
	routeLimits(t, file, 41, 50)

	parked(t, file, "old idea", 40*day, nil)

	// A plain task for dev2 is refused, as the routes say dev ...
	_, _, err := zm(t, file, "task", "new", "dev2", "x")
	require.ErrorContains(t, err, "needs --because")

	// ... a sweep for it is not.
	out, _, err := zm(t, file, "later", "sweep", "dev2")
	require.NoError(t, err)
	assert.Contains(t, out, "T-1 · dev2 · sweep of 1 parked item (L-1)")

	rec := readTaskFile(t, file, "T-1")
	route, ok := rec["route"].(map[string]any)
	require.True(t, ok, "the routes' answer is kept, as for any task")
	assert.Equal(t, "dev", route["said"])
	assert.Equal(t, sweepBecause, route["override"])
}
