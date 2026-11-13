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
// untracked but not ignored, go to a commit over the worktree's HEAD under refs/zmina-preserve-dirty/<id>-<tree8>, named by
// the tree of the snapshot, so that keeping the same changes again is the same ref. Whether there are changes is found by
// comparing the snapshot's tree with HEAD's, never by git status, whose idea of untracked files is the user's config. The
// snapshot is made with a temporary index (GIT_INDEX_FILE) and plumbing, so the worktree's own index and files are as they
// were, no other worktree is disturbed, and the stash stack, which other sessions share, is not used. A branch or a ref
// that is there already for the same commit (the same tree, for the changes) is the same work kept: nothing is made twice.
// id is a short neutral name for the task, a part of the branch name.
//
// What a snapshot cannot hold is refused, and so is the removal that would lose it: a nested git repository or a submodule
// (a 160000 entry, or an untracked directory with a .git of its own). A directory that is not the worktree's own checkout (git's
// top level for it is another one) is no worktree to keep, as a directory that is gone is none; a path that cannot be read at
// all is an error. Nothing to keep gives the zero Preserved and no error; an error means the work was not kept, nothing was
// made, and the caller must not remove it.
func (t *Tree) Preserve(id string) (Preserved, error) {
	var out Preserved

	id = refPart(id)
	if id == "" {
		return out, errors.New("a task id is needed to name the kept work")
	}

	ctx := t.ctx()

	hasTree, err := t.hasWorktree(ctx)
	if err != nil {
		return out, err
	}

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

	// What can be refused is found before anything is made: the snapshot's objects (which are no refs) first.
	var snap *snapshot

	if hasTree {
		if snap, err = t.snapshotChanges(ctx, id, head); err != nil {
			return out, err
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

	if snap != nil {
		if err := t.keepRef(ctx, snap.ref, snap.commit, snap.tree); err != nil {
			return out, err
		}

		out.Dirty, out.DirtyOver = snap.ref, short(head)
	}

	return out, nil
}

// hasWorktree says whether there is a worktree of this tree to look into: false for a directory that is gone, and for one
// that is not the checkout the task made (git's top level for the path is another directory, say a plain directory left inside
// another repository), which is never snapshotted; an error for a path that cannot be read, or whose top level cannot be told.
func (t *Tree) hasWorktree(ctx context.Context) (bool, error) {
	if _, err := os.Stat(t.Path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("the worktree %s cannot be read: %w", t.Path, err)
	}

	top, err := gitCtx(ctx, t.Path, "rev-parse", "--show-toplevel")
	if err != nil {
		return false, err
	}

	want, err := filepath.EvalSymlinks(t.Path)
	if err != nil {
		return false, fmt.Errorf("the worktree %s cannot be read: %w", t.Path, err)
	}

	got, err := filepath.EvalSymlinks(strings.TrimSpace(top))
	if err != nil {
		return false, fmt.Errorf("the top level of %s, %s, cannot be read: %w", t.Path, strings.TrimSpace(top), err)
	}

	return filepath.Clean(want) == filepath.Clean(got), nil
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

	if err := t.keepRef(ctx, "refs/heads/"+name, tip, ""); err != nil {
		return KeptBranch{}, err
	}

	return KeptBranch{Name: name, Tip: tip, Commits: n}, nil
}

// snapshot is the uncommitted changes of a worktree as a commit that is made and not yet named by a ref: the tree, the commit
// over the worktree's HEAD, and the ref it goes under (named by the tree).
type snapshot struct {
	tree, commit, ref string
}

// snapshotChanges makes the objects of a snapshot of the worktree's uncommitted changes; nil when there are none, that is
// when the tree of everything the worktree holds that is not ignored is HEAD's own. It refuses what a tree cannot hold.
func (t *Tree) snapshotChanges(ctx context.Context, id, head string) (*snapshot, error) {
	dir, err := os.MkdirTemp("", "zmina-index-")
	if err != nil {
		return nil, err
	}

	defer func() { _ = os.RemoveAll(dir) }()

	// The index git writes is this file's, never the worktree's own.
	index := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}

	if head != "" {
		if _, err := gitEnv(ctx, t.Path, index, "read-tree", head); err != nil {
			return nil, err
		}
	}

	if _, err := gitEnv(ctx, t.Path, index, "add", "-A"); err != nil {
		return nil, err
	}

	if err := t.refuseNested(ctx, index); err != nil {
		return nil, err
	}

	tree, err := gitEnv(ctx, t.Path, index, "write-tree")
	if err != nil {
		return nil, err
	}

	tree = strings.TrimSpace(tree)

	if head != "" {
		base, err := gitCtx(ctx, t.Path, "rev-parse", "--verify", head+"^{tree}")
		if err != nil {
			return nil, err
		}

		if strings.TrimSpace(base) == tree {
			return nil, nil
		}
	} else if files, err := gitCtx(ctx, t.Path, "ls-tree", tree); err != nil {
		return nil, err
	} else if strings.TrimSpace(files) == "" {
		return nil, nil
	}

	snap := &snapshot{tree: tree, ref: PreserveDirtyPrefix + id + "-" + short(tree)}

	// Kept before, for this very tree: the same ref is the same work, and no second commit is made for it.
	if have, ok, err := t.revOf(ctx, t.Repo, snap.ref); err != nil {
		return nil, err
	} else if ok {
		snap.commit = have

		return snap, nil
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
		return nil, err
	}

	snap.commit = strings.TrimSpace(commit)

	return snap, nil
}

// refuseNested is an error for what a snapshot cannot hold: a nested git repository or a submodule, whose content a tree
// holds only as a commit id (a 160000 entry of the snapshot's index), and an untracked directory with a .git of its own
// (git's own listing of the files that are not tracked, not ignored, whatever the user's status config says, names it as a
// directory, with a slash). It names the first path and how many there are. The removal of such a worktree is refused with it.
func (t *Tree) refuseNested(ctx context.Context, index []string) error {
	var nested []string

	staged, err := gitEnv(ctx, t.Path, index, "ls-files", "-s", "-z")
	if err != nil {
		return err
	}

	for _, entry := range strings.Split(staged, "\x00") {
		if meta, path, ok := strings.Cut(entry, "\t"); ok && strings.HasPrefix(meta, "160000 ") {
			nested = append(nested, path)
		}
	}

	others, err := gitCtx(ctx, t.Path, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}

	for _, path := range strings.Split(others, "\x00") {
		if strings.HasSuffix(path, "/") && !contains(nested, strings.TrimSuffix(path, "/")) {
			nested = append(nested, strings.TrimSuffix(path, "/"))
		}
	}

	if len(nested) == 0 {
		return nil
	}

	more := ""
	if len(nested) > 1 {
		more = fmt.Sprintf(" (and %d more)", len(nested)-1)
	}

	return fmt.Errorf("%s%s is a nested git repository or a submodule: a snapshot of the worktree holds only the commit it is at, "+
		"not its files", nested[0], more)
}

// keepRef points ref at commit in the main repository. A ref that is there already for the same commit is the work kept
// once before; for the changes the same tree is as good as the same commit (tree, when it is not empty), as a retry makes a
// commit of its own. A ref that is there for something else is a name taken, and nothing is overwritten.
func (t *Tree) keepRef(ctx context.Context, ref, commit, tree string) error {
	if have, ok, err := t.revOf(ctx, t.Repo, ref); err != nil {
		return err
	} else if ok {
		if have == commit {
			return nil
		}

		if tree != "" {
			if kept, err := gitCtx(ctx, t.Repo, "rev-parse", "--verify", have+"^{tree}"); err == nil && strings.TrimSpace(kept) == tree {
				return nil
			}
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
