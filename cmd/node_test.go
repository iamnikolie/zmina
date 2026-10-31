package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/shift"
)

// TestTellOnThisNode: a line for another crew of this node goes straight into its dispatcher's signals, from
// <node>/<crew>.
func TestTellOnThisNode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZMINA_ROLE", "")

	crewText := "namespace: " + filepath.Join(home, "ns") + "\ndir: " + home +
		"\nclaude: /usr/bin/true\nremote_control: false\nroles:\n  dispatcher:\n    boot: hello\n"
	def := filepath.Join(home, ".zmina", "crew.yaml")
	ops := filepath.Join(home, ".zmina", "ops", "crew.yaml")

	require.NoError(t, os.MkdirAll(filepath.Dir(ops), 0o700))
	require.NoError(t, os.WriteFile(def, []byte(crewText), 0o600))
	require.NoError(t, os.WriteFile(ops, []byte("name: ops\n"+crewText), 0o600))

	_, _, err := zm(t, def, "tell", "laptop/ops", "hi")
	require.ErrorContains(t, err, "not a node yet")

	out, _, err := zm(t, def, "node", "init", "--name", "laptop", "--owner")
	require.NoError(t, err)
	assert.Contains(t, out, "node laptop · holds the owner key")

	out, _, err = zm(t, def, "tell", "laptop/ops", "the  dump\nis ready")
	require.NoError(t, err)
	assert.Equal(t, "queued for laptop/ops's dispatcher\n", out)

	st, err := shift.Read(filepath.Dir(ops))
	require.NoError(t, err)
	assert.Equal(t, []string{"laptop/default: the dump is ready"}, st.Signals)

	_, _, err = zm(t, def, "tell", "laptop/nope", "x")
	require.ErrorContains(t, err, `no crew "nope" here (crews: default, ops)`)

	out, _, err = zm(t, def, "node", "ls")
	require.NoError(t, err)
	assert.Equal(t, "node laptop · owner · crews: default, ops\n", out)

	_, _, err = zm(t, def, "node", "peer", "add", "box", "--ssh", "-oProxyCommand=x")
	require.Error(t, err, "an ssh address is never an option")
}
