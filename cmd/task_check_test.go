package cmd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	require.ErrorContains(t, err, "nothing beyond main, and nothing since the worktree was made")
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

// A check that could not run (the shell's 126 or 127, or the check's env_fault_rc) is no failed check: exit 6 and not 4, the
// task stays open with the fault on it and no closed.check, the dispatcher is told once for the cause, the message says the
// environment is broken and the work was not judged. A check that ran and failed is exit 4 as ever.
func TestTaskDoneEnvironmentFaultIsNotAFailedCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		code int
		why  string
	}{
		{name: "not found", key: "done_check: \"echo FAULT-DETAIL; exit 127\"\n", code: exitCheckEnv, why: "exit 127: the shell did not find the command"},
		{name: "not executable", key: "done_check: \"exit 126\"\n", code: exitCheckEnv, why: "exit 126: the shell found the command and could not execute it"},
		{name: "the crew's status", key: "done_check: {run: \"echo FAULT-DETAIL; exit 9\", env_fault_rc: 9}\n", code: exitCheckEnv, why: "exit 9: the status the check names"},
		{name: "another status", key: "done_check: {run: \"exit 8\", env_fault_rc: 9}\n", code: exitCheckFailed},
		{name: "a plain failure", key: "done_check: \"exit 1\"\n", code: exitCheckFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCheckCrew(t, tc.key)
			c.task(t)

			_, _, err := zm(t, c.file, "task", "done", "1")
			require.Error(t, err)
			assert.Equal(t, tc.code, exitCodeOf(t, err), err.Error())
			assert.Equal(t, "open", readTaskFile(t, c.file, "T-1")["state"])
			assert.NotContains(t, readTaskFile(t, c.file, "T-1"), "closed")

			if tc.code == exitCheckFailed {
				assert.ErrorContains(t, err, "check failed")
				assert.NotContains(t, readTaskFile(t, c.file, "T-1"), "check_env")
				assert.Empty(t, stateOf(t, c.file).Signals, "a failed check tells nobody, as it always did")

				return
			}

			assert.NotContains(t, err.Error(), "check failed")
			assert.ErrorContains(t, err, "the check could not run, so the work was not judged")
			assert.ErrorContains(t, err, "the environment is broken")
			assert.ErrorContains(t, err, "a file it runs is missing or not executable in the worktree")
			assert.ErrorContains(t, err, tc.why)
			assert.ErrorContains(t, err, "the task stays open and no failed check is recorded")
			assert.ErrorContains(t, err, "the dispatcher has been told")

			env, _ := readTaskFile(t, c.file, "T-1")["check_env"].(map[string]any)
			require.NotNil(t, env)
			assert.Equal(t, "environment", env["kind"])

			if strings.Contains(tc.key, "FAULT-DETAIL") {
				assert.Equal(t, "FAULT-DETAIL", env["output"])
				assert.ErrorContains(t, err, "FAULT-DETAIL", "the end of the output is in the message")
			}

			assert.Len(t, stateOf(t, c.file).Signals, 1, "told once")

			var told []any
			told, _ = readTaskFile(t, c.file, "T-1")["check_env_told"].([]any)
			assert.Len(t, told, 1, "the cause is marked told once the note is queued and the state saved")
			assert.Contains(t, stateOf(t, c.file).Signals[0], "its done check could not run, so the work was not judged")

			// Again: the same cause is kept and not told again.
			_, _, err = zm(t, c.file, "task", "set", "1", "done")
			require.Error(t, err)
			assert.Equal(t, exitCheckEnv, exitCodeOf(t, err))
			assert.ErrorContains(t, err, "the dispatcher was told of this cause already")
			assert.Len(t, stateOf(t, c.file).Signals, 1)

			shown, _, err := zm(t, c.file, "task", "show", "1")
			require.NoError(t, err)
			assert.Contains(t, shown, "check could not run: ")
			assert.Contains(t, shown, "the environment is broken, the work was not judged")

			log, _, err := zm(t, c.file, "log", "--task", "1")
			require.NoError(t, err)
			assert.Contains(t, log, "environment fault, the work was not judged: ")

			// --force is as it was: it closes past the check, and the fault goes with the close.
			_, _, err = zm(t, c.file, "task", "done", "1", "--force")
			require.NoError(t, err)
			assert.NotContains(t, readTaskFile(t, c.file, "T-1"), "check_env")
		})
	}
}

