//go:build !darwin && !linux

package cmd

import "os"

// fileInode does not know a file's inode here: the courier tells a change by the time and the size alone.
func fileInode(os.FileInfo) uint64 { return 0 }
