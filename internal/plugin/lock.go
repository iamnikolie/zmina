package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/iamnikolie/zmina/internal/fsx"
)

// Lock takes a plugin's delivery lock (crew.Layout.PluginLock) without waiting, and writes who holds it and since when into the
// file, for doctor to read; false when another run holds it. The release is the caller's.
func Lock(path string, now time.Time) (release func(), ok bool) {
	if fsx.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return nil, false
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}

	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		_ = f.Close()

		return nil, false
	}

	// Advisory: a record that cannot be written leaves the lock as good as it was.
	if f.Truncate(0) == nil {
		_, _ = fmt.Fprintf(f, "%d %s\n", os.Getpid(), now.UTC().Format(time.RFC3339))
	}

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true
}

// Holder looks at a delivery lock without taking it: whether another run holds it, and, from the record that run wrote, its pid and
// since when. A lock nobody holds says nothing; a record that does not read says held with no pid.
func Holder(path string) (held bool, pid int, since time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return false, 0, time.Time{}
	}

	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

		return false, 0, time.Time{}
	}

	raw := make([]byte, 128)
	n, _ := f.Read(raw)
	fields := strings.Fields(string(raw[:n]))

	if len(fields) == 2 {
		pid, _ = strconv.Atoi(fields[0])
		since, _ = time.Parse(time.RFC3339, fields[1])
	}

	return true, pid, since
}
