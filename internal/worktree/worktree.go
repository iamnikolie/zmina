// Package worktree gives a task its own git worktree and branch, so two seats never edit one checkout, and takes it
// away again once the branch is merged.
package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Timeout bounds every git call: a hung git must not hold zmina's lock.
var Timeout = time.Minute

// Tree is one task's worktree.
type Tree struct {
	// Repo is the main checkout's top level.
	Repo string
	Path string
	// Branch is the task's branch; Base is the branch (or commit) it started from.
	Branch string
	Base   string
}

// TopLevel is the top of the git checkout dir is in.
func TopLevel(dir string) (string, error) {
	out, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s is not in a git repository", dir)
	}

	return strings.TrimSpace(out), nil
}

// ResolveBase is the base a new branch starts from: base as given, else the repo's current branch, else (a detached
// checkout) its commit.
func ResolveBase(repo, base string) (string, error) {
	if base != "" {
		if _, err := git(repo, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
			return "", fmt.Errorf("base %q is not a branch or commit in %s", base, repo)
		}

		return base, nil
	}

	out, err := git(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}

	if b := strings.TrimSpace(out); b != "HEAD" {
		return b, nil
	}

	out, err = git(repo, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(out), nil
}

// Add creates path as a worktree of repo on a new branch from base; an empty base is the repo's current branch.
func Add(repo, path, branch, base string) (*Tree, error) {
	top, err := TopLevel(repo)
	if err != nil {
		return nil, err
	}

	if base, err = ResolveBase(top, base); err != nil {
		return nil, err
	}

	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%s already exists", path)
	}

	if exists, err := branchExists(top, branch); err != nil {
		return nil, err
	} else if exists {
		return nil, fmt.Errorf("branch %s already exists in %s", branch, top)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}

	if _, err := git(top, "worktree", "add", "-b", branch, path, base); err != nil {
		return nil, err
	}

	return &Tree{Repo: top, Path: path, Branch: branch, Base: base}, nil
}

// Dirty lists the worktree's uncommitted changes (git status --short), empty when clean. A worktree that is gone
// is clean.
func (t *Tree) Dirty() (string, error) {
	if _, err := os.Stat(t.Path); errors.Is(err, os.ErrNotExist) {
		return "", nil
	}

	out, err := git(t.Path, "status", "--short")

	return strings.TrimSpace(out), err
}

// Merged reports whether every commit of the branch is in its base. A branch that no longer exists has nothing to
// lose and counts as merged.
func (t *Tree) Merged() (bool, error) {
	exists, err := branchExists(t.Repo, t.Branch)
	if err != nil || !exists {
		return !exists, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()

	err = exec.CommandContext(ctx, "git", "-C", t.Repo, "merge-base", "--is-ancestor", t.Branch, t.Base).Run()

	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("git merge-base: %w", err)
	}

	return true, nil
}

// Remove deletes the worktree and its branch. force also drops uncommitted changes and an unmerged branch.
func (t *Tree) Remove(force bool) error {
	if _, err := os.Stat(t.Path); err == nil {
		args := []string{"worktree", "remove", t.Path}
		if force {
			args = []string{"worktree", "remove", "--force", t.Path}
		}

		if _, err := git(t.Repo, args...); err != nil {
			return err
		}
	} else if _, err := git(t.Repo, "worktree", "prune"); err != nil {
		return err
	}

	exists, err := branchExists(t.Repo, t.Branch)
	if err != nil || !exists {
		return err
	}

	flag := "-d"
	if force {
		flag = "-D"
	}

	_, err = git(t.Repo, "branch", flag, t.Branch)

	return err
}

// branchExists tells "no such branch" (git exit 1) from a git that failed.
func branchExists(repo, branch string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "git", "-C", repo, "show-ref", "--verify", "--quiet",
		"refs/heads/"+branch).CombinedOutput()

	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("git show-ref: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return true, nil
}

func git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if ctx.Err() != nil {
		return "", fmt.Errorf("git %s: no answer in %s", args[0], Timeout)
	}

	if err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(out)))
	}

	return string(out), nil
}
