package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAuth is a claude that passes doctor's flag check and answers auth status with the given JSON.
func fakeAuth(t *testing.T, dir, status string) {
	t.Helper()

	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = auth ]; then echo '%s'; exit 0; fi
echo '--name --resume --remote-control --model'
`, status)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700))
}

func TestDoctorLogin(t *testing.T) {
	crewFile := testCrew(t, "")
	dir := filepath.Dir(crewFile)

	fakeAuth(t, dir, `{"loggedIn": false}`)
	out, _, err := zm(t, crewFile, "doctor")
	assert.Contains(t, out, "fail  login")
	assert.Contains(t, out, "is not logged in")
	assert.Error(t, err)

	soon := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	fakeAuth(t, dir, `{"loggedIn": true, "loginExpiresAt": "`+soon+`"}`)
	out, _, _ = zm(t, crewFile, "doctor")
	assert.Contains(t, out, "warn  login")
	assert.Contains(t, out, "the login expires")

	later := time.Now().Add(60 * 24 * time.Hour).UTC().Format(time.RFC3339)
	fakeAuth(t, dir, `{"loggedIn": true, "loginExpiresAt": "`+later+`"}`)
	out, _, _ = zm(t, crewFile, "doctor")
	assert.Contains(t, out, "ok    login")

	fakeAuth(t, dir, `not json`)
	out, _, _ = zm(t, crewFile, "doctor")
	assert.Contains(t, out, "warn  login")
	assert.Contains(t, out, "could not tell")
}

func TestDoctorHygiene(t *testing.T) {
	crewFile := testCrew(t, "")
	dir := filepath.Dir(crewFile)
	fakeAuth(t, dir, `{"loggedIn": true}`)

	out, _, err := zm(t, crewFile, "doctor")
	require.NoError(t, err)
	for _, name := range []string{"asks", "later", "worktrees"} {
		assert.NotContains(t, out, "warn  "+name, "a fresh crew has nothing piled up")
	}

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	now := time.Now()
	old := now.Add(-20 * 24 * time.Hour)

	_, err = askStore(c).Add(asks.Ask{Kind: asks.Check, Text: "old check"}, old)
	require.NoError(t, err)
	_, err = askStore(c).Add(asks.Ask{Kind: asks.Check, Text: "new check"}, now)
	require.NoError(t, err)
	_, err = laterStore(c).Add(later.Item{Text: "old idea"}, old)
	require.NoError(t, err)

	wt := filepath.Join(dir, "wt")
	require.NoError(t, os.MkdirAll(wt, 0o700))

	done, err := taskStore(c).Add(task.Task{Role: "dispatcher", Asked: "x", Worktree: wt}, old)
	require.NoError(t, err)
	done.State = task.Done
	require.NoError(t, taskStore(c).Put(done))
	_, err = taskStore(c).Add(task.Task{Role: "dispatcher", Asked: "y", Worktree: wt}, old)
	require.NoError(t, err)

	out, _, _ = zm(t, crewFile, "doctor")
	assert.NotContains(t, out, "copies of the crew file", "zmina keeps no copy beside the file now, only crew.d/history")
	assert.Contains(t, out, "warn  asks      1 open for the owner over 7 days (A-1)")
	assert.Contains(t, out, "warn  later     1 parked over 14 days (L-1)")
	assert.Contains(t, out, "warn  worktrees 1 closed tasks keep their worktree", "an open task's worktree is not counted")
	assert.Contains(t, out, "worktree_gc: daily in the crew file", "the warning names the key that sweeps them")

	require.NoError(t, os.WriteFile(crewFile+".bak", []byte("x"), 0o600))

	out, _, _ = zm(t, crewFile, "doctor")
	assert.Contains(t, out, "warn  crew      1 copy of the crew file next to it (crew.yaml.bak)")
	assert.Contains(t, out, "zmina crew history", "the warning names the command that keeps versions now")

	require.NoError(t, os.WriteFile(crewFile+".bak-by-hand", []byte("x"), 0o600))

	out, _, _ = zm(t, crewFile, "doctor")
	assert.Contains(t, out, "warn  crew      2 copies of the crew file next to it")
}

// With worktree_gc: daily the worktrees line is ok: it says when the sweep last began, and how many closed tasks still
// keep a worktree (what the sweep's checks keep).
func TestDoctorWorktreeGC(t *testing.T) {
	crewFile := testCrew(t, "")
	dir := filepath.Dir(crewFile)
	fakeAuth(t, dir, `{"loggedIn": true}`)

	body := string(mustRead(t, crewFile))
	require.NoError(t, os.WriteFile(crewFile, []byte(strings.Replace(body, "roles:\n", "worktree_gc: daily\nroles:\n", 1)), 0o600))

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	out, _, err := zm(t, crewFile, "doctor")
	require.NoError(t, err)
	assert.Contains(t, out, "ok    worktrees worktree_gc: daily · no sweep yet\n")

	wt := filepath.Join(dir, "wt")
	require.NoError(t, os.MkdirAll(wt, 0o700))

	done, err := taskStore(c).Add(task.Task{Role: "dispatcher", Asked: "x", Worktree: wt}, time.Now())
	require.NoError(t, err)
	done.State = task.Done
	require.NoError(t, taskStore(c).Put(done))

	at := time.Date(2026, 11, 10, 4, 5, 0, 0, c.Location())

	l, err := shift.Lock(c.StateDir())
	require.NoError(t, err)

	l.WorktreeGCAt = at
	require.NoError(t, l.Save())

	out, _, err = zm(t, crewFile, "doctor")
	require.NoError(t, err)
	assert.Contains(t, out, "ok    worktrees worktree_gc: daily · last sweep 2026-11-10 04:05 · 1 closed tasks still keep theirs")
	assert.NotContains(t, out, "warn  worktrees", "what the sweep keeps is not a warning")
}

// A claude whose child holds stdout (in the background, or after claude exits) does not hold doctor past the
// deadline.
func TestDoctorLoginDeadlineHoldsForChildren(t *testing.T) {
	oldTimeout, oldDelay := loginTimeout, loginWaitDelay
	// Long enough for macOS to look at a new executable before its first run.
	loginTimeout, loginWaitDelay = 2*time.Second, 300*time.Millisecond
	t.Cleanup(func() { loginTimeout, loginWaitDelay = oldTimeout, oldDelay })

	for name, script := range map[string]string{
		"waits for its child": "sleep 30 &\necho $! > %s\nwait\n",
		"leaves its child":    "echo '{\"loggedIn\": true}'\nsleep 30 &\necho $! > %s\n",
	} {
		t.Run(name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "claude")
			require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"+fmt.Sprintf(script, bin+".child")), 0o700))

			var lines []string
			start := time.Now()
			checkLogin(bin, "ns", time.Now(), func(name, level, format string, args ...any) {
				lines = append(lines, level+" "+fmt.Sprintf(format, args...))
			})

			assert.Less(t, time.Since(start), 10*time.Second, "bounded by the deadline, not the child")
			require.Len(t, lines, 1)

			raw, err := os.ReadFile(bin + ".child")
			require.NoError(t, err)
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			require.NoError(t, err)
			assert.Eventually(t, func() bool { return syscall.Kill(pid, 0) != nil }, 2*time.Second, 20*time.Millisecond,
				"the child is not left running")
		})
	}
}

// A role that names no model is a warning, not a failure; one that does, in model: or in its args, is not named.
func TestDoctorModels(t *testing.T) {
	crewFile := testCrew(t, "  pinned:\n    model: sonnet\n  by-args:\n    args: [--model, opus]\n  by-flag:\n    args: [--model=opus]\n"+
		"  open:\n  open2: {}\n")
	fakeAuth(t, filepath.Dir(crewFile), `{"loggedIn": true}`)

	out, _, err := zm(t, crewFile, "doctor")
	require.NoError(t, err, "a role without a model does not fail doctor")
	assert.Contains(t, out, "warn  models    3 roles with no model: (dispatcher, open, open2) run on whatever default")
	assert.NotContains(t, out, "pinned")
	assert.NotContains(t, out, "by-args")

	body := string(mustRead(t, crewFile))
	body = strings.Replace(body, "  dispatcher:\n    boot: hello\n", "  dispatcher:\n    boot: hello\n    model: sonnet\n", 1)
	body = strings.Replace(body, "  open:\n", "  open:\n    model: opus\n", 1)
	body = strings.Replace(body, "  open2: {}\n", "  open2:\n    model: opus\n", 1)
	require.NoError(t, os.WriteFile(crewFile, []byte(body), 0o600))

	out, _, err = zm(t, crewFile, "doctor")
	require.NoError(t, err)
	assert.Contains(t, out, "ok    models    every role names its model")
}

// A Codex seat is pinned by its model or by a profile, which carries one of its own.
func TestCheckModelsCodexSeats(t *testing.T) {
	crewFile := testCrew(t, "  cdx:\n    runtime: codex\n  cdx-model:\n    runtime: codex\n    model: gpt-x\n"+
		"  cdx-profile:\n    runtime: codex\n    profile: fast\n")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	var lines []string

	checkModels(c, func(name, level, format string, args ...any) {
		lines = append(lines, level+" "+name+" "+fmt.Sprintf(format, args...))
	})

	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "warn models 2 roles with no model: (dispatcher, cdx)")
}

// fakeClaudeVersion is a claude that passes doctor's flag check, is logged in and answers --version with the given text
// (a script of its own when it starts with #!).
func fakeClaudeVersion(t *testing.T, dir, version string) {
	t.Helper()

	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = auth ]; then echo '{"loggedIn": true}'; exit 0; fi
if [ "$1" = --version ]; then %s; exit 0; fi
echo '--name --resume --remote-control --model'
`, version)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700))
}

