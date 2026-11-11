package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