// --check-env-rc gives a task's own check the status that says it could not run; it goes with --check and is 1 to 255.
func TestTaskCheckEnvRCFlag(t *testing.T) {
	c := newCheckCrew(t, "")
	c.task(t, "--check", "exit 5", "--check-env-rc", "5")

	spec, _ := readTaskFile(t, c.file, "T-1")["check"].(map[string]any)
	assert.EqualValues(t, 5, spec["env_fault_rc"])

	_, _, err := zm(t, c.file, "task", "done", "1")
	require.Error(t, err)
	assert.Equal(t, exitCheckEnv, exitCodeOf(t, err))

	shown, _, err := zm(t, c.file, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, shown, "exit 5 is an environment fault")

	for _, args := range [][]string{
		{"task", "set", "1", "--check-env-rc", "5"},
		{"task", "set", "1", "--check", "true", "--check-env-rc", "0"},
		{"task", "set", "1", "--check", "true", "--check-env-rc", "256"},
		{"task", "set", "1", "--check", "-", "--check-env-rc", "5"},
	} {
		_, _, err = zm(t, c.file, args...)
		require.Error(t, err, args)
	}

	// 1 and 2 are the statuses of a check that ran and failed.
	for _, rc := range []string{"1", "2"} {
		_, _, err = zm(t, c.file, "task", "set", "1", "--check", "true", "--check-env-rc", rc)
		require.ErrorContains(t, err, "--check-env-rc: env_fault_rc "+rc+" is the status of a check that ran and failed", rc)
		require.ErrorContains(t, err, "name a status from 3 to 255")
	}

	_, _, err = zm(t, c.file, "task", "new", "dev", "--repo", "api", "--worktree", "--check", "true", "--check-env-rc", "1", "more")
	require.ErrorContains(t, err, "is the status of a check that ran and failed")
	_, _, err = zm(t, c.file, "task", "set", "1", "--check", "exit 6", "--check-env-rc", "6")
	require.NoError(t, err)
	assert.NotContains(t, readTaskFile(t, c.file, "T-1"), "check_env", "a new check takes the old one's fault away")

	// The crew file refuses a status that is none, and the two that a failed check exits.
	for rc, want := range map[string]string{"0": "env_fault_rc is an exit status from 3 to 255, not 0",
		"1": "done_check: check env_fault_rc 1 is the status of a check that ran and failed", "2": "done_check: check env_fault_rc 2 is the status",
		"256": "done_check: check env_fault_rc 256: an exit status from 3 to 255"} {
		bad := newCheckCrew(t, "done_check: {run: x, env_fault_rc: "+rc+"}\n")
		_, _, err = zm(t, bad.file, "task", "ls")
		require.ErrorContains(t, err, want, rc)
	}
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

// A check does not see what looks like a secret in the environment it was started from, and is told it is a check.
func TestTaskCheckRunsWithoutTheSecretsOfItsEnvironment(t *testing.T) {
	c := newCheckCrew(t, "done_check: \"env | sort; echo value-for-test-one\"\n")
	c.task(t)

	t.Setenv("DEPLOY_TOKEN", "value-for-test-one")
	t.Setenv("ZMINA_CHECK_VISIBLE", "visible")
	t.Setenv("GOPRIVATE", "example.com/internal")
	t.Setenv("DATABASE_URL", "postgres://app:fixture-pass-nine@db.example/app")

	_, _, err := zm(t, c.file, "task", "done", "1")
	require.NoError(t, err)

	chk, _ := closedOf(t, c.file, "T-1")["check"].(map[string]any)
	out, _ := chk["output"].(string)
	assert.Contains(t, out, "ZMINA_CHECK_VISIBLE=visible")
	assert.Contains(t, out, "GOPRIVATE=example.com/internal", "a name that only looks like a secret's stays")
	assert.Contains(t, out, "DATABASE_URL=postgres://app:[redacted]@db.example/app", "it has the variable, and its password is not recorded")
	assert.NotContains(t, out, "fixture-pass-nine")
	assert.NotContains(t, out, "DEPLOY_TOKEN")
	assert.NotContains(t, out, "value-for-test-one")
	assert.Contains(t, out, "[redacted]")
	assert.NotContains(t, out, "ZMINA_ROLE", "it is not run as a child of a role's session")
	assert.Contains(t, out, "ZMINA_IN_CHECK=1", "and it is told it is a check")
}

// A zmina that a check started has no owner's hand: it sets no check and forces past none, however it says who it is.
func TestZminaRunInsideACheckActsForNoOne(t *testing.T) {
	c := newCheckCrew(t, "done_check: \"exit 1\"\n")
	c.task(t)

	t.Setenv("ZMINA_IN_CHECK", "1")

	for _, who := range []string{"", "dispatcher"} {
		t.Setenv("ZMINA_ROLE", who)

		_, _, err := zm(t, c.file, "task", "set", "1", "--check", "touch pwned")
		require.ErrorContains(t, err, "acts for no one", who)

		_, _, err = zm(t, c.file, "task", "set", "1", "--check", "-")
		require.ErrorContains(t, err, "acts for no one", who)

		_, _, err = zm(t, c.file, "task", "new", "dev", "--repo", "api", "--worktree", "--check", "true", "x")
		require.ErrorContains(t, err, "acts for no one", who)

		_, _, err = zm(t, c.file, "task", "done", "1", "--force")
		require.ErrorContains(t, err, "acts for no one", who)
	}

	assert.Equal(t, "open", readTaskFile(t, c.file, "T-1")["state"])
	assert.NotContains(t, readTaskFile(t, c.file, "T-1"), "check")

	// The rest of zmina is what it was.
	_, _, err := zm(t, c.file, "task", "set", "1", "--note", "from inside a check")
	require.NoError(t, err)

	t.Setenv("ZMINA_IN_CHECK", "")
	t.Setenv("ZMINA_ROLE", "")

	_, _, err = zm(t, c.file, "task", "done", "1", "--force")
	require.NoError(t, err)
}

// A signal that ends zmina ends the check with it, hangup and quit as well as interrupt and terminate: the check has a
// session and process group of its own, which a signal to zmina does not reach.
func TestTaskDoneSignalEndsTheCheckAndIsNotAFailure(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			marks := t.TempDir()
			started, pidFile := filepath.Join(marks, "started"), filepath.Join(marks, "pid")

			c := newCheckCrew(t, "done_check: {run: \"echo $$ > "+pidFile+"; touch "+started+"; sleep 60 & wait\", timeout: 5m}\n")
			c.task(t)

			done := make(chan error, 1)

			go func() {
				_, _, err := zm(t, c.file, "task", "done", "1")
				done <- err
			}()

			require.Eventually(t, func() bool { _, err := os.Stat(started); return err == nil }, 20*time.Second,
				20*time.Millisecond, "the check began")

			raw, err := os.ReadFile(pidFile)
			require.NoError(t, err)

			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			require.NoError(t, err)
			require.NoError(t, syscall.Kill(pid, 0), "it is running")

			// zmina gets the signal (this process stands for it: the handler is the one the command installed).
			require.NoError(t, syscall.Kill(syscall.Getpid(), sig))

			select {
			case err := <-done:
				require.ErrorContains(t, err, "check interrupted")
				assert.NotContains(t, err.Error(), "check failed")
				assert.NotEqual(t, exitCheckFailed, exitCodeOf(t, err), "it did not fail")
			case <-time.After(30 * time.Second):
				t.Fatal("task done did not end")
			}

			assert.Eventually(t, func() bool { return syscall.Kill(-pid, 0) != nil }, 5*time.Second, 20*time.Millisecond,
				"the check's whole group is gone, not left running")

			assert.Equal(t, "open", readTaskFile(t, c.file, "T-1")["state"])

			log, _, err := zm(t, c.file, "log", "--task", "1")
			require.NoError(t, err)
			assert.Contains(t, log, "interrupted:")
			assert.NotContains(t, log, "failed:")
		})
	}
}

