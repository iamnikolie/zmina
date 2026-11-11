package cmd

import (
	"encoding/json"
	"errors"
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

// routeSeatsYAML are the roles of the crews these tests route over; every one is down, so nothing starts.
const routeSeatsYAML = "  dev: {}\n  dev2: {}\n  codex-dev:\n    runtime: codex\n  reviewer:\n    runtime: codex\n  designer: {}\n"

// routesYAML is a table of every rule kind. acme-lg is a directory next to the crew file.
const routesYAML = `routes:
  - other_crew: {repo_under: acme-lg, tell: box/acme}
  - continuation: {fresh_over: 80}
  - project: {docs: designer}
  - kind: {design: designer}
  - review: {first: reviewer, again: [codex-dev, dev, dev2]}
  - pool: {seats: [dev, dev2, codex-dev]}
`

func routeCrew(t *testing.T, extra string) string {
	t.Helper()

	return testCrew(t, routeSeatsYAML+routesYAML+extra)
}

// routeLimits gives the crew the plans' readings: Claude's 7 days and Codex's week, in percent.
func routeLimits(t *testing.T, file string, claudeUsed, codexUsed float64) {
	t.Helper()

	c, err := crew.Load(file)
	require.NoError(t, err)

	now := time.Now()

	_, err = limits.Tap(c.Layout().Limits(), []byte(fmt.Sprintf(`{"rate_limits":{"seven_day":{"used_percentage":%v,"resets_at":%d}}}`,
		claudeUsed, now.Add(72*time.Hour).Unix())), now.Add(-2*time.Minute))
	require.NoError(t, err)

	limitsRollout(t, filepath.Join(os.Getenv("HOME"), ".codex"), now.Add(-time.Hour), codexUsed, now.Add(48*time.Hour))
}

func exitCode(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}

	if err != nil {
		return 1
	}

	return 0
}

func TestRouteSaysTheSeatWhyAndTheCommandsThatCarryItOut(t *testing.T) {
	file := routeCrew(t, "")
	routeLimits(t, file, 41, 50)

	repo := filepath.Join(filepath.Dir(file), "site")

	out, errOut, err := zm(t, file, "route", "--kind", "code", "--repo", repo, "fix the login form")
	require.NoError(t, err)
	assert.Empty(t, errOut)

	crewArg := " --crew " + file

	assert.Equal(t, "dev · rule 6 pool by limits: claude 7d 41% < codex week 50%\n"+
		"  zmina"+crewArg+" task new dev --repo "+repo+" --worktree --title '…' 'fix the login form'\n"+
		"  dev is down: then  zmina"+crewArg+" up dev --task T-<n>\n", out)
}

func TestRoutePicksTheOtherPlanWhenItIsEmptier(t *testing.T) {
	file := routeCrew(t, "")
	routeLimits(t, file, 60, 20)

	out, _, err := zm(t, file, "route", "--kind", "code")
	require.NoError(t, err)

	assert.Contains(t, out, "codex-dev · rule 6 pool by limits: codex week 20% < claude 7d 60%\n")
	assert.Contains(t, out, " task new codex-dev --title '…' '…'\n", "no repo, no asked text: nothing but placeholders")
}

