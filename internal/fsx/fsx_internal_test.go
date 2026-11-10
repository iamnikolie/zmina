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

// Where the hard link is refused (state.json leading to a file on another file system), the backup is a copy of
// its own: whole, synced, and unchanged when the file it copies changes.
func TestBackupCopiesWhereItCannotLink(t *testing.T) {
	link = func(oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { link = os.Link })

	dir := t.TempDir()
	src, dst := filepath.Join(dir, "state.json"), filepath.Join(dir, "state.json.prev")

	if err := os.WriteFile(src, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(dst, []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Backup(src, dst); err != nil {
		t.Fatal(err)
	}

	if err := WriteFile(src, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(dst); string(got) != "first" {
		t.Fatalf("backup: %q, want the state before the save", got)
	}
}

// A backup that cannot be made is an error and leaves the one there.
func TestBackupThatFailsKeepsTheOldOne(t *testing.T) {
	link = func(oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { link = os.Link })

	dir := t.TempDir()
	src, dst := filepath.Join(dir, "state.json"), filepath.Join(dir, "state.json.prev")

	if err := os.WriteFile(src, []byte("first"), 0o000); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(dst, []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}

	if os.Getuid() == 0 {
		t.Skip("root reads a file of mode 000")
	}

	if err := Backup(src, dst); err == nil {
		t.Fatal("no error")
	}

	if got, _ := os.ReadFile(dst); string(got) != "older" {
		t.Fatalf("backup: %q, want the old one kept", got)
	}
}