// doctor prints the installed Claude Code and warns, never fails, outside the range this build was tried with.
func TestDoctorClaudeCodeVersion(t *testing.T) {
	crewFile := testCrew(t, "")
	dir := filepath.Dir(crewFile)

	old := claudeCodeTested
	claudeCodeTested = versionRange{min: "2.1.224", max: "2.1.322"}
	t.Cleanup(func() { claudeCodeTested = old })

	for name, tc := range map[string]struct{ says, level, want string }{
		"in range":      {"echo '2.1.300 (Claude Code)'", "ok", "Claude Code 2.1.300 (this zmina was tried with 2.1.224 to 2.1.322)"},
		"the newest":    {"echo '2.1.322 (Claude Code)'", "ok", "Claude Code 2.1.322"},
		"newer":         {"echo '2.1.323 (Claude Code)'", "warn", "Claude Code 2.1.323 is newer than the newest this zmina was tried with (2.1.224 to 2.1.322)"},
		"newer minor":   {"echo '2.2.0 (Claude Code)'", "warn", "is newer than the newest"},
		"older":         {"echo '2.1.100 (Claude Code)'", "warn", "Claude Code 2.1.100 is older than the oldest"},
		"no number":     {"echo 'claude, a coding agent'", "warn", "could not read the version"},
		"fails":         {"echo nope >&2; exit 3", "warn", "could not read the version"},
		"two-digit one": {"echo '10.0.0'", "warn", "is newer than the newest"},
	} {
		t.Run(name, func(t *testing.T) {
			fakeClaudeVersion(t, dir, tc.says)

			out, _, _ := zm(t, crewFile, "doctor")
			assert.NotContains(t, out, "fail  claude", "a version out of range is a warning")
			assert.Regexp(t, "(?m)^"+tc.level+" +claude +.*"+regexp.QuoteMeta(tc.want), out)
		})
	}
}

