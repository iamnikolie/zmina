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

// The program is the path run gives, else <crew dir>/plugins/<name>, else PATH (zmina-plugin-<name>, or run's word): nowhere else,
// and always an absolute path.
func TestResolveWhereAProgramIsLookedFor(t *testing.T) {
	crewDir, bin := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bin)

	_, err := Resolve(crew.Plugin{Name: "pager"}, crewDir)
	require.ErrorIs(t, err, ErrNotFound)
	assert.ErrorContains(t, err, "zmina-plugin-pager is not on PATH")
	assert.ErrorContains(t, err, filepath.Join(crewDir, "plugins"))

	// PATH, last.
	onPath := script(t, bin, "zmina-plugin-pager", "exit 0\n")
	p, err := Resolve(crew.Plugin{Name: "pager"}, crewDir)
	require.NoError(t, err)
	assert.Equal(t, Program{Path: onPath, Via: "PATH"}, p)

	// The crew dir's plugins/<name> comes before it.
	local := script(t, mkdir(t, crewDir, "plugins"), "pager", "exit 0\n")
	p, err = Resolve(crew.Plugin{Name: "pager"}, crewDir)
	require.NoError(t, err)
	assert.Equal(t, Program{Path: local, Via: "crew dir"}, p)

	// run: a path, absolute or from the crew dir, first of all; a bare word is looked for in plugins/ and then on PATH.
	other := script(t, bin, "bridge", "exit 0\n")
	p, err = Resolve(crew.Plugin{Name: "pager", Run: []string{"bridge", "--quiet", "x"}}, crewDir)
	require.NoError(t, err)
	assert.Equal(t, Program{Path: other, Args: []string{"--quiet", "x"}, Via: "PATH"}, p)

	pinned := script(t, mkdir(t, crewDir, "plugins"), "bridge", "exit 0\n")
	p, err = Resolve(crew.Plugin{Name: "pager", Run: []string{"bridge", "--quiet"}}, crewDir)
	require.NoError(t, err)
	assert.Equal(t, Program{Path: pinned, Args: []string{"--quiet"}, Via: "crew dir"}, p, "the crew's own copy before PATH")

	p, err = Resolve(crew.Plugin{Name: "pager", Run: []string{other}}, crewDir)
	require.NoError(t, err)
	assert.Equal(t, Program{Path: other, Via: "run"}, p, "an absolute path is taken as it is")

	rel := script(t, mkdir(t, crewDir, "tools"), "tell", "exit 0\n")
	p, err = Resolve(crew.Plugin{Name: "pager", Run: []string{"./tools/tell"}}, crewDir)
	require.NoError(t, err)
	assert.Equal(t, rel, p.Path, "a relative path is read from the crew dir")
	assert.True(t, filepath.IsAbs(p.Path))

	_, err = Resolve(crew.Plugin{Name: "pager", Run: []string{"nothing-like-it"}}, crewDir)
	require.ErrorIs(t, err, ErrNotFound)
	assert.ErrorContains(t, err, "nothing-like-it is not on PATH")

	_, err = Resolve(crew.Plugin{Name: "pager", Run: []string{"/nowhere/at/all"}}, crewDir)
	require.ErrorIs(t, err, ErrNotFound)

	// With run given, the lookup by name is not made.
	_, err = Resolve(crew.Plugin{Name: "other", Run: []string{"nothing-like-it"}}, crewDir)
	assert.Error(t, err)
}

// A PATH with a relative entry still gives an absolute path.
// run is an argv with no shell, so a leading ~ in it is the home of whoever runs zmina: in the first word (a path) and in every argument
// after it, under the same rule as every other command of the crew file given as a list. The crew's own list is not rewritten.
func TestResolveExpandsAHomeInTheArgumentsOfRun(t *testing.T) {
	home, crewDir := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)

	bin := mkdir(t, home, "bin")
	program := script(t, bin, "pager", "exit 0\n")
	run := []string{"~/bin/pager", "~/data", "--dir=~/x", "~bob/y", "a~/b", "~"}

	p, err := Resolve(crew.Plugin{Name: "pager", Run: run}, crewDir)
	require.NoError(t, err)
	assert.Equal(t, program, p.Path)
	assert.Equal(t, []string{filepath.Join(home, "data"), "--dir=~/x", "~bob/y", "a~/b", home}, p.Args)
	assert.Equal(t, []string{"~/bin/pager", "~/data", "--dir=~/x", "~bob/y", "a~/b", "~"}, run, "the crew's own list is as written")
}

