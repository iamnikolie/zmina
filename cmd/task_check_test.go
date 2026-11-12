package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/shift"
)

// checkCrew is a crew with an api repo, for tasks that have worktrees and a check to close on.
type checkCrew struct {
	file, repo string
}

// newCheckCrew makes the crew; key is the lines of the crew file to put before its roles ("done_check: …\n", or
// nothing).
func newCheckCrew(t *testing.T, key string) *checkCrew {
	t.Helper()

	file := testCrew(t, "  dev: {}\n")

	raw := mustRead(t, file)
	require.NoError(t, os.WriteFile(file, []byte(strings.Replace(string(raw), "roles:\n", key+"roles:\n", 1)), 0o600))

	repo := filepath.Join(filepath.Dir(file), "api")
	require.NoError(t, os.MkdirAll(repo, 0o700))
	gitIn(t, repo, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a"), []byte("a"), 0o600))
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "a")

	return &checkCrew{file: file, repo: repo}
}

// task records a task with a worktree and returns the worktree's path; extra are flags of task new.
func (c *checkCrew) task(t *testing.T, extra ...string) string {
	t.Helper()

	args := append([]string{"task", "new", "dev", "--repo", "api", "--worktree"}, extra...)
	out, _, err := zm(t, c.file, append(args, "work")...)
	require.NoError(t, err)

	return strings.Fields(strings.SplitN(out, "worktree ", 2)[1])[0]
}

// commit commits a new file in a worktree, so the task has a commit beyond its base.
func commit(t *testing.T, wt, name string) string {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(wt, name), []byte(name), 0o600))
	gitIn(t, wt, "add", ".")
	gitIn(t, wt, "commit", "-q", "-m", name)

	return gitIn(t, wt, "rev-parse", "HEAD")
}

// exitCodeOf is the exit code an error ends the command with: its own, else 1.
func exitCodeOf(t *testing.T, err error) int {
	t.Helper()

	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}

	return 1
}

func closedOf(t *testing.T, file, id string) map[string]any {
	t.Helper()

	closed, _ := readTaskFile(t, file, id)["closed"].(map[string]any)

	return closed
}

// A crew's done_check runs when a task with a worktree closes as done: what ran, its exit, its duration, the commit it
// ran at and the end of its output are on the closed task, and task show says so.
func TestTaskDoneRunsTheCrewCheckAndRecordsIt(t *testing.T) {
	c := newCheckCrew(t, "done_check: {run: \"echo crew-ran; pwd\", commit: true}\n")
	wt := c.task(t)

	shown, _, err := zm(t, c.file, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, shown, "check: echo crew-ran; pwd (10m, a commit beyond the base) · crew's")
	assert.Contains(t, shown, "not that the work is right")

	// No commit on the branch: refused before anything runs.
	_, _, err = zm(t, c.file, "task", "done", "1")
	require.ErrorContains(t, err, "beyond main, and there is none")
	assert.Equal(t, exitCheckFailed, exitCodeOf(t, err))

	head := commit(t, wt, "b")

	out, errOut, err := zm(t, c.file, "task", "done", "1", "--note", "shipped")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev · done\n", out, "the line scripts read is as it was")
	assert.Contains(t, errOut, "T-1: running its check: echo crew-ran; pwd")

	closed := closedOf(t, c.file, "T-1")
	require.NotNil(t, closed)
	assert.Equal(t, "done", closed["state"])

	chk, _ := closed["check"].(map[string]any)
	require.NotNil(t, chk)
	assert.Equal(t, "echo crew-ran; pwd", chk["run"])
	assert.Equal(t, "crew", chk["source"])
	assert.Equal(t, head, chk["commit"], "the commit it ran at")
	assert.EqualValues(t, 0, chk["exit"])
	assert.Contains(t, chk["output"], "crew-ran")
	assert.Contains(t, chk["output"], filepath.Base(wt), "it ran in the task's worktree")
	assert.NotNil(t, chk["duration_ms"])
	assert.NotNil(t, chk["at"])
	assert.NotContains(t, closed, "unchecked")

	shown, _, err = zm(t, c.file, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, shown, "checked: echo crew-ran; pwd · exit 0 in ")
	assert.Contains(t, shown, "at "+head)
	assert.NotContains(t, shown, "\ncheck: ", "a closed task no longer says what closing will run")
	assert.NotContains(t, shown, "crew-ran\n  ", "the output is for --full")

	full, _, err := zm(t, c.file, "task", "show", "1", "--full")
	require.NoError(t, err)
	assert.Contains(t, full, "\n  crew-ran\n")

	log, _, err := zm(t, c.file, "log", "--task", "1")
	require.NoError(t, err)
	assert.Contains(t, log, "passed: echo crew-ran; pwd (crew, exit 0")
}

