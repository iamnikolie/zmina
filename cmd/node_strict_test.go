package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
)

// nodeHome makes a home with a default crew and an ops crew, and a node that holds the owner key.
func nodeHome(t *testing.T) (def, ops string) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZMINA_ROLE", "")

	crewText := "namespace: " + filepath.Join(home, "ns") + "\ndir: " + home +
		"\nclaude: /usr/bin/true\nremote_control: false\nroles:\n  dispatcher:\n    boot: hello\n"
	def = filepath.Join(home, ".zmina", "crew.yaml")
	ops = filepath.Join(home, ".zmina", "ops", "crew.yaml")

	require.NoError(t, os.MkdirAll(filepath.Dir(ops), 0o700))
	require.NoError(t, os.WriteFile(def, []byte(crewText), 0o600))
	require.NoError(t, os.WriteFile(ops, []byte("name: ops\n"+crewText), 0o600))

	_, _, err := zm(t, def, "node", "init", "--name", "laptop", "--owner")
	require.NoError(t, err)

	return def, ops
}

// strict is off until the owner says so, is kept, shows in zmina node ls, and a peer that said terminal_v2 shows it.
func TestNodeStrictSwitch(t *testing.T) {
	def, _ := nodeHome(t)

	out, _, err := zm(t, def, "node", "strict")
	require.NoError(t, err)
	assert.Equal(t, "strict requests: off\n", out)

	out, _, err = zm(t, def, "node", "strict", "on")
	require.NoError(t, err)
	assert.Contains(t, out, "strict requests: on")

	out, _, err = zm(t, def, "node", "strict")
	require.NoError(t, err)
	assert.Equal(t, "strict requests: on\n", out)

	out, _, err = zm(t, def, "node", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "node laptop · owner · strict requests · crews: ")

	out, _, err = zm(t, def, "node", "ls", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"strict_requests":true`)

	_, _, err = zm(t, def, "node", "strict", "maybe")
	require.ErrorContains(t, err, `strict is on or off, not "maybe"`)

	out, _, err = zm(t, def, "node", "strict", "off")
	require.NoError(t, err)
	assert.Contains(t, out, "strict requests: off")

	out, _, err = zm(t, def, "node", "ls")
	require.NoError(t, err)
	assert.NotContains(t, out, "strict")
}

// A node's own event lands in the log of every crew on the machine.
func TestNodeEventReachesEveryCrew(t *testing.T) {
	def, ops := nodeHome(t)

	nodeEvent("terminal-unbound", "took crew/status from owner")

	for _, file := range []string{def, ops} {
		c, err := crew.Load(file)
		require.NoError(t, err)

		evs, err := eventLog(c).Read(events.Filter{})
		require.NoError(t, err)
		require.Len(t, evs, 1, file)
		assert.Equal(t, "terminal-unbound", evs[0].Event)
		assert.Equal(t, "mesh", evs[0].By)
		assert.Equal(t, "took crew/status from owner", evs[0].Detail)
	}
}