func TestRouteWithLimitsUnknownStillAnswersAndSaysSo(t *testing.T) {
	file := routeCrew(t, "")

	out, _, err := zm(t, file, "route", "--kind", "code")
	require.NoError(t, err)
	assert.Contains(t, out, "dev · rule 6 pool by limits: limits unknown: fewest open tasks\n")

	out, _, err = zm(t, file, "route", "--kind", "code", "--json")
	require.NoError(t, err)

	var got struct {
		To     string
		Action string
		Stale  bool
		Limits map[string]any
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.True(t, got.Stale)
	assert.Equal(t, map[string]any{"claude": nil, "codex": nil}, got.Limits)
}

func TestRouteJSONHasTheDecision(t *testing.T) {
	file := routeCrew(t, "")
	routeLimits(t, file, 41, 50)

	out, _, err := zm(t, file, "route", "--kind", "code", "--json")
	require.NoError(t, err)

	var got struct {
		To           string
		Action       string
		Rule         *int
		Name         string
		Why          string
		Alternatives []struct{ To, Why string }
		Limits       struct{ Claude, Codex *int }
		Stale        bool
		Explain      []any
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))

	assert.Equal(t, "dev", got.To)
	assert.Equal(t, "up+task", got.Action, "dev is down")
	require.NotNil(t, got.Rule)
	assert.Equal(t, 6, *got.Rule)
	assert.Equal(t, "pool by limits", got.Name)
	assert.Equal(t, "claude 7d 41% < codex week 50%", got.Why)
	assert.Contains(t, out, `"why": "claude 7d 41% < codex week 50%"`, "a < is printed as it is, not as \\u003c")
	assert.Equal(t, []struct{ To, Why string }{{"dev2", "down"}, {"codex-dev", "codex week 50%"}}, got.Alternatives)
	require.NotNil(t, got.Limits.Claude)
	assert.Equal(t, 41, *got.Limits.Claude)
	assert.Equal(t, 50, *got.Limits.Codex)
	assert.False(t, got.Stale)
	assert.Empty(t, got.Explain, "the rules asked are for --explain")

	out, _, err = zm(t, file, "route", "--kind", "code", "--json", "--explain")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Len(t, got.Explain, 6, "every rule asked, up to the one that answered")
}

func TestRouteExplainListsTheRulesAsked(t *testing.T) {
	file := routeCrew(t, "")
	routeLimits(t, file, 41, 50)

	out, _, err := zm(t, file, "route", "--explain", "--kind", "code", "--repo", filepath.Join(filepath.Dir(file), "site"))
	require.NoError(t, err)

	rows, answer, _ := strings.Cut(out, "\n\n")
	assert.Equal(t, `0 other crew       no    repo not under acme-lg
2 continuation     no    no --after
3 project          no    project site has no owner
4 kind             no    kind code has no seat in routes (design)
5 review           no    not a review
6 pool by limits   yes   dev (claude 7d 41% < codex week 50%)`, rows)
	assert.Contains(t, answer, "dev · rule 6 pool by limits")
	assert.Contains(t, answer, "task new dev --repo ")
}

func TestRouteTellsAnotherCrew(t *testing.T) {
	file := routeCrew(t, "")
	repo := filepath.Join(filepath.Dir(file), "acme-lg", "shop")

	out, _, err := zm(t, file, "route", "--repo", repo, "add a settle button")
	require.NoError(t, err)

	assert.Equal(t, "tell box/acme · rule 0 other crew: repo under acme-lg\n"+
		"  zmina --crew "+file+" tell box/acme 'add a settle button'\n", out)

	out, _, err = zm(t, file, "route", "--repo", repo, "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"action": "tell"`)
	assert.Contains(t, out, `"rule": 0`)
}

func TestRouteNoRuleMatchedExitsTwo(t *testing.T) {
	file := testCrew(t, routeSeatsYAML+"routes:\n  - kind: {design: designer, film: reviewer}\n")

	out, errOut, err := zm(t, file, "route", "--kind", "movie")
	require.Error(t, err)
	assert.Equal(t, 2, exitCode(err))
	assert.Equal(t, "no rule matched: kind movie has no seat in routes (design, film): pick by hand\n", out)
	assert.Empty(t, errOut, "the answer is the output, not an error")

	out, _, err = zm(t, file, "route", "--kind", "movie", "--json")
	assert.Equal(t, 2, exitCode(err))

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, "none", got["action"])
	assert.Nil(t, got["rule"])
	assert.Equal(t, "", got["to"])

	out, _, err = zm(t, file, "route", "--explain", "--kind", "movie")
	assert.Equal(t, 2, exitCode(err))
	assert.Contains(t, out, "4 kind   ")
	assert.Contains(t, out, "no rule matched:")
}

func TestRouteWithoutRoutesInTheCrewFileExitsTwo(t *testing.T) {
	file := testCrew(t, routeSeatsYAML)

	out, _, err := zm(t, file, "route", "--kind", "code")
	assert.Equal(t, 2, exitCode(err))
	assert.Equal(t, "no routes in the crew file\n", out)

	out, _, err = zm(t, file, "route", "--json")
	assert.Equal(t, 2, exitCode(err))
	assert.Contains(t, out, `"action": "none"`)
	assert.Contains(t, out, `"why": "no routes in the crew file"`)
}