// A check that leaves files in the worktree has failed, and says which: the task is not left unclosable by uncommitted
// changes it cannot explain.
func TestTaskDoneCheckThatLeavesFilesBehind(t *testing.T) {
	c := newCheckCrew(t, "done_check: \"touch build.o a.tmp b.tmp c.tmp d.tmp e.tmp\"\n")
	wt := c.task(t)

	_, _, err := zm(t, c.file, "task", "done", "1")
	require.Error(t, err)
	assert.Equal(t, exitCheckFailed, exitCodeOf(t, err))
	assert.ErrorContains(t, err, "check failed")
	assert.ErrorContains(t, err, "left 6 file(s) in the clean worktree")
	assert.ErrorContains(t, err, "?? a.tmp, ?? b.tmp, ?? build.o, ?? c.tmp, ?? d.tmp and 1 more")
	assert.NotContains(t, err.Error(), "uncommitted changes")
	assert.Equal(t, "open", readTaskFile(t, c.file, "T-1")["state"])

	// Fixed: the check cleans up after itself.
	require.NoError(t, os.RemoveAll(wt))
	c.task(t, "--check", "touch scratch && rm scratch")

	_, _, err = zm(t, c.file, "task", "done", "2")
	require.NoError(t, err)
}

// The arguments are checked before the command runs, and a task that is done is not checked again.
func TestTaskDoneChecksArgumentsBeforeRunningAndNeverTwice(t *testing.T) {
	marks := t.TempDir()
	log := filepath.Join(marks, "runs")

	c := newCheckCrew(t, "done_check: \"echo run >> "+log+"\"\n")
	c.task(t)

	runs := func() int {
		raw, _ := os.ReadFile(log)

		return strings.Count(string(raw), "run\n")
	}

	for name, args := range map[string][]string{
		"a check with the close":   {"task", "set", "1", "done", "--check", "true"},
		"a budget that is none":    {"task", "set", "1", "done", "--budget", "plenty"},
		"a criterion that is none": {"task", "set", "1", "done", "--criteria", "a\nb"},
		"a role that is none":      {"task", "set", "1", "done", "--role", "nobody"},
		"--on without waiting":     {"task", "set", "1", "done", "--on", "alex"},
	} {
		_, _, err := zm(t, c.file, args...)
		require.Error(t, err, name)
		assert.Zero(t, runs(), "%s: nothing ran", name)
		assert.Equal(t, "open", readTaskFile(t, c.file, "T-1")["state"], name)
	}

	_, _, err := zm(t, c.file, "task", "done", "1")
	require.NoError(t, err)
	assert.Equal(t, 1, runs())

	// Closed already: closing again says more and runs nothing, and keeps what the first close recorded.
	out, errOut, err := zm(t, c.file, "task", "done", "1", "--note", "one more word")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev · done\n", out)
	assert.NotContains(t, errOut, "running its check")
	assert.Equal(t, 1, runs(), "a task that is done is not checked again")

	closed := closedOf(t, c.file, "T-1")
	assert.Equal(t, "one more word", closed["note"])
	assert.NotNil(t, closed["check"])
}

