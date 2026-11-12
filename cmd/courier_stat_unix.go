//go:build darwin || linux

package cmd

import (
	"os"
	"syscall"
)

// fileInode is a file's inode number, 0 when the system does not say. state.json is replaced by a rename, so every save
// of it is a new inode.
func fileInode(st os.FileInfo) uint64 {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}

	return uint64(s.Ino) //nolint:unconvert // Ino is uint32 on some systems
}
