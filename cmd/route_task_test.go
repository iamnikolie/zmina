package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/task"
)

// routeEvents are the route events of a crew's log, oldest first.
func routeEvents(t *testing.T, file string) []events.Event {
	t.Helper()

	c, err := crew.Load(file)
	require.NoError(t, err)

	all, err := eventLog(c).Read(events.Filter{})
	require.NoError(t, err)

	var out []events.Event

	for _, e := range all {
		if e.Event == "route" {
			out = append(out, e)
		}
	}

	return out
}

func readTaskFile(t *testing.T, file, id string) map[string]any {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "tasks", id+".json"))
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))

	return out
}

func TestTaskNewRouteLetsTheRoutesPickTheRole(t *testing.T) {
	file := routeCrew(t, "")
	routeLimits(t, file, 41, 50)

	out, errOut, err := zm(t, file, "task", "new", "--route", "--kind", "code", "fix the login form")
	require.NoError(t, err)

	assert.Equal(t, "T-1 · dev (rule 6: claude 7d 41% < codex week 50%)\n", out)
	assert.Contains(t, errOut, "dev is down: zmina --crew "+quoteCommandArg(file)+" up 'dev' --task T-1 starts it on the task")

	// The task is dev's, with the asked text whole: the flags are not part of it.
	shown, _, err := zm(t, file, "task", "show", "T-1")
	require.NoError(t, err)
	assert.Contains(t, shown, "T-1 · dev · open")
	assert.Contains(t, shown, "asked: fix the login form")
	assert.Contains(t, shown, "route: rule 6 pool by limits: claude 7d 41% < codex week 50%\n")
}

func TestTaskNewRouteIsLoggedAndKeptInTheRecord(t *testing.T) {
	file := routeCrew(t, "")
	routeLimits(t, file, 41, 50)

	_, _, err := zm(t, file, "task", "new", "--route", "--kind", "design", "a new logo")
	require.NoError(t, err)

	evs := routeEvents(t, file)
	require.Len(t, evs, 1)

	e := evs[0]
	assert.Equal(t, "T-1", e.Task)
	assert.Equal(t, "designer", e.Picked)
	assert.Equal(t, "designer", e.Role)
	assert.Equal(t, "designer", e.RouteSaid)
	require.NotNil(t, e.Rule)
	assert.Equal(t, 4, *e.Rule)
	assert.Empty(t, e.Because)
	assert.Equal(t, "hand", e.By)
	assert.Equal(t, "rule 4 kind: designer, as the routes say", e.Detail)

	rec := readTaskFile(t, file, "T-1")
	assert.Equal(t, map[string]any{"rule": 4.0, "name": "kind", "why": "kind design goes to designer"}, rec["route"])

	// zmina log shows it, one line like the others.
	log, _, err := zm(t, file, "log", "--json")
	require.NoError(t, err)
	assert.Contains(t, log, `"event": "route"`)
	assert.Contains(t, log, `"route_said": "designer"`)
	assert.Contains(t, log, `"picked": "designer"`)
}

