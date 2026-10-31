package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// zm runs one command line against a crew file, as a fresh process would.
func zm(t *testing.T, crewFile string, args ...string) (string, string, error) {
	t.Helper()

	var out, errb bytes.Buffer

	root := NewRoot(&out, &errb)
	root.SetArgs(append([]string{"--crew", crewFile}, args...))
	err := root.Execute()

	return out.String(), errb.String(), err
}

// fakeClaude prints its arguments and an input line (a ❯ under a rule), logs every line it takes to typed.log next
// to itself, echoes it and ends on /exit. C-c drops what is typed and draws a fresh input line, as Claude Code does.
// bash, not sh: dash (Debian's sh) runs the INT trap only once read returns, so C-c would draw nothing.
const fakeClaude = `#!/usr/bin/env bash
echo "claude $*"
prompt() { printf '───\n❯ '; }
trap 'echo; prompt' INT
prompt
while :; do
  read l || { [ $? -gt 128 ] && continue; exit 0; }
  echo "$l" >> "$(dirname "$0")/typed.log"
  [ "$l" = /exit ] && exit 0
  echo "got: $l"
  prompt
done
`

// testCrew writes a crew file in a temp dir with its own tmux server and a fake claude (fakeClaude), and returns
// its path. The tmux server is killed after the test.
func testCrew(t *testing.T, roles string) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("ZMINA_ROLE", "")

	dir := t.TempDir()
	name := fmt.Sprintf("t%d", time.Now().UnixNano()%1_000_000_000)

	claude := filepath.Join(dir, "claude")
	require.NoError(t, os.WriteFile(claude, []byte(fakeClaude), 0o700))

	path := filepath.Join(dir, "crew.yaml")
	require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf(`name: %s
namespace: %s
dir: %s
claude: %s
remote_control: false
roles:
  dispatcher:
    boot: hello
%s`, name, filepath.Join(dir, "ns"), dir, claude, roles)), 0o600))

	t.Cleanup(func() { _ = exec.Command("tmux", "-L", "zmina-"+name, "kill-server").Run() })

	return path
}

func TestWatchDryRunWritesNothing(t *testing.T) {
	crewFile := testCrew(t, `watch:
  ci-red:
    kind: exec
    every: 5m
    baseline: false
    run: "printf '%s\n' '{\"key\":\"main\",\"fingerprint\":\"red\",\"text\":\"pipeline red\"}' '{\"key\":\"dev\",\"fingerprint\":\"1\",\"text\":\"x\"}'"
`)

	out, _, err := zm(t, crewFile, "watch", "run", "--dry-run", "ci-red")
	require.NoError(t, err)
	assert.Equal(t, "note         main  ci-red: pipeline red\nnote         dev  ci-red: x\n", out,
		"every event with its fate")
	assert.NoFileExists(t, filepath.Join(filepath.Dir(crewFile), "state.json"), "a dry run writes no state")
}

func TestTaskWorktreeFlow(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n")
	repo := filepath.Join(filepath.Dir(crewFile), "api")
	git := func(dir string, args ...string) {
		t.Helper()

		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"},
			args...)...).CombinedOutput()
		require.NoError(t, err, string(out))
	}

	require.NoError(t, os.MkdirAll(repo, 0o700))
	git(repo, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a"), []byte("a"), 0o600))
	git(repo, "add", ".")
	git(repo, "commit", "-q", "-m", "a")

	out, _, err := zm(t, crewFile, "task", "new", "dev", "--repo", "api", "--worktree", "fix the login")
	require.NoError(t, err)
	assert.Contains(t, out, "T-1 · dev · worktree ")
	assert.Contains(t, out, "-t-1 from main", "a named crew's branch carries its name")

	wt := strings.Fields(strings.SplitN(out, "worktree ", 2)[1])[0]
	require.NoError(t, os.WriteFile(filepath.Join(wt, "b"), []byte("b"), 0o600))

	_, _, err = zm(t, crewFile, "task", "done", "1")
	require.ErrorContains(t, err, "uncommitted changes")

	git(wt, "add", ".")
	git(wt, "commit", "-q", "-m", "b")

	out, _, err = zm(t, crewFile, "task", "done", "T-1", "--note", "shipped")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev · done\n", out)

	out, _, err = zm(t, crewFile, "task", "gc")
	require.NoError(t, err)
	assert.Contains(t, out, "kept: branch")

	branch := strings.TrimSpace(strings.Split(strings.Split(string(mustRead(t, filepath.Join(filepath.Dir(crewFile),
		"tasks", "T-1.json"))), `"branch": "`)[1], `"`)[0])
	git(repo, "merge", "-q", "--ff-only", branch)

	out, _, err = zm(t, crewFile, "task", "gc")
	require.NoError(t, err)
	assert.Contains(t, out, "T-1: removed")

	out, _, err = zm(t, crewFile, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "note: shipped")
	assert.Contains(t, out, "worktree removed")

	out, _, err = zm(t, crewFile, "log", "--task", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "new: fix the login")
}