// A failing check refuses the close with its own exit code and the end of its output; the task stays open, and every way
// to close it as done runs the check.
func TestTaskDoneFailingCheckRefusesTheClose(t *testing.T) {
	c := newCheckCrew(t, "done_check: \"echo FAIL-DETAIL; exit 7\"\n")
	c.task(t)

	for _, closing := range [][]string{{"task", "done", "1"}, {"task", "set", "1", "done"}} {
		out, _, err := zm(t, c.file, closing...)
		require.Error(t, err, closing)
		assert.Empty(t, out, "nothing says it was closed")
		assert.ErrorContains(t, err, "check failed")
		assert.ErrorContains(t, err, "exit 7")
		assert.ErrorContains(t, err, "FAIL-DETAIL")
		assert.ErrorContains(t, err, "--force")
		assert.Equal(t, exitCheckFailed, exitCodeOf(t, err))
	}

	assert.Equal(t, "open", readTaskFile(t, c.file, "T-1")["state"])
	assert.NotContains(t, readTaskFile(t, c.file, "T-1"), "closed")

	// Dropping is no close as done.
	_, _, err := zm(t, c.file, "task", "drop", "1")
	require.NoError(t, err)
}

func TestTaskDoneCheckThatTimesOutRefusesTheClose(t *testing.T) {
	c := newCheckCrew(t, "done_check: {run: \"echo before; sleep 60\", timeout: 1s}\n")
	c.task(t)

	began := time.Now()
	_, _, err := zm(t, c.file, "task", "done", "1")

	require.ErrorContains(t, err, "timed out after 1s")
	assert.ErrorContains(t, err, "before")
	assert.Equal(t, exitCheckFailed, exitCodeOf(t, err))
	assert.Less(t, time.Since(began), 15*time.Second)
	assert.Equal(t, "open", readTaskFile(t, c.file, "T-1")["state"])
}

// --force closes past a check, for the owner and the dispatcher, and says on the task who did.
func TestTaskDoneForceClosesUncheckedAndSaysWho(t *testing.T) {
	c := newCheckCrew(t, "done_check: \"exit 1\"\n")

	for range 3 {
		c.task(t)
	}

	// A worker does not force its own way past the check.
	t.Setenv("ZMINA_ROLE", "dev")

	_, _, err := zm(t, c.file, "task", "done", "1", "--force")
	require.ErrorContains(t, err, "only the owner or the dispatcher closes a task past its check")
	assert.Equal(t, "open", readTaskFile(t, c.file, "T-1")["state"])

	// The owner does.
	t.Setenv("ZMINA_ROLE", "")

	out, errOut, err := zm(t, c.file, "task", "done", "1", "--force", "--note", "accepted by hand")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev · done\n", out)
	assert.NotContains(t, errOut, "running its check", "forcing does not run it")

	closed := closedOf(t, c.file, "T-1")
	assert.Equal(t, map[string]any{"by": "hand", "run": "exit 1"}, closed["unchecked"])
	assert.NotContains(t, closed, "check", "it was not run")

	// So does the dispatcher.
	t.Setenv("ZMINA_ROLE", "dispatcher")

	_, _, err = zm(t, c.file, "task", "set", "2", "done", "--force")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"by": "dispatcher", "run": "exit 1"}, closedOf(t, c.file, "T-2")["unchecked"])

	t.Setenv("ZMINA_ROLE", "")

	shown, _, err := zm(t, c.file, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, shown, "unchecked: closed by hand past its check (exit 1), which was not run")

	ls, _, err := zm(t, c.file, "task", "ls", "--all")
	require.NoError(t, err)
	assert.Contains(t, ls, "done (unchecked)")
	assert.Equal(t, 2, strings.Count(ls, "(unchecked)"), "the task still open is not marked")
}

