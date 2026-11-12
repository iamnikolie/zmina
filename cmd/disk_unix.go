//go:build darwin || linux

package cmd

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// statDisk is the free space (to an ordinary user) and size of the filesystem a path is on. A path that is not there
// yet (a config directory Claude Code has not made) is measured by its nearest directory that is.
func statDisk(path string) (diskSpace, error) {
	path = nearestDir(path)

	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return diskSpace{}, err
	}

	// The widths of these fields differ by system (Bsize is signed on Linux), so each goes through uint64.
	block := uint64(fs.Bsize) //nolint:unconvert,gosec // see above

	out := diskSpace{Free: uint64(fs.Bavail) * block, Total: uint64(fs.Blocks) * block} //nolint:unconvert

	if st, err := os.Stat(path); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			out.ID = "dev:" + strconv.FormatUint(uint64(sys.Dev), 10) //nolint:unconvert,gosec // int32 on macOS
		}
	}

	return out, nil
}

// nearestDir is path, or the closest of its parents that exists.
func nearestDir(path string) string {
	for p := path; ; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil {
			return p
		}

		if parent := filepath.Dir(p); parent == p {
			return path
		}
	}
}
