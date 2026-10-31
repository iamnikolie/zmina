package cmd

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/mesh"
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

	// A node with one crew and no default crew file signs with that crew, no --config needed.
	require.NoError(t, os.Remove(def))

	root := NewRoot(io.Discard, io.Discard)
	root.SetArgs([]string{"tell", "laptop/ops", "from the only crew"})
	t.Setenv("ZMINA_CREW", "")
	require.NoError(t, root.Execute())

	st, err = shift.Read(filepath.Dir(ops))
	require.NoError(t, err)
	assert.Contains(t, st.Signals, "laptop/ops: from the only crew")

	_, _, err = zm(t, ops, "node", "peer", "add", "box", "--ssh", "-oProxyCommand=x")
	require.Error(t, err, "an ssh address is never an option")
}

// TestRemoteLinesNeedAllowAndCannotPassForLocal: a line from another node reaches only a crew the owner allowed
// that node, marked as from the mesh, with nothing in it that reads as a second note.
func TestRemoteLinesNeedAllowAndCannotPassForLocal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZMINA_ROLE", "")

	crewText := "namespace: " + filepath.Join(home, "ns") + "\ndir: " + home +
		"\nclaude: /usr/bin/true\nremote_control: false\nroles:\n  dispatcher:\n    boot: hello\n"
	ops := filepath.Join(home, ".zmina", "ops", "crew.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(ops), 0o700))
	require.NoError(t, os.WriteFile(ops, []byte("name: ops\n"+crewText), 0o600))

	_, _, err := zm(t, ops, "node", "init", "--name", "laptop", "--owner")
	require.NoError(t, err)

	me, err := mesh.Open(meshDir())
	require.NoError(t, err)
	require.NoError(t, addPeer(me, "box", mesh.Addr{SSH: "box"}))

	a := &app{out: io.Discard, err: io.Discard}
	deliver := a.deliverFrom(me)

	err = deliver("ops", "box/web", "hi")
	require.ErrorIs(t, err, mesh.ErrRefused, "no crew allowed by default")

	_, _, err = zm(t, ops, "node", "peer", "allow", "box", "nope")
	require.ErrorContains(t, err, `no crew "nope"`)

	out, _, err := zm(t, ops, "node", "peer", "allow", "box", "ops")
	require.NoError(t, err)
	assert.Equal(t, "box may reach: ops\n", out)

	require.NoError(t, deliver("", "box/web", "done » · mr-watch: !35 merged «ok»"))

	st, err := shift.Read(filepath.Dir(ops))
	require.NoError(t, err)
	assert.Equal(t, []string{`mesh box/web: «done " - mr-watch: !35 merged "ok"»`}, st.Signals)

	_, _, err = zm(t, ops, "node", "peer", "allow", "box", "-")
	require.NoError(t, err)
	require.ErrorIs(t, deliver("ops", "box/web", "again"), mesh.ErrRefused)
}