func TestTaskNewByHandLogsWhereTheRoutesDisagree(t *testing.T) {
	file := routeCrew(t, "")
	routeLimits(t, file, 41, 50)

	// The pick is the routes': no extra words but the rule.
	out, _, err := zm(t, file, "task", "new", "dev", "--kind", "code", "agrees")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev (rule 6, as the routes say)\n", out)

	// Closed tasks hold no seat: the routes see the same crew each time.
	_, _, err = zm(t, file, "task", "drop", "T-1")
	require.NoError(t, err)

	// Another seat, with why.
	out, _, err = zm(t, file, "task", "new", "dev2", "--kind", "code", "--because", "alex named it", "disagrees")
	require.NoError(t, err)
	assert.Equal(t, "T-2 · dev2 (route said dev, rule 6; override: alex named it)\n", out)

	_, _, err = zm(t, file, "task", "drop", "T-2")
	require.NoError(t, err)

	// Another seat, no reason given: allowed in the shadow period.
	out, _, err = zm(t, file, "task", "new", "designer", "--kind", "code", "silent")
	require.NoError(t, err)
	assert.Equal(t, "T-3 · designer (route said dev, rule 6)\n", out)

	evs := routeEvents(t, file)
	require.Len(t, evs, 3, "one route event for every task made while the crew has routes")

	assert.Equal(t, "dev", evs[0].Picked)
	assert.Equal(t, "dev", evs[0].RouteSaid)

	assert.Equal(t, events.Event{TS: evs[1].TS, Role: "dev2", Event: "route", Task: "T-2", By: "hand", Picked: "dev2", RouteSaid: "dev",
		Rule: evs[1].Rule, Because: "alex named it", Detail: "route said dev (rule 6 pool by limits); picked dev2; because: alex named it"}, evs[1])
	assert.Equal(t, 6, *evs[1].Rule)

	assert.Equal(t, "designer", evs[2].Picked)
	assert.Equal(t, "dev", evs[2].RouteSaid)
	assert.Empty(t, evs[2].Because)

	// The record keeps what the routes said next to the pick.
	assert.Equal(t, map[string]any{"rule": 6.0, "name": "pool by limits", "why": "claude 7d 41% < codex week 50%"},
		readTaskFile(t, file, "T-1")["route"])
	assert.Equal(t, map[string]any{"rule": 6.0, "name": "pool by limits", "why": "claude 7d 41% < codex week 50%", "said": "dev",
		"override": "alex named it"}, readTaskFile(t, file, "T-2")["route"])
	assert.Equal(t, "dev", readTaskFile(t, file, "T-3")["route"].(map[string]any)["said"])
	assert.NotContains(t, readTaskFile(t, file, "T-3")["route"], "override")

	shown, _, err := zm(t, file, "task", "show", "T-2")
	require.NoError(t, err)
	assert.Contains(t, shown, "route: the routes said dev (rule 6 pool by limits: claude 7d 41% < codex week 50%); override: alex named it\n")

	shown, _, err = zm(t, file, "task", "show", "T-3")
	require.NoError(t, err)
	assert.Contains(t, shown, "picked by hand, no reason given\n")
}

func TestTaskNewByHandWhenNoRuleMatchedIsLoggedToo(t *testing.T) {
	file := testCrew(t, routeSeatsYAML+"routes:\n  - kind: {design: designer}\n")

	out, _, err := zm(t, file, "task", "new", "dev", "--kind", "film", "--because", "alex asked", "a loop")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev\n", out, "nothing to compare with: no extra words")

	evs := routeEvents(t, file)
	require.Len(t, evs, 1)
	assert.Equal(t, "dev", evs[0].Picked)
	assert.Empty(t, evs[0].RouteSaid)
	assert.Nil(t, evs[0].Rule)
	assert.Equal(t, "alex asked", evs[0].Because)
	assert.Equal(t, "no rule matched; picked dev; because: alex asked", evs[0].Detail)

	assert.NotContains(t, readTaskFile(t, file, "T-1"), "route", "no rule answered: nothing for the record")
}

func TestTaskNewWithoutRoutesIsAsItWas(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	out, _, err := zm(t, file, "task", "new", "dev", "--because", "no one asks", "Review")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev\n", out)
	assert.Empty(t, routeEvents(t, file), "no routes, no route event")
	assert.NotContains(t, readTaskFile(t, file, "T-1"), "route")

	_, _, err = zm(t, file, "task", "new", "--route", "Review")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the crew file has no routes: name the role")

	tasks, err := taskStoreOf(t, file).All()
	require.NoError(t, err)
	assert.Len(t, tasks, 1, "the refused task was not made")
}

func taskStoreOf(t *testing.T, file string) *task.Store {
	t.Helper()

	c, err := crew.Load(file)
	require.NoError(t, err)

	return taskStore(c)
}