// With no check configured a close is what it was: nothing runs, nothing is recorded, --force is the old --force.
func TestTaskDoneWithNoCheckIsAsBefore(t *testing.T) {
	c := newCheckCrew(t, "")
	c.task(t)

	out, errOut, err := zm(t, c.file, "task", "done", "1")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev · done\n", out)
	assert.NotContains(t, errOut, "check")
	assert.NotContains(t, closedOf(t, c.file, "T-1"), "check")
	assert.NotContains(t, closedOf(t, c.file, "T-1"), "unchecked")

	shown, _, err := zm(t, c.file, "task", "show", "1")
	require.NoError(t, err)
	assert.NotContains(t, shown, "check")

	// Uncommitted work still refuses, and --force still closes it, with nothing said of a check.
	wt2 := c.task(t)
	require.NoError(t, os.WriteFile(filepath.Join(wt2, "x"), []byte("x"), 0o600))

	_, _, err = zm(t, c.file, "task", "done", "2")
	require.ErrorContains(t, err, "uncommitted changes")
	assert.NotEqual(t, exitCheckFailed, exitCodeOf(t, err))

	_, _, err = zm(t, c.file, "task", "done", "2", "--force")
	require.NoError(t, err)
	assert.NotContains(t, closedOf(t, c.file, "T-2"), "unchecked")
}

// A crew's check does not reach a task with no worktree: it has nowhere for it to run.
func TestCrewCheckLeavesATaskWithNoWorktreeAlone(t *testing.T) {
	c := newCheckCrew(t, "done_check: \"exit 1\"\n")

	_, _, err := zm(t, c.file, "task", "new", "dev", "research")
	require.NoError(t, err)

	shown, _, err := zm(t, c.file, "task", "show", "1")
	require.NoError(t, err)
	assert.NotContains(t, shown, "check:")

	out, errOut, err := zm(t, c.file, "task", "done", "1")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev · done\n", out)
	assert.NotContains(t, errOut, "check")
}

// A task's own check replaces the crew's.
func TestTaskCheckOverridesTheCrewCheck(t *testing.T) {
	c := newCheckCrew(t, "done_check: \"exit 1\"\n")
	c.task(t, "--check", "echo task-level", "--check-timeout", "5m")
	c.task(t)

	assert.Equal(t, map[string]any{"run": "echo task-level", "timeout": "5m"}, readTaskFile(t, c.file, "T-1")["check"])

	shown, _, err := zm(t, c.file, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, shown, "check: echo task-level (5m) · task's")

	_, _, err = zm(t, c.file, "task", "done", "1")
	require.NoError(t, err)

	chk, _ := closedOf(t, c.file, "T-1")["check"].(map[string]any)
	assert.Equal(t, "task", chk["source"])
	assert.Equal(t, "task-level", chk["output"])

	// The other task has only the crew's, which fails.
	_, _, err = zm(t, c.file, "task", "done", "2")
	require.ErrorContains(t, err, "exit 1")

	// Set it, change it, clear it: the crew's check is back.
	_, _, err = zm(t, c.file, "task", "set", "2", "--check", "echo now-it-passes", "--check-commit")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"run": "echo now-it-passes", "commit": true}, readTaskFile(t, c.file, "T-2")["check"])

	_, _, err = zm(t, c.file, "task", "done", "2")
	require.ErrorContains(t, err, "beyond main", "commit: true on the task")

	_, _, err = zm(t, c.file, "task", "set", "2", "--check", "-")
	require.NoError(t, err)
	assert.NotContains(t, readTaskFile(t, c.file, "T-2"), "check")

	_, _, err = zm(t, c.file, "task", "done", "2")
	require.ErrorContains(t, err, "exit 1", "the crew's check again")
}

