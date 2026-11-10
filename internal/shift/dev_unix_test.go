//go:build unix

package shift

import (
	"os"
	"syscall"
	"testing"
)

// devOf is the device a path is on.
func devOf(t *testing.T, path string) uint64 {
	t.Helper()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	return uint64(fi.Sys().(*syscall.Stat_t).Dev) //nolint:unconvert // int32 on darwin, uint64 on linux
}
