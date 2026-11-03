package cmd

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestReportCLIRepeatCountsAndExplicitRemoval(t *testing.T) {
	crew := testCrew(t, "")
	for i := 0; i < 2; i++ {
		_, _, err := zm(t, crew, "report", "Ready", "--key", "T-1/review")
		require.NoError(t, err)
	}
	out, _, err := zm(t, crew, "report", "ls", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"count": 2`)
	out, _, err = zm(t, crew, "queue", "ls")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(out, "report T-1/review: Ready"))
	_, _, err = zm(t, crew, "report", "drop", "T-1/review")
	require.NoError(t, err)
	out, _, err = zm(t, crew, "report", "ls")
	require.NoError(t, err)
	assert.Empty(t, out)
}