// Only the owner and the dispatcher give a task a check: a role cannot make its own closing run a command of its choosing.
func TestOnlyTheOwnerAndTheDispatcherSetATasksCheck(t *testing.T) {
	c := newCheckCrew(t, "")
	c.task(t)

	t.Setenv("ZMINA_ROLE", "dev")

	_, _, err := zm(t, c.file, "task", "set", "1", "--check", "touch pwned")
	require.ErrorContains(t, err, "only the owner or the dispatcher sets a task's check")

	_, _, err = zm(t, c.file, "task", "new", "dev", "--repo", "api", "--worktree", "--check", "touch pwned", "mine")
	require.ErrorContains(t, err, "only the owner or the dispatcher sets a task's check")

	_, _, err = zm(t, c.file, "task", "set", "1", "--check", "-")
	require.ErrorContains(t, err, "only the owner or the dispatcher")

	assert.NotContains(t, readTaskFile(t, c.file, "T-1"), "check")

	got, _, err := zm(t, c.file, "task", "ls")
	require.NoError(t, err)
	assert.NotContains(t, got, "T-2", "the refused task was not made")

	// Its note is its own to set, as before.
	_, _, err = zm(t, c.file, "task", "set", "1", "--note", "half done")
	require.NoError(t, err)

	t.Setenv("ZMINA_ROLE", "dispatcher")

	_, _, err = zm(t, c.file, "task", "set", "1", "--check", "echo by-dispatcher")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"run": "echo by-dispatcher"}, readTaskFile(t, c.file, "T-1")["check"])

	// The dispatcher's check runs when the worker closes.
	t.Setenv("ZMINA_ROLE", "dev")

	_, _, err = zm(t, c.file, "task", "done", "1")
	require.NoError(t, err)
	assert.Equal(t, "echo by-dispatcher", closedOf(t, c.file, "T-1")["check"].(map[string]any)["run"])
}

func TestCheckFlagsAreChecked(t *testing.T) {
	c := newCheckCrew(t, "")
	c.task(t)

	_, _, err := zm(t, c.file, "task", "set", "1", "--check-timeout", "5m")
	require.ErrorContains(t, err, "go with --check")

	_, _, err = zm(t, c.file, "task", "set", "1", "--check", "-", "--check-commit")
	require.ErrorContains(t, err, "nothing to go with")

	_, _, err = zm(t, c.file, "task", "set", "1", "--check", "x", "--check-timeout", "2h")
	require.ErrorContains(t, err, "at most 30m")

	_, _, err = zm(t, c.file, "task", "set", "1", "--check", "")
	require.ErrorContains(t, err, "needs a command")

	_, _, err = zm(t, c.file, "task", "set", "1", "done", "--check", "true")
	require.ErrorContains(t, err, "before closing the task")

	_, _, err = zm(t, c.file, "task", "new", "dev", "no tree", "--check", "true")
	require.ErrorContains(t, err, "task has none")

	_, _, err = zm(t, c.file, "task", "new", "dev", "--repo", "api", "--worktree", "--check", "-", "x")
	require.ErrorContains(t, err, "no check to clear")

	assert.NotContains(t, readTaskFile(t, c.file, "T-1"), "check")
}

