package plugin

import (
	"os"
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

// script writes an executable shell script and returns its path.
func script(t *testing.T, dir, name, body string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700))

	return path
}

func spec(path string, timeout time.Duration, stdin string, args ...string) Spec {
	return Spec{Program: Program{Path: path, Args: args}, Env: os.Environ(), Stdin: []byte(stdin), Timeout: timeout}
}

// The program is run's first word, else zmina-plugin-<name> on PATH, else <crew dir>/plugins/<name>: nowhere else.
func TestResolveWhereAProgramIsLookedFor(t *testing.T) {
	crewDir, bin := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin)

	_, err := Resolve(crew.Plugin{Name: "pager"}, crewDir)
	require.ErrorIs(t, err, ErrNotFound)
	assert.ErrorContains(t, err, "zmina-plugin-pager is not on PATH")
	assert.ErrorContains(t, err, filepath.Join(crewDir, "plugins", "pager"))

	// The crew dir's plugins/<name>.
	local := script(t, mkdir(t, crewDir, "plugins"), "pager", "exit 0\n")
	p, err := Resolve(crew.Plugin{Name: "pager"}, crewDir)
	require.NoError(t, err)
	assert.Equal(t, Program{Path: local, Via: "crew dir"}, p)

	// PATH comes first.
	onPath := script(t, bin, "zmina-plugin-pager", "exit 0\n")
	p, err = Resolve(crew.Plugin{Name: "pager"}, crewDir)
	require.NoError(t, err)
	assert.Equal(t, Program{Path: onPath, Via: "PATH"}, p)

	// run: a name on PATH with its arguments; a path, absolute or from the crew dir.
	other := script(t, bin, "bridge", "exit 0\n")
	p, err = Resolve(crew.Plugin{Name: "pager", Run: []string{"bridge", "--quiet", "x"}}, crewDir)
	require.NoError(t, err)
	assert.Equal(t, Program{Path: other, Args: []string{"--quiet", "x"}, Via: "run"}, p)

	p, err = Resolve(crew.Plugin{Name: "pager", Run: []string{other}}, crewDir)
	require.NoError(t, err)
	assert.Equal(t, other, p.Path)

	rel := script(t, mkdir(t, crewDir, "tools"), "tell", "exit 0\n")
	p, err = Resolve(crew.Plugin{Name: "pager", Run: []string{"./tools/tell"}}, crewDir)
	require.NoError(t, err)
	assert.Equal(t, rel, p.Path, "a relative path is read from the crew dir")

	_, err = Resolve(crew.Plugin{Name: "pager", Run: []string{"nothing-like-it"}}, crewDir)
	require.ErrorIs(t, err, ErrNotFound)
	assert.ErrorContains(t, err, "run's nothing-like-it is not on PATH")

	_, err = Resolve(crew.Plugin{Name: "pager", Run: []string{"/nowhere/at/all"}}, crewDir)
	require.ErrorIs(t, err, ErrNotFound)

	// With run given, the lookup by name is not made.
	_, err = Resolve(crew.Plugin{Name: "pager", Run: []string{"nothing-like-it"}}, crewDir)
	assert.Error(t, err)
}

func mkdir(t *testing.T, parent, name string) string {
	t.Helper()

	dir := filepath.Join(parent, name)
	require.NoError(t, os.MkdirAll(dir, 0o700))

	return dir
}

// A program that is not a regular executable file, or that anyone but its owner could change, is refused, with why; and
// it is not passed over for another place.
func TestResolveRefusesWhatIsNotSafe(t *testing.T) {
	crewDir, bin := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin)
	dir := mkdir(t, crewDir, "plugins")
	path := script(t, dir, "pager", "exit 0\n")

	for mode, want := range map[os.FileMode]string{
		0o700 | 0o020: "writable by group or others",
		0o700 | 0o002: "writable by group or others",
		0o600:         "not executable",
	} {
		require.NoError(t, os.Chmod(path, mode))

		_, err := Resolve(crew.Plugin{Name: "pager"}, crewDir)
		require.Error(t, err, "%o", mode)
		assert.ErrorContains(t, err, want, "%o", mode)
		assert.NotErrorIs(t, err, ErrNotFound)
	}

	require.NoError(t, os.Chmod(path, 0o755))
	_, err := Resolve(crew.Plugin{Name: "pager"}, crewDir)
	require.NoError(t, err, "readable and executable by all is fine; only writing is not")

	// A directory is not a program.
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	_, err = Resolve(crew.Plugin{Name: "pager"}, crewDir)
	assert.ErrorContains(t, err, "not a regular file")

	// One found on PATH is checked as well, and does not fall through to the crew dir's.
	require.NoError(t, os.Remove(path))
	script(t, dir, "pager", "exit 0\n")
	loose := script(t, bin, "zmina-plugin-pager", "exit 0\n")
	require.NoError(t, os.Chmod(loose, 0o777))
	_, err = Resolve(crew.Plugin{Name: "pager"}, crewDir)
	assert.ErrorContains(t, err, "writable by group or others")

	// A symlink is judged by what it points at.
	require.NoError(t, os.Remove(loose))
	target := script(t, t.TempDir(), "real", "exit 0\n")
	require.NoError(t, os.Symlink(target, loose))
	p, err := Resolve(crew.Plugin{Name: "pager"}, crewDir)
	require.NoError(t, err)
	assert.Equal(t, loose, p.Path)
}

