//go:build !darwin && !linux

package cmd

import (
	"os"
	"time"
)

// fileChange does not know a file's ctime or links here: a sent file outside the roots is not served.
func fileChange(os.FileInfo) (time.Time, uint64, bool) { return time.Time{}, 0, false }
