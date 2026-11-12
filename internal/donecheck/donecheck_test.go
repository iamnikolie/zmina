package donecheck

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestSpecValidate(t *testing.T) {
	assert.NoError(t, Spec{Run: "make test"}.Validate())
	assert.NoError(t, Spec{Run: "make test", Timeout: "30m", Commit: true}.Validate())
	assert.NoError(t, Spec{Run: "go test ./...\ngo vet ./..."}.Validate(), "a script is a command")

	assert.ErrorContains(t, Spec{}.Validate(), "needs a command")
	assert.ErrorContains(t, Spec{Run: "  \n"}.Validate(), "needs a command")
	assert.ErrorContains(t, Spec{Run: "a\x00b"}.Validate(), "is text")
	assert.ErrorContains(t, Spec{Run: strings.Repeat("a", runMax+1)}.Validate(), "at most")
	assert.ErrorContains(t, Spec{Run: "x", Timeout: "forever"}.Validate(), "a span such as")
	assert.ErrorContains(t, Spec{Run: "x", Timeout: "-5m"}.Validate(), "a span such as")
	assert.ErrorContains(t, Spec{Run: "x", Timeout: "31m"}.Validate(), "at most 30m")
}

func TestSpanAndString(t *testing.T) {
	assert.Equal(t, "10m", Span(10*time.Minute))
	assert.Equal(t, "1h", Span(time.Hour))
	assert.Equal(t, "90s", Span(90*time.Second))
	assert.Equal(t, "90m", Span(90*time.Minute))
	assert.Equal(t, "1.5s", Span(1500*time.Millisecond))

	assert.Equal(t, "make test (10m)", Spec{Run: "make test"}.String())
	assert.Equal(t, "make test (5m, a commit beyond the base)", Spec{Run: "make test", Timeout: "5m", Commit: true}.String())
}

func TestSpecLimit(t *testing.T) {
	assert.Equal(t, DefaultTimeout, Spec{Run: "x"}.Limit())
	assert.Equal(t, 90*time.Second, Spec{Run: "x", Timeout: "90s"}.Limit())
	assert.Equal(t, MaxTimeout, Spec{Run: "x", Timeout: "10h"}.Limit(), "never longer than the cap")
}

// The crew file says a check as one command line, or as its three keys.
func TestSpecReadsAStringOrAMapping(t *testing.T) {
	var doc struct {
		A *Spec `yaml:"a"`
		B *Spec `yaml:"b"`
		C *Spec `yaml:"c"`
	}

	require.NoError(t, yaml.Unmarshal([]byte("a: make test\nb: {run: make test, timeout: 5m, commit: true}\n"), &doc))
	assert.Equal(t, &Spec{Run: "make test"}, doc.A)
	assert.Equal(t, &Spec{Run: "make test", Timeout: "5m", Commit: true}, doc.B)
	assert.Nil(t, doc.C)

	require.Error(t, yaml.Unmarshal([]byte("a: [make, test]\n"), &doc))
	require.ErrorContains(t, yaml.Unmarshal([]byte("a: {run: x, comit: true}\n"), &doc), `no key "comit"`)
}

func run(t *testing.T, line string, spec func(*Spec)) Result {
	t.Helper()

	s := Spec{Run: line}
	if spec != nil {
		spec(&s)
	}

	return Run(context.Background(), s, t.TempDir(), nil)
}

func TestRunPassesAndFailsByExitStatus(t *testing.T) {
	r := run(t, "echo fine", nil)
	assert.True(t, r.Passed())
	assert.Equal(t, 0, r.Exit)
	assert.Equal(t, "fine", r.Output)
	assert.False(t, r.Truncated)
	assert.False(t, r.At.IsZero())

	r = run(t, "echo to-out; echo to-err >&2; exit 3", nil)
	assert.False(t, r.Passed())
	assert.Equal(t, 3, r.Exit)
	assert.Equal(t, "exit 3", r.Failure(time.Minute))
	assert.Equal(t, "to-out\nto-err", r.Output, "both streams, in the order they came")

	r = run(t, "exit 0", nil)
	assert.True(t, r.Passed())
	assert.Equal(t, "(no output)", r.Tail())
}

