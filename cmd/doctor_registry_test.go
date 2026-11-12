package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goodEntry is a session entry with every field zmina reads.
func goodEntry(pid int) string {
	return fmt.Sprintf(`{"pid": %d, "sessionId": "sid-%d", "name": "zm-dev", "status": "idle", "statusUpdatedAt": 1761300000000,
  "version": "2.1.310"}`, pid, pid)
}

// sessionsDir writes session files (name -> body) into a fresh namespace and returns it.
func sessionsDir(t *testing.T, files map[string]string) string {
	t.Helper()

	ns := t.TempDir()
	dir := filepath.Join(ns, "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}

	return ns
}

// A running claude whose session file is empty or cut short fails the check, in words that say what it is, likely why,
// and which role to restart; the same file of a process that is gone is a warning that nothing waits on it.
func TestRegistryReportNamesAnEmptyOrCutShortSessionFile(t *testing.T) {
	me := os.Getpid()
	gone := 999999

	whole := goodEntry(me)

	for name, body := range map[string]string{
		"empty":        "",
		"blank":        " \n",
		"no fields":    "{}",
		"cut short":    whole[:len(whole)/2],
		"cut at a key": `{"pid": 5, "status": `,
	} {
		t.Run(name, func(t *testing.T) {
			asked := 0
			ns := sessionsDir(t, map[string]string{strconv.Itoa(me) + ".json": body, strconv.Itoa(gone) + ".json": body})
			file := filepath.Join(ns, "sessions", strconv.Itoa(me)+".json")

			lines := registryReport(ns, func() map[int]string { asked++; return map[int]string{me: "dev"} })
			require.Len(t, lines, 2)
			assert.Equal(t, 1, asked, "the roles are asked once, and only for a bad file")

			// The two files come in name order, which is not the order of the levels.
			var running, leftover registryLine

			for _, l := range lines {
				if l.level == "fail" {
					running = l
				} else {
					leftover = l
				}
			}

			assert.Equal(t, "fail", running.level)
			assert.Contains(t, running.text, "session file "+file)
			assert.Contains(t, running.text, "likely written while the disk was full")
			assert.Contains(t, running.text, "restart the role to get a fresh one")
			assert.Contains(t, running.text, "it shows as up unknown until restarted: zmina restart dev")
			assert.NotRegexp(t, `^\S+ does not parse: unexpected end`, running.text, "not a bare parse error")

			assert.Equal(t, "warn", leftover.level)
			assert.Contains(t, leftover.text, "pid "+strconv.Itoa(gone)+" does not run, so nothing waits on it")
			assert.NotContains(t, leftover.text, "restart the role", "nothing to restart")

			if body == "" || body == " \n" || body == "{}" {
				assert.Contains(t, running.text, " is empty")
			} else {
				assert.Contains(t, running.text, "does not parse")
			}
		})
	}
}

// A bad file whose pid no role of the crew owns is still said, with where to look, and a good file next to a bad one
// still gets its own line.
func TestRegistryReportBadFileOfNoKnownRoleAndAGoodOne(t *testing.T) {
	me := os.Getpid()
	ns := sessionsDir(t, map[string]string{
		strconv.Itoa(me) + ".json": "",
		"1.json":                   goodEntry(1),
	})

	lines := registryReport(ns, func() map[int]string { return nil })
	require.Len(t, lines, 2)

	assert.Equal(t, "fail", lines[0].level)
	assert.Contains(t, lines[0].text, "the role that runs it shows as up unknown until restarted (zmina ls shows the pids)")

	assert.Equal(t, "ok", lines[1].level)
	assert.Equal(t, "1 entries in "+filepath.Join(ns, "sessions")+" · written by Claude Code 2.1.310", lines[1].text)

	// No owners to ask: the same text, no crash.
	lines = registryReport(ns, nil)
	assert.Equal(t, "fail", lines[0].level)
}

// What the check said before still holds: no files, a good file, a file that lacks fields, a file that cannot be read.
func TestRegistryReportOtherShapes(t *testing.T) {
	ns := sessionsDir(t, nil)
	lines := registryReport(ns, nil)
	require.Len(t, lines, 1)
	assert.Equal(t, "warn", lines[0].level)
	assert.Contains(t, lines[0].text, "no session entries in "+filepath.Join(ns, "sessions")+" yet")

	ns = sessionsDir(t, map[string]string{"7.json": goodEntry(7), "8.json": goodEntry(8)})
	assert.Equal(t, []registryLine{{"ok", "2 entries in " + filepath.Join(ns, "sessions") +
		" · written by Claude Code 2.1.310"}}, registryReport(ns, nil))

	ns = sessionsDir(t, map[string]string{"7.json": `{"pid": 7, "name": "zm-dev"}`})
	lines = registryReport(ns, nil)
	require.Len(t, lines, 1)
	assert.Equal(t, "fail", lines[0].level)
	assert.Equal(t, "entries lack sessionId, status, statusUpdatedAt: Claude Code changed its registry; idle/busy and "+
		"delivery will not work", lines[0].text)

	ns = sessionsDir(t, map[string]string{"7.json": `{"pid": 7}`})
	require.NoError(t, os.Chmod(filepath.Join(ns, "sessions", "7.json"), 0))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(ns, "sessions", "7.json"), 0o600) })

	if os.Geteuid() != 0 {
		lines = registryReport(ns, nil)
		require.Len(t, lines, 1)
		assert.Equal(t, "fail", lines[0].level)
		assert.Contains(t, lines[0].text, "does not read")
	}
}

// zmina doctor puts it in the registry line, as the owner reads it.
func TestDoctorNamesAnEmptySessionFile(t *testing.T) {
	crewFile := testCrew(t, "")
	fakeAuth(t, filepath.Dir(crewFile), `{"loggedIn": true}`)

	ns := filepath.Join(filepath.Dir(crewFile), "ns", "sessions")
	require.NoError(t, os.MkdirAll(ns, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ns, "999999.json"), nil, 0o600))

	out, _, err := zm(t, crewFile, "doctor")
	require.NoError(t, err, "a leftover of a process that is gone only warns")
	assert.Regexp(t, `(?m)^warn  registry  session file \S+999999\.json is empty: its pid 999999 does not run, so nothing waits on it`, out)
	assert.NotContains(t, out, "does not parse")
	assert.NotContains(t, out, "lacks")
}

// A claude that prints nothing for --help does not "lack" the flags: the check cannot be made, and says so.
func TestDoctorClaudeThatPrintsNothing(t *testing.T) {
	crewFile := testCrew(t, "")
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(crewFile), "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o700))

	out, _, err := zm(t, crewFile, "doctor")
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^warn  claude    \S+ --help printed nothing, so the flags zmina passes cannot be checked`, out)
	assert.NotContains(t, out, "lacks")
}
