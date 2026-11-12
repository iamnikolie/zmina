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
	"strconv"
	"strings"
	"time"
)

// Timeout bounds every git call: a hung git must not hold zmina's lock.
var Timeout = time.Minute

// waitDelay is how long after the end of a call made under a Tree's Ctx zmina still waits for its output pipes: a
// process git started (a hook, a credential helper) that outlives it and holds them would keep Wait for as long as it
// lives, and the deadline would not be one. Calls with no Ctx wait as they always did.
const waitDelay = 2 * time.Second

// gitCmd is git in ctx; bounded gives up its pipes waitDelay after ctx ends.
func gitCmd(ctx context.Context, bounded bool, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)

	if bounded {
		cmd.WaitDelay = waitDelay
	}

	return cmd
}

// Tree is one task's worktree.
type Tree struct {
	// Repo is the main checkout's top level.
	Repo string
	Path string
	// Branch is the task's branch; Base is the branch (or commit) it started from.
	Branch string
	Base   string
	// Ctx, when set, ends the git calls of Dirty and Merged when it is done (a sweep with a deadline). Remove does
	// not take it: a removal cut in the middle leaves a half-deleted worktree that no check can read, a slow one
	// only leaves the tick waiting, and each of its calls has Timeout.
	Ctx context.Context
}

// ctx is the context of the tree's reading calls.
func (t *Tree) ctx() context.Context {
	if t.Ctx != nil {
		return t.Ctx
	}

	return context.Background()
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

	if exists, err := branchExists(context.Background(), top, branch); err != nil {
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

	out, err := gitCtx(t.ctx(), t.Path, "status", "--short")

	return strings.TrimSpace(out), err
}

// Head is the commit the worktree stands at.
func (t *Tree) Head() (string, error) {
	out, err := gitCtx(t.ctx(), t.Path, "rev-parse", "--verify", "HEAD")

	return strings.TrimSpace(out), err
}

// Ahead is how many commits the worktree has beyond its base (git rev-list --count <base>..HEAD): none for a branch
// nothing was committed on, or one whose work its base already has.
func (t *Tree) Ahead() (int, error) {
	out, err := gitCtx(t.ctx(), t.Path, "rev-list", "--count", t.Base+"..HEAD")
	if err != nil {
		return 0, err
	}

	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("git rev-list: %q is not a count", strings.TrimSpace(out))
	}

	return n, nil
}

// Since is how many commits the worktree has since the commit from (git rev-list --count <from>..HEAD).
func (t *Tree) Since(from string) (int, error) {
	out, err := gitCtx(t.ctx(), t.Path, "rev-list", "--count", from+"..HEAD")
	if err != nil {
		return 0, err
	}

	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("git rev-list: %q is not a count", strings.TrimSpace(out))
	}

	return n, nil
}

// Created is the commit the branch was made at: the first entry of its reflog, when that is the entry that made it.
// It is empty when git kept no such entry (the reflog is off or has expired), and the caller then does not know.
func (t *Tree) Created() (string, error) {
	out, err := gitCtx(t.ctx(), t.Path, "reflog", "show", "--format=%H %gs", "refs/heads/"+t.Branch)
	if err != nil {
		return "", err
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")

	hash, subject, _ := strings.Cut(strings.TrimSpace(lines[len(lines)-1]), " ")
	if !strings.HasPrefix(subject, "branch: Created") || hash == "" {
		return "", nil
	}

	return hash, nil
}

// Merged reports whether every commit of the branch is in its base. A branch that no longer exists has nothing to
// lose and counts as merged.
func (t *Tree) Merged() (bool, error) {
	exists, err := branchExists(t.ctx(), t.Repo, t.Branch)
	if err != nil || !exists {
		return !exists, err
	}

	ctx, cancel := context.WithTimeout(t.ctx(), Timeout)
	defer cancel()

	err = gitCmd(ctx, t.Ctx != nil, "-C", t.Repo, "merge-base", "--is-ancestor", t.Branch, t.Base).Run()

	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && ctx.Err() == nil {
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

	exists, err := branchExists(context.Background(), t.Repo, t.Branch)
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
func branchExists(parent context.Context, repo, branch string) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, Timeout)
	defer cancel()

	out, err := gitCmd(ctx, parent.Done() != nil, "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).CombinedOutput()

	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && ctx.Err() == nil {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("git show-ref: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return true, nil
}

func git(dir string, args ...string) (string, error) {
	return gitCtx(context.Background(), dir, args...)
}

// gitCtx is git that also ends when parent does.
func gitCtx(parent context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, Timeout)
	defer cancel()

	out, err := gitCmd(ctx, parent.Done() != nil, append([]string{"-C", dir}, args...)...).CombinedOutput()
	if parent.Err() != nil {
		return "", fmt.Errorf("git %s: stopped: %w", args[0], parent.Err())
	}

	if ctx.Err() != nil {
		return "", fmt.Errorf("git %s: no answer in %s", args[0], Timeout)
	}

	if err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(out)))
	}

	return string(out), nil
}
