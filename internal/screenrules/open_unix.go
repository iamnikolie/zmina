//go:build unix

package screenrules

import (
	"fmt"
	"os"
	"syscall"
)

// openPlain opens a path that was a plain file a moment ago, and makes sure it still is: opened without waiting for a writer, so
// that a path swapped for a FIFO between the look and the open is refused, not waited on, and never as a terminal that would
// become the command's own.
func openPlain(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}

	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()

		return nil, fmt.Errorf("%s is not a regular file: it is not read", path)
	}

	return f, nil
}