func TestRunsInTheGivenDirectoryWithStdinClosed(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)

	// cat would wait for the terminal for ever if stdin were open.
	r := Run(context.Background(), Spec{Run: "pwd; cat; echo after"}, dir, nil)
	require.True(t, r.Passed(), r.Output)
	assert.Equal(t, resolved+"\nafter", r.Output)
}

func TestRunWithNoDirectoryOrNoShellSaysItDidNotStart(t *testing.T) {
	r := Run(context.Background(), Spec{Run: "true"}, filepath.Join(t.TempDir(), "gone"), nil)
	assert.False(t, r.Passed())
	assert.Equal(t, -1, r.Exit)
	assert.Contains(t, r.Err, "did not start")
	assert.Contains(t, r.Failure(time.Minute), "did not start")
}

// A check that runs past its limit is ended, with whatever it started, and says so.
func TestRunTimesOutAndEndsWhatItStarted(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")

	began := time.Now()
	r := Run(context.Background(), Spec{Run: "sleep 60 & echo $! > child.pid; echo started; wait", Timeout: "1s"}, dir, nil)

	assert.True(t, r.TimedOut)
	assert.False(t, r.Passed())
	assert.Equal(t, -1, r.Exit)
	assert.Equal(t, "timed out after 1s", r.Failure(time.Second))
	assert.Contains(t, r.Output, "started", "what it printed before the end stays")
	assert.Less(t, time.Since(began), 10*time.Second)

	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err)

	pid := 0
	for _, c := range strings.TrimSpace(string(raw)) {
		pid = pid*10 + int(c-'0')
	}

	require.Positive(t, pid)
	assert.Eventually(t, func() bool { return syscall.Kill(pid, 0) != nil }, 3*time.Second, 20*time.Millisecond,
		"the process the check started is gone with it")
}

func TestRunStopsWhenItsContextIs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	r := Run(ctx, Spec{Run: "sleep 60"}, t.TempDir(), nil)
	assert.False(t, r.Passed())
	assert.False(t, r.TimedOut)
	assert.Contains(t, r.Err, "stopped")
}

// A process the command leaves running with the output open does not hold the run for as long as it lives.
func TestRunDoesNotWaitForAProcessLeftHoldingItsOutput(t *testing.T) {
	began := time.Now()
	r := run(t, "(sleep 60 &) ; echo done", nil)

	assert.True(t, r.Passed(), r.Output)
	assert.Less(t, time.Since(began), 10*time.Second)
}

func TestScrubDropsSecretNamesAndKeepsTheRest(t *testing.T) {
	clean, secrets := Scrub([]string{
		"PATH=/usr/bin", "HOME=/home/alex", "GITHUB_TOKEN=ghp_abcdefgh1234", "AWS_SECRET_ACCESS_KEY=abcdefghijkl",
		"DB_PASSWORD=short", "SIGNING_KEY=0123456789", "api_key=lowercasevalue1", "DATABASE_DSN=postgres://u:p@h/db",
		"KEYBOARD=us", "MONKEY=banana-republic", "NO_EQUALS",
	})

	assert.Equal(t, []string{"PATH=/usr/bin", "HOME=/home/alex", "KEYBOARD=us", "MONKEY=banana-republic", "NO_EQUALS"}, clean)
	assert.ElementsMatch(t, []string{"ghp_abcdefgh1234", "abcdefghijkl", "0123456789", "lowercasevalue1",
		"postgres://u:p@h/db"}, secrets, "short values are not blanked: they would blank ordinary words")

	empty, _ := Scrub([]string{"A_TOKEN=xxxxxxxxxx"})
	assert.NotNil(t, empty, "nothing left is still an environment, not 'inherit everything'")
}

