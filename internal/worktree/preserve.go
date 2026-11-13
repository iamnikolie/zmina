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
)

// The names work is kept under before a worktree that holds it is thrown away: a branch for the commits the base does not
// have, and a ref outside refs/heads and refs/stash for the uncommitted changes, so that neither shows among the owner's
// branches nor touches the stash stack other sessions share. Nothing prunes them.
const (
	PreserveBranchPrefix = "zmina-preserve/"
	PreserveDirtyPrefix  = "refs/zmina-preserve-dirty/"
)

// Preserved says where the work of a worktree was kept (Tree.Preserve). The zero value is nothing to keep.
type Preserved struct {
	// Branches are the branches that hold commits the base does not have, each at the tip it was kept at.
	Branches []KeptBranch
	// Dirty is the ref that holds the uncommitted changes, tracked and untracked (not ignored), as a commit over the
	// worktree's HEAD; empty when the worktree was clean.
	Dirty string
	// DirtyOver is the commit that snapshot is over, the worktree's HEAD, shortened; empty for a repository with no commit.
	DirtyOver string
}

// KeptBranch is a branch Preserve made for commits the base does not have.
type KeptBranch struct {
	Name    string
	Tip     string
	Commits int
}

// Empty reports that there was nothing to keep.
func (p Preserved) Empty() bool { return len(p.Branches) == 0 && p.Dirty == "" }

// Preserve keeps the work a removal of the worktree and its branch would lose, in git, before anything is removed. The
// commits that are not in the base go to a branch zmina-preserve/<id>-<tip8>, one for each tip they are at (the task's
// branch, and the worktree's HEAD when that is somewhere else, a detached one); the uncommitted changes, tracked and
// untracked but not ignored, go to a commit over the worktree's HEAD under refs/zmina-preserve-dirty/<id>-<commit8>. The
// snapshot is made with a temporary index (GIT_INDEX_FILE) and plumbing, so the worktree's own index and files are as they
// were, no other worktree is disturbed, and the stash stack, which other sessions share, is not used. A branch or a ref
// that is there already for the same commit is the same work kept: nothing is made twice. id is a short neutral name for
// the task, a part of the branch name. Nothing to keep gives the zero Preserved and no error; an error means the work was
// not kept, and the caller must not remove it.
func (t *Tree) Preserve(id string) (Preserved, error) {
	var out Preserved

	id = refPart(id)
	if id == "" {
		return out, errors.New("a task id is needed to name the kept work")
	}

	ctx := t.ctx()
	_, statErr := os.Stat(t.Path)
	hasTree := statErr == nil

	var tips []string

	tip, ok, err := t.revOf(ctx, t.Repo, "refs/heads/"+t.Branch)
	if err != nil {
		return out, err
	}

	if ok {
		tips = append(tips, tip)
	}

	head := ""

	if hasTree {
		h, ok, err := t.revOf(ctx, t.Path, "HEAD")
		if err != nil {
			return out, err
		}

		if ok {
			head = h

			if !contains(tips, head) {
				tips = append(tips, head)
			}
		}
	}

	for _, tip := range tips {
		kept, err := t.keepCommits(ctx, id, tip)
		if err != nil {
			return out, err
		}

		if kept.Name != "" {
			out.Branches = append(out.Branches, kept)
		}
	}

	if hasTree {
		ref, over, err := t.keepChanges(ctx, id, head)
		if err != nil {
			return out, err
		}

		out.Dirty, out.DirtyOver = ref, over
	}

	return out, nil
}

// revOf is the commit a name is in dir, false when there is none (a branch that is gone, a repository with no commit).
func (t *Tree) revOf(ctx context.Context, dir, name string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	out, err := gitCmd(ctx, t.Ctx != nil, "-C", dir, "rev-parse", "--verify", "--quiet", name+"^{commit}").Output()

	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && ctx.Err() == nil {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("git rev-parse %s: %w", name, err)
	}

	return strings.TrimSpace(string(out)), true, nil
}

