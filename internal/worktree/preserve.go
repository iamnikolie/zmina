package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	// worktree's HEAD; empty when the worktree was clean. DirtyOver is the commit that one is over, as the commit itself
	// says (the parent of the commit kept, also when a ref kept before is the one reused), shortened; empty for none.
	Dirty     string
	DirtyOver string
	// Index is the ref that holds what the worktree's own index holds when that is neither HEAD's tree nor the files': work
	// that lives in the index alone, a file staged and then deleted from the disk, an ignored file added with force and not
	// committed. IndexOver is the commit it is over, as DirtyOver is.
	Index     string
	IndexOver string
}

// KeptBranch is a branch Preserve made for commits the base does not have.
type KeptBranch struct {
	Name    string
	Tip     string
	Commits int
}

// Empty reports that there was nothing to keep.
func (p Preserved) Empty() bool { return len(p.Branches) == 0 && p.Dirty == "" && p.Index == "" }

// Preserve keeps the work a removal of the worktree and its branch would lose, in git, before anything is removed. The
// commits that are not in the base go to a branch zmina-preserve/<id>-<tip8>, one for each tip they are at (the task's
// branch, and the worktree's HEAD when that is somewhere else, a detached one); the uncommitted changes, tracked and
// untracked but not ignored, go to a commit over the worktree's HEAD under refs/zmina-preserve-dirty/<id>-<tree8>, named by
// the tree of the snapshot, so that keeping the same changes again is the same ref. What lives in the worktree's own index
// alone (a file staged and then deleted from the disk, an ignored file added with force) is kept as a second commit under
// refs/zmina-preserve-dirty/<id>-index-<tree8> when its tree is neither HEAD's nor the files'. Whether there are changes is
// found by comparing trees, never by git status, whose idea of untracked files is the user's config. The snapshot is made
// with a temporary index (GIT_INDEX_FILE) and plumbing, and the worktree's own index is only read, from a copy, so that its
// file, its files, any other worktree and the stash stack, which other sessions share, are as they were. A branch or a ref
// that is there already for the same commit (the same tree, for the changes) is the same work kept: nothing is made twice.
// id is a short neutral name for the task, a part of the branch name.
//
// What cannot be kept is refused, and so is the removal that would lose it: a nested git repository, an untracked directory
// with a .git of its own, a submodule whose commit is not the one HEAD records or whose working tree has work of its own, an
// index with conflicts, a directory that exists but is not the checkout git sees there, a path that cannot be read. Only a
// directory that is gone means there is no worktree to look into. Nothing to keep gives the zero Preserved and no error; an
// error means the work was not kept, no ref was made, and the caller must not remove it.
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

	// What can be refused is found before anything is made: the objects of the snapshots (which are no refs) first.
	var snaps []*snapshot

	if hasTree {
		if snaps, err = t.capture(ctx, id, head); err != nil {
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

	for _, snap := range snaps {
		if err := t.keepRef(ctx, snap.ref, snap.commit, snap.tree); err != nil {
			return out, err
		}

		if snap.indexOnly {
			out.Index, out.IndexOver = snap.ref, snap.over
		} else {
			out.Dirty, out.DirtyOver = snap.ref, snap.over
		}
	}

	return out, nil
}

// hasWorktree says whether there is a worktree of this tree to look into. A directory that is gone is none (false, no error):
// nothing is left there to lose. A directory that exists is looked into, and it must be the checkout git sees there: the
// same file as git's top level for the path, compared as files (os.SameFile), so that a spelling that differs by case or by
// Unicode form, or goes through a symlink, is the same directory. One that exists and is not that checkout (a plain
// directory inside another repository, a leftover) is an error, not a worktree with nothing in it: nothing could be kept of
// it, and the removal of work that was not looked at is refused. So is a path that cannot be read, or whose top level
// cannot be told.
func (t *Tree) hasWorktree(ctx context.Context) (bool, error) {
	have, err := os.Stat(t.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("the worktree %s cannot be read: %w", t.Path, err)
	}

	top, err := gitCtx(ctx, t.Path, "rev-parse", "--show-toplevel")
	if err != nil {
		return false, fmt.Errorf("%s exists and git does not see a checkout in it: %w", t.Path, err)
	}

	top = strings.TrimSpace(top)

	seen, err := os.Stat(top)
	if err != nil {
		return false, fmt.Errorf("the top level of %s, %s, cannot be read: %w", t.Path, top, err)
	}

	if !os.SameFile(have, seen) {
		return false, fmt.Errorf("%s exists but is not a checkout of its own: git's top level for it is %s, which is another "+
			"checkout's, so nothing of it is kept", t.Path, top)
	}

	return true, nil
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

// snapshot is work of a worktree as a commit that is made and not yet named by a ref: the tree, the commit over the worktree's
// HEAD, the ref it goes under (named by the tree) and the commit it is over, as the commit says. indexOnly marks the one made
// of the worktree's own index.
type snapshot struct {
	tree, commit, ref, over string
	indexOnly               bool
}

// capture makes the objects of the snapshots of the worktree's uncommitted work: of the files (everything the worktree holds
// that is not ignored) when their tree is not HEAD's, and of the worktree's own index when its tree is neither HEAD's nor
// the files'. No ref is made. It refuses what a tree cannot hold (refuseNested) and an index that cannot be written as one
// (conflicts).
func (t *Tree) capture(ctx context.Context, id, head string) ([]*snapshot, error) {
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

	if err := t.refuseNested(ctx, index, head); err != nil {
		return nil, err
	}

	filesTree, err := t.writeTree(ctx, index)
	if err != nil {
		return nil, err
	}

	headTree, err := t.treeOf(ctx, dir, head)
	if err != nil {
		return nil, err
	}

	var snaps []*snapshot

	if filesTree != headTree {
		snap, err := t.snapshotOf(ctx, head, filesTree, PreserveDirtyPrefix+id+"-"+short(filesTree), false,
			"zmina: uncommitted changes of "+id+" in "+t.Branch)
		if err != nil {
			return nil, err
		}

		snaps = append(snaps, snap)
	}

	// What the worktree's own index holds, read from a copy: it is never touched, and an index with conflicts cannot be
	// written as a tree, which is an error, not a clean worktree.
	indexTree, ok, err := t.ownIndexTree(ctx, dir)
	if err != nil {
		return nil, err
	}

	if ok && indexTree != headTree && indexTree != filesTree {
		snap, err := t.snapshotOf(ctx, head, indexTree, PreserveDirtyPrefix+id+"-index-"+short(indexTree), true,
			"zmina: what the index holds of "+id+" in "+t.Branch)
		if err != nil {
			return nil, err
		}

		snaps = append(snaps, snap)
	}

	return snaps, nil
}

// writeTree is the tree of the index the env names, written to the object store.
func (t *Tree) writeTree(ctx context.Context, env []string) (string, error) {
	tree, err := gitEnv(ctx, t.Path, env, "write-tree")

	return strings.TrimSpace(tree), err
}

// treeOf is the tree of a commit; the empty tree for none (a repository with no commit yet).
func (t *Tree) treeOf(ctx context.Context, dir, commit string) (string, error) {
	if commit == "" {
		return t.writeTree(ctx, []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "empty-index")})
	}

	tree, err := gitCtx(ctx, t.Path, "rev-parse", "--verify", commit+"^{tree}")

	return strings.TrimSpace(tree), err
}

