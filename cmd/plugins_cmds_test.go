package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// plugin ls names each plugin's program, or says it is not found or refused, and how its last run ended.
func TestPluginLs(t *testing.T) {
	file := pluginCrew(t, "  dev: {}\n", "  - {name: pager, on: [ask, dialog], quiet_when_active: off}\n  - {name: ghost}\n  - {name: loose}\n  - {name: bad, quiet_when_active: off}\n")
	dir := filepath.Join(filepath.Dir(file), "plugins")
	log := recorder(t, file, "pager", "")
	recorder(t, file, "loose", "")
	recorder(t, file, "bad", "echo 'no route to host' >&2; exit 1")
	require.NoError(t, os.Chmod(filepath.Join(dir, "loose"), 0o770))

	out, _, err := zm(t, file, "plugin", "ls")
	require.NoError(t, err)
	assert.Regexp(t, `pager +`+regexpQuote(filepath.Join(dir, "pager"))+` +ask,dialog +never run`, out)
	assert.Regexp(t, `ghost +not found +ask,blocking,dialog,answered +never run`, out)
	assert.Contains(t, out, "ghost: program not found: zmina-plugin-ghost is not on PATH and "+filepath.Join(dir, "ghost")+" does not exist")
	assert.Contains(t, out, "loose: "+filepath.Join(dir, "loose")+" is writable by group or others")

	// After a delivery, and a failure, ls says how the last run ended.
	now := time.Now()
	deliver(t, file, now)

	_, _, err = zm(t, file, "todo", "add", "Which name?", "--from", "dev")
	require.NoError(t, err)
	deliver(t, file, now.Add(time.Minute))
	require.Len(t, told(t, log), 1)

	out, _, err = zm(t, file, "plugin", "ls")
	require.NoError(t, err)
	assert.Regexp(t, `pager +\S+ +ask,dialog +ask A-1 ok \w{3} \d\d:\d\d`, out)
	assert.Regexp(t, `bad +\S+ +\S+ +ask A-1 failed \w{3} \d\d:\d\d: exit 1: no route to host · 1 waiting for a retry`, out)

	var rows []pluginRow

	out, _, err = zm(t, file, "plugin", "ls", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &rows))
	require.Len(t, rows, 4)

	by := map[string]pluginRow{}
	for _, r := range rows {
		by[r.Name] = r
	}

	assert.True(t, by["pager"].Found)
	assert.Equal(t, filepath.Join(dir, "pager"), by["pager"].Program)
	assert.Equal(t, "crew dir", by["pager"].Via)
	assert.Equal(t, []string{"ask", "dialog"}, by["pager"].Events)
	assert.Equal(t, "off", by["pager"].QuietWhenActive)
	assert.Equal(t, "10s", by["pager"].Timeout)
	assert.Equal(t, "1m0s", by["pager"].DialogAfter)
	assert.Equal(t, 1, by["pager"].Open)
	require.NotNil(t, by["pager"].Last)
	assert.True(t, by["pager"].Last.OK)
	assert.False(t, by["ghost"].Found)
	assert.Contains(t, by["ghost"].Problem, "not found")
	assert.Contains(t, by["loose"].Problem, "writable by group or others")
	assert.Equal(t, 1, by["bad"].Waiting)
	assert.Equal(t, "5m0s", by["loose"].QuietWhenActive)

	// No plugins: say so; json is an empty list.
	bare := testCrew(t, "  dev: {}\n")
	out, _, err = zm(t, bare, "plugin", "ls")
	require.NoError(t, err)
	assert.Equal(t, "no plugins in the crew file\n", out)
	out, _, err = zm(t, bare, "plugin", "ls", "--json")
	require.NoError(t, err)
	assert.Equal(t, "[]\n", out)
}

func regexpQuote(s string) string { return strings.NewReplacer(".", `\.`, "+", `\+`).Replace(s) }

