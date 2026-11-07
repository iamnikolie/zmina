package cmd

import (
	"os"
	"syscall"
	"time"
)

// fileChange is when a file's inode last changed (its ctime: a write, a rename, a new link, a chmod; only root can set
// it back) and how many links it has; ok is false when the system does not say.
func fileChange(st os.FileInfo) (changed time.Time, links uint64, ok bool) {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, 0, false
	}

	return time.Unix(s.Ctimespec.Sec, s.Ctimespec.Nsec), uint64(s.Nlink), true
}