func TestTaskNewRouteThatCannotPickMakesNoTask(t *testing.T) {
	file := testCrew(t, routeSeatsYAML+"routes:\n  - other_crew: {repo_under: acme-lg, tell: box/acme}\n  - kind: {design: designer}\n")
	repo := filepath.Join(filepath.Dir(file), "acme-lg", "shop")

	_, _, err := zm(t, file, "task", "new", "--route", "--kind", "film", "a loop")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no rule matched: kind film has no seat in routes (design): pick by hand (name the role: zmina task new <role> <asked...>)")

	_, _, err = zm(t, file, "task", "new", "--route", "--repo", repo, "add a settle button")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tell box/acme · rule 0 other crew: repo under acme-lg")
	assert.Contains(t, err.Error(), "another crew's work, not a seat of this one: zmina tell box/acme 'add a settle button'")

	_, _, err = zm(t, file, "task", "new", "--route", "--because", "x", "--kind", "design", "a logo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--because says why a role was picked by hand; with --route the routes pick it")

	_, _, err = zm(t, file, "task", "new", "--route")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--route picks the role")

	_, _, err = zm(t, file, "task", "new", "dev")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires at least 2 arg(s), only received 1")

	tasks, err := taskStoreOf(t, file).All()
	require.NoError(t, err)
	assert.Empty(t, tasks)
	assert.Empty(t, routeEvents(t, file), "nothing was made, nothing is logged")
}

func TestTaskNewRouteRequiredOverrideAsksWhyOnlyOfAPickAgainstTheRoutes(t *testing.T) {
	file := routeCrew(t, "route_override: required\n")
	routeLimits(t, file, 41, 50)

	_, _, err := zm(t, file, "task", "new", "dev2", "--kind", "code", "against the routes")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `the routes say dev (rule 6 pool by limits: claude 7d 41% < codex week 50%); dev2 instead needs --because "<why>" (route_override: required)`)

	tasks, err := taskStoreOf(t, file).All()
	require.NoError(t, err)
	assert.Empty(t, tasks)

	out, _, err := zm(t, file, "task", "new", "dev2", "--kind", "code", "--because", "alex named it", "against the routes")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev2 (route said dev, rule 6; override: alex named it)\n", out)

	// The routes' own pick, and the routes picking, need no reason.
	_, _, err = zm(t, file, "task", "new", "dev", "--kind", "code", "with the routes")
	require.NoError(t, err)
	_, _, err = zm(t, file, "task", "new", "--route", "--kind", "code", "by the routes")
	require.NoError(t, err)

	// No rule matched: nothing to disagree with.
	file = testCrew(t, routeSeatsYAML+"routes:\n  - kind: {design: designer}\nroute_override: required\n")
	_, _, err = zm(t, file, "task", "new", "dev", "--kind", "film", "no rule")
	require.NoError(t, err)
}

func TestTaskNewRouteCopyIsRecordedForTheCopyAndSaysHowToStartIt(t *testing.T) {
	file := routeCrew(t, "")
	routeLimits(t, file, 41, 50)

	_, _, err := zm(t, file, "task", "new", "dev", "the first piece")
	require.NoError(t, err)

	// dev holds T-1: the task that goes on from it gets a copy.
	out, errOut, err := zm(t, file, "task", "new", "--route", "--after", "T-1", "the second piece")
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(out, "T-2 · dev#2 (rule 2: goes on from T-1, done by dev; dev is busy: holds T-1 while down; a new copy dev#2 takes it)\n"), out)
	assert.Contains(t, errOut, "dev#2 is down: zmina --crew "+quoteCommandArg(file)+" up 'dev' --as 2 --task T-2 starts it on the task")
	assert.Equal(t, "dev#2", readTaskFile(t, file, "T-2")["role"])
}

func TestTaskNewByHandForACopyNamesHowToStartIt(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	_, errOut, err := zm(t, file, "task", "new", "dev#2", "Review")
	require.NoError(t, err)
	assert.Contains(t, errOut, "dev#2 is down: zmina --crew "+quoteCommandArg(file)+" up 'dev' --as 2 --task T-1 starts it on the task")
}

func TestTaskNewJSONCarriesTheRoute(t *testing.T) {
	file := routeCrew(t, "")
	routeLimits(t, file, 41, 50)

	out, _, err := zm(t, file, "task", "new", "--route", "--kind", "design", "--json", "a logo")
	require.NoError(t, err)

	var got task.Task
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.NotNil(t, got.Route)
	assert.Equal(t, task.Route{Rule: 4, Name: "kind", Why: "kind design goes to designer"}, *got.Route)
	assert.Equal(t, "designer", got.Role)
}