// plugin test sends a made-up event marked as a test and says how it went; it touches no item and keeps no record.
func TestPluginTest(t *testing.T) {
	file := pluginCrew(t, "  dev: {}\n", "  - {name: pager, quiet_when_active: off}\n  - {name: bad}\n  - {name: ghost}\n")
	log := recorder(t, file, "pager", "echo 'got it'; echo 'careful' >&2")
	recorder(t, file, "bad", "echo 'no route to host' >&2; exit 4")

	out, _, err := zm(t, file, "plugin", "test", "pager")
	require.NoError(t, err)
	assert.Regexp(t, `pager: ask \(test\) → taken, \d+ms`, out)
	assert.Contains(t, out, "exit code: 0")
	assert.Contains(t, out, "stdout:\n  got it")
	assert.Contains(t, out, "stderr:\n  careful")

	require.Len(t, told(t, log), 1)
	ev := told(t, log)[0]
	assert.Equal(t, "ask", ev.Event)
	assert.Equal(t, "pager", ev.Plugin)
	assert.Equal(t, true, ev.Line["test"])
	assert.Equal(t, float64(1), ev.Line["v"])

	for _, kind := range []string{"blocking", "dialog", "reminder", "answered"} {
		_, _, err = zm(t, file, "plugin", "test", "pager", "--event", kind)
		require.NoError(t, err, kind)
	}

	assert.Equal(t, []string{"ask A-0", "blocking A-0", "dialog dev:0000000000000000", "reminder A-0", "answered A-0"}, toldIDs(t, log))

	for _, g := range told(t, log) {
		assert.Equal(t, true, g.Line["test"], g.Event)
	}

	_, _, err = zm(t, file, "plugin", "test", "pager", "--event", "shout")
	require.ErrorContains(t, err, `--event "shout": one of ask, blocking, dialog, reminder, answered`)

	// A plugin that fails: the outcome and what it printed, and a failing exit.
	out, _, err = zm(t, file, "plugin", "test", "bad")
	require.Error(t, err)
	assert.ErrorContains(t, err, "bad: exit 4: no route to host")
	assert.Contains(t, out, "failed (exit 4: no route to host)")
	assert.Contains(t, out, "exit code: 4")
	assert.Contains(t, out, "stderr:\n  no route to host")

	_, _, err = zm(t, file, "plugin", "test", "ghost")
	require.ErrorContains(t, err, "program not found")

	_, _, err = zm(t, file, "plugin", "test", "nobody")
	require.ErrorContains(t, err, `no plugin "nobody" in the crew file (plugins: pager, bad, ghost)`)

	// Nothing of it is kept: no told file, no log line.
	assert.NoFileExists(t, filepath.Join(filepath.Dir(file), "plugins", "pager.told.json"))

	out, _, err = zm(t, file, "todo", "ls")
	require.NoError(t, err)
	assert.Equal(t, "nothing waits on you\n", out)
}

// A test run has no timeout of its own: the plugin's deadline holds.
func TestPluginTestDeadline(t *testing.T) {
	file := pluginCrew(t, "  dev: {}\n", "  - {name: slow, timeout: 1s}\n")
	recorder(t, file, "slow", "sleep 30")

	start := time.Now()
	out, _, err := zm(t, file, "plugin", "test", "slow")
	require.Error(t, err)
	assert.Less(t, time.Since(start), 15*time.Second)
	assert.Contains(t, out, "failed (timed out after 1s)")
}

