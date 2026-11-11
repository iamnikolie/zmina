package crew

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/route"
)

const routesRoles = "dir: /tmp\nroles:\n  dispatcher:\n  dev:\n  dev2:\n  codex-dev:\n    runtime: codex\n  designer:\n  writer:\n"

func TestNoRoutesKeyBehavesAsToday(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	c, err := Load(write(t, routesRoles))
	require.NoError(t, err)

	assert.Empty(t, c.RouteTable().Rules)
	assert.Empty(t, c.RouteTable().Busy)
	assert.False(t, c.RouteOverrideRequired())
	assert.Empty(t, c.Warnings)
}

func TestRoutesAreReadInOrder(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	dir := t.TempDir()
	repos := filepath.Join(dir, "acme-lg")
	require.NoError(t, os.MkdirAll(repos, 0o700))

	file := filepath.Join(dir, "crew.yaml")
	require.NoError(t, os.WriteFile(file, []byte(`routes:
  - other_crew: {repo_under: acme-lg, tell: box/acme}
  - other_crew: {repo_under: ~/work/erp, tell: box/erp}
  - continuation: {fresh_over: 70}
  - project: {app: dev, docs: writer}
  - kind: {design: designer, film: dev2}
  - review:
      first: codex-dev
      again: [dev, dev2]
  - pool: {seats: [dev, dev2, codex-dev], by: limits}
busy: pool
route_override: required
`+routesRoles), 0o600))

	c, err := Load(file)
	require.NoError(t, err)

	got := c.RouteTable()
	assert.Equal(t, route.PoolOnly, got.Busy)
	assert.True(t, c.RouteOverrideRequired())

	require.Len(t, got.Rules, 7)

	kinds := make([]route.Kind, len(got.Rules))
	for i, r := range got.Rules {
		kinds[i] = r.Kind
	}

	assert.Equal(t, []route.Kind{route.OtherCrew, route.OtherCrew, route.Continuation, route.Project, route.WorkKind, route.Review,
		route.Pool}, kinds, "the order of the file")

	realDir, err := filepath.EvalSymlinks(repos)
	require.NoError(t, err)

	assert.Equal(t, route.Rule{Kind: route.OtherCrew, Under: realDir, UnderText: "acme-lg", Tell: "box/acme"}, got.Rules[0],
		"a relative directory is relative to the crew file, as every path in it")
	assert.Equal(t, ResolvePath("", ExpandHome("~/work/erp")), got.Rules[1].Under, "a ~ is the home directory")
	assert.Equal(t, "~/work/erp", got.Rules[1].UnderText, "messages say it as the file wrote it")
	assert.Equal(t, 70, got.Rules[2].FreshOver)
	assert.Equal(t, map[string]string{"app": "dev", "docs": "writer"}, got.Rules[3].Owners)
	assert.Equal(t, map[string]string{"design": "designer", "film": "dev2"}, got.Rules[4].Owners)
	assert.Equal(t, "codex-dev", got.Rules[5].First)
	assert.Equal(t, []string{"dev", "dev2"}, got.Rules[5].Again)
	assert.Equal(t, []string{"dev", "dev2", "codex-dev"}, got.Rules[6].Seats)
	assert.Empty(t, c.Warnings)
}

func TestRouteDefaults(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	for _, in := range []string{"routes:\n  - continuation:\n", "routes:\n  - continuation: {}\n", "routes:\n  - continuation: ~\n"} {
		c, err := Load(write(t, in+routesRoles))
		require.NoError(t, err, in)
		assert.Equal(t, route.DefaultFreshOver, c.RouteTable().Rules[0].FreshOver, in)
	}

	c, err := Load(write(t, "routes:\n  - pool: {seats: [dev]}\n"+routesRoles))
	require.NoError(t, err)
	assert.Empty(t, c.RouteTable().Busy, "no busy: key is copy-or-pool, said by the empty value")

	c, err = Load(write(t, "busy: copy-or-pool\nroute_override: optional\n"+routesRoles))
	require.NoError(t, err)
	assert.Equal(t, route.CopyOrPool, c.RouteTable().Busy)
	assert.False(t, c.RouteOverrideRequired())
}

