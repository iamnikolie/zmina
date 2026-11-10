package cmd

import (
	"io"
	"os/exec"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every command says in its help line what it does, and every top-level one sits in a help group.
func TestEveryCommandHasAHelpLineAndAGroup(t *testing.T) {
	root := NewRoot(io.Discard, io.Discard)

	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if !c.Hidden && c != root {
			assert.NotEmpty(t, c.Short, "%s has no help line", c.CommandPath())
		}

		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)

	ids := map[string]bool{}
	for _, g := range root.Groups() {
		ids[g.ID] = true
	}

	for _, c := range root.Commands() {
		assert.True(t, ids[c.GroupID], "%s is in no help group", c.Name())
	}
}

// up --fresh on a role that is up names restart, which does what the flag asked.
func TestUpFreshOnARoleThatIsUpNamesRestart(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}

	file := testCrew(t, "  dev: {}\n")

	_, _, err := zm(t, file, "up", "dev")
	require.NoError(t, err)

	t.Cleanup(func() { _, _, _ = zm(t, file, "down", "dev") })

	_, _, err = zm(t, file, "up", "dev", "--fresh")
	require.ErrorContains(t, err, "zmina restart dev --fresh")

	_, _, err = zm(t, file, "up", "dev")
	require.ErrorContains(t, err, "zmina attach dev", "a plain up keeps pointing at attach")
}
