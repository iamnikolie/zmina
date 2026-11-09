package cmd

import (
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHookCommandsResolve: the command lines zmina writes into a role's start — the hooks in a Claude role's
// --settings, a Codex role's bridge — are an API. A role keeps them for its whole life and runs them with whatever
// zmina is installed later, so each command and flag they name must exist in this zmina. A rename keeps the old
// name as an alias; this test fails when one goes. The lines are read from the start command itself, so the list
// cannot drift from what roles run.
func TestHookCommandsResolve(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n    boot: hi\n    read_guard: true\n  rev:\n    runtime: codex\n    boot: hi\n")

	a := &app{out: io.Discard, err: io.Discard, crew: crewFile}
	s, err := a.newShift()
	require.NoError(t, err)

	s.Self = "/opt/zmina/zmina"
	base := "'" + s.Self + "' --crew '" + s.Crew.Path + "' "

	var lines [][]string

	argv := s.Command(s.Crew.Roles["dev"], "")
	settings := ""
	for i, arg := range argv {
		if arg == "--settings" && i+1 < len(argv) {
			settings = argv[i+1]
		}
	}
	require.NotEmpty(t, settings, "dev starts with zmina's settings")

	var parsed struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal([]byte(settings), &parsed))

	events := map[string]bool{}
	for event, groups := range parsed.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				rest, ok := strings.CutPrefix(h.Command, base)
				require.True(t, ok, "%s hook runs zmina with its crew: %s", event, h.Command)

				rest = strings.TrimSuffix(rest, " >/dev/null 2>&1 || true")
				lines = append(lines, shellSplit(t, rest))
				events[event] = true
			}
		}
	}
	assert.Equal(t, map[string]bool{"SessionStart": true, "Stop": true, "PreToolUse": true}, events)

	bridge := s.Command(s.Crew.Roles["rev"], "thread-1")
	require.Equal(t, "_codex", bridge[1])
	lines = append(lines, bridge[1:])

	for _, args := range lines {
		root := newRoot(&app{out: io.Discard, err: io.Discard})
		c, rest, err := root.Find(args)
		require.NoError(t, err, "%v", args)
		require.NotSame(t, root, c, "%v names a command", args)

		require.NoError(t, c.ParseFlags(rest), "%v: every flag exists", args)
	}
}

// shellSplit splits a command line zmina quoted (single quotes) the way sh does.
func shellSplit(t *testing.T, line string) []string {
	t.Helper()

	out, err := exec.Command("/bin/sh", "-c", `printf '%s\0' `+line).Output()
	require.NoError(t, err, line)

	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}
