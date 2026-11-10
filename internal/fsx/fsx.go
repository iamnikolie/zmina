// Package fsx writes the files zmina keeps: whole or not at all, and on disk before the call returns.
//
// Every write goes to a temporary file in the same directory (hidden, ending in .tmp, so no store reads it), is
// synced, and only then takes the file's name: a reader sees the old content or the new, never an empty or a half
// written file, and a process killed half way leaves the old file and at most a stray temporary file. The directory
// is synced after the name changes, so the new name survives a power loss too.
package fsx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// link is os.Link; tests replace it to stand for a file system without hard links.
var link = os.Link

// BeforeCommit, when set, runs after the temporary file is written and synced and before it takes its name. Tests
// set it to stop a process at that moment; it is nil in zmina.
var BeforeCommit func(tmp, path string)

// WriteFile puts data at path, replacing what is there. A path that is a symbolic link is written through: the
// file it points to is replaced, the link stays. A link that leads nowhere (dangling, a loop, a directory it may not
// read) is an error, and nothing is written.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	path, err := Resolve(path)
	if err != nil {
		return err
	}

	tmp, err := stage(path, data, perm)
	if err != nil {
		return err
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)

		return err
	}

	return SyncDir(filepath.Dir(path))
}

// CreateFile puts data at path only when nothing is there; otherwise it fails with an error that is os.ErrExist.
// The name appears with its whole content: there is no moment it names an empty file.
func CreateFile(path string, data []byte, perm os.FileMode) error {
	tmp, err := stage(path, data, perm)
	if err != nil {
		return err
	}

	defer func() { _ = os.Remove(tmp) }()

	// A hard link takes the name only if it is free, with the content already in place. There is no fallback: any
	// other way to take only a free name shows the name before its content, which is what this exists to prevent.
	if err := link(tmp, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s: %w", path, os.ErrExist)
		}

		if errors.Is(err, os.ErrNotExist) {
			return err
		}

		return fmt.Errorf("%s: a new file is made with a hard link, and this file system refused one: %w", path, err)
	}

	return SyncDir(filepath.Dir(path))
}

// Resolve is the file path names: path itself when it is not a symbolic link or does not exist, else the file the
// link leads to. A link that leads nowhere is an error.
func Resolve(path string) (string, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}

	if err != nil {
		return "", err
	}

	if fi.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}

	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%s is a link that leads nowhere, not written: %w", path, err)
	}

	return target, nil
}

// stage writes data to a new temporary file beside path and syncs it.
func stage(path string, data []byte, perm os.FileMode) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return "", err
	}

	tmp := f.Name()

	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(perm)
	}

	if err == nil {
		err = f.Sync()
	}

	if cerr := f.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		_ = os.Remove(tmp)

		return "", err
	}

	if BeforeCommit != nil {
		BeforeCommit(tmp, path)
	}

	return tmp, nil
}

// SyncDir makes a directory's entries durable: a file renamed or created in it keeps its name after a power loss.
// A file system that cannot sync a directory is not an error.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}

	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}

	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EBADF) {
		return nil
	}

	return err
}
