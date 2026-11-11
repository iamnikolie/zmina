package crew

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The recommended values are advice: a crew file that says nothing keeps the defaults.
func TestRecommendedAreAdviceNotDefaults(t *testing.T) {
	assert.Equal(t, 250_000, RecommendedContextMax)
	assert.Equal(t, 180_000, RecommendedRestartAt)
	assert.Equal(t, 60_000, RecommendedRestartDone)

	for _, v := range []int{RecommendedContextMax, RecommendedRestartAt, RecommendedRestartDone} {
		_, err := ParseTokens(strconv.Itoa(v))
		assert.NoError(t, err, "a recommended value is one the crew file takes")
	}

	c, err := Load(write(t, "dir: "+t.TempDir()+"\nroles:\n  dispatcher:\n  dev:\n"))
	require.NoError(t, err)
	assert.Zero(t, c.Roles["dev"].ContextLimit(), "context_max has no default")
	assert.Equal(t, DefaultRestartAt, c.Roles["dev"].RestartContext())
	assert.Equal(t, DefaultRestartDone, c.Roles["dev"].RestartDoneAt())
	assert.Equal(t, ContextLimits{RestartAt: DefaultRestartAt, RestartDone: DefaultRestartDone}, c.CrewContext())
}

func TestCrewContext(t *testing.T) {
	dir := t.TempDir()

	c, err := Load(write(t, "dir: "+dir+"\ncontext_max: 400k\nrestart_at: 1.2m\nrestart_done: off\nroles:\n  dispatcher:\n"))
	require.NoError(t, err)
	assert.Equal(t, ContextLimits{ContextMax: 400_000, RestartAt: 1_200_000}, c.CrewContext(), "off is 0")

	c, err = Load(write(t, "dir: "+dir+"\nrestart_at: 300k\nroles:\n  dispatcher:\n  dev:\n    restart_at: 100k\n"))
	require.NoError(t, err)
	assert.Equal(t, 300_000, c.CrewContext().RestartAt, "the crew's value, not a role's own")
	assert.Equal(t, 100_000, c.Roles["dev"].RestartContext())
}
