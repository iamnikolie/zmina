package donecheck

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

func TestSpecValidate(t *testing.T) {
	assert.NoError(t, Spec{Run: "make test"}.Validate())
	assert.NoError(t, Spec{Run: "make test", Timeout: "30m", Commit: true}.Validate())
	assert.NoError(t, Spec{Run: "  go test ./... && go vet ./...\n"}.Validate(), "the edges are trimmed: a block scalar ends in a line break")

	assert.ErrorContains(t, Spec{}.Validate(), "needs a command")
	assert.ErrorContains(t, Spec{Run: "  \n"}.Validate(), "needs a command")
	assert.ErrorContains(t, Spec{Run: "a\x00b"}.Validate(), "one line of plain text")
	assert.ErrorContains(t, Spec{Run: strings.Repeat("a", runMax+1)}.Validate(), "at most")
	assert.ErrorContains(t, Spec{Run: "x", Timeout: "forever"}.Validate(), "a span such as")
	assert.ErrorContains(t, Spec{Run: "x", Timeout: "-5m"}.Validate(), "a span such as")
	assert.EqualError(t, Spec{Run: "x", Timeout: "31m"}.Validate(), "check timeout 31m: at most 30m", "30m, not 30m0s")
}

// A command is one line of plain text: nothing in it can hide what it runs or be told from what it shows.
func TestSpecRefusesControlCharactersInTheCommand(t *testing.T) {
	for name, run := range map[string]string{
		"line break":         "make test\nrm -rf x",
		"carriage return":    "make test\rmake lint",
		"tab":                "make\ttest",
		"escape sequence":    "make test \x1b[2K",
		"bell":               "make test\a",
		"delete":             "make test\x7f",
		"C1 control":         "make test \u0085 x",
		"C1 CSI":             "make \u009b31m test",
		"right-to-left mark": "make test \u200f",
		"bidi override":      "echo \u202etset ekam",
		"bidi isolate":       "echo \u2066x\u2069",
		"zero width space":   "make\u200b test",
		"byte order mark":    "\ufeffmake test",
		"line separator":     "make test\u2028x",
	} {
		assert.ErrorContains(t, Spec{Run: run}.Validate(), "plain text", name)
	}

	assert.NoError(t, Spec{Run: "echo naïve ünïcode — 日本語"}.Validate(), "text in any script is text")
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
	assert.True(t, r.Interrupted, "stopped by its caller")
	assert.Equal(t, "interrupted", r.Failure(time.Minute), "it did not fail: it was stopped")
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
		"PATH=/usr/bin", "HOME=/home/alex", "GITHUB_TOKEN=fixture-secret-one", "AWS_SECRET_ACCESS_KEY=fixture-secret-two",
		"DB_PASSWORD=short", "SIGNING_KEY=fixture-secret-three", "api_key=lowercase-secret-value", "DATABASE_DSN=db://u:p@h/db",
		"KEYBOARD=us", "MONKEY=banana-republic", "NO_EQUALS",
	})

	assert.Equal(t, []string{"PATH=/usr/bin", "HOME=/home/alex", "KEYBOARD=us", "MONKEY=banana-republic", "NO_EQUALS"}, clean)
	assert.ElementsMatch(t, []string{"fixture-secret-one", "fixture-secret-two", "fixture-secret-three",
		"lowercase-secret-value", "db://u:p@h/db"}, secrets, "short values are not blanked: they would blank ordinary words")

	empty, _ := Scrub([]string{"A_TOKEN=xxxxxxxxxx"})
	assert.NotNil(t, empty, "nothing left is still an environment, not 'inherit everything'")
}

// A name is a secret's by one of its words, not by a few letters inside a word.
func TestSecretNamesAreWholeWords(t *testing.T) {
	for _, name := range []string{
		"TOKEN", "GITHUB_TOKEN", "NPM_TOKEN_FILE", "token", "GITHUBTOKEN", "CLIENT_SECRET", "SECRET_KEY_BASE", "DB_PASSWORD",
		"PASSWORD", "MYSQL_PWD", "PWD_FILE", "DB_PASS", "PASS", "DB_PASSWD", "GPG_PASSPHRASE", "GITHUB_PAT", "PAT", "HTTP_AUTH",
		"AUTH", "AUTH_HEADER", "AUTHORIZATION", "GOOGLE_APPLICATION_CREDENTIALS", "SESSION_COOKIE", "STRIPE_API_KEY", "APIKEY",
		"AWS_ACCESS_KEY_ID", "SSH_PRIVATE_KEY", "PRIVATE_KEY_PATH", "SIGNING_KEY", "KEY", "SENTRY_DSN", "DSN",
	} {
		assert.True(t, IsSecretName(name), name)
	}

	for _, name := range []string{
		"PATH", "HOME", "PWD", "OLDPWD", "GOPRIVATE", "GONOSUMDB", "GONOPROXY", "SSH_AUTH_SOCK", "PASSWORD_STORE_DIR",
		"TOKENIZERS_PARALLELISM", "GIT_ASKPASS", "SSH_ASKPASS", "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "XAUTHORITY",
		"PASSENGER_PORT", "PATTERN", "COMPASS", "KEYBOARD", "MONKEY", "LANG", "DATABASE_URL", "REGISTRY_URL", "PRIVATE",
		"SECRETS_DIR", "CI", "GOFLAGS", "TMPDIR", "XDG_CONFIG_HOME", "SHELL",
	} {
		assert.False(t, IsSecretName(name), name)
	}
}

