package fsx_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/iamnikolie/zmina/internal/fsx"
)

// TestHelperProcess is the process the kill tests stop between the write and the rename.
func TestHelperProcess(t *testing.T) {
	path := os.Getenv("FSX_KILL_PATH")
	if path == "" {
		t.Skip("run by the kill tests")
	}

	fsx.BeforeCommit = func(string, string) { os.Exit(7) }

	var err error
	if os.Getenv("FSX_KILL_OP") == "create" {
		err = fsx.CreateFile(path, []byte("new"), 0o600)
	} else {
		err = fsx.WriteFile(path, []byte("new"), 0o600)
	}

	t.Fatalf("not stopped before the rename: %v", err)
}

// killMidWrite runs a process that writes path and is killed after the temporary file is synced, before the rename.
func killMidWrite(t *testing.T, op, path string) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "FSX_KILL_PATH="+path, "FSX_KILL_OP="+op)

	err := cmd.Run()

	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("helper: %v", err)
	}
}

func strays(t *testing.T, path string) []string {
	t.Helper()

	got, err := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp"))
	if err != nil {
		t.Fatal(err)
	}

	return got
}

func TestWriteFileKilledBeforeRenameKeepsTheOldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "T-1.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	killMidWrite(t, "write", path)

	if got, err := os.ReadFile(path); err != nil || string(got) != "old" {
		t.Fatalf("after a kill mid write: %q, %v; want the old content", got, err)
	}

	// What the killed process left is a hidden temporary file no store reads.
	if s := strays(t, path); len(s) != 1 {
		t.Fatalf("strays %v, want the one temporary file", s)
	}
}

func TestCreateFileKilledBeforeRenameLeavesNoName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "T-1.json")

	killMidWrite(t, "create", path)

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("after a kill mid create the name exists (%v): a reader would see an empty file", err)
	}
}

func TestNoReaderSeesAnEmptyOrPartFile(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "T-1.json")
	old := filepath.Join(dir, "T-2.json")

	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	var seen []string

	fsx.BeforeCommit = func(_, path string) {
		raw, err := os.ReadFile(path)
		if err != nil {
			seen = append(seen, "missing")

			return
		}

		seen = append(seen, string(raw))
	}

	t.Cleanup(func() { fsx.BeforeCommit = nil })

	if err := fsx.CreateFile(fresh, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := fsx.WriteFile(old, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Just before each commit a reader sees no file (create) or the old one (replace).
	if len(seen) != 2 || seen[0] != "missing" || seen[1] != "old" {
		t.Fatalf("before commit a reader saw %q", seen)
	}

	for _, p := range []string{fresh, old} {
		if got, _ := os.ReadFile(p); string(got) != "new" {
			t.Fatalf("%s: %q", p, got)
		}

		if s := strays(t, p); len(s) != 0 {
			t.Fatalf("temporary files left: %v", s)
		}
	}
}

func TestCreateFileNeverReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "A-3.json")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := fsx.CreateFile(path, []byte("second"), 0o600)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want os.ErrExist", err)
	}

	if got, _ := os.ReadFile(path); string(got) != "first" {
		t.Fatalf("replaced: %q", got)
	}

	if s := strays(t, path); len(s) != 0 {
		t.Fatalf("temporary files left: %v", s)
	}
}

func TestCreateFileOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "L-1.json")

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)

	for range 20 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if fsx.CreateFile(path, []byte("x"), 0o600) == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	if wins != 1 {
		t.Fatalf("%d creates won, want 1", wins)
	}
}

func TestPermIsKept(t *testing.T) {
	dir := t.TempDir()

	for _, c := range []struct {
		name string
		do   func(string) error
	}{
		{"create", func(p string) error { return fsx.CreateFile(p, []byte("x"), 0o644) }},
		{"write", func(p string) error { return fsx.WriteFile(p, []byte("x"), 0o644) }},
	} {
		p := filepath.Join(dir, c.name)
		if err := c.do(p); err != nil {
			t.Fatal(err)
		}

		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
			t.Fatalf("%s: mode %v", c.name, fi.Mode().Perm())
		}
	}
}

// A crew file kept elsewhere and linked in stays a link: the file it points to gets the new content.
func TestWriteFileKeepsASymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "crew.yaml")
	link := filepath.Join(dir, "crew.yaml")

	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(real, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if err := fsx.WriteFile(link, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v, %v", fi, err)
	}

	if got, _ := os.ReadFile(real); string(got) != "new" {
		t.Fatalf("target: %q", got)
	}
}

// A link that leads nowhere is not replaced by a plain file: the write fails and the link stays as it was.
func TestWriteFileRefusesABrokenLink(t *testing.T) {
	dir := t.TempDir()

	loopA, loopB := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	dangling := filepath.Join(dir, "dangling")

	for _, l := range [][2]string{{filepath.Join(dir, "missing"), dangling}, {loopB, loopA}, {loopA, loopB}} {
		if err := os.Symlink(l[0], l[1]); err != nil {
			t.Fatal(err)
		}
	}

	for _, p := range []string{dangling, loopA} {
		if err := fsx.WriteFile(p, []byte("new"), 0o600); err == nil {
			t.Fatalf("%s: written", filepath.Base(p))
		}

		if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s: the link was replaced (%v)", filepath.Base(p), err)
		}
	}

	if _, err := os.Lstat(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the missing target was made: %v", err)
	}
}
