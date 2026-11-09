package cmd

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

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/lesson"
	"github.com/iamnikolie/zmina/internal/shift"
)

// withTop puts top-level keys into a test crew file (testCrew writes roles last).
func withTop(t *testing.T, path, top string) {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(raw), "roles:\n", top+"roles:\n", 1)), 0o600))
}

func TestLessonAddRefusesSecretsAndStoresTheRest(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	t.Setenv("ZMINA_ROLE", "dev")

	out, _, err := zm(t, file, "lesson", "add", "Run make lint before a commit", "--why", "CI failed twice",
		"--applies", "this role", "--task", "1")
	require.NoError(t, err)
	assert.Equal(t, "LS-1\n", out)

	key := "AKIA" + strings.Repeat("Z", 16)
	_, _, err = zm(t, file, "lesson", "add", "Use the key "+key, "--why", "it works")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aws key")
	assert.NotContains(t, err.Error(), key, "the scan never echoes the secret")

	md := filepath.Join(t.TempDir(), "l.md")
	require.NoError(t, os.WriteFile(md, []byte("### Say the file a result is in\nWhy: the owner looked twice\n"+
		"Applies to: every seat\nIn a file already: no\n"), 0o600))
	out, _, err = zm(t, file, "lesson", "add", "--file", md)
	require.NoError(t, err)
	assert.Equal(t, "LS-2\n", out)

	out, _, err = zm(t, file, "lesson", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "LS-1   new     dev        role         Run make lint before a commit")
	assert.Contains(t, out, "lessons: 2 waiting · triage off (lessons: on in the crew file)")

	out, _, err = zm(t, file, "lesson", "show", "ls-1")
	require.NoError(t, err)
	assert.Contains(t, out, "from dev T-1")

	_, _, err = zm(t, file, "lesson", "triage")
	require.Error(t, err, "off by default: a run by hand says so")
	assert.Contains(t, err.Error(), "lessons: on")
}