func TestRoutesAreChecked(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"an unknown rule kind", "routes:\n  - owner: {app: dev}\n", `routes: rule 1 (line 2): unknown rule kind "owner": one of other_crew, continuation, project, kind, review, pool`},
		{"the second rule is named", "routes:\n  - pool: {seats: [dev]}\n  - kinds: {x: dev}\n", "rule 2 (line 3): unknown rule kind \"kinds\""},
		{"an unknown role in project", "routes:\n  - project: {app: ghost}\n", `project.app: no role "ghost" in the crew (roles: dispatcher, codex-dev, designer, dev, dev2, writer)`},
		{"an unknown role in kind", "routes:\n  - kind: {film: ghost}\n", `kind.film: no role "ghost" in the crew`},
		{"an unknown role as first reviewer", "routes:\n  - review: {first: ghost}\n", `review.first: no role "ghost" in the crew`},
		{"an unknown role in again", "routes:\n  - review: {again: [dev, ghost]}\n", `review.again: no role "ghost" in the crew`},
		{"an unknown role in the pool", "routes:\n  - pool: {seats: [dev, ghost]}\n", `pool.seats: no role "ghost" in the crew`},
		{"an empty pool", "routes:\n  - pool: {seats: []}\n", "pool.seats is empty: a pool needs at least one seat"},
		{"a pool without seats", "routes:\n  - pool: {by: limits}\n", "pool: names no seats"},
		{"a pool with nothing", "routes:\n  - pool:\n", "pool: names no seats"},
		{"a pool that lists a seat twice", "routes:\n  - pool: {seats: [dev, dev]}\n", "pool.seats: dev is listed twice"},
		{"a pool by something else", "routes:\n  - pool: {seats: [dev], by: luck}\n", `pool.by is limits, not "luck"`},
		{"a copy is no seat of a rule", "routes:\n  - pool: {seats: ['dev#2']}\n", `pool.seats: "dev#2" is a copy: name the role (dev)`},
		{"a review with no seat", "routes:\n  - review: {}\n", "review: names no seat"},
		{"a rule with two keys", "routes:\n  - project: {app: dev}\n    kind: {film: dev}\n", "a rule has one key, its kind, not 2 (project, kind)"},
		{"a rule that is a word", "routes:\n  - pool\n", "a rule is one kind and its value"},
		{"a typo inside a rule", "routes:\n  - pool: {seat: [dev]}\n", `pool: unknown key "seat" (seats, by)`},
		{"a typo in other_crew", "routes:\n  - other_crew: {repo: /x, tell: box/acme}\n", `other_crew: unknown key "repo" (repo_under, tell)`},
		{"other_crew without a repo", "routes:\n  - other_crew: {tell: box/acme}\n", "other_crew.repo_under is missing"},
		{"other_crew without an address", "routes:\n  - other_crew: {repo_under: /x}\n", "other_crew.tell is missing"},
		{"a tell that is no address", "routes:\n  - other_crew: {repo_under: /x, tell: acme}\n", `other_crew.tell "acme": want <node>/<crew>`},
		{"a tell that is a path", "routes:\n  - other_crew: {repo_under: /x, tell: a/b/c}\n", "want <node>/<crew>"},
		{"a share of zero", "routes:\n  - continuation: {fresh_over: 0}\n", `continuation.fresh_over is a share of the seat's context, 1 to 100, not "0"`},
		{"a share over 100", "routes:\n  - continuation: {fresh_over: 101}\n", "1 to 100"},
		{"a share that is a word", "routes:\n  - continuation: {fresh_over: much}\n", "1 to 100"},
		{"no projects", "routes:\n  - project: {}\n", "project: a mapping of names to seats"},
		{"a kind with a list", "routes:\n  - kind: {film: [dev]}\n", "kind.film is a seat"},
		{"a project named twice", "routes:\n  - project: {app: dev, app: dev2}\n", "project.app: named twice"},
		{"routes that are not a list", "routes: dev\n", "cannot unmarshal"},
		{"a busy policy that is none", "busy: queue\n", `busy is copy-or-pool or pool, not "queue"`},
		{"a route_override that is none", "route_override: maybe\n", `route_override is optional or required, not "maybe"`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(write(t, c.yaml+routesRoles))
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

func TestRoutesNameSeatsOfTheCrewSoTheCrewCannotLoseThem(t *testing.T) {
	// A crew file edited so a rule names a seat that is gone does not load: zmina seat rm saves only what loads.
	_, err := Load(write(t, "routes:\n  - project: {app: dev2}\n"+"dir: /tmp\nroles:\n  dispatcher:\n  dev:\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `project.app: no role "dev2" in the crew (roles: dispatcher, dev)`)
}

func TestARoleKnowsHowMuchContextIsFull(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	c, err := Load(write(t, routesRoles+"    context_max: 150k\n"))
	require.NoError(t, err)

	// context_max is the role's own limit; without one the context zmina restarts it at is the measure.
	assert.Equal(t, 250_000, c.Roles["writer"].restartAt)
	assert.Equal(t, 150_000, c.Roles["writer"].ContextBudget())
	assert.Equal(t, 250_000, c.Roles["dev"].ContextBudget())
	assert.Equal(t, 150_000, c.Roles["writer"].Instance(2).ContextBudget())

	c, err = Load(write(t, routesRoles+"    restart_at: off\n"))
	require.NoError(t, err)
	assert.Zero(t, c.Roles["writer"].ContextBudget(), "no limit, no share")
}

func TestResolvePathFollowsLinksAsFarAsThePathIsThere(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	require.NoError(t, os.MkdirAll(real, 0o700))
	require.NoError(t, os.Symlink(real, filepath.Join(root, "link")))

	want, err := filepath.EvalSymlinks(real)
	require.NoError(t, err)

	assert.Equal(t, want, ResolvePath("", filepath.Join(root, "link")))
	assert.Equal(t, filepath.Join(want, "a", "b"), ResolvePath("", filepath.Join(root, "link", "a", "b")), "a path not there yet is below the link's target")
	assert.Equal(t, filepath.Join(want, "x"), ResolvePath(filepath.Join(root, "link"), "x"), "relative to the crew file's directory")
	home, err := filepath.EvalSymlinks(ExpandHome("~"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "x"), ResolvePath("/nowhere", "~/x"))
	assert.Equal(t, "/nowhere/at/all", ResolvePath("", "/nowhere/./at//all"))
}
