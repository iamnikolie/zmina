package lesson

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitT runs git in dir for a test.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()

	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)

	return strings.TrimSpace(string(out))
}

// repoFixture is an origin (bare) and the crew's checkout of it, with CLAUDE.md and agents/dev/AGENT.md on main,
// and one commit in the checkout that was never pushed.
func repoFixture(t *testing.T) (origin, checkout string) {
	t.Helper()

	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "alex")
	t.Setenv("GIT_AUTHOR_EMAIL", "alex@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "alex")
	t.Setenv("GIT_COMMITTER_EMAIL", "alex@example.com")

	root := t.TempDir()
	origin, checkout = filepath.Join(root, "origin.git"), filepath.Join(root, "os")

	gitT(t, root, "init", "-q", "--bare", "-b", "main", origin)
	gitT(t, root, "clone", "-q", origin, checkout)
	gitT(t, checkout, "checkout", "-q", "-b", "main")

	require.NoError(t, os.MkdirAll(filepath.Join(checkout, "agents", "dev"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "CLAUDE.md"), []byte("# Rules\n\n- Reply short.\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "agents", "dev", "AGENT.md"),
		[]byte("# dev\n\n- Run make test before a commit.\n- Use the worktree of the task.\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "Makefile"), []byte("all:\n"), 0o644))
	gitT(t, checkout, "add", ".")
	gitT(t, checkout, "commit", "-q", "-m", "start")
	gitT(t, checkout, "push", "-q", "origin", "main")
	gitT(t, checkout, "remote", "set-head", "origin", "main")

	require.NoError(t, os.WriteFile(filepath.Join(checkout, "LOCAL.md"), []byte("not pushed\n"), 0o644))
	gitT(t, checkout, "add", ".")
	gitT(t, checkout, "commit", "-q", "-m", "local work the owner has not pushed")

	return origin, checkout
}

// fakeSeat answers the sorting prompt with plan and the checker's with checks, each at a cost.
func fakeSeat(t *testing.T, plan Plan, checks []Check, calls *[]string) Seat {
	t.Helper()

	return func(_ context.Context, dir, prompt, schema string, budget float64) ([]byte, error) {
		*calls = append(*calls, schema[:20]+" "+dir)

		var structured any = plan
		if schema == CheckSchema {
			structured = map[string]any{"checks": checks}
		}

		raw, err := json.Marshal(structured)
		require.NoError(t, err)

		return json.Marshal(map[string]any{"type": "result", "subtype": "success", "total_cost_usd": budget / 4,
			"structured_output": json.RawMessage(raw)})
	}
}

func lessonsFixture() []Lesson {
	return []Lesson{
		{ID: "LS-1", Rule: "Say which file a result is in", Why: "the owner looked twice", Applies: Every, Role: "dev"},
		{ID: "LS-2", Rule: "Run make lint too", Why: "CI failed on lint", Applies: Role, Role: "dev", Task: "T-1"},
		{ID: "LS-3", Rule: "Reply short", Why: "said already", Applies: Every, Role: "dev"},
		{ID: "LS-4", Rule: "Push without review", Why: "faster", Applies: Every, Role: "dev"},
		{ID: "LS-5", Rule: "Never commit to main", Why: "a hook", Applies: Role, Role: "dev"},
	}
}

func TestTriageCommitsCheckedEditsOnABranchFromOrigin(t *testing.T) {
	origin, checkout := repoFixture(t)

	plan := Plan{Items: []Item{
		{ID: "LS-1", Verdict: VerdictEvery, File: "CLAUDE.md", New: "- Say which file a result is in.", Reason: "all seats"},
		{ID: "LS-2", Verdict: VerdictRole, File: "agents/dev/AGENT.md", Old: "- Run make test before a commit.",
			New: "- Run make test and make lint before a commit.", Reason: "dev only"},
		{ID: "ls-3", Verdict: VerdictWritten, Reason: "CLAUDE.md: Reply short."},
		{ID: "LS-4", Verdict: VerdictConflict, Reason: "against review", Question: "Allow pushes without review?"},
		{ID: "LS-5", Verdict: VerdictRole, File: "agents/dev/AGENT.md", Old: "- Use the worktree of the task.",
			New: "- Use the worktree of the task; never commit to main.", Reason: "dev only"},
	}, Questions: []string{"Should lessons name their task?"}}
	checks := []Check{{ID: "LS-1", OK: true, Reason: "fine"},
		{ID: "LS-2", OK: false, Problem: "duplicate", Reason: "CLAUDE.md already asks for lint"}}

	var calls []string

	cfg := Config{Repo: checkout, Branch: "main", Agents: filepath.Join(checkout, "agents"), Roles: []string{"dev"},
		Work: filepath.Join(t.TempDir(), "work"), Budget: 8, Now: time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)}

	res, err := Triage(context.Background(), cfg, lessonsFixture(), fakeSeat(t, plan, checks, &calls))
	require.NoError(t, err)
	require.Len(t, calls, 2, "the sorting seat, then the checker")

	assert.Equal(t, "lessons/2026-11-09", res.Branch)
	assert.Equal(t, gitT(t, checkout, "rev-parse", "origin/main"), res.Base, "cut from origin, not the checkout's main")
	require.Len(t, res.Applied, 1, "LS-1 only: LS-2 held back by the checker, LS-5 never checked")
	assert.Equal(t, "LS-1", res.Applied[0].ID)
	require.Len(t, res.Dropped, 1)
	assert.Equal(t, "LS-3", res.Dropped[0].ID)
	assert.InDelta(t, 8*0.6/4+8*0.4/4, res.Cost, 0.001, "both passes' cost, each within its share")

	q := strings.Join(res.Questions, "\n")
	assert.Contains(t, q, "Should lessons name their task?")
	assert.Contains(t, q, "LS-2 (Run make lint too): edit to agents/dev/AGENT.md held back, the checker: duplicate")
	assert.Contains(t, q, "LS-4 (Push without review): Allow pushes without review?")
	assert.Contains(t, q, "LS-5 (Never commit to main): edit to agents/dev/AGENT.md held back, the checker did not look")

	msg := gitT(t, checkout, "log", "-1", "--format=%B", res.Branch)
	assert.Contains(t, msg, "Say which file a result is in")
	assert.Contains(t, msg, "Lesson: LS-1")
	assert.NotContains(t, gitT(t, checkout, "show", res.Branch+":CLAUDE.md"), "not pushed")
	_, err = os.Stat(filepath.Join(cfg.Work, "lessons-2026-11-09"))
	assert.True(t, os.IsNotExist(err), "the run's worktree is removed")

	// The push carries the branch and nothing of the checkout's own unpushed work. A plain bare remote takes no
	// push options: the branch goes anyway and the result says so.
	require.NoError(t, Push(cfg, &res, "Lessons", "one line"))
	assert.True(t, res.Pushed)
	assert.NotEmpty(t, res.PushNote)
	assert.Equal(t, res.Tip, gitT(t, origin, "rev-parse", "refs/heads/lessons/2026-11-09"))
	assert.Equal(t, "1", gitT(t, origin, "rev-list", "--count", "main..lessons/2026-11-09"))
	assert.Equal(t, "1", gitT(t, origin, "rev-list", "--count", "main"), "origin's main untouched")
	assert.Empty(t, gitT(t, origin, "log", "--all", "--format=%s", "--grep", "local work"), "local work never pushed")

	// The next run the same day gets a branch of its own.
	res2, err := Triage(context.Background(), cfg, lessonsFixture()[:1], fakeSeat(t, Plan{Items: plan.Items[:1]},
		checks[:1], &calls))
	require.NoError(t, err)
	assert.Equal(t, "lessons/2026-11-09-2", res2.Branch)
}

func TestPushRefusesAnythingButTheRunsOwnCommits(t *testing.T) {
	_, checkout := repoFixture(t)
	cfg := Config{Repo: checkout, Branch: "main"}
	base := gitT(t, checkout, "rev-parse", "origin/main")

	assert.Error(t, Push(cfg, &Result{Branch: "main", Base: base, Applied: []Applied{{ID: "LS-1"}}}, "t", "d"),
		"only lessons/ branches")
	assert.Error(t, Push(cfg, &Result{Branch: "lessons/x", Base: base}, "t", "d"), "nothing committed")

	// A lessons branch that also carries the checkout's unpushed commit is refused.
	gitT(t, checkout, "branch", "lessons/x", "main")
	tip := gitT(t, checkout, "rev-parse", "lessons/x")
	err := Push(cfg, &Result{Branch: "lessons/x", Base: base, Applied: []Applied{{ID: "LS-1", Commit: tip}}}, "t", "d")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not exactly the lessons this run made")
}

func TestApplyChecks(t *testing.T) {
	_, checkout := repoFixture(t)
	gitT(t, checkout, "checkout", "-q", "-b", "lessons/t")

	allowed := allowlist(Config{Repo: checkout, Agents: filepath.Join(checkout, "agents")})
	l := Lesson{ID: "LS-1", Rule: "r", Why: "w"}

	require.NoError(t, os.Symlink(filepath.Join(checkout, "CLAUDE.md"), filepath.Join(checkout, "agents", "dev", "AGENTS.md")))

	key := "AKIA" + strings.Repeat("Q", 16)
	for name, c := range map[string]struct {
		it   Item
		want string
	}{
		"outside the allowlist": {Item{File: "Makefile", New: "x"}, "not an instruction file"},
		"up and out":            {Item{File: "../os/CLAUDE.md", New: "x"}, "not an instruction file"},
		"absolute":              {Item{File: filepath.Join(checkout, "CLAUDE.md"), New: "x"}, "not an instruction file"},
		"a link":                {Item{File: "agents/dev/AGENTS.md", New: "x"}, "not a file"},
		"missing":               {Item{File: "agents/ops/AGENT.md", New: "x"}, "not a file"},
		"old not found":         {Item{File: "CLAUDE.md", Old: "- Nope.", New: "x"}, "found 0 times"},
		"empty":                 {Item{File: "CLAUDE.md", New: " "}, "empty"},
		"too long":              {Item{File: "CLAUDE.md", New: strings.Repeat("line\n", 41)}, "more than 40"},
		"a secret":              {Item{File: "CLAUDE.md", New: "- key " + key}, "carries aws key"},
	} {
		_, why := apply(checkout, c.it, l, allowed, nil)
		assert.Contains(t, why, c.want, name)
	}

	assert.Equal(t, "", gitT(t, checkout, "status", "--porcelain", "--untracked-files=no"), "nothing changed")

	_, why := apply(checkout, Item{File: "CLAUDE.md", New: "- About acme: x"}, l, allowed, []string{"acme"})
	assert.Contains(t, why, "crew word")
}

func TestFinishedAndCatchup(t *testing.T) {
	origin, checkout := repoFixture(t)

	// The owner's push of his local work first, so the checkout is clean and not ahead.
	gitT(t, checkout, "push", "-q", "origin", "main")

	gitT(t, checkout, "checkout", "-q", "-b", "lessons/m", "origin/main")
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "CLAUDE.md"), []byte("# Rules\n\n- New.\n"), 0o644))
	gitT(t, checkout, "commit", "-q", "-am", "a lesson")
	tip := gitT(t, checkout, "rev-parse", "HEAD")
	gitT(t, checkout, "push", "-q", "origin", "lessons/m")
	gitT(t, checkout, "checkout", "-q", "main")

	done, _, err := Finished(checkout, "origin", "lessons/m", "main", tip)
	require.NoError(t, err)
	assert.False(t, done, "open: the branch is there, not merged")

	// The owner merges it on the forge (here: in a second clone) and the source branch goes.
	other := filepath.Join(t.TempDir(), "other")
	gitT(t, filepath.Dir(other), "clone", "-q", origin, other)
	gitT(t, other, "merge", "-q", "--no-ff", "-m", "Merge lessons", "origin/lessons/m")
	gitT(t, other, "push", "-q", "origin", "main", ":lessons/m")

	done, merged, err := Finished(checkout, "origin", "lessons/m", "main", tip)
	require.NoError(t, err)
	assert.True(t, done)
	assert.True(t, merged)

	ok, cmdLine := Catchup(checkout, "origin", "main")
	assert.True(t, ok, cmdLine)
	assert.Contains(t, gitT(t, checkout, "show", "HEAD:CLAUDE.md"), "- New.")

	// Ahead of origin: left alone, the owner gets the one command.
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "LOCAL2.md"), []byte("x\n"), 0o644))
	gitT(t, checkout, "add", ".")
	gitT(t, checkout, "commit", "-q", "-m", "more local work")
	ok, cmdLine = Catchup(checkout, "origin", "main")
	assert.False(t, ok)
	assert.Equal(t, "git -C "+checkout+" pull --no-rebase origin main", cmdLine)

	// A branch closed without a merge and deleted: done, not merged.
	gitT(t, checkout, "push", "-q", "origin", "main:lessons/c")
	gitT(t, checkout, "push", "-q", "origin", ":lessons/c")
	done, merged, err = Finished(checkout, "origin", "lessons/c", "main", "")
	require.NoError(t, err)
	assert.True(t, done)
	assert.False(t, merged)
}

func TestFindRepoAndDefaultBranch(t *testing.T) {
	_, checkout := repoFixture(t)

	top, err := FindRepo(filepath.Join(checkout, "agents"))
	require.NoError(t, err)

	want, _ := filepath.EvalSymlinks(checkout)
	got, _ := filepath.EvalSymlinks(top)
	assert.Equal(t, want, got)
	assert.Equal(t, "main", DefaultBranch(checkout, "origin"))

	_, err = FindRepo(t.TempDir())
	assert.Error(t, err)
}
