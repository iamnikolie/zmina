package cmd

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/shift"
)

// A resumed claude that exits at once (it found no such conversation) is reported as ended, never as up; a resume
// whose conversation is not under the namespace is refused before anything starts.
func TestUpResumeThatEndsAtOnce(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	hasTranscript = realHasTranscript
	dir := filepath.Dir(file)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\necho 'No conversation found with session ID: abc-1'\nexit 1\n"), 0o700))

	_, _, err := zm(t, file, "up", "dev", "--resume=abc-1")
	require.ErrorContains(t, err, "no conversation abc-1 under")
	require.ErrorContains(t, err, "zmina up dev --fresh")

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "ns", "projects", "p"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ns", "projects", "p", "abc-1.jsonl"), nil, 0o600))

	out, _, err := zm(t, file, "up", "dev", "--resume=abc-1")
	require.ErrorContains(t, err, "dev ended right after its start: No conversation found")
	assert.NotContains(t, out, "dev up as")
}

// realHasTranscript is the check as zmina runs it, kept before any test swaps it.
var realHasTranscript = hasTranscript

// A namespace with no projects yet holds no conversation: one recorded elsewhere is not there.
func TestHasTranscriptOfAFreshNamespace(t *testing.T) {
	ns := t.TempDir()
	assert.False(t, realHasTranscript(ns, "abc-1"))
	require.NoError(t, os.MkdirAll(filepath.Join(ns, "projects", "p"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ns, "projects", "p", "abc-1.jsonl"), nil, 0o600))
	assert.True(t, realHasTranscript(ns, "abc-1"))
	assert.False(t, realHasTranscript(ns, "../p/abc-1"))
}

// A plain up that resumes by the role's policy is checked the same way: a claude that ends at once is never «up».
func TestPlainUpThatResumesAndEndsAtOnce(t *testing.T) {
	file := testCrew(t, "  dev:\n    resume: always\n")
	dir := filepath.Dir(file)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\necho 'No conversation found with session ID: abc-1'\nexit 1\n"), 0o700))
	a := &app{crew: file, out: io.Discard, err: io.Discard}
	require.NoError(t, a.withState(func(s *shift.Shift) error {
		s.State.Role("dev").LastSession, s.State.Role("dev").LastRuntime = "abc-1", "claude"
		return nil
	}))

	out, _, err := zm(t, file, "up", "dev")
	require.ErrorContains(t, err, "dev ended right after its start")
	assert.NotContains(t, out, "dev up as")
}

// A namespace whose path reads as a glob is taken literally.
func TestHasTranscriptInAGlobLikeNamespace(t *testing.T) {
	ns := filepath.Join(t.TempDir(), "profile[dev]")
	require.NoError(t, os.MkdirAll(filepath.Join(ns, "projects", "p"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ns, "projects", "p", "abc-1.jsonl"), nil, 0o600))
	assert.True(t, realHasTranscript(ns, "abc-1"))
}

// Spaces in the namespace and a namespace reached through a symlink find their transcript too.
func TestHasTranscriptWithSpacesAndASymlink(t *testing.T) {
	real := filepath.Join(t.TempDir(), "my claude home")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "projects", "a b"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(real, "projects", "a b", "abc-1.jsonl"), nil, 0o600))
	assert.True(t, realHasTranscript(real, "abc-1"))

	link := filepath.Join(t.TempDir(), "ns")
	require.NoError(t, os.Symlink(real, link))
	assert.True(t, realHasTranscript(link, "abc-1"))
	assert.False(t, realHasTranscript(link, "abc-2"))
}