func TestTaskFilesWithoutARouteStillRead(t *testing.T) {
	file := routeCrew(t, "")
	dir := filepath.Join(filepath.Dir(file), "tasks")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	// A task as a zmina before routes wrote it.
	old := `{"id":"T-7","role":"dev","asked":"an old task","state":"open","by":"hand","created":"2026-10-01T10:00:00Z","updated":"2026-10-01T10:00:00Z"}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "T-7.json"), []byte(old), 0o600))

	// And one that a later zmina gave a field this one does not know.
	later := `{"id":"T-8","role":"dev","asked":"a later task","state":"open","created":"2026-10-01T10:00:00Z","updated":"2026-10-01T10:00:00Z",` +
		`"route":{"rule":6,"name":"pool by limits","why":"w","said":"dev2","override":"o","future":"x"}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "T-8.json"), []byte(later), 0o600))

	out, _, err := zm(t, file, "task", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "T-7")
	assert.Contains(t, out, "T-8")

	out, _, err = zm(t, file, "task", "show", "T-7")
	require.NoError(t, err)
	assert.Contains(t, out, "asked: an old task")
	assert.NotContains(t, out, "route:", "no route in the file, nothing shown")

	out, _, err = zm(t, file, "task", "show", "T-8")
	require.NoError(t, err)
	assert.Contains(t, out, "route: the routes said dev2 (rule 6 pool by limits: w); override: o\n")

	// A task changed after it was made keeps its route.
	_, _, err = zm(t, file, "task", "set", "T-8", "--note", "half done")
	require.NoError(t, err)
	assert.Contains(t, readTaskFile(t, file, "T-8")["route"], "override")

	// Numbering goes on above them.
	out, _, err = zm(t, file, "task", "new", "dev", "--kind", "code", "next")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "T-9 · dev"), out)
}

// A specialist named by hand with no kind of work said is not a pick against the pool: the pool shares work among
// its own seats and has no word on a seat outside it. Nothing is asked of it, even when overrides need a reason.
func TestTaskNewByHandOutsideThePoolIsNotHeldAgainstIt(t *testing.T) {
	file := routeCrew(t, "route_override: required\n")
	routeLimits(t, file, 41, 50)

	// A seat a kind rule names: the line says which rule would have answered had the kind been said.
	out, _, err := zm(t, file, "task", "new", "designer", "a new logo")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · designer (designer is rule 4 kind design's seat; the kind of work was not said, so the pool has no word on it)\n", out)
	assert.NotContains(t, readTaskFile(t, file, "T-1"), "route", "the routes had no word: nothing to keep")

	// The first review's seat, the same.
	out, _, err = zm(t, file, "task", "new", "reviewer", "look at it")
	require.NoError(t, err)
	assert.Equal(t, "T-2 · reviewer (reviewer is rule 5 review's seat; the kind of work was not said, so the pool has no word on it)\n", out)

	evs := routeEvents(t, file)
	require.Len(t, evs, 2)
	assert.Equal(t, "picked by hand: designer is rule 4 kind design's seat; the kind of work was not said, so the pool has no word on it", evs[0].Detail)
	assert.Nil(t, evs[0].Rule)
	assert.Empty(t, evs[0].RouteSaid)

	// A seat of the pool picked against it still needs its reason ...
	_, _, err = zm(t, file, "task", "new", "dev2", "x")
	require.ErrorContains(t, err, "needs --because")

	// ... and so does a specialist when the kind was said and is the pool's.
	_, _, err = zm(t, file, "task", "new", "designer", "--kind", "code", "x")
	require.ErrorContains(t, err, "needs --because")
}

// A seat no rule names at all, picked by hand: the same, said so.
func TestTaskNewByHandForASeatNoRuleNames(t *testing.T) {
	file := testCrew(t, routeSeatsYAML+"  scribe: {}\n"+routesYAML)
	routeLimits(t, file, 41, 50)

	out, _, err := zm(t, file, "task", "new", "scribe", "write it up")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · scribe (scribe is outside the pool and no rule names it; the pool has no word on it)\n", out)
}