// A checkpoint's lessons section is filed as lessons and does not stay in the record.
func TestCheckpointLessonsSection(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	_, _, err := zm(t, file, "task", "new", "dev", "Fix the build")
	require.NoError(t, err)

	body := filepath.Join(t.TempDir(), "cp.json")
	raw, err := json.Marshal(map[string]string{"summary": "halfway", "done": "x", "remaining": "x",
		"open_questions": "none", "artifacts": "x", "next_step": "x",
		"lessons": "### Pin the Go version\nWhy: a bump raised go.mod\nApplies to: one task\n"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(body, raw, 0o600))

	_, _, err = zm(t, file, "task", "checkpoint", "1", "--file", body)
	require.NoError(t, err)

	out, _, err := zm(t, file, "lesson", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "LS-1   new     dev        task         Pin the Go version")

	out, _, err = zm(t, file, "task", "checkpoint", "1", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, "halfway")
	assert.NotContains(t, out, "Pin the Go version", "filed, not kept in the record")
}

func TestLessonsDue(t *testing.T) {
	c := &crew.Crew{Lessons: crew.Lessons{On: true, At: "06:00"}}
	morning := time.Date(2026, 11, 9, 6, 30, 0, 0, c.Location())

	assert.True(t, lessonsDue(c, lesson.RunState{}, 3, morning))
	assert.False(t, lessonsDue(c, lesson.RunState{}, 0, morning), "nothing waits")
	assert.False(t, lessonsDue(c, lesson.RunState{}, 3, morning.Add(-time.Hour)), "before its time")
	assert.False(t, lessonsDue(c, lesson.RunState{LastRun: morning.Add(-10 * time.Minute)}, 3, morning), "ran today")
	assert.True(t, lessonsDue(c, lesson.RunState{LastRun: morning.Add(-24 * time.Hour)}, 3, morning), "ran yesterday")
	assert.False(t, lessonsDue(c, lesson.RunState{Open: &lesson.Open{Branch: "lessons/x"}}, 3, morning), "an MR is open")
	assert.False(t, lessonsDue(c, lesson.RunState{Running: morning.Add(-5 * time.Minute)}, 3, morning), "one is going")

	off := &crew.Crew{Lessons: crew.Lessons{}}
	assert.False(t, lessonsDue(off, lesson.RunState{}, 3, morning), "off")
}

// lessonsRepo is an agentic-os with an origin, the crew's agents dir in it.
func lessonsRepo(t *testing.T) (origin, checkout string) {
	t.Helper()

	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "alex")
	t.Setenv("GIT_AUTHOR_EMAIL", "alex@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "alex")
	t.Setenv("GIT_COMMITTER_EMAIL", "alex@example.com")

	root := t.TempDir()
	origin, checkout = filepath.Join(root, "origin.git"), filepath.Join(root, "agentic-os")

	run := func(dir string, args ...string) {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		require.NoError(t, err, "%v: %s", args, out)
	}

	run(root, "init", "-q", "--bare", "-b", "main", origin)
	run(root, "clone", "-q", origin, checkout)
	run(checkout, "checkout", "-q", "-b", "main")
	require.NoError(t, os.MkdirAll(filepath.Join(checkout, "agents", "dev"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "CLAUDE.md"), []byte("# Rules\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "agents", "dev", "AGENT.md"), []byte("# dev\n"), 0o644))
	run(checkout, "add", ".")
	run(checkout, "commit", "-q", "-m", "start")
	run(checkout, "push", "-q", "origin", "main")
	run(checkout, "remote", "set-head", "origin", "main")

	return origin, checkout
}

// A run by hand on a crew with lessons on: the branch is pushed, the lessons ride in it, the owner gets one todo
// with the MR and the waiting count; when the MR is merged the todo is done and the checkout catches up.
func TestLessonTriageEndToEnd(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	origin, checkout := lessonsRepo(t)
	withTop(t, file, "agents: "+filepath.Join(checkout, "agents")+"\nlessons: on\n")
	t.Setenv("ZMINA_ROLE", "dev")

	for _, rule := range []string{"Say which file a result is in", "Reply short"} {
		_, _, err := zm(t, file, "lesson", "add", rule, "--why", "the owner asked", "--applies", "every")
		require.NoError(t, err)
	}

	was := lessonSeat
	t.Cleanup(func() { lessonSeat = was })

	lessonSeat = func(string) lesson.Seat {
		return func(_ context.Context, _, _, schema string, _ float64) ([]byte, error) {
			var structured any = lesson.Plan{Items: []lesson.Item{
				{ID: "LS-1", Verdict: "every", File: "CLAUDE.md", New: "- Say which file a result is in.", Reason: "all"},
				{ID: "LS-2", Verdict: "written", Reason: "said in CLAUDE.md"},
			}}
			if schema == lesson.CheckSchema {
				structured = map[string]any{"checks": []lesson.Check{{ID: "LS-1", OK: true, Reason: "fine"}}}
			}

			raw, _ := json.Marshal(structured)

			return json.Marshal(map[string]any{"subtype": "success", "total_cost_usd": 0.4,
				"structured_output": json.RawMessage(raw)})
		}
	}

	out, _, err := zm(t, file, "lesson", "triage")
	require.NoError(t, err)
	assert.Contains(t, out, "LS-1 -> CLAUDE.md")
	assert.Contains(t, out, "cost $0.80")

	_, _, err = zm(t, file, "lesson", "add", "A third", "--why", "later", "--applies", "unsure")
	require.NoError(t, err)

	out, _, err = zm(t, file, "lesson", "ls", "--all")
	require.NoError(t, err)
	assert.Contains(t, out, "LS-1   in-mr")
	assert.Contains(t, out, "LS-2   dropped")
	assert.Contains(t, out, "lessons: 1 waiting · branch lessons/")

	_, _, err = zm(t, file, "lesson", "triage")
	require.Error(t, err, "one MR at a time: new lessons wait")
	assert.Contains(t, err.Error(), "new lessons wait")

	todo, _, err := zm(t, file, "todo", "ls")
	require.NoError(t, err)
	assert.Contains(t, todo, "Lessons MR to review")

	branch := gitOut(t, origin, "for-each-ref", "--format=%(refname:short)", "refs/heads/lessons/")
	require.NotEmpty(t, branch)

	a := &app{out: os.Stdout, err: os.Stderr, crew: file}

	// While it is open the todo learns how many lessons wait.
	a.lessonsBeat(time.Now())

	var st lesson.RunState
	require.NoError(t, a.readState(func(s *shift.Shift) error {
		st, _ = lesson.LoadState(lessonDir(s.Crew))
		ask, err := askStore(s.Crew).Get(st.Open.Todo)
		require.NoError(t, err)
		assert.Contains(t, ask.Text, "1 new lessons wait for this MR.")

		return nil
	}))

	// The owner merges it on the forge and the source branch goes.
	other := filepath.Join(t.TempDir(), "other")
	gitOut(t, filepath.Dir(other), "clone", "-q", origin, other)
	gitOut(t, other, "merge", "-q", "--no-ff", "-m", "Merge lessons", "origin/"+branch)
	gitOut(t, other, "push", "-q", "origin", "main", ":"+branch)

	// He ticks the todo before the hourly check: the forge still says it was merged, and the checkout catches up.
	_, _, err = zm(t, file, "todo", "done", st.Open.Todo)
	require.NoError(t, err)
	a.lessonsBeat(time.Now())

	out, _, err = zm(t, file, "lesson", "ls", "--all")
	require.NoError(t, err)
	assert.Contains(t, out, "LS-1   merged")
	assert.NotContains(t, out, "branch lessons/")

	todo, _, err = zm(t, file, "todo", "ls")
	require.NoError(t, err)
	assert.NotContains(t, todo, "Lessons MR to review", "done once merged")
	assert.Contains(t, gitOut(t, checkout, "show", "HEAD:CLAUDE.md"), "Say which file a result is in", "caught up")

	usage, _, err := zm(t, file, "usage", "--since", "24h")
	require.NoError(t, err)
	assert.Contains(t, usage, "Lessons triage $0.80")
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()

	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	require.NoError(t, err, "%v: %s", args, out)

	return strings.TrimSpace(string(out))
}

func TestLessonDraftLine(t *testing.T) {
	r := lesson.Result{Branch: "lessons/2026-11-09"}
	assert.Equal(t, "pushed origin refs/heads/lessons/2026-11-09 (no MR url in the push reply)", lessonDraftLine(r, ""))

	r.MRURL = "https://gitlab.com/acme/agentic-os/-/merge_requests/12"
	assert.Contains(t, lessonDraftLine(r, "origin"), "not read back")

	r.Draft, r.DraftVia = "yes", "glab"
	assert.Equal(t, "draft https://gitlab.com/acme/agentic-os/-/merge_requests/12 (read by glab)", lessonDraftLine(r, "origin"))

	r.Draft = "no"
	assert.Contains(t, lessonDraftLine(r, "origin"), "NOT A DRAFT")

	r.Pushed = true
	assert.True(t, strings.HasPrefix(lessonTodoText(r, lessonRun{}, 0), "NOT A DRAFT: GitLab holds "+r.MRURL),
		"the owner reads the todo, not the log")
}

// A deny file that is named but cannot be read refuses lessons instead of letting them through unchecked.
func TestLessonDenyFailsClosed(t *testing.T) {
	file := testCrew(t, "  dev:\n")
	withTop(t, file, "lessons:\n  enabled: false\n  deny: "+filepath.Join(t.TempDir(), "missing")+"\n")
	t.Setenv("ZMINA_ROLE", "dev")

	_, _, err := zm(t, file, "lesson", "add", "Reply short", "--why", "asked")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be read")
}
