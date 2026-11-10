package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A file system without hard links gets an error, never a name that shows before its content.
func TestCreateFileWithoutHardLinksTouchesNoName(t *testing.T) {
	link = func(oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.ENOTSUP}
	}
	t.Cleanup(func() { link = os.Link })

	path := filepath.Join(t.TempDir(), "T-1.json")

	err := CreateFile(path, []byte("x"), 0o600)
	if err == nil || !errors.Is(err, syscall.ENOTSUP) {
		t.Fatalf("err = %v, want the refused link", err)
	}

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the name was taken (%v): a reader would see an empty or a half file", err)
	}

	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".*.tmp")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}