// The environment a check starts with has no secret in it, and a value that still reaches the output is blanked.
func TestRunKeepsSecretsOutOfTheEnvironmentAndTheOutput(t *testing.T) {
	env := []string{"PATH=" + os.Getenv("PATH"), "MY_API_TOKEN=tok-0123456789", "VISIBLE=yes"}

	r := Run(context.Background(), Spec{Run: "env | sort; echo literal tok-0123456789"}, t.TempDir(), env)
	require.True(t, r.Passed(), r.Output)
	assert.Contains(t, r.Output, "VISIBLE=yes")
	assert.NotContains(t, r.Output, "MY_API_TOKEN")
	assert.NotContains(t, r.Output, "tok-0123456789")
	assert.Contains(t, r.Output, "literal [redacted]")
}

func TestRunWithNoEnvironmentGivenUsesThisProcessesLessItsSecrets(t *testing.T) {
	t.Setenv("ZMINA_TEST_PASSWORD", "hunter2-hunter2")
	t.Setenv("ZMINA_TEST_PLAIN", "plain")

	r := Run(context.Background(), Spec{Run: `echo "[$ZMINA_TEST_PLAIN][$ZMINA_TEST_PASSWORD]"`}, t.TempDir(), nil)
	require.True(t, r.Passed(), r.Output)
	assert.Equal(t, "[plain][]", r.Output)
}

func TestOutputIsTheLastLinesBounded(t *testing.T) {
	r := run(t, "i=1; while [ $i -le 1000 ]; do echo \"line $i\"; i=$((i+1)); done", nil)
	require.True(t, r.Passed())

	lines := strings.Split(r.Output, "\n")
	assert.Len(t, lines, TailLines)
	assert.Equal(t, "line 1000", lines[len(lines)-1])
	assert.Equal(t, "line 961", lines[0])
	assert.True(t, r.Truncated)
	assert.True(t, strings.HasPrefix(r.Tail(), "…\n"), "a cut output says so")
}

func TestOutputOfAnEndlessLineIsBoundedAndLosesOnlyItself(t *testing.T) {
	// 5 MB with no newline, then two short lines: only the end is ever held, and the line that was cut is not shown.
	r := run(t, "head -c 5000000 /dev/zero | tr '\\0' 'x'; echo; echo mid; echo end", nil)
	require.True(t, r.Passed())

	assert.Equal(t, "mid\nend", r.Output)
	assert.True(t, r.Truncated)
}

func TestALongLineIsCut(t *testing.T) {
	r := run(t, "head -c 1000 /dev/zero | tr '\\0' 'z'; echo; echo end", nil)
	require.True(t, r.Passed())

	lines := strings.Split(r.Output, "\n")
	require.Len(t, lines, 2)
	assert.Equal(t, LineRunes, len([]rune(lines[0])))
	assert.True(t, strings.HasSuffix(lines[0], "…"))
	assert.False(t, r.Truncated, "a cut line is not a cut output: nothing was left out before it")
}

func TestOutputBytesAreBoundedWhenLinesAreLong(t *testing.T) {
	r := run(t, "i=0; while [ $i -lt 40 ]; do head -c 290 /dev/zero | tr '\\0' 'y'; echo; i=$((i+1)); done", nil)
	require.True(t, r.Passed())

	assert.LessOrEqual(t, len(r.Output), TailBytes)
	assert.True(t, r.Truncated, "40 lines of 290 bytes are more than the bytes kept")
	assert.True(t, strings.HasPrefix(r.Output, "y"), "it starts at a line, not in the middle of one")
}

func TestOutputIsPlainText(t *testing.T) {
	r := run(t, `printf '\033[31mred\033[0m text\n\033]0;title\007next\nprogress 10%%\rprogress 100%%\nbell\007gone\n'`, nil)
	require.True(t, r.Passed())
	assert.Equal(t, "red text\nnext\nprogress 100%\nbellgone", r.Output)

	r = run(t, `printf 'ok \377\376 bad bytes\n'`, nil)
	require.True(t, r.Passed())
	assert.Contains(t, r.Output, "ok ")
	assert.True(t, utf8Valid(r.Output))
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "") == s }
