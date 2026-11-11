package cmd

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

const tapStatusline = `{"model":{"display_name":"M"},"rate_limits":{"five_hour":{"used_percentage":12,"resets_at":1893500000},` +
	`"seven_day":{"used_percentage":41,"resets_at":1894000000}}}`

// tap runs `zmina _limits-tap` as Claude Code runs a status line: the JSON on stdin, the output on stdout.
func tap(t *testing.T, crewFile, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	var out, errb bytes.Buffer

	root := NewRoot(&out, &errb)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"--crew", crewFile, "_limits-tap"}, args...))
	err = root.Execute()

	return out.String(), errb.String(), err
}

func tapFile(t *testing.T, crewFile string) string {
	t.Helper()

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	return filepath.Join(c.Layout().Limits(), "claude.json")
}

func TestTapPrintsTheWrappedStatuslineUnchangedAndRecordsTheLimits(t *testing.T) {
	crewFile := testCrew(t, "")

	// What the wrapped line prints is passed byte for byte: colours, a missing final newline, whatever it is.
	out, errOut, err := tap(t, crewFile, tapStatusline, "--", `printf '\033[32mM\033[0m  5h 12%%  ~/work'`)
	require.NoError(t, err)
	assert.Equal(t, "\x1b[32mM\x1b[0m  5h 12%  ~/work", out)
	assert.Empty(t, errOut)

	raw, err := os.ReadFile(tapFile(t, crewFile))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"used":41`)
	assert.Contains(t, string(raw), `"source":"statusline"`)

	shown, _, err := zm(t, crewFile, "limits", "--json")
	require.NoError(t, err)
	assert.Contains(t, shown, `"used": 41`, "zmina limits reads what the tap wrote")
}

func TestTapGivesTheWrappedCommandTheSameBytesOnStdin(t *testing.T) {
	crewFile := testCrew(t, "")

	for _, in := range []string{tapStatusline, tapStatusline + "\n", "not json at all", "", "\x00\xff binary\n"} {
		out, _, err := tap(t, crewFile, in, "--", "cat")
		require.NoError(t, err)
		assert.Equal(t, in, out, "cat gets exactly what Claude Code wrote")
	}
}

func TestTapRunsTheWrappedCommandOneWordAsAShellLineSeveralAsAProgram(t *testing.T) {
	crewFile := testCrew(t, "")

	out, _, err := tap(t, crewFile, "{}", "--", "echo one; echo two | tr a-z A-Z")
	require.NoError(t, err)
	assert.Equal(t, "one\nTWO\n", out)

	out, _, err = tap(t, crewFile, "{}", "--", "printf", "%s|", "a b", "c;d")
	require.NoError(t, err)
	assert.Equal(t, "a b|c;d|", out, "words after the program are its arguments, not shell")
}

func TestTapPassesTheWrappedExitCodeAndStderrOn(t *testing.T) {
	crewFile := testCrew(t, "")

	out, errOut, err := tap(t, crewFile, tapStatusline, "--", "echo partial; echo oops >&2; exit 3")
	assert.Equal(t, "partial\n", out)
	assert.Equal(t, "oops\n", errOut)

	var ee *exitError
	require.True(t, errors.As(err, &ee))
	assert.Equal(t, 3, ee.code)

	_, errOut, err = tap(t, crewFile, tapStatusline, "--", "/nonexistent/statusline", "--flag")
	require.True(t, errors.As(err, &ee))
	assert.Equal(t, 127, ee.code, "a program that is not there, as a shell says it")
	assert.Contains(t, errOut, "_limits-tap")
	assert.FileExists(t, tapFile(t, crewFile), "the limits were recorded all the same")
}

func TestTapWithNothingWrappedPrintsNothing(t *testing.T) {
	crewFile := testCrew(t, "")

	out, errOut, err := tap(t, crewFile, tapStatusline)
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.Empty(t, errOut)
	assert.FileExists(t, tapFile(t, crewFile))

	out, errOut, err = tap(t, crewFile, tapStatusline, "--")
	require.NoError(t, err)
	assert.Empty(t, out+errOut)
}

func TestTapWritesOnlyWhenAValueChanged(t *testing.T) {
	crewFile := testCrew(t, "")
	path := tapFile(t, crewFile)

	_, _, err := tap(t, crewFile, tapStatusline, "--", "true")
	require.NoError(t, err)

	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	_, _, err = tap(t, crewFile, tapStatusline, "--", "true")
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.WithinDuration(t, old, info.ModTime(), time.Second, "same figures: the file is left alone")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))

	moved := strings.Replace(tapStatusline, `"used_percentage":41`, `"used_percentage":42`, 1)
	_, _, err = tap(t, crewFile, moved, "--", "true")
	require.NoError(t, err)

	changed, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(changed), `"used":42`)
}

func TestTapRecordsNothingFromAStatuslineWithoutLimits(t *testing.T) {
	crewFile := testCrew(t, "")

	out, _, err := tap(t, crewFile, `{"model":{"display_name":"M"}}`, "--", "cat")
	require.NoError(t, err)
	assert.Equal(t, `{"model":{"display_name":"M"}}`, out)
	assert.NoFileExists(t, tapFile(t, crewFile))
}

// The tap never breaks the status line: a write that fails, a crew file that is not there, a crew file with
// something the loader would warn about — the wrapped command still runs, its output is printed, nothing else is.
func TestTapFailingToWriteStillPrintsTheStatusline(t *testing.T) {
	crewFile := testCrew(t, "")

	// A file where the limits directory should be: the write cannot happen.
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(crewFile), "limits"), []byte("in the way"), 0o600))

	out, errOut, err := tap(t, crewFile, tapStatusline, "--", "echo line")
	require.NoError(t, err)
	assert.Equal(t, "line\n", out)
	assert.Empty(t, errOut)

	// No crew directory at all: nothing is made, the line still prints.
	gone := filepath.Join(t.TempDir(), "no-such-crew", "crew.yaml")
	out, errOut, err = tap(t, gone, tapStatusline, "--", "echo line")
	require.NoError(t, err)
	assert.Equal(t, "line\n", out)
	assert.Empty(t, errOut)
	assert.NoDirExists(t, filepath.Dir(gone))

	// A crew file that does not even load: the tap does not need it.
	broken := filepath.Join(t.TempDir(), "crew.yaml")
	require.NoError(t, os.WriteFile(broken, []byte("roles: [this is not a crew"), 0o600))

	out, errOut, err = tap(t, broken, tapStatusline, "--", "echo line")
	require.NoError(t, err)
	assert.Equal(t, "line\n", out)
	assert.Empty(t, errOut)
	assert.FileExists(t, filepath.Join(filepath.Dir(broken), "limits", "claude.json"))
}

func TestTapIsPlumbingNotListedAnywhere(t *testing.T) {
	out, _, err := zm(t, testCrew(t, ""), "--help")
	require.NoError(t, err)
	assert.NotContains(t, out, "_limits-tap")
}

// pidFileGone reports whether the process whose pid a script wrote to path no longer runs. A pid that is not written yet is
// not gone.
func pidFileGone(t *testing.T, path string) bool {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil || !strings.HasSuffix(string(raw), "\n") {
		return false
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)

	return syscall.Kill(pid, 0) != nil
}

// killAtEnd ends what a test's script left running, so a failing test leaves no process behind.
func killAtEnd(t *testing.T, paths ...string) {
	t.Helper()

	t.Cleanup(func() {
		for _, path := range paths {
			if raw, err := os.ReadFile(path); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 1 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
	})
}

// A wrapped command that never exits does not hold the status line: the tap cuts it at the time it has, quietly, with
// what it printed so far, and nothing it started is left running.
func TestTapCutsAWrappedCommandThatNeverEnds(t *testing.T) {
	pids := filepath.Join(t.TempDir(), "sleep.pid")
	killAtEnd(t, pids)

	var out, errOut bytes.Buffer

	a := &app{out: &out, err: &errOut}
	start := time.Now()

	err := a.runWrappedWithin([]string{"echo partial; sleep 30 & echo $! > " + pids + "; wait"}, []byte(tapStatusline), 700*time.Millisecond)

	assert.NoError(t, err, "out of time is not a failure of the status line")
	assert.Equal(t, "partial\n", out.String(), "what was printed stays")
	assert.Empty(t, errOut.String(), "no error text into the status line")
	assert.Less(t, time.Since(start), 10*time.Second, "bounded by its time, not by the command")
	assert.Eventually(t, func() bool { return pidFileGone(t, pids) }, 3*time.Second, 20*time.Millisecond,
		"the whole group is killed, what the shell started included")
}

// What the command started and left holding the output does not hold the tap past the wait delay once the command
// itself is done.
func TestTapDoesNotWaitForPipesAChildOfTheCommandHolds(t *testing.T) {
	pids := filepath.Join(t.TempDir(), "sleep.pid")
	killAtEnd(t, pids)

	var out, errOut bytes.Buffer

	a := &app{out: &out, err: &errOut}
	start := time.Now()

	err := a.runWrappedWithin([]string{"echo hi; sleep 8 & echo $! > " + pids}, []byte(tapStatusline), time.Minute)

	assert.NoError(t, err)
	assert.Equal(t, "hi\n", out.String())
	assert.Empty(t, errOut.String())
	assert.Less(t, time.Since(start), 5*time.Second, "bounded by the wait delay, not by the child")
}

// The wrapped command's exit code and a program that is not there are still as before.
func TestTapKeepsTheWrappedExitCodeUnderTheTimeout(t *testing.T) {
	var out, errOut bytes.Buffer

	a := &app{out: &out, err: &errOut}

	var ee *exitError

	require.True(t, errors.As(a.runWrappedWithin([]string{"echo x; exit 4"}, nil, time.Minute), &ee))
	assert.Equal(t, 4, ee.code)
	assert.Equal(t, "x\n", out.String())

	require.True(t, errors.As(a.runWrappedWithin([]string{"kill -TERM $$"}, nil, time.Minute), &ee))
	assert.Equal(t, 128+int(syscall.SIGTERM), ee.code, "ended by a signal of its own: as a shell says it")

	require.True(t, errors.As(a.runWrappedWithin([]string{"/nonexistent/statusline"}, nil, time.Minute), &ee))
	assert.Equal(t, 127, ee.code)
}

// tapHelperEnv makes the test binary run as `zmina`: TestTapHelperProcess runs what follows `--` in its arguments.
const tapHelperEnv = "ZMINA_TEST_TAP_HELPER"

func TestTapHelperProcess(t *testing.T) {
	if os.Getenv(tapHelperEnv) == "" {
		t.Skip("run by the tests that start a tap")
	}

	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]

			break
		}
	}

	root := NewRoot(os.Stdout, os.Stderr)
	root.SetArgs(args)

	if err := root.Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}

		os.Exit(1)
	}

	os.Exit(0)
}

// A tap that is told to end takes its wrapped command with it, whole group: Claude Code ends a status line that
// takes too long, and the command must not outlive the tap.
func TestTapEndsItsWrappedCommandWhenTold(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			crewFile := testCrew(t, "")
			dir := t.TempDir()
			shell, child := filepath.Join(dir, "sh.pid"), filepath.Join(dir, "sleep.pid")
			killAtEnd(t, shell, child)

			script := "echo $$ > " + shell + "; sleep 30 & echo $! > " + child + "; wait"
			c := exec.Command(os.Args[0], "-test.run=^TestTapHelperProcess$", "--", "--crew", crewFile, "_limits-tap", "--", script)
			c.Env = append(os.Environ(), tapHelperEnv+"=1")
			c.Stdin = strings.NewReader(tapStatusline)

			var out bytes.Buffer

			c.Stdout, c.Stderr = &out, &out
			require.NoError(t, c.Start())

			ended := make(chan error, 1)

			go func() { ended <- c.Wait() }()

			t.Cleanup(func() { _ = c.Process.Kill() })

			require.Eventually(t, func() bool { return fileHasLine(child) }, 10*time.Second, 20*time.Millisecond,
				"the wrapped command is running")
			require.NoError(t, c.Process.Signal(sig))

			select {
			case err := <-ended:
				var exit *exec.ExitError
				require.True(t, errors.As(err, &exit), "the tap ends with the signal's code: %v\n%s", err, out.String())
				assert.Equal(t, 128+int(sig), exit.ExitCode())
			case <-time.After(10 * time.Second):
				t.Fatalf("the tap did not end after %s\n%s", sig, out.String())
			}

			assert.Eventually(t, func() bool { return pidFileGone(t, shell) && pidFileGone(t, child) }, 3*time.Second, 20*time.Millisecond,
				"the wrapped shell and what it started are gone")
			assert.NotContains(t, out.String(), "_limits-tap", "no error text of the tap's")
		})
	}
}

// fileHasLine reports whether a script has written a whole line to path.
func fileHasLine(path string) bool {
	raw, err := os.ReadFile(path)

	return err == nil && strings.HasSuffix(string(raw), "\n")
}