// ownIndexTree is the tree of the worktree's own index, from a copy of its file; false when there is no index file (a
// repository with nothing staged yet).
func (t *Tree) ownIndexTree(ctx context.Context, dir string) (string, bool, error) {
	path, err := gitCtx(ctx, t.Path, "rev-parse", "--git-path", "index")
	if err != nil {
		return "", false, err
	}

	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(t.Path, path)
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("the index of %s cannot be read: %w", t.Path, err)
	}

	copied := filepath.Join(dir, "own-index")
	if err := os.WriteFile(copied, data, 0o600); err != nil {
		return "", false, err
	}

	tree, err := t.writeTree(ctx, []string{"GIT_INDEX_FILE=" + copied})
	if err != nil {
		return "", false, fmt.Errorf("the index of %s cannot be written as a tree (conflicts?): %w", t.Path, err)
	}

	return tree, true, nil
}

// snapshotOf is the snapshot of a tree: a commit of it over head, unless the ref it goes under is there already for this
// tree, when that commit is the one kept (a retry makes no second commit). The commit it is over is what that commit says.
func (t *Tree) snapshotOf(ctx context.Context, head, tree, ref string, indexOnly bool, message string) (*snapshot, error) {
	snap := &snapshot{tree: tree, ref: ref, indexOnly: indexOnly}

	have, ok, err := t.revOf(ctx, t.Repo, ref)
	if err != nil {
		return nil, err
	}

	if ok {
		snap.commit = have
	} else {
		// The snapshot is authored by zmina and signed by nobody: the identity and the signing of whoever's repository this
		// is are not asked for.
		who := []string{"GIT_AUTHOR_NAME=zmina", "GIT_AUTHOR_EMAIL=zmina@localhost", "GIT_COMMITTER_NAME=zmina",
			"GIT_COMMITTER_EMAIL=zmina@localhost", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=commit.gpgsign", "GIT_CONFIG_VALUE_0=false"}

		args := []string{"commit-tree", tree, "-m", message}
		if head != "" {
			args = append(args, "-p", head)
		}

		commit, err := gitEnv(ctx, t.Path, who, args...)
		if err != nil {
			return nil, err
		}

		snap.commit = strings.TrimSpace(commit)
	}

	if parent, ok, err := t.revOf(ctx, t.Repo, snap.commit+"^"); err != nil {
		return nil, err
	} else if ok {
		snap.over = short(parent)
	}

	return snap, nil
}

// refuseNested is an error for what a snapshot cannot hold, naming the first path and how many there are:
//   - an untracked directory with a .git of its own (git's own listing of the files that are not tracked and not ignored,
//     whatever the user's status config says, names it as a directory, with a slash);
//   - a submodule or nested repository whose commit in the snapshot's index (a 160000 entry, after add -A has looked at the
//     working tree) is not the one HEAD records for that path: it was moved or is new, and the commit is all a tree holds;
//   - an initialised submodule, one HEAD records as it is, whose working tree is not clean, or that git cannot be asked.
//
// A submodule that is recorded as HEAD records it and is clean, or is not initialised, holds nothing a tree would lose and is
// not in the way: a repository that merely has submodules is preserved.
func (t *Tree) refuseNested(ctx context.Context, index []string, head string) error {
	recorded := map[string]string{}

	if head != "" {
		tree, err := gitCtx(ctx, t.Path, "ls-tree", "-r", "-z", head)
		if err != nil {
			return err
		}

		for _, entry := range strings.Split(tree, "\x00") {
			if meta, path, ok := strings.Cut(entry, "\t"); ok && strings.HasPrefix(meta, "160000 commit ") {
				recorded[path] = strings.Fields(meta)[2]
			}
		}
	}

	var nested []string

	staged, err := gitEnv(ctx, t.Path, index, "ls-files", "-s", "-z")
	if err != nil {
		return err
	}

	for _, entry := range strings.Split(staged, "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok || !strings.HasPrefix(meta, "160000 ") {
			continue
		}

		if recorded[path] != strings.Fields(meta)[1] {
			nested = append(nested, path+" (a nested repository, or a submodule at a commit HEAD does not record)")

			continue
		}

		if why := t.submoduleWork(ctx, path); why != "" {
			nested = append(nested, path+" ("+why+")")
		}
	}

	others, err := gitCtx(ctx, t.Path, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}

	for _, path := range strings.Split(others, "\x00") {
		if dir := strings.TrimSuffix(path, "/"); dir != path && !slices.ContainsFunc(nested, func(n string) bool { return strings.HasPrefix(n, dir+" (") }) {
			nested = append(nested, dir+" (an untracked directory with a repository of its own)")
		}
	}

	if len(nested) == 0 {
		return nil
	}

	more := ""
	if len(nested) > 1 {
		more = fmt.Sprintf(" (and %d more)", len(nested)-1)
	}

	return fmt.Errorf("%s%s: a snapshot of the worktree holds only the commit such a repository is at, not the work in it",
		nested[0], more)
}

// submoduleWork says what is in an initialised submodule that a snapshot of its parent would not hold: its working tree is not
// clean, or it cannot be asked. Empty for a submodule that is clean, and for one that is not initialised (no .git of its own
// in its directory).
func (t *Tree) submoduleWork(ctx context.Context, path string) string {
	dir := filepath.Join(t.Path, filepath.FromSlash(path))

	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}

		return "its directory cannot be read: " + err.Error()
	}

	status, err := gitCtx(ctx, dir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return "it cannot be asked: " + err.Error()
	}

	if strings.TrimSpace(status) != "" {
		return "it has uncommitted changes"
	}

	return ""
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
