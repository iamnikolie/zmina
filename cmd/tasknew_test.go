package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskNewForADownRoleSaysHowToStartIt(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")

	out, errOut, err := zm(t, file, "task", "new", "dev", "Review")
	require.NoError(t, err)
	assert.Equal(t, "T-1 · dev\n", out, "the first line stays as scripts read it")
	assert.Contains(t, errOut, "dev is down: zmina up dev --task T-1 starts it on the task")
}
