package cmd

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// execWithoutDeadline are the files that may call exec.Command (no context, so no deadline), each because nothing
// waits for the program, or the owner's terminal drives it and it ends by their hand. Every other external program
// runs through exec.CommandContext with a deadline (runTool, the lesson package's git and seat calls, the doctor's
// version reads), so a program that never answers cannot hold a beat or a command for good. A new line here needs
// that reason in this comment.
var execWithoutDeadline = map[string]string{
	"cmd/attach.go":            "the owner's terminal is attached to the role; it ends by their hand (the command is only built here)",
	"cmd/turnend.go":           "started detached and never waited for; it ends itself within turnEndWait",
	"cmd/lesson.go":            "the daily run started detached and never waited for; its own git and seat calls end",
	"cmd/ui.go":                "the browser opener is started and never waited for",
	"internal/codex/runner.go": "the App Server is a long-lived child by design; closing its pipe ends it, then a kill",
}

// Directories other work owns; their calls are checked there.
var execSkipDirs = map[string]bool{"internal/shift": true, "internal/tmux": true, "internal/mesh": true}

func TestExecCommandOnlyWhereNothingWaits(t *testing.T) {
	root := ".."

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if execSkipDirs[rel] || d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		if strings.Contains(string(raw), "exec.Command(") {
			if _, ok := execWithoutDeadline[rel]; !ok {
				t.Errorf("%s calls exec.Command, which has no deadline: use exec.CommandContext with one (runTool in cmd), "+
					"or list the file in execWithoutDeadline with the reason nothing waits for it", rel)
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for rel := range execWithoutDeadline {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || !strings.Contains(string(raw), "exec.Command(") {
			t.Errorf("%s is listed in execWithoutDeadline but no longer calls exec.Command: remove it from the list", rel)
		}
	}
}