func TestRouteFollowsATaskAndNeverReviewsItsOwn(t *testing.T) {
	file := routeCrew(t, "")
	routeLimits(t, file, 41, 50)

	_, _, err := zm(t, file, "task", "new", "dev", "the login form")
	require.NoError(t, err)

	// T-1 is held by dev (down, but it holds the task): the continuation finds the seat busy and a copy takes it.
	out, _, err := zm(t, file, "route", "--after", "T-1", "--repo", filepath.Join(filepath.Dir(file), "site"), "more of it")
	require.NoError(t, err)

	crewArg := " --crew " + file
	assert.Equal(t, "dev#2 · rule 2 continuation: goes on from T-1, done by dev; dev is busy: holds T-1 while down; a new copy dev#2 takes it\n"+
		"  zmina"+crewArg+" task new 'dev#2' --repo "+filepath.Join(filepath.Dir(file), "site")+" --title '…' 'more of it'\n"+
		"  dev#2 is a new copy: then  zmina"+crewArg+" up dev --as 2 --task T-<n>\n", out, "a task that goes on gets no worktree of its own")

	out, _, err = zm(t, file, "route", "--kind", "review", "--of", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "reviewer · rule 5 review: T-1 by dev (claude), first review\n")

	out, _, err = zm(t, file, "route", "--kind", "review", "--of", "T-1", "--rereview")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "codex-dev · rule 5 review: re-review, T-1 by dev (claude); codex-dev down, not the author\n"), out+": the author never reviews")
}

func TestRouteChecksItsFlags(t *testing.T) {
	file := routeCrew(t, "")

	cases := []struct {
		want string
		args []string
	}{
		{"--rereview is for --kind review", []string{"route", "--rereview"}},
		{"--rereview is for --kind review", []string{"route", "--kind", "code", "--rereview"}},
		{"--of is for --kind review", []string{"route", "--of", "T-1"}},
		{"--kind review needs --of T-n", []string{"route", "--kind", "review"}},
		{"--after: no task T-9", []string{"route", "--after", "T-9"}},
		{"--of: no task T-8", []string{"route", "--kind", "review", "--of", "8"}},
		{"unknown flag: --role", []string{"route", "--role", "dev"}},
	}

	for _, c := range cases {
		_, errOut, err := zm(t, file, c.args...)
		require.Error(t, err, c.args)
		assert.Equal(t, 1, exitCode(err), c.args)
		assert.Contains(t, err.Error()+errOut, c.want, c.args)
	}
}

func TestRouteChangesNothingAndTakesNoLock(t *testing.T) {
	file := routeCrew(t, "")
	routeLimits(t, file, 41, 50)

	_, _, err := zm(t, file, "task", "new", "dev", "a task")
	require.NoError(t, err)

	dir := filepath.Dir(file)
	before := snapshotDir(t, dir)

	// Another zmina holds the state lock for the whole run: a route that wanted it would wait for it.
	l, err := shift.Lock(dir)
	require.NoError(t, err)

	defer l.Release()

	done := make(chan error, 1)

	go func() {
		_, _, err := zm(t, file, "route", "--kind", "code", "--explain")
		done <- err
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("zmina route waited for the state lock")
	}

	l.Release()
	assert.Equal(t, before, snapshotDir(t, dir), "no file written, none changed")
}

// snapshotDir is every file of a crew directory (not its tmux or lock files) with its content.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()

	out := map[string]string{}

	require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == "lock" {
			return err
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		out[path] = string(raw)

		return nil
	}))

	return out
}

func TestRouteIsListedForTheDispatcher(t *testing.T) {
	out, _, err := zm(t, testCrew(t, ""), "--help")
	require.NoError(t, err)

	_, dispatcher, _ := strings.Cut(out, "For the dispatcher:")
	dispatcher, _, _ = strings.Cut(dispatcher, "Plumbing")
	assert.Contains(t, dispatcher, "route")
}