// keepCommits makes the branch for the commits at tip that the base does not have; the zero KeptBranch when it has none.
func (t *Tree) keepCommits(ctx context.Context, id, tip string) (KeptBranch, error) {
	ahead, err := gitCtx(ctx, t.Repo, "rev-list", "--count", t.Base+".."+tip)
	if err != nil {
		return KeptBranch{}, err
	}

	n, err := strconv.Atoi(strings.TrimSpace(ahead))
	if err != nil {
		return KeptBranch{}, fmt.Errorf("git rev-list: %q is not a count", strings.TrimSpace(ahead))
	}

	if n == 0 {
		return KeptBranch{}, nil
	}

	name := PreserveBranchPrefix + id + "-" + short(tip)

	if err := t.keepRef(ctx, "refs/heads/"+name, tip); err != nil {
		return KeptBranch{}, err
	}

	return KeptBranch{Name: name, Tip: tip, Commits: n}, nil
}

// keepChanges snapshots the worktree's uncommitted changes as a commit over head and returns its ref and what it is over;
// empty when the worktree has none (or the files are exactly what head has).
func (t *Tree) keepChanges(ctx context.Context, id, head string) (ref, over string, err error) {
	status, err := gitCtx(ctx, t.Path, "status", "--short")
	if err != nil {
		return "", "", err
	}

	if strings.TrimSpace(status) == "" {
		return "", "", nil
	}

	dir, err := os.MkdirTemp("", "zmina-index-")
	if err != nil {
		return "", "", err
	}

	defer func() { _ = os.RemoveAll(dir) }()

	// The index git writes is this file's, never the worktree's own.
	index := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}

	if head != "" {
		if _, err := gitEnv(ctx, t.Path, index, "read-tree", head); err != nil {
			return "", "", err
		}
	}

	if _, err := gitEnv(ctx, t.Path, index, "add", "-A"); err != nil {
		return "", "", err
	}

	tree, err := gitEnv(ctx, t.Path, index, "write-tree")
	if err != nil {
		return "", "", err
	}

	tree = strings.TrimSpace(tree)

	if head != "" {
		base, err := gitCtx(ctx, t.Path, "rev-parse", "--verify", head+"^{tree}")
		if err != nil {
			return "", "", err
		}

		if strings.TrimSpace(base) == tree {
			return "", "", nil
		}
	}

	// The snapshot is authored by zmina and signed by nobody: the identity and the signing of whoever's repository this is
	// are not asked for.
	who := []string{"GIT_AUTHOR_NAME=zmina", "GIT_AUTHOR_EMAIL=zmina@localhost", "GIT_COMMITTER_NAME=zmina",
		"GIT_COMMITTER_EMAIL=zmina@localhost", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=commit.gpgsign", "GIT_CONFIG_VALUE_0=false"}

	args := []string{"commit-tree", tree, "-m", "zmina: uncommitted changes of " + id + " in " + t.Branch}
	if head != "" {
		args = append(args, "-p", head)
	}

	commit, err := gitEnv(ctx, t.Path, who, args...)
	if err != nil {
		return "", "", err
	}

	commit = strings.TrimSpace(commit)
	ref = PreserveDirtyPrefix + id + "-" + short(commit)

	if err := t.keepRef(ctx, ref, commit); err != nil {
		return "", "", err
	}

	return ref, short(head), nil
}

// keepRef points ref at commit in the main repository. A ref that is there already for the same commit is the work kept
// once before; for another commit the name is taken by something else, and nothing is overwritten.
func (t *Tree) keepRef(ctx context.Context, ref, commit string) error {
	if have, ok, err := t.revOf(ctx, t.Repo, ref); err != nil {
		return err
	} else if ok {
		if have == commit {
			return nil
		}

		return fmt.Errorf("%s is there already, at %s, not %s", ref, short(have), short(commit))
	}

	// The old value is "nothing": a ref made meanwhile by another is not overwritten.
	_, err := gitCtx(ctx, t.Repo, "update-ref", ref, commit, strings.Repeat("0", len(commit)))

	return err
}

// refPart is s as one part of a ref name: letters, digits and . _ - only, lower case, no leading dot or dash.
func refPart(s string) string {
	var b strings.Builder

	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	return strings.Trim(b.String(), ".-")
}

// short is the first 8 characters of a commit id.
func short(commit string) string { return commit[:min(len(commit), 8)] }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}

	return false
}