// One JSON line goes on stdin; exit 0 is taken; what it prints is kept, bounded; the exit code is reported.
func TestExecTakesAndReports(t *testing.T) {
	dir := t.TempDir()
	got := filepath.Join(dir, "got")
	ok := script(t, dir, "ok", `cat > `+got+"\necho taken; echo warn >&2\n")

	res := Exec(spec(ok, 5*time.Second, `{"v":1}`+"\n"))
	assert.True(t, res.OK())
	assert.Zero(t, res.Code)
	assert.Equal(t, "taken\n", res.Stdout)
	assert.Equal(t, "warn\n", res.Stderr)
	assert.Equal(t, `{"v":1}`+"\n", string(mustRead(t, got)))

	bad := script(t, dir, "bad", "echo nope >&2; exit 3\n")
	res = Exec(spec(bad, 5*time.Second, ""))
	assert.False(t, res.OK())
	assert.Equal(t, 3, res.Code)
	assert.Equal(t, "exit 3: nope", res.Reason(5*time.Second))

	missing := Exec(spec(filepath.Join(dir, "gone"), 5*time.Second, ""))
	assert.False(t, missing.OK())
	require.Error(t, missing.Err)
	assert.Equal(t, -1, missing.Code)
	assert.Contains(t, missing.Reason(time.Second), "no such file")

	sig := script(t, dir, "sig", "kill -9 $$\n")
	res = Exec(spec(sig, 5*time.Second, ""))
	assert.False(t, res.OK())
	assert.Contains(t, res.Reason(time.Second), "signal")

	// A program that does not read its stdin is no failure.
	deaf := script(t, dir, "deaf", "exit 0\n")
	assert.True(t, Exec(spec(deaf, 5*time.Second, strings.Repeat("x", 1<<20))).OK())
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	return raw
}

// What a plugin prints is kept to OutputMax each, and a plugin that prints without end is not waited on forever.
func TestExecBoundsWhatItKeeps(t *testing.T) {
	dir := t.TempDir()
	loud := script(t, dir, "loud", "head -c 200000 /dev/zero | tr '\\0' x; head -c 200000 /dev/zero | tr '\\0' y >&2\n")

	res := Exec(spec(loud, 10*time.Second, ""))
	assert.True(t, res.OK())
	assert.Len(t, res.Stdout, OutputMax)
	assert.Len(t, res.Stderr, OutputMax)
}

// The deadline ends the whole process group: a child the plugin started does not outlive it.
func TestExecDeadlineKillsTheGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	slow := script(t, dir, "slow", "sleep 60 &\necho $! > "+pidFile+"\nwait\n")

	start := time.Now()
	res := Exec(spec(slow, 400*time.Millisecond, ""))

	assert.True(t, res.TimedOut)
	assert.False(t, res.OK())
	assert.Equal(t, "timed out after 400ms", res.Reason(400*time.Millisecond))
	assert.Less(t, time.Since(start), 10*time.Second, "not waited for the sleep")

	var pid int

	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}

		pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))

		return err == nil
	}, 5*time.Second, 20*time.Millisecond)

	require.Eventually(t, func() bool { return syscall.Kill(pid, 0) != nil }, 5*time.Second, 20*time.Millisecond,
		"the child the plugin started is gone with it")
}

// The environment is the caller's less zmina's own secrets, plus the plugin's own variables, whatever the caller held of them.
func TestEnv(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/home/alex", "ZMINA_UI_TOKEN=secret", "ZMINA_UI_CODE_COMMAND=x", "ZMINA_PLUGIN=other",
		"ZMINA_PLUGIN_EVENT=old", "ZMINA_CREW=/old"}

	env := Env(base, "pager", "ask", "/c/crew.yaml")
	assert.Contains(t, env, "PATH=/bin")
	assert.Contains(t, env, "HOME=/home/alex")
	assert.NotContains(t, strings.Join(env, "\n"), "ZMINA_UI_")
	assert.NotContains(t, strings.Join(env, "\n"), "secret")

	dir := t.TempDir()
	printer := script(t, dir, "env", "echo \"$ZMINA_PLUGIN|$ZMINA_PLUGIN_EVENT|$ZMINA_CREW|${ZMINA_UI_TOKEN:-unset}\"\n")
	s := spec(printer, 5*time.Second, "")
	s.Env = env

	res := Exec(s)
	assert.Equal(t, "pager|ask|/c/crew.yaml|unset\n", res.Stdout, "the last of two values of a name wins")
}

// The reason a run failed is one clean line of what it printed: no control character, cut.
func TestReasonIsOneCleanLine(t *testing.T) {
	r := Result{Code: 1, Stderr: "\x1b[31mred\x1b[0m\tfailure\x07\nsecond line"}
	assert.Equal(t, "exit 1: [31mred[0m failure", r.Reason(time.Second))

	long := Result{Code: 1, Stdout: strings.Repeat("é", 500)}
	assert.Len(t, []rune(strings.TrimPrefix(long.Reason(time.Second), "exit 1: ")), 200)

	assert.Equal(t, "exit 5", Result{Code: 5}.Reason(time.Second))
	assert.Empty(t, Result{}.Reason(time.Second), "taken: no reason")
}