// A claude that never answers --version does not hold doctor: the check is bounded, and says so.
func TestDoctorVersionCommandHasADeadline(t *testing.T) {
	oldT, oldD := versionTimeout, versionWaitDelay
	versionTimeout, versionWaitDelay = time.Second, 200*time.Millisecond
	t.Cleanup(func() { versionTimeout, versionWaitDelay = oldT, oldD })

	bin := filepath.Join(t.TempDir(), "claude")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30 &\necho $! > "+bin+".child\nwait\n"), 0o700))

	var lines []string

	start := time.Now()
	checkRelease("claude", "Claude Code", bin, "", versionRange{max: "2.1.322"}, func(name, level, format string, args ...any) {
		lines = append(lines, level+" "+fmt.Sprintf(format, args...))
	})

	assert.Less(t, time.Since(start), 10*time.Second)
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "warn Claude Code: could not read the version")
	assert.Contains(t, lines[0], "no answer in 1s")

	raw, err := os.ReadFile(bin + ".child")
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return syscall.Kill(pid, 0) != nil }, 2*time.Second, 20*time.Millisecond,
		"the child is not left running")
}

// Codex is read when a role runs on it; a range with no lower end has no lower bound.
func TestCheckReleaseCodex(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho 'codex-cli 0.150.2'\n"), 0o700))

	var lines []string

	checkRelease("codex", "Codex", bin, "", versionRange{max: "0.167.0"}, func(name, level, format string, args ...any) {
		lines = append(lines, level+" "+name+" "+fmt.Sprintf(format, args...))
	})

	require.Len(t, lines, 1)
	assert.Equal(t, "ok codex Codex 0.150.2 (this zmina was tried with up to 0.167.0)", lines[0])
}

// A crew that never said anything of notify gets the warning; one that says notify: off gets a plain line; one with a
// command gets neither.
func TestDoctorNotifyWording(t *testing.T) {
	crewFile := testCrew(t, "")
	fakeAuth(t, filepath.Dir(crewFile), `{"loggedIn": true}`)

	out, _, err := zm(t, crewFile, "doctor")
	require.NoError(t, err)
	assert.Contains(t, out, "warn  alarms    no notify command:")
	assert.Contains(t, out, "notify: off if you want none")

	body := string(mustRead(t, crewFile))
	require.NoError(t, os.WriteFile(crewFile, []byte(strings.Replace(body, "roles:\n", "notify: off\nroles:\n", 1)), 0o600))

	out, _, err = zm(t, crewFile, "doctor")
	require.NoError(t, err)
	assert.Contains(t, out, "ok    alarms    no notify channel set: nothing is pushed to the phone\n")
	assert.NotContains(t, out, "warn  alarms")

	require.NoError(t, os.WriteFile(crewFile, []byte(strings.Replace(body, "roles:\n", "notify: [push, --title, zmina]\nroles:\n", 1)), 0o600))

	out, _, err = zm(t, crewFile, "doctor")
	require.NoError(t, err)
	assert.NotContains(t, out, "notify")
}
