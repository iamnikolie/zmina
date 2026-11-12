package cmd

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHookCommandsResolve: the command lines zmina writes into a role's start — the hooks in a Claude role's
// --settings, a Codex role's bridge — are an API. A role keeps them for its whole life and runs them with whatever
// zmina is installed later. Every line a released zmina wrote is frozen in testdata/hook-lines.txt and must keep
// resolving: a rename keeps the old name as an alias. The lines this zmina writes are read from the start command
// itself and must be in that list too, so a new line or flag is frozen with the release that writes it.
func TestHookCommandsResolve(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n    boot: hi\n    read_guard: true\n  rev:\n    runtime: codex\n    boot: hi\n")

	// The owner's own status line, which the tap wraps: the shape the frozen lines must keep resolving.
	ns := filepath.Join(filepath.Dir(crewFile), "ns")
	require.NoError(t, os.MkdirAll(ns, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ns, "settings.json"),
		[]byte(`{"statusLine":{"type":"command","command":"/opt/line/status.sh --short"}}`), 0o600))

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
		StatusLine struct {
			Command string `json:"command"`
		} `json:"statusLine"`
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
	assert.Equal(t, map[string]bool{"SessionStart": true, "Stop": true, "PreToolUse": true, "PermissionRequest": true}, events)

	// The status line is not a hook but is kept the same way: a role's session runs the line it started with.
	rest, ok := strings.CutPrefix(parsed.StatusLine.Command, base)
	require.True(t, ok, "the status line runs zmina with its crew: %s", parsed.StatusLine.Command)

	tapLine := shellSplit(t, rest)
	require.Equal(t, []string{"_limits-tap", "--", "/opt/line/status.sh --short"}, tapLine, "it wraps the owner's own line as one word")
	lines = append(lines, tapLine)

	bridge := s.Command(s.Crew.Roles["rev"], "thread-1")
	require.Equal(t, "_codex", bridge[1])
	lines = append(lines, bridge[1:])

	// What released zminas wrote: each resolves, command and flags.
	frozen := map[string]map[string]bool{}
	raw, err := os.ReadFile(filepath.Join("testdata", "hook-lines.txt"))
	require.NoError(t, err)

	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		_, words, _ := strings.Cut(line, " ")
		path, flags := resolveLine(t, strings.Fields(words))
		if frozen[path] == nil {
			frozen[path] = map[string]bool{}
		}

		for _, f := range flags {
			frozen[path][f] = true
		}
	}

	require.NotEmpty(t, frozen)

	// What this zmina writes: frozen too, so the next release keeps it.
	for _, args := range lines {
		path, flags := resolveLine(t, args)
		require.Contains(t, frozen, path, "%v: a new hook line; append it to testdata/hook-lines.txt", args)

		for _, f := range flags {
			assert.True(t, frozen[path][f], "%v: --%s is new; append the line to testdata/hook-lines.txt", args, f)
		}
	}
}

// resolveLine finds the command a line names in this zmina and parses its flags: the command's path and the flags
// the line sets. A name or flag this zmina does not know fails the test.
func resolveLine(t *testing.T, args []string) (string, []string) {
	t.Helper()

	root := newRoot(&app{out: io.Discard, err: io.Discard})
	c, rest, err := root.Find(args)
	require.NoError(t, err, "%v", args)
	require.NotSame(t, root, c, "%v names a command", args)
	require.NoError(t, c.ParseFlags(rest), "%v: every flag exists", args)

	var flags []string

	c.Flags().Visit(func(f *pflag.Flag) { flags = append(flags, f.Name) })

	return c.CommandPath(), flags
}

// shellSplit splits a command line zmina quoted (single quotes) the way sh does.
func shellSplit(t *testing.T, line string) []string {
	t.Helper()

	out, err := exec.Command("/bin/sh", "-c", `printf '%s\0' `+line).Output()
	require.NoError(t, err, line)

	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}
