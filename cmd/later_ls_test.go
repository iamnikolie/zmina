package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// later ls marks an item that comes back on a day and, once items pile up, says how many are old and undated.
func TestLaterLsMarksTheDayAndOldItems(t *testing.T) {
	crewFile := testCrew(t, "")

	_, _, err := zm(t, crewFile, "later", "add", "the one-pager", "--until", "+3h")
	require.NoError(t, err)

	out, _, err := zm(t, crewFile, "later", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "[back ")
	assert.NotContains(t, out, "parked over", "nothing is old yet")

	c, err := crew.Load(crewFile)
	require.NoError(t, err)

	_, err = laterStore(c).Add(later.Item{Text: "an old idea"}, time.Now().Add(-20*24*time.Hour))
	require.NoError(t, err)

	out, _, err = zm(t, crewFile, "later", "ls")
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(out), "\n")
	assert.Contains(t, out, "an old idea")
	assert.Equal(t, "1 parked over 14 days, 1 with no day to come back: take, drop or keep them", lines[len(lines)-1])

	out, _, err = zm(t, crewFile, "later", "ls", "--json")
	require.NoError(t, err)
	assert.NotContains(t, out, "parked over", "JSON stays the items alone")
}
