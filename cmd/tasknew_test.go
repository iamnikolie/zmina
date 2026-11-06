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
	// The command names the crew it was given: copied into another shell it must not reach the default one.
	assert.Contains(t, errOut, "dev is down: zmina --crew "+quoteCommandArg(file)+" up 'dev' --task T-1 starts it on the task")
}