func TestASeatARouteNamesCannotBeRemoved(t *testing.T) {
	file := routeCrew(t, "")

	_, _, err := zm(t, file, "seat", "rm", "designer")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not saved, the crew would not load")
	assert.Contains(t, err.Error(), `no role "designer" in the crew`)

	raw, rerr := os.ReadFile(file)
	require.NoError(t, rerr)
	assert.Contains(t, string(raw), "designer", "the crew file is as it was")

	_, _, err = zm(t, file, "seat", "rm", "dev")
	assert.Error(t, err, "dev is in the pool, the review and the table's other rules")
}

func TestSeatFromViewReadsHowASeatStands(t *testing.T) {
	now := time.Date(2030, 1, 8, 20, 58, 0, 0, time.UTC)

	cases := []struct {
		name  string
		view  shift.View
		state string
		note  string
	}{
		{"up and idle", shift.View{State: shift.PresenceUp, Status: "idle"}, "idle", ""},
		{"up with no status read", shift.View{State: shift.PresenceUp, Status: "unknown"}, "idle", ""},
		{"up and busy", shift.View{State: shift.PresenceUp, Status: "busy"}, "busy", "busy"},
		{"up, idle, holding a task", shift.View{State: shift.PresenceUp, Status: "idle", Tasks: []string{"T-12"}}, "busy", "busy on T-12"},
		{"up, holding two", shift.View{State: shift.PresenceUp, Status: "idle", Tasks: []string{"T-12", "T-14"}}, "busy", "busy on T-12, T-14"},
		{"up at a dialog", shift.View{State: shift.PresenceUp, Status: "waiting", Prompt: true}, "busy", "busy: a dialog waits on its screen"},
		{"down", shift.View{State: shift.PresenceDown}, "down", ""},
		{"ended, collected soon", shift.View{State: shift.PresenceDead}, "down", ""},
		{"down holding a task", shift.View{State: shift.PresenceDown, Tasks: []string{"T-9"}}, "busy", "busy: holds T-9 while down"},
		{"down, its directory gone", shift.View{State: shift.PresenceDown, Problem: "role dev: dir /x is not a directory"}, "unavailable",
			"role dev: dir /x is not a directory"},
		{"asleep", shift.View{State: shift.PresenceDown, Asleep: now.Add(12 * time.Hour)}, "unavailable", "asleep until Wed 08:58"},
		{"broken", shift.View{State: shift.PresenceDown, Broken: true}, "unavailable", "broken: it dies on boot (zmina ls)"},
		{"outside zmina", shift.View{State: shift.PresenceOutside}, "unavailable", "running outside zmina"},
		{"tmux not answering", shift.View{State: shift.PresenceUnknown, Problem: "no server"}, "unavailable", "not readable: no server"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.view.Role, c.view.Context = "dev", 40_000
			s := seatFromView(c.view, 100_000, now)

			assert.Equal(t, c.state, string(s.State))
			assert.Equal(t, c.note, s.Note)
			assert.Equal(t, "claude", s.Runtime, "a view without a runtime is a Claude seat")
			assert.Equal(t, 40, mustShare(t, s))
		})
	}

	s := seatFromView(shift.View{Role: "codex-dev", Runtime: "codex", State: shift.PresenceUp, Status: "idle"}, 0, now)
	assert.Equal(t, "codex", s.Runtime)
	_, known := s.ContextShare()
	assert.False(t, known, "no context read, no share")
}

func mustShare(t *testing.T, s interface{ ContextShare() (int, bool) }) int {
	t.Helper()

	n, ok := s.ContextShare()
	require.True(t, ok)

	return n
}

func TestShellWordQuotesWhatAShellWouldRead(t *testing.T) {
	assert.Equal(t, "dev", shellWord("dev"))
	assert.Equal(t, "/tmp/a-b_c.d/e", shellWord("/tmp/a-b_c.d/e"))
	assert.Equal(t, "'dev#2'", shellWord("dev#2"), "# and ~ are plain words in some shells and not in others")
	assert.Equal(t, "'~/work'", shellWord("~/work"))
	assert.Equal(t, `'a b'`, shellWord("a b"))
	assert.Equal(t, `'it'"'"'s'`, shellWord("it's"))
}
