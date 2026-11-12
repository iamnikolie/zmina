package cmd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A look at a lock nobody has made: not held, no error, and the file is still not there.
func TestProbeCourierLockOfAFileThatIsNotThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "courier.lock")

	got, err := probeCourierLock(path)
	require.NoError(t, err)
	assert.Equal(t, courierLock{}, got)
	assert.NoFileExists(t, path, "a look makes nothing")

	_, err = probeCourierLock(filepath.Join(path, "below-a-file-that-is-not-a-dir"))
	assert.NoError(t, err, "a directory that is not there holds no lock either")
}

// A lock a courier let go (it ended, however it ended) is free whatever pid is left in the file; the look leaves the
// file as it was and the lock free for the next courier.
func TestProbeCourierLockOfAFreeLockLeavesItAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "courier.lock")

	release, err := lockCourier(dir)
	require.NoError(t, err)

	release()

	before, err := os.Stat(path)
	require.NoError(t, err)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(raw), "\n", "the pid of the courier that ended is still written")

	got, err := probeCourierLock(path)
	require.NoError(t, err)
	assert.False(t, got.Held, "the pid left in the file does not make a courier")
	assert.Zero(t, got.PID)

	after, err := os.Stat(path)
	require.NoError(t, err, "the file is not removed")
	assert.Equal(t, before.ModTime(), after.ModTime(), "nor written")

	again, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, raw, again)

	// Never left held: a courier takes the lock right after.
	release, err = lockCourier(dir)
	require.NoError(t, err, "the look let the lock go")

	release()
}

// A courier that holds the lock is seen to, with its pid and when it started; the look changes nothing and a second
// look says the same, and the lock is free again once the courier lets it go.
func TestProbeCourierLockSeesTheCourierThatHoldsIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "courier.lock")

	before := time.Now().Add(-2 * time.Second)

	release, err := lockCourier(dir)
	require.NoError(t, err)

	st, err := os.Stat(path)
	require.NoError(t, err)

	for range 2 {
		got, err := probeCourierLock(path)
		require.NoError(t, err)
		assert.True(t, got.Held)
		assert.Equal(t, os.Getpid(), got.PID)
		assert.Equal(t, st.ModTime(), got.Since, "when the courier wrote its pid, at its start")
		assert.True(t, got.Since.After(before))
	}

	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, st.ModTime(), after.ModTime(), "a look writes nothing")

	_, err = lockCourier(dir)
	require.Error(t, err, "and the courier's lock is still its own")

	release()

	got, err := probeCourierLock(path)
	require.NoError(t, err)
	assert.False(t, got.Held)
}

// Holding the lock in a way the courier does not (a plain shared lock, as another look has for an instant) is no
// courier: the probe's own lock is shared, so two looks never take each other for one.
func TestProbeCourierLockLooksDoNotTakeEachOtherForACourier(t *testing.T) {
	path := filepath.Join(t.TempDir(), "courier.lock")
	require.NoError(t, os.WriteFile(path, []byte("1\n"), 0o600))

	other, err := os.Open(path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = other.Close() })

	require.NoError(t, syscall.Flock(int(other.Fd()), syscall.LOCK_SH|syscall.LOCK_NB))

	got, err := probeCourierLock(path)
	require.NoError(t, err)
	assert.False(t, got.Held)
}

// A file that cannot be opened says why; it is not taken for a free lock.
func TestProbeCourierLockSaysWhatItCannotOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any file")
	}

	path := filepath.Join(t.TempDir(), "courier.lock")
	require.NoError(t, os.WriteFile(path, []byte("1\n"), 0o000))

	_, err := probeCourierLock(path)
	require.Error(t, err)
}

// An unreadable pid in the file of a lock that is held is still a held lock: the courier is there, only its pid is not.
func TestProbeCourierLockHeldWithNoPidToRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "courier.lock")
	require.NoError(t, os.WriteFile(path, []byte("not a pid\n"), 0o600))

	holder, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)

	t.Cleanup(func() { _ = holder.Close() })

	require.NoError(t, syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))

	got, err := probeCourierLock(path)
	require.NoError(t, err)
	assert.True(t, got.Held)
	assert.Zero(t, got.PID)
}