// What is not a secret stays in the environment and in the output, whatever its name nearly says.
func TestRunKeepsTheNonSecretsThatLookLikeOnes(t *testing.T) {
	env := []string{"PATH=" + os.Getenv("PATH"), "GOPRIVATE=example.com/internal", "PASSWORD_STORE_DIR=/store/pass",
		"TOKENIZERS_PARALLELISM=false", "SSH_AUTH_SOCK=/run/agent.sock", "PWD=/work/here", "DB_PASS=fixture-secret-five"}

	r := Run(context.Background(), Spec{Run: "env | sort"}, t.TempDir(), env)
	require.True(t, r.Passed(), r.Output)

	for _, kept := range []string{"GOPRIVATE=example.com/internal", "PASSWORD_STORE_DIR=/store/pass", "TOKENIZERS_PARALLELISM=false",
		"SSH_AUTH_SOCK=/run/agent.sock"} {
		assert.Contains(t, r.Output, kept)
	}

	assert.NotContains(t, r.Output, "DB_PASS")
	assert.NotContains(t, r.Output, "fixture-secret-five")
}

// An address keeps its password out of the output wherever it is written, and stays in the environment: a check that
// needs a DATABASE_URL has it.
func TestRunBlanksTheRecordedPasswordOfAnAddressNotTheVariable(t *testing.T) {
	env := []string{"PATH=" + os.Getenv("PATH"), "DATABASE_URL=postgres://app:fixture-pass-six@db.example:5432/app"}

	r := Run(context.Background(), Spec{Run: `echo "$DATABASE_URL"; echo clone https://alex:fixture-pass-seven@git.example/x.git; ` +
		`echo ftp://anonymous@ftp.example/pub; echo http://host.example:8080/path; echo "db: $DATABASE_URL" | cut -c1-200; ` +
		`test -n "$DATABASE_URL"`}, t.TempDir(), env)
	require.True(t, r.Passed(), r.Output)

	assert.Contains(t, r.Output, "postgres://app:[redacted]@db.example:5432/app")
	assert.Contains(t, r.Output, "clone https://alex:[redacted]@git.example/x.git")
	assert.NotContains(t, r.Output, "fixture-pass")
	assert.Contains(t, r.Output, "ftp://anonymous@ftp.example/pub", "no password, nothing to blank")
	assert.Contains(t, r.Output, "http://host.example:8080/path", "a port is not a password")
}

// A longer secret that holds a shorter one is blanked whole, not left with a tail.
func TestRunBlanksTheLongerSecretFirst(t *testing.T) {
	env := []string{"PATH=" + os.Getenv("PATH"), "A_SHORT_TOKEN=abcdefgh", "B_LONG_TOKEN=abcdefghXYZXYZXYZ"}

	r := Run(context.Background(), Spec{Run: "echo abcdefghXYZXYZXYZ; echo abcdefgh"}, t.TempDir(), env)
	require.True(t, r.Passed(), r.Output)
	assert.Equal(t, "[redacted]\n[redacted]", r.Output)

	_, secrets := Scrub([]string{"A_SHORT_TOKEN=abcdefgh", "B_LONG_TOKEN=abcdefghXYZXYZXYZ", "C_MID_TOKEN=abcdefghXY"})
	assert.Equal(t, []string{"abcdefghXYZXYZXYZ", "abcdefghXY", "abcdefgh"}, secrets)
}

// A check is told it is one: a zmina that runs inside it acts for no one.
func TestRunMarksItsEnvironment(t *testing.T) {
	env := []string{"PATH=" + os.Getenv("PATH"), InCheckEnv + "=0"}

	r := Run(context.Background(), Spec{Run: "env | grep ^" + InCheckEnv}, t.TempDir(), env)
	require.True(t, r.Passed(), r.Output)
	assert.Equal(t, InCheckEnv+"=1", r.Output, "set once, to 1, whatever it was")
}

