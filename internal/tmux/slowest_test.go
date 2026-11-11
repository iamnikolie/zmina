package tmux

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The slowest call is told by name and time. A script stands for tmux, so no tmux is needed.
func TestSlowestKeepsTheLongestCallsSubcommand(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "tmux")
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = slow-one ] && sleep 0.15; done\nexit 0\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o700))

	c := &Client{Bin: bin, Socket: "x"}

	ResetSlowest()

	name, took := Slowest()
	assert.Empty(t, name)
	assert.Zero(t, took)

	_, err := c.run("slow-one")
	require.NoError(t, err)
	_, err = c.run("list-panes", "-a")
	require.NoError(t, err)

	name, took = Slowest()
	assert.Equal(t, "slow-one", name, "a quicker call after it does not replace it")
	assert.GreaterOrEqual(t, took, 150*time.Millisecond)

	ResetSlowest()

	name, took = Slowest()
	assert.Empty(t, name)
	assert.Zero(t, took)
}
