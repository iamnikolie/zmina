package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A resumed claude that exits at once (it found no such conversation) is reported as ended, never as up; a resume
// whose conversation is not under the namespace is refused before anything starts.
func TestUpResumeThatEndsAtOnce(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	dir := filepath.Dir(file)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\necho 'No conversation found with session ID: abc-1'\nexit 1\n"), 0o700))

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "ns", "projects", "other"), 0o700))
	_, _, err := zm(t, file, "up", "dev", "--resume=abc-1")
	require.ErrorContains(t, err, "no conversation abc-1 under")
	require.ErrorContains(t, err, "zmina up dev --fresh")

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "ns", "projects", "p"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ns", "projects", "p", "abc-1.jsonl"), nil, 0o600))

	out, _, err := zm(t, file, "up", "dev", "--resume=abc-1")
	require.ErrorContains(t, err, "dev ended right after its start: No conversation found")
	assert.NotContains(t, out, "dev up as")
}
