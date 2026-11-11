package mesh

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every ssh call that only collects output asks the far side whether it is alive, so a dead link ends ssh itself.
func TestSSHCallsAskIfTheLinkIsAlive(t *testing.T) {
	alive := []string{"-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=3"}

	c := Command(context.Background(), Addr{SSH: "box"}, "node", "serve", "--stdio")
	assert.Equal(t, []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}, c.Args[:5])
	assert.Equal(t, alive, c.Args[5:9])
	assert.Equal(t, []string{"--", "box"}, c.Args[9:11])
	assert.True(t, strings.HasSuffix(c.Args[11], " zmina node serve --stdio"))

	own, err := OwnerCommand(context.Background(), Addr{SSH: "box"}, false, "ls")
	require.NoError(t, err)
	assert.Equal(t, alive, own.Args[5:9])

	// A command line is the owner's own: nothing is added to it.
	line := Command(context.Background(), Addr{Cmd: "ssh box zmina"}, "ls")
	assert.Equal(t, []string{"sh", "-c", "ssh box zmina ls"}, line.Args)
}
