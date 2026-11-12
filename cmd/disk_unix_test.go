//go:build darwin || linux

package cmd

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The filesystem calls of this system read a real directory, and one that is not made yet by its nearest parent.
func TestStatDiskReadsTheFilesystem(t *testing.T) {
	dir := t.TempDir()

	sp, err := statDisk(dir)
	require.NoError(t, err)
	assert.NotZero(t, sp.Total)
	assert.LessOrEqual(t, sp.Free, sp.Total)

	later, err := statDisk(filepath.Join(dir, "not", "made", "yet"))
	require.NoError(t, err)
	assert.Equal(t, sp.Total, later.Total, "measured at the nearest directory that is there")
	assert.Equal(t, sp.ID, later.ID, "the same filesystem")
}