// A command is one line of plain text, and is said as one wherever zmina says it, even from a file edited by hand.
func TestTaskCheckCommandIsPlainText(t *testing.T) {
	c := newCheckCrew(t, "")
	c.task(t)

	for _, bad := range []string{"make test\nrm -rf x", "make\ttest", "make test \x1b[2K", "echo \u202etset"} {
		_, _, err := zm(t, c.file, "task", "set", "1", "--check", bad)
		require.ErrorContains(t, err, "one line of plain text", "%q", bad)
	}

	assert.NotContains(t, readTaskFile(t, c.file, "T-1"), "check")

	// A task file edited by hand, with a command that is not text.
	path := filepath.Join(filepath.Dir(c.file), "tasks", "T-1.json")
	raw := mustRead(t, path)
	edited := strings.Replace(string(raw), `"state": "open"`, `"state": "open", "check": {"run": "make test\u202e\n\u001b[2Krm -rf x"}`, 1)
	require.NotEqual(t, string(raw), edited)
	require.NoError(t, os.WriteFile(path, []byte(edited), 0o600))

	shown, _, err := zm(t, c.file, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, shown, "check: make test rm -rf x (10m) · task's")
	assert.NotContains(t, shown, "\u202e")
	assert.NotContains(t, shown, "\x1b")

	// It is not run: it is refused as the command it is.
	_, _, err = zm(t, c.file, "task", "done", "1")
	require.ErrorContains(t, err, "plain text")
}

