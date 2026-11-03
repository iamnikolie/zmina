package cmd

import (
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestSkillPrimaryTaskWorkflowUsesPacketsAcceptanceAndCheckpoints(t *testing.T) {
	for _, prefix := range []string{"| `zmina up <role> [--as N] --task", "| `zmina later take <id> <role> --resume`"} {
		var row string
		for _, line := range strings.Split(skillDoc, "\n") {
			if strings.HasPrefix(line, prefix) {
				row = line
				break
			}
		}
		require.NotEmpty(t, row)
		require.Contains(t, row, "handoff packet", "primary command reference must describe the file handoff")
		require.Contains(t, row, "task accept", "primary command reference must require explicit acceptance")
	}
	start := strings.Index(skillDoc, "   **Release**:")
	require.NotEqual(t, -1, start)
	end := strings.Index(skillDoc[start:], "5. Watches")
	require.Positive(t, end)
	release := skillDoc[start : start+end]
	require.Contains(t, release, "task checkpoint")
	require.Contains(t, release, "task park")
	require.Contains(t, release, "incomplete")
	require.NotContains(t, release, "write what remains in one or two lines")
	require.NotContains(t, skillDoc, "[zmina T-14] <asked>")
	require.NotContains(t, skillDoc, "[zmina T-61] back from L-15: <text>")
}