// doctor has a line for each plugin: the program (ok), not found or refused, or the last failure and when.
func TestDoctorSaysOfPlugins(t *testing.T) {
	file := pluginCrew(t, "  dev: {}\n", "  - {name: pager, quiet_when_active: off}\n  - {name: ghost}\n  - {name: loose}\n  - {name: bad, quiet_when_active: off}\n")
	dir := filepath.Join(filepath.Dir(file), "plugins")
	recorder(t, file, "pager", "")
	recorder(t, file, "loose", "")
	recorder(t, file, "bad", "echo 'no route to host' >&2; exit 1")
	require.NoError(t, os.Chmod(filepath.Join(dir, "loose"), 0o770))

	out, _, _ := zm(t, file, "doctor")
	assert.Regexp(t, `ok +plugins +pager: `+regexpQuote(filepath.Join(dir, "pager"))+` · told of ask, blocking, dialog, answered · no run yet`, out)
	assert.Regexp(t, `warn +plugins +ghost: program not found: zmina-plugin-ghost is not on PATH and \S+ does not exist — nothing is told to it`, out)
	assert.Regexp(t, `warn +plugins +loose: \S+loose is writable by group or others, so it is not run \(chmod go-w\) — nothing is told to it`, out)

	now := time.Now()
	deliver(t, file, now)
	_, _, err := zm(t, file, "todo", "add", "Which name?", "--from", "dev")
	require.NoError(t, err)
	deliver(t, file, now.Add(time.Minute))

	out, _, _ = zm(t, file, "doctor")
	assert.Regexp(t, `ok +plugins +pager: \S+ · last run ask A-1 \w{3} \d+ \w{3} \d\d:\d\d`, out)
	assert.Regexp(t, `warn +plugins +bad: last run failed \w{3} \d+ \w{3} \d\d:\d\d \(ask A-1: exit 1: no route to host\) · 1 waiting for a retry`, out)

	var checks []check

	out, _, _ = zm(t, file, "doctor", "--json")
	require.NoError(t, json.Unmarshal([]byte(out), &checks))

	n := 0

	for _, c := range checks {
		if c.Name == "plugins" {
			n++
		}
	}

	assert.Equal(t, 4, n, "a line each")

	// A crew with none says nothing of plugins.
	bare := testCrew(t, "  dev: {}\n")
	out, _, _ = zm(t, bare, "doctor")
	assert.NotContains(t, out, "plugins")
}

// The example plugin of the repo appends each event to the file the environment names, and exits 0.
func TestExamplePluginLog(t *testing.T) {
	src := filepath.Join("..", "examples", "plugins", "zmina-plugin-log")
	raw, err := os.ReadFile(src)
	require.NoError(t, err)

	fi, err := os.Stat(src)
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode().Perm()&0o111, "the example is executable in the repo")

	file := pluginCrew(t, "  dev: {}\n", "  - {name: log, quiet_when_active: off}\n")
	dir := filepath.Join(filepath.Dir(file), "plugins")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "log"), raw, 0o755))

	// By hand: the file named by the environment.
	want := filepath.Join(t.TempDir(), "events.log")
	t.Setenv("ZMINA_PLUGIN_LOG", want)

	out, _, err := zm(t, file, "plugin", "test", "log", "--event", "dialog")
	require.NoError(t, err, out)

	lines := strings.Split(strings.TrimSpace(string(mustRead(t, want))), "\n")
	require.Len(t, lines, 1)

	var ev map[string]any

	require.NoError(t, json.Unmarshal([]byte(lines[0]), &ev))
	assert.Equal(t, "dialog", ev["event"])
	assert.Equal(t, true, ev["test"])

	// Through the delivery: by default beside the crew file.
	t.Setenv("ZMINA_PLUGIN_LOG", "")

	now := time.Now()
	deliver(t, file, now)

	_, _, err = zm(t, file, "todo", "add", "Which name?", "--title", "Site name", "--from", "dev")
	require.NoError(t, err)
	deliver(t, file, now.Add(time.Minute))

	beside := filepath.Join(filepath.Dir(file), "plugin.log")
	lines = strings.Split(strings.TrimSpace(string(mustRead(t, beside))), "\n")
	require.Len(t, lines, 1)

	ev = map[string]any{}

	require.NoError(t, json.Unmarshal([]byte(lines[0]), &ev))
	assert.Equal(t, "ask", ev["event"])
	assert.Equal(t, "Site name", ev["title"])
	assert.Nil(t, ev["test"])
}