// A branch merged into its base still has commits of its own: a task closed after its merge passes a check that wants a
// commit, and one nothing was committed on does not.
func TestTaskDoneCommitCheckAfterTheBranchWasMerged(t *testing.T) {
	c := newCheckCrew(t, "done_check: {run: \"true\", commit: true}\n")
	wt := c.task(t)
	c.task(t)

	commit(t, wt, "b")
	gitIn(t, c.repo, "merge", "-q", "--ff-only", gitIn(t, wt, "rev-parse", "--abbrev-ref", "HEAD"))

	assert.NotContains(t, readTaskFile(t, c.file, "T-1"), "check", "the crew's check, not the task's")
	assert.NotEmpty(t, readTaskFile(t, c.file, "T-1")["fork"], "the commit the worktree was made at is in the task file")

	_, _, err := zm(t, c.file, "task", "done", "1")
	require.NoError(t, err, "its work is in main; it had commits")

	_, _, err = zm(t, c.file, "task", "done", "2")
	require.ErrorContains(t, err, "a commit of the task's own, and")
	assert.ErrorContains(t, err, "has none: nothing beyond main, and nothing since the worktree was made")
	assert.Equal(t, exitCheckFailed, exitCodeOf(t, err))
}

// A dispatcher that cannot be told is not said to have been: a queue that takes no more signals leaves the fault on the task
// and the message says the dispatcher could not be told; the cause is not marked told, so the next run, when there is room,
// tells it.
func TestTaskDoneEnvironmentFaultSaysWhenTheDispatcherCouldNotBeTold(t *testing.T) {
	c := newCheckCrew(t, "done_check: \"exit 127\"\n")
	c.task(t)

	a := &app{out: io.Discard, err: io.Discard, crew: c.file}
	require.NoError(t, a.withState(func(s *shift.Shift) error {
		for i := range shift.SignalsMax {
			if _, err := s.Note("mrs", "acme/site!"+strconv.Itoa(i)+" approved", false); err != nil {
				return err
			}
		}

		return nil
	}))

	_, _, err := zm(t, c.file, "task", "done", "1")
	require.Error(t, err)
	assert.Equal(t, exitCheckEnv, exitCodeOf(t, err))
	assert.ErrorContains(t, err, "the dispatcher could not be told: ")
	assert.ErrorContains(t, err, "it is not reading them")
	assert.NotContains(t, err.Error(), "has been told")
	assert.NotContains(t, err.Error(), "was told of this")

	task := readTaskFile(t, c.file, "T-1")
	assert.NotNil(t, task["check_env"], "the fault is on the task")
	assert.NotContains(t, task, "check_env_told", "and nobody was told")

	// Room again: the next run tells it.
	require.NoError(t, a.withState(func(s *shift.Shift) error { s.State.Signals = nil; return nil }))

	_, _, err = zm(t, c.file, "task", "done", "1")
	require.ErrorContains(t, err, "the dispatcher has been told")
	assert.Len(t, readTaskFile(t, c.file, "T-1")["check_env_told"], 1)

	_, _, err = zm(t, c.file, "task", "done", "1")
	require.ErrorContains(t, err, "the dispatcher was told of this cause already")
}