// The command is the session leader of its own session, so the group kill still reaches what it started, and nothing
// ties it to the terminal zmina runs in.
func TestRunGivesTheCommandASessionOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "shell.pid")

	done := make(chan Result, 1)

	go func() {
		done <- Run(context.Background(), Spec{Run: "echo $$ > shell.pid; sleep 60 & wait", Timeout: "2s"}, dir, nil)
	}()

	var pid int

	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(pidFile)
		if err != nil || !strings.HasSuffix(string(raw), "\n") {
			return false
		}

		pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))

		return err == nil
	}, 5*time.Second, 10*time.Millisecond)

	sid, err := unix.Getsid(pid)
	require.NoError(t, err)
	assert.Equal(t, pid, sid, "the shell leads its own session")
	assert.NotEqual(t, syscall.Getpid(), pid)

	own, err := unix.Getsid(0)
	require.NoError(t, err)
	assert.NotEqual(t, own, sid, "it is not in zmina's session")

	pgid, err := syscall.Getpgid(pid)
	require.NoError(t, err)
	assert.Equal(t, pid, pgid)

	r := <-done
	assert.True(t, r.TimedOut)
	assert.Eventually(t, func() bool { return syscall.Kill(-pid, 0) != nil }, 3*time.Second, 20*time.Millisecond,
		"the whole group is gone with the timeout")
}

// The environment a check starts with has no secret in it, and a value that still reaches the output is blanked.
func TestRunKeepsSecretsOutOfTheEnvironmentAndTheOutput(t *testing.T) {
	env := []string{"PATH=" + os.Getenv("PATH"), "MY_API_TOKEN=value-for-test-one", "VISIBLE=yes"}

	r := Run(context.Background(), Spec{Run: "env | sort; echo literal value-for-test-one"}, t.TempDir(), env)
	require.True(t, r.Passed(), r.Output)
	assert.Contains(t, r.Output, "VISIBLE=yes")
	assert.NotContains(t, r.Output, "MY_API_TOKEN")
	assert.NotContains(t, r.Output, "value-for-test-one")
	assert.Contains(t, r.Output, "literal [redacted]")
}

func TestRunWithNoEnvironmentGivenUsesThisProcessesLessItsSecrets(t *testing.T) {
	t.Setenv("ZMINA_TEST_PASSWORD", "fixture-secret-four")
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

// Output and commands lose every character that could rewrite what a terminal or a reader sees.
func TestCleanIsPlainText(t *testing.T) {
	assert.Equal(t, "ab", Clean("a\u0085b"), "C1 control")
	assert.Equal(t, "a31mb", Clean("a\u009b31mb"), "C1 CSI is taken out, what follows is text")
	assert.Equal(t, "ab", Clean("a\u202eb"), "bidi override")
	assert.Equal(t, "ab", Clean("a\u200fb\u2066\u2069\u061c"), "marks and isolates")
	assert.Equal(t, "ab", Clean("a\u200bb\ufeff"), "zero width and byte order mark")
	assert.Equal(t, "ab", Clean("a\u2028b\u2029"), "separators")
	assert.Equal(t, "ab", Clean("a\x1b[31mb\x1b[0m"), "escape sequences")
	assert.Equal(t, "a?b", Clean("a\xffb"), "bytes that are no text")
	assert.Equal(t, "a\nb\tc", Clean("a\r\nb\tc"), "line breaks and tabs stay")
	assert.Equal(t, "naïve — 日本語", Clean("naïve — 日本語"))

	assert.Equal(t, "make test && echo done", Line("  make\u202e test\n\t&&  echo   done\x1b[2K "))
	assert.Equal(t, "git clone https://alex:[redacted]@git.example/x", Line("git clone https://alex:fixture-pass-eight@git.example/x"))
}

// What a stored command shows of itself is the same line: a file that was edited by hand shows no raw control either.
func TestSpecStringIsTheSanitisedLine(t *testing.T) {
	s := Spec{Run: "make test\u202e\n\x1b[2Krm -rf x", Timeout: "5m"}
	assert.Equal(t, "make test rm -rf x (5m)", s.String())
}

func TestSpecOff(t *testing.T) {
	for _, run := range []string{"", "  ", "off", "OFF", "None", " none "} {
		assert.True(t, Spec{Run: run}.Off(), run)
	}

	assert.False(t, Spec{Run: "make test"}.Off())
	assert.False(t, Spec{Run: "off", Commit: true}.Off(), "a mapping that says more is no 'off'")
	assert.False(t, Spec{Run: "", Timeout: "5m"}.Off())
	assert.False(t, Spec{Run: "off-site-backup"}.Off())
}