func TestResolveReturnsAnAbsolutePath(t *testing.T) {
	crewDir, bin := t.TempDir(), t.TempDir()
	script(t, bin, "zmina-plugin-rel", "exit 0\n")
	t.Chdir(bin)
	t.Setenv("PATH", ".")

	p, err := Resolve(crew.Plugin{Name: "rel"}, crewDir)
	if err != nil {
		t.Skipf("this go refuses a relative PATH entry: %v", err)
	}

	assert.True(t, filepath.IsAbs(p.Path), p.Path)
}

type ownedFile struct {
	os.FileInfo
	uid uint32
}

func (f ownedFile) Sys() any { return &syscall.Stat_t{Uid: f.uid} }

// A program owned by someone else than the user or root is refused: whoever owns a file may change it.
func TestCheckRefusesAProgramOwnedBySomeoneElse(t *testing.T) {
	path := script(t, t.TempDir(), "pager", "exit 0\n")
	fi, err := os.Stat(path)
	require.NoError(t, err)

	me := os.Getuid()
	require.NoError(t, checkOwner(path, ownedFile{fi, uint32(me)}, me), "the user's own")
	require.NoError(t, checkOwner(path, ownedFile{fi, 0}, me), "root's")

	err = checkOwner(path, ownedFile{fi, uint32(me + 4242)}, me)
	require.Error(t, err)
	assert.ErrorContains(t, err, "not by you or root")
	assert.ErrorContains(t, err, strconv.Itoa(me+4242))

	assert.NoError(t, Check(path), "a real file of the user's own")
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

	// One found on PATH is checked as well, and a refused one in the crew dir is not passed over for the one on PATH.
	require.NoError(t, os.RemoveAll(path))
	good := script(t, bin, "zmina-plugin-pager", "exit 0\n")
	loose := script(t, dir, "pager", "exit 0\n")
	require.NoError(t, os.Chmod(loose, 0o777))
	_, err = Resolve(crew.Plugin{Name: "pager"}, crewDir)
	assert.ErrorContains(t, err, "writable by group or others", "the crew's own is refused, not skipped")

	require.NoError(t, os.Remove(loose))
	require.NoError(t, os.Chmod(good, 0o777))
	_, err = Resolve(crew.Plugin{Name: "pager"}, crewDir)
	assert.ErrorContains(t, err, "writable by group or others", "and the one on PATH too")

	// A symlink is judged by what it points at.
	require.NoError(t, os.Remove(good))
	target := script(t, t.TempDir(), "real", "exit 0\n")
	require.NoError(t, os.Symlink(target, good))
	p, err := Resolve(crew.Plugin{Name: "pager"}, crewDir)
	require.NoError(t, err)
	assert.Equal(t, good, p.Path)
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

	// Long enough for a loaded machine to start the script and let it write its child's pid before the deadline.
	start := time.Now()
	res := Exec(spec(slow, 2*time.Second, ""))

	assert.True(t, res.TimedOut)
	assert.False(t, res.OK())
	assert.Equal(t, "timed out after 2s", res.Reason(2*time.Second))
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

// A plugin that exits 0 and leaves a child holding its stdout (a send put in the background) has taken the event: it is no
// failure, and not retried.
func TestExecExitZeroWithAChildOnItsPipesIsTaken(t *testing.T) {
	keep := waitDelay
	waitDelay = 300 * time.Millisecond

	t.Cleanup(func() { waitDelay = keep })

	dir := t.TempDir()
	bg := script(t, dir, "bg", "echo sent\nsleep 3 &\nexit 0\n")

	res := Exec(spec(bg, 10*time.Second, ""))
	assert.True(t, res.OK(), "%+v", res)
	assert.Zero(t, res.Code)
	assert.NoError(t, res.Err)
	assert.False(t, res.TimedOut)
	assert.Equal(t, "sent\n", res.Stdout)

	// The same with a failing exit is a failure still.
	bad := script(t, dir, "bgbad", "sleep 3 &\nexit 2\n")
	res = Exec(spec(bad, 10*time.Second, ""))
	assert.False(t, res.OK())
	assert.Equal(t, 2, res.Code)
}

// The delivery lock is taken by one run at a time; a look at it says who holds it and since when, and holds nothing itself.
func TestLockAndHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugins", "pager.lock")
	at := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	held, _, _ := Holder(path)
	assert.False(t, held, "no file: no holder")

	release, ok := Lock(path, at)
	require.True(t, ok)

	_, again := Lock(path, at)
	assert.False(t, again, "one at a time")

	held, pid, since := Holder(path)
	assert.True(t, held)
	assert.Equal(t, os.Getpid(), pid)
	assert.True(t, since.Equal(at))

	release()

	held, _, _ = Holder(path)
	assert.False(t, held, "free again, whatever the file still says")

	release2, ok := Lock(path, at)
	require.True(t, ok, "the look took nothing")
	release2()
}