// The check holds no state lock while it runs: another zmina command goes through while it sleeps, and the close that
// follows refuses a task that changed meanwhile.
func TestTaskDoneHoldsNoLockWhileItsCheckRuns(t *testing.T) {
	marks := t.TempDir()
	started, release := filepath.Join(marks, "started"), filepath.Join(marks, "release")

	// The check says it began, then waits for the test to let it go (never for more than 30 s).
	run := "touch " + started + "; i=0; while [ ! -f " + release + " ] && [ $i -lt 300 ]; do sleep 0.1; i=$((i+1)); done; test -f " + release

	c := newCheckCrew(t, "done_check: {run: \""+run+"\", timeout: 1m}\n")
	c.task(t)

	_, _, err := zm(t, c.file, "task", "new", "dev", "another task")
	require.NoError(t, err)

	stateDir := filepath.Dir(c.file)

	old := shift.LockWait
	shift.LockWait = 3 * time.Second

	t.Cleanup(func() { shift.LockWait = old })

	done := make(chan error, 1)

	go func() {
		_, _, err := zm(t, c.file, "task", "done", "1")
		done <- err
	}()

	require.Eventually(t, func() bool { _, err := os.Stat(started); return err == nil }, 20*time.Second,
		20*time.Millisecond, "the check began")

	// The state lock is free: taking it, without waiting, works.
	l, err := shift.TryLockAs(stateDir, shift.LockOpts{Verb: "test"})
	require.NoError(t, err, "the lock is not held while the check runs")
	l.Release()

	// And a command that writes the state goes through while it sleeps.
	_, _, err = zm(t, c.file, "task", "set", "2", "--note", "written while the check runs")
	require.NoError(t, err)

	_, statErr := os.Stat(release)
	require.True(t, os.IsNotExist(statErr), "the check is still running")

	select {
	case err := <-done:
		t.Fatalf("task done ended before the check was let go: %v", err)
	default:
	}

	// A change to the task itself meanwhile: the result is of what was there before, and does not close it.
	_, _, err = zm(t, c.file, "task", "set", "1", "--note", "changed under the check")
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(release, []byte("go"), 0o600))

	select {
	case err := <-done:
		require.ErrorContains(t, err, "changed while its check ran")
		assert.Equal(t, 1, exitCodeOf(t, err), "not the exit code of a failed check: it did not fail")
	case <-time.After(30 * time.Second):
		t.Fatal("task done did not end")
	}

	assert.Equal(t, "open", readTaskFile(t, c.file, "T-1")["state"])

	// Run again, nothing in its way: it closes.
	_, _, err = zm(t, c.file, "task", "done", "1")
	require.NoError(t, err)
	assert.Equal(t, "done", readTaskFile(t, c.file, "T-1")["state"])
}

// A check does not see what looks like a secret in the environment it was started from.
func TestTaskCheckRunsWithoutTheSecretsOfItsEnvironment(t *testing.T) {
	c := newCheckCrew(t, "done_check: \"env | sort; echo tok-0123456789\"\n")
	c.task(t)

	t.Setenv("DEPLOY_TOKEN", "tok-0123456789")
	t.Setenv("ZMINA_CHECK_VISIBLE", "visible")

	_, _, err := zm(t, c.file, "task", "done", "1")
	require.NoError(t, err)

	chk, _ := closedOf(t, c.file, "T-1")["check"].(map[string]any)
	out, _ := chk["output"].(string)
	assert.Contains(t, out, "ZMINA_CHECK_VISIBLE=visible")
	assert.NotContains(t, out, "DEPLOY_TOKEN")
	assert.NotContains(t, out, "tok-0123456789")
	assert.Contains(t, out, "[redacted]")
	assert.NotContains(t, out, "ZMINA_ROLE", "it runs as a role's tools do, not as a child of a role's session")
}

// A result saved with the close comes first, then the check; a task with criteria and no result is refused before it.
func TestTaskDoneWithResultAndCheck(t *testing.T) {
	c := newCheckCrew(t, "done_check: \"echo ran-after-result\"\n")
	c.task(t, "--criteria", "tests pass")

	_, _, err := zm(t, c.file, "task", "done", "1")
	require.ErrorContains(t, err, "has criteria and no saved result")
	assert.NotEqual(t, exitCheckFailed, exitCodeOf(t, err), "it is not the check that refused")

	body := filepath.Join(t.TempDir(), "result.json")
	require.NoError(t, os.WriteFile(body, []byte(criteriaResult), 0o600))

	out, errOut, err := zm(t, c.file, "task", "done", "1", "--result", body)
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(out, "T-1 · dev · done\n"), out)
	assert.Contains(t, errOut, "running its check")

	chk, _ := closedOf(t, c.file, "T-1")["check"].(map[string]any)
	assert.Equal(t, "ran-after-result", chk["output"])
}