func TestSeatFlow(t *testing.T) {
	crewFile := testCrew(t, "")

	_, _, err := zm(t, crewFile, "seat", "add", "temp", "--about", "a temp seat", "--expires", "3d")
	require.NoError(t, err)

	_, _, err = zm(t, crewFile, "seat", "set", "temp", "model=opus", "remote_control=true")
	require.NoError(t, err)

	body := string(mustRead(t, crewFile))
	assert.Contains(t, body, "remote_control: true")
	assert.Contains(t, body, "model: opus")

	_, _, err = zm(t, crewFile, "seat", "set", "temp", "remote_control=maybe")
	require.Error(t, err)

	out, _, err := zm(t, crewFile, "ls", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"role": "temp"`)

	_, _, err = zm(t, crewFile, "seat", "rm", "temp")
	require.NoError(t, err)
	assert.NotContains(t, string(mustRead(t, crewFile)), "temp:")
}

func TestBrokenStateBlocksWritesNotLooks(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n")
	state := filepath.Join(filepath.Dir(crewFile), "state.json")
	require.NoError(t, os.WriteFile(state, []byte(`{"version": 2, "roles": {`), 0o600))

	_, _, err := zm(t, crewFile, "send", "--when-idle", "dev", "hi")
	require.ErrorContains(t, err, "zmina state recover")

	out, errOut, err := zm(t, crewFile, "ls")
	require.NoError(t, err, "a look still works")
	assert.Contains(t, out, "dev")
	assert.Contains(t, errOut, "unreadable")

	out, _, err = zm(t, crewFile, "state", "recover")
	require.NoError(t, err)
	assert.Contains(t, out, "state.json.broken-")

	_, _, err = zm(t, crewFile, "send", "--when-idle", "dev", "hi")
	require.NoError(t, err)

	out, _, err = zm(t, crewFile, "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "inbox    hi")
}

func TestNoteIfUpExitsThree(t *testing.T) {
	crewFile := testCrew(t, "")

	_, _, err := zm(t, crewFile, "note", "--if-up", "--from", "mr-watch", "!35")

	var ee *exitError
	require.True(t, errors.As(err, &ee))
	assert.Equal(t, exitDown, ee.code)

	out, _, err := zm(t, crewFile, "note", "--from", "mr-watch", "!35")
	require.NoError(t, err)
	assert.Contains(t, out, "queued")
}

func TestStartStopWithRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}

	crewFile := testCrew(t, "  dev:\n    boot: hi dev\n")

	out, _, err := zm(t, crewFile, "start")
	require.NoError(t, err)
	assert.Contains(t, out, "shift started")

	_, _, err = zm(t, crewFile, "up", "dev")
	require.NoError(t, err)

	out, _, err = zm(t, crewFile, "ls")
	require.NoError(t, err)
	assert.Regexp(t, `dev\s+\S+-dev\s+up`, out)

	for i := 0; i < 40 && !strings.Contains(out, "claude --name"); i++ {
		time.Sleep(50 * time.Millisecond)

		out, _, err = zm(t, crewFile, "peek", "dev", "-n", "5")
		require.NoError(t, err)
	}

	assert.Contains(t, out, "claude --name")
	assert.NotContains(t, out, "--remote-control", "remote_control: false")

	out, _, err = zm(t, crewFile, "stop")
	require.NoError(t, err)
	assert.Contains(t, out, "shift stopped")
	assert.Contains(t, out, "dev down")
	assert.Contains(t, out, "dispatcher down")

	out, _, err = zm(t, crewFile, "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "STOPPED")

	out, _, err = zm(t, crewFile, "tick")
	require.NoError(t, err)
	assert.NotContains(t, out, "started dispatcher", "a stopped shift starts nothing")
}

func TestDoctorReports(t *testing.T) {
	crewFile := testCrew(t, "")
	dir := filepath.Dir(crewFile)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"),
		[]byte("#!/bin/sh\necho '--name --resume --remote-control --model'\n"), 0o700))

	out, _, err := zm(t, crewFile, "doctor")
	assert.Contains(t, out, "ok    crew")
	assert.Contains(t, out, "ok    claude")
	assert.Contains(t, out, "warn  registry")
	assert.Contains(t, out, "warn  ticker")
	assert.Contains(t, out, "ok    lock")
	assert.NoError(t, err, "warnings only")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\necho '--model'\n"), 0o700))

	out, _, err = zm(t, crewFile, "doctor")
	assert.Contains(t, out, "fail  claude")
	assert.Error(t, err)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	return raw
}

func TestUpWithTask(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}

	crewFile := testCrew(t, "  dev:\n")
	repo := filepath.Join(filepath.Dir(crewFile), "api")

	require.NoError(t, os.MkdirAll(repo, 0o700))

	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "a"}} {
		out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.email=t@t", "-c", "user.name=t"},
			args...)...).CombinedOutput()
		require.NoError(t, err, string(out))
	}

	out, _, err := zm(t, crewFile, "task", "new", "dev", "--repo", "api", "--worktree", "fix the login")
	require.NoError(t, err)
	wt := strings.Fields(strings.SplitN(out, "worktree ", 2)[1])[0]

	out, _, err = zm(t, crewFile, "up", "dev", "--as", "2", "--task", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "T-1 handed to dev#2")

	out, _, err = zm(t, crewFile, "ls", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"dir": "`+wt+`"`, "started in the task's worktree")

	out, _, err = zm(t, crewFile, "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "[zmina T-1] fix the login · you are in its worktree "+wt)

	out, _, err = zm(t, crewFile, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "dev#2")

	_, _, err = zm(t, crewFile, "task", "done", "1", "--force")
	require.NoError(t, err)

	_, _, err = zm(t, crewFile, "up", "dev", "--as", "3", "--task", "1")
	assert.ErrorContains(t, err, "only an active task gets a seat")
}

func TestUsage(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n")
	dir := filepath.Dir(crewFile)
	proj := filepath.Join(dir, "ns", "projects", "-work")
	require.NoError(t, os.MkdirAll(proj, 0o700))

	name := strings.TrimPrefix(strings.SplitN(string(mustRead(t, crewFile)), "\n", 2)[0], "name: ")
	ts := time.Now().UTC().Format(time.RFC3339)
	body := `{"type":"agent-name","agentName":"` + name + `-dev-2"}` + "\n" +
		`{"type":"assistant","timestamp":"` + ts + `","message":{"id":"m1","usage":{"input_tokens":4,` +
		`"cache_creation_input_tokens":1000,"cache_read_input_tokens":200000,"output_tokens":500}}}` + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(proj, "s1.jsonl"), []byte(body), 0o600))

	out, _, err := zm(t, crewFile, "usage")
	require.NoError(t, err)
	assert.Regexp(t, `dev\s+1\s+1\s+4\s+1k\s+200k\s+500\s+201k\s+201k`, out, "a copy counts with its role")
}

func TestLaterFlow(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n")

	out, _, err := zm(t, crewFile, "later", "ls")
	require.NoError(t, err)
	assert.Equal(t, "nothing parked\n", out)

	out, _, err = zm(t, crewFile, "ls")
	require.NoError(t, err)
	assert.NotContains(t, out, "later:", "no line while nothing is parked")

	out, _, err = zm(t, crewFile, "later", "add", "rotate", "the", "deploy", "key", "--note", "all repos")
	require.NoError(t, err)
	assert.Equal(t, "L-1\n", out)

	for _, text := range []string{"disk ticket", "idea"} {
		_, _, err = zm(t, crewFile, "later", "add", text)
		require.NoError(t, err)
	}

	out, _, err = zm(t, crewFile, "later", "ls")
	require.NoError(t, err)
	assert.Regexp(t, `^L-1\s+0m\s+rotate the deploy key · all repos\nL-2\s+0m\s+disk ticket\nL-3\s+0m\s+idea\n$`, out)

	out, _, err = zm(t, crewFile, "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "\nlater: 3 parked (zmina later ls)\n")

	out, _, err = zm(t, crewFile, "later", "drop", "3")
	require.NoError(t, err)
	assert.Equal(t, "L-3 · dropped\n", out)

	_, _, err = zm(t, crewFile, "later", "take", "1", "nobody")
	require.Error(t, err, "an unknown role")

	out, _, err = zm(t, crewFile, "later", "take", "L-1", "dev")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev · L-1 taken\n", out)

	out, _, err = zm(t, crewFile, "task", "show", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "asked: rotate the deploy key")
	assert.Contains(t, out, "note: from L-1: all repos")

	_, _, err = zm(t, crewFile, "later", "take", "1", "dev")
	require.ErrorContains(t, err, "L-1 is already taken T-1")

	out, _, err = zm(t, crewFile, "later", "ls", "--all")
	require.NoError(t, err)
	assert.Regexp(t, `L-1\s+0m\s+taken T-1\s+rotate`, out)
	assert.Regexp(t, `L-3\s+0m\s+dropped\s+idea`, out)

	out, _, err = zm(t, crewFile, "later", "ls", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"id": "L-2"`)
	assert.NotContains(t, out, `"id": "L-1"`)

	out, _, err = zm(t, crewFile, "log", "--task", "L-1")
	require.NoError(t, err)
	assert.Contains(t, out, "taken as T-1")
}

func TestDownNeverSendsADraft(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}

	crewFile := testCrew(t, "  dev:\n")
	socket := "zmina-" + strings.TrimPrefix(strings.SplitN(string(mustRead(t, crewFile)), "\n", 2)[0], "name: ")
	typedLog := filepath.Join(filepath.Dir(crewFile), "typed.log")

	_, _, err := zm(t, crewFile, "up", "dev")
	require.NoError(t, err)

	screen := func() string {
		out, _, _ := zm(t, crewFile, "peek", "dev", "-n", "5")
		return out
	}

	for i := 0; i < 40 && !strings.Contains(screen(), "❯"); i++ {
		time.Sleep(50 * time.Millisecond)
	}

	// The owner's half-typed line, not sent.
	draft := "drop the steps, start with steps 1–2 ✓"
	keyOut, err := exec.Command("tmux", "-L", socket, "send-keys", "-t", "=dev:", "-l", draft).CombinedOutput()
	require.NoError(t, err, string(keyOut))

	for i := 0; i < 40 && !strings.Contains(screen(), "steps 1–2"); i++ {
		time.Sleep(50 * time.Millisecond)
	}

	_, _, err = zm(t, crewFile, "down", "dev")
	require.ErrorContains(t, err, "a draft sits in its input line")
	assert.Contains(t, screen(), draft, "refused: the draft is left as it was")

	out, _, err := zm(t, crewFile, "down", "--clear", "dev")
	require.NoError(t, err)
	assert.Contains(t, out, "dev down")

	typed, _ := os.ReadFile(typedLog)
	assert.Equal(t, "/exit\n", string(typed), "the draft was dropped, only /exit was taken")
}

// version, skill and help answer on a fresh machine: no crew file, nothing under the home directory.
func TestNoCrewCommands(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZMINA_CREW", "")

	for _, args := range [][]string{{"version"}, {"--version"}, {"skill"}, {"help"}, {"--help"}} {
		var out, errb bytes.Buffer

		root := NewRoot(&out, &errb)
		root.SetArgs(args)
		require.NoError(t, root.Execute(), args)
		assert.NotEmpty(t, out.String(), "%v prints to stdout", args)
	}

	out, _, err := zm(t, filepath.Join(t.TempDir(), "absent.yaml"), "version")
	require.NoError(t, err)
	assert.Equal(t, "zmina version dev\n", out)
}

func TestTaskParkAndResume(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}

	crewFile := testCrew(t, "  dev:\n")

	_, _, err := zm(t, crewFile, "task", "new", "dev", "--title", "faster site", "make the site faster")
	require.NoError(t, err)

	_, _, err = zm(t, crewFile, "task", "set", "1", "parked")
	require.ErrorContains(t, err, "zmina task park")

	out, _, err := zm(t, crewFile, "task", "park", "T-1", "--note", "images left; the CSS is done")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev · parked L-1\n", out)

	out, _, err = zm(t, crewFile, "task", "ls")
	require.NoError(t, err)
	assert.Equal(t, "no tasks\n", out, "a parked task holds no role")

	out, _, err = zm(t, crewFile, "task", "ls", "--all")
	require.NoError(t, err)
	assert.Contains(t, out, "parked L-1")

	out, _, err = zm(t, crewFile, "later", "ls")
	require.NoError(t, err)
	assert.Regexp(t, `^L-1\s+0m\s+images left; the CSS is done · from T-1 \(dev\)\n$`, out)

	_, _, err = zm(t, crewFile, "task", "park", "1")
	require.ErrorContains(t, err, "T-1 is parked L-1")

	_, _, err = zm(t, crewFile, "later", "take", "1", "dev", "--resume")
	require.ErrorContains(t, err, "no conversation to resume", "dev was never up: no session id")

	// As a released role's item: the conversation it was parked from.
	path := filepath.Join(filepath.Dir(crewFile), "later", "L-1.json")
	raw := strings.Replace(string(mustRead(t, path)), `"role": "dev",`, `"role": "dev", "session": "abc-123",`, 1)
	require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))

	_, _, err = zm(t, crewFile, "later", "take", "1", "dispatcher", "--resume")
	require.ErrorContains(t, err, "parked from dev")

	out, _, err = zm(t, crewFile, "later", "take", "1", "dev", "--resume")
	require.NoError(t, err)
	assert.Equal(t, "T-2 · dev · L-1 taken · dev up on abc-123\n", out)

	screen := ""
	for i := 0; i < 40 && !strings.Contains(screen, "--resume abc-123"); i++ {
		time.Sleep(50 * time.Millisecond)
		screen, _, _ = zm(t, crewFile, "peek", "dev", "-n", "5")
	}

	assert.Contains(t, screen, "--resume abc-123")

	out, _, err = zm(t, crewFile, "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "[zmina T-2] back from L-1: images left; the CSS is done")
}
