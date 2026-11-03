package cmd

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestDispatcherFollowupRecoveryAcrossCommands(t *testing.T) {
	file := testCrew(t, "  designer: {}\n")
	out, _, err := zm(t, file, "context", "followup", "add", "Check the draft", "--source", "owner message 1")
	require.NoError(t, err)
	assert.Equal(t, "F-1\n", out)
	out, _, err = zm(t, file, "context", "pack", "dispatcher", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, "Check the draft")
	assert.Contains(t, out, "full_index")
	_, _, err = zm(t, file, "context", "followup", "resolve", "F-1")
	require.NoError(t, err)
	out, _, err = zm(t, file, "context", "followup", "ls")
	require.NoError(t, err)
	assert.Empty(t, out)
	out, _, err = zm(t, file, "context", "pack")
	require.NoError(t, err)
	assert.Contains(t, out, "no open dispatcher threads")
}
