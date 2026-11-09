package crew

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLessonsKey(t *testing.T) {
	dir := t.TempDir()

	c, err := Load(write(t, "dir: "+dir+"\nroles:\n  dispatcher:\n"))
	require.NoError(t, err)
	assert.False(t, c.Lessons.On, "off unless the crew file says so")

	c, err = Load(write(t, "dir: "+dir+"\nlessons: on\nroles:\n  dispatcher:\n"))
	require.NoError(t, err)
	assert.True(t, c.Lessons.On)
	assert.Equal(t, "opus", c.Lessons.RunModel())
	assert.InDelta(t, 8.0, c.Lessons.RunBudget(), 0.001)
	assert.InDelta(t, 50.0, c.Lessons.RunWeekBudget(), 0.001)
	h, m := c.Lessons.RunAt()
	assert.Equal(t, [2]int{6, 0}, [2]int{h, m})

	c, err = Load(write(t, "dir: "+dir+"\nlessons: off\nroles:\n  dispatcher:\n"))
	require.NoError(t, err)
	assert.False(t, c.Lessons.On)

	path := write(t, "dir: "+dir+"\nlessons:\n  repo: os\n  branch: trunk\n  at: \"07:30\"\n  model: sonnet\n  budget: 1.5\n"+
		"roles:\n  dispatcher:\n")
	c, err = Load(path)
	require.NoError(t, err)
	assert.True(t, c.Lessons.On, "a block is on")
	assert.Equal(t, filepath.Join(filepath.Dir(path), "os"), c.Lessons.Repo, "relative to the crew file, as agents is")
	assert.Equal(t, "trunk", c.Lessons.Branch)
	assert.Equal(t, "sonnet", c.Lessons.RunModel())
	h, m = c.Lessons.RunAt()
	assert.Equal(t, [2]int{7, 30}, [2]int{h, m})

	c, err = Load(write(t, "dir: "+dir+"\nlessons:\n  enabled: false\n  budget: 2\nroles:\n  dispatcher:\n"))
	require.NoError(t, err)
	assert.False(t, c.Lessons.On, "enabled: false keeps the block, off")

	for _, body := range []string{"lessons: maybe\n", "lessons:\n  at: soon\n", "lessons:\n  budget: -1\n", "lessons:\n  week_budget: -5\n"} {
		_, err := Load(write(t, "dir: "+dir+"\n"+body+"roles:\n  dispatcher:\n"))
		assert.ErrorContains(t, err, "lessons", body)
	}
}
