//go:build unix

package screenrules

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadWithin loads an override path and fails the test if that takes long: a path that is not a plain file must be refused
// without being opened, and never held a beat waiting for a writer.
func loadWithin(t *testing.T, path string) Overrides {
	t.Helper()

	done := make(chan Overrides, 1)

	go func() {
		_, o := LoadOverrides(Default(), path)
		done <- o
	}()

	select {
	case o := <-done:
		return o
	case <-time.After(5 * time.Second):
		// Let the blocked open go, so that the test does not leave a goroutine behind.
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}

		t.Fatal("loading the override file hung: it was opened before it was known to be a plain file")

		return Overrides{}
	}
}

func TestOverrideThatIsAFIFOIsRefusedWithoutOpeningIt(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "screen-rules.yaml")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	o := loadWithin(t, fifo)
	require.Error(t, o.Err)
	assert.Contains(t, o.Err.Error(), "not a regular file")
	assert.True(t, o.Present)
	assert.False(t, o.InForce())

	// A symlink to one is refused the same way.
	link := filepath.Join(dir, "link.yaml")
	require.NoError(t, os.Symlink(fifo, link))

	o = loadWithin(t, link)
	require.Error(t, o.Err)
	assert.Contains(t, o.Err.Error(), "not a regular file")

	// A directory, and a device.
	o = loadWithin(t, dir)
	require.Error(t, o.Err)
	assert.Contains(t, o.Err.Error(), "not a regular file")

	if _, err := os.Stat("/dev/null"); err == nil {
		o = loadWithin(t, "/dev/null")
		require.Error(t, o.Err)
		assert.Contains(t, o.Err.Error(), "not a regular file")
	}
}

// A symlink to a plain file is a plain file to read: dotfile managers link them.
func TestOverrideThatIsASymlinkToAPlainFileIsRead(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.yaml")
	require.NoError(t, os.WriteFile(target, []byte("rules:\n  - id: claude.peek.hint-todos\n    disabled: true\n"), 0o600))

	link := filepath.Join(dir, "screen-rules.yaml")
	require.NoError(t, os.Symlink(target, link))

	o := loadWithin(t, link)
	require.NoError(t, o.Err)
	assert.Equal(t, 1, o.Count)
}

// A symlink that points at nothing is a file that was meant to be there: it is refused and said, not taken for no file.
func TestOverrideThatIsADanglingSymlinkIsRefused(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "screen-rules.yaml")
	require.NoError(t, os.Symlink(filepath.Join(dir, "gone.yaml"), link))

	got, o := LoadOverrides(Default(), link)
	assert.True(t, o.Present, "the link is there")
	require.Error(t, o.Err)
	assert.Contains(t, o.Err.Error(), "symlink")
	assert.False(t, o.InForce())
	assert.Same(t, Default(), got)

	// No path at all is still no file.
	_, o = LoadOverrides(Default(), filepath.Join(dir, "nothing.yaml"))
	assert.False(t, o.Present)
	assert.NoError(t, o.Err)
}
