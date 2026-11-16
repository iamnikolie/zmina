package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// MoveTrailer is the trailer of the commit a move makes of a worktree's uncommitted work: "Zmina-Move: <id>". That commit is never on
// a branch: it is kept under the private ref of the move (MoveRef), locally and on origin, and the node that takes the work restores
// it from there (RestoreWIP), and only a commit that carries its own move's id.
const MoveTrailer = "Zmina-Move"

// NetTimeout bounds a git call that talks to a remote (push, fetch): longer than Timeout, which bounds the local ones.
var NetTimeout = 5 * time.Minute

// branchRe is what a branch zmina carries between nodes may be made of, before git is asked: no leading dash or dot, no spaces,
// no characters a shell or a refspec reads.
var branchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)

// shaRe is a commit id as git prints it: 40 hex digits, or 64 for a repository in the sha256 format.
var shaRe = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// IsSHA reports whether s is a whole commit id.
func IsSHA(s string) bool { return shaRe.MatchString(s) }

// CheckBranch refuses a branch name that is not a plain, valid branch (git check-ref-format), so that it is safe in a refspec and
// in a command line: it is data that came from another node.
func CheckBranch(name string) error {
	if !branchRe.MatchString(name) || strings.Contains(name, "..") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("branch %q is not a plain branch name", name)
	}

	if _, err := gitCtx(context.Background(), "", "check-ref-format", "refs/heads/"+name); err != nil {
		return fmt.Errorf("branch %q is not a valid branch name: %w", name, err)
	}

	return nil
}

// netGit is git that talks to a remote: it never asks for a password on a terminal, and gives up after NetTimeout.
func netGit(parent context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, NetTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = waitDelay

	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		if parent.Err() != nil {
			return "", fmt.Errorf("git %s: stopped, the time it was given ran out", args[0])
		}

		return "", fmt.Errorf("git %s: no answer in %s", args[0], NetTimeout)
	}

	if err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(out)))
	}

	return strings.TrimSpace(string(out)), nil
}

// Origin is the URL fetches from the repository's remote named origin use, as git resolves it (a url.<base>.insteadOf rewrite
// applied); empty when it has none.
func Origin(repo string) (string, error) { return originURL(repo, false) }

// PushOrigin is the URL pushes to origin use, as git resolves it (pushurl, and url.<base>.pushInsteadOf, applied); empty when there
// is no origin. It is what Origin says when nothing sets a push URL of its own.
func PushOrigin(repo string) (string, error) { return originURL(repo, true) }

func originURL(repo string, push bool) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()

	args := []string{"-C", repo, "remote", "get-url"}
	if push {
		args = append(args, "--push")
	}

	out, err := exec.CommandContext(ctx, "git", append(args, "origin")...).Output()

	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 2 {
		// git remote get-url exits 2 for a remote that is not there.
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("git remote get-url in %s: %w", repo, err)
	}

	return strings.TrimSpace(string(out)), nil
}

// RevParse is the commit a name is in repo ("" and no error for a name that is none).
func RevParse(repo, name string) (string, error) {
	t := &Tree{Repo: repo}
	rev, ok, err := t.revOf(context.Background(), repo, name)

	if err != nil || !ok {
		return "", err
	}

	return rev, nil
}

// Branch is the commit of a local branch of repo, "" when there is none.
func Branch(repo, branch string) (string, error) { return RevParse(repo, "refs/heads/"+branch) }

// CarriesMove reports whether the commit rev of repo is the commit a move with this id made: its message ends in the trailer
// "Zmina-Move: <id>", the whole line.
func CarriesMove(repo, rev, id string) (bool, error) {
	out, err := gitCtx(context.Background(), repo, "log", "-1", "--format=%B", rev)
	if err != nil {
		return false, err
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return false, nil
	}

	return strings.TrimSpace(lines[len(lines)-1]) == MoveTrailer+": "+id, nil
}

// moveRefRe is the id of a move as a part of a ref name.
var moveRefRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,40}$`)

// MoveRef is the private ref a move keeps the work of its task n under, on both nodes' side of origin and on the node that leaves:
// refs/zmina-move/<id>/<n>. It is no branch and no tag: nothing lists it among the owner's branches.
func MoveRef(id string, n int) string { return fmt.Sprintf("refs/zmina-move/%s/%d", refPart(id), n) }

// moveRefOK is whether ref is one a move keeps (a name zmina makes, never a path or an option).
func moveRefOK(ref string) bool {
	rest, ok := strings.CutPrefix(ref, "refs/zmina-move/")
	if !ok || strings.ContainsAny(ref, " :^~?*[\\") || strings.Contains(ref, "..") || strings.HasSuffix(ref, "/") {
		return false
	}

	id, name, ok := strings.Cut(rest, "/")

	return ok && moveRefRe.MatchString(id) && name != "" && !strings.Contains(name, "/")
}

// carryStates are what a worktree in the middle of is no worktree whose work a snapshot can give back: the git directory has a file or
// a directory for each (git rev-parse --git-path finds it for a linked worktree too).
var carryStates = []struct{ path, what string }{
	{"MERGE_HEAD", "a merge in progress"},
	{"CHERRY_PICK_HEAD", "a cherry-pick in progress"},
	{"REVERT_HEAD", "a revert in progress"},
	{"rebase-merge", "a rebase in progress"},
	{"rebase-apply", "a rebase or an am in progress"},
	{"sequencer", "a cherry-pick or revert sequence in progress"},
	{"BISECT_LOG", "a bisect in progress"},
}

// CheckCarry says whether the uncommitted work of the worktree can travel as CommitWIP makes it, and names what stands in the way when
// it cannot: a merge, rebase, cherry-pick, revert or bisect in progress, conflicts, a HEAD that is not the task's branch, a nested
// repository, a submodule with work of its own. Nothing is changed, and a worktree that is gone has nothing to carry (nil). It is the
// owner's preflight, so that a role is refused before it is parked and not after.
func (t *Tree) CheckCarry() error {
	ctx := t.ctx()

	has, err := t.hasWorktree(ctx)
	if err != nil {
		return err
	}

	if !has {
		return nil
	}

	if err := t.carryState(ctx); err != nil {
		return err
	}

	head, ok, err := t.revOf(ctx, t.Path, "HEAD")
	if err != nil || !ok {
		return err
	}

	dir, err := os.MkdirTemp("", "zmina-index-")
	if err != nil {
		return err
	}

	defer func() { _ = os.RemoveAll(dir) }()

	_, _, err = t.filesTrees(ctx, dir, head)

	return err
}

// carryState refuses a worktree that is in the middle of something, or whose HEAD is not the task's branch.
func (t *Tree) carryState(ctx context.Context) error {
	ref, err := gitCtx(ctx, t.Path, "symbolic-ref", "-q", "HEAD")
	if err != nil || strings.TrimSpace(ref) != "refs/heads/"+t.Branch {
		return fmt.Errorf("the worktree %s is not on the task's branch %s: its HEAD is %s", t.Path, t.Branch, strings.TrimSpace(ref))
	}

	for _, st := range carryStates {
		p, err := gitCtx(ctx, t.Path, "rev-parse", "--git-path", st.path)
		if err != nil {
			return err
		}

		p = strings.TrimSpace(p)
		if !filepath.IsAbs(p) {
			p = filepath.Join(t.Path, p)
		}

		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("the worktree %s has %s: finish or abort it first (a move carries files, not the state of a git operation)", t.Path, st.what)
		}
	}

	if unmerged, err := gitCtx(ctx, t.Path, "ls-files", "--unmerged"); err != nil {
		return err
	} else if strings.TrimSpace(unmerged) != "" {
		return fmt.Errorf("the worktree %s has conflicts (unmerged paths): resolve them first", t.Path)
	}

	return nil
}

// CommitWIP makes a commit of the uncommitted work of the worktree (every file it holds that is not ignored, tracked or untracked,
// found by comparing trees as Preserve finds it, whatever the index holds: what is staged does not travel, the files do) over the
// task's branch tip, with a message that ends in the trailer of the move id, and keeps it under the private ref MoveRef(id, n) of the
// repository. It touches nothing else: the branch stays where it is, and so do the index and the files, so that a move that is called off
// leaves the worktree as it found it. made is false, and commit the branch's tip as it is, when there was nothing to commit; a commit
// an earlier try made of the same files over the same tip is the one returned. The commit is made with plumbing under zmina's own name,
// signed by nobody, and no hook runs. It refuses what a tree cannot hold (CheckCarry), and a worktree that is gone.
func (t *Tree) CommitWIP(id string, n int) (commit string, made bool, err error) {
	ctx := t.ctx()

	has, err := t.hasWorktree(ctx)
	if err != nil {
		return "", false, err
	}

	if !has {
		return "", false, fmt.Errorf("the worktree %s is gone: nothing to commit", t.Path)
	}

	if err := t.carryState(ctx); err != nil {
		return "", false, err
	}

	head, ok, err := t.revOf(ctx, t.Path, "HEAD")
	if err != nil {
		return "", false, err
	}

	if !ok {
		return "", false, fmt.Errorf("the branch %s has no commit yet: commit once before it is moved", t.Branch)
	}

	dir, err := os.MkdirTemp("", "zmina-index-")
	if err != nil {
		return "", false, err
	}

	defer func() { _ = os.RemoveAll(dir) }()

	filesTree, headTree, err := t.filesTrees(ctx, dir, head)
	if err != nil {
		return "", false, err
	}

	if filesTree == headTree {
		return head, false, nil
	}

	ref := MoveRef(id, n)

	have, haveOK, err := t.revOf(ctx, t.Repo, ref)
	if err != nil {
		return "", false, err
	}

	if haveOK {
		// The commit an earlier try made of these files over this tip, whose answer was lost, is the one.
		if tree, err := gitCtx(ctx, t.Repo, "rev-parse", "--verify", have+"^{tree}"); err == nil && strings.TrimSpace(tree) == filesTree {
			if parent, ok, _ := t.revOf(ctx, t.Repo, have+"^"); ok && parent == head {
				return have, true, nil
			}
		}
	}

	commit, err = commitTree(ctx, t.Path, head, filesTree, "Work in progress, carried by a move of its role", MoveTrailer+": "+id)
	if err != nil {
		return "", false, err
	}

	// The ref is ours alone: made, or moved on from what an earlier try left, only if it is still what was read.
	old := strings.Repeat("0", len(commit))
	if haveOK {
		old = have
	}

	if _, err := gitCtx(ctx, t.Repo, "update-ref", "-m", "zmina move "+id, ref, commit, old); err != nil {
		return "", false, err
	}

	return commit, true, nil
}

// commitTree makes a commit of tree over parent with paragraphs as its message, authored by zmina, signed by nobody.
func commitTree(ctx context.Context, dir, parent, tree string, paragraphs ...string) (string, error) {
	who := []string{"GIT_AUTHOR_NAME=zmina", "GIT_AUTHOR_EMAIL=zmina@localhost", "GIT_COMMITTER_NAME=zmina",
		"GIT_COMMITTER_EMAIL=zmina@localhost", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=commit.gpgsign", "GIT_CONFIG_VALUE_0=false"}

	args := []string{"commit-tree", tree}
	for _, p := range paragraphs {
		args = append(args, "-m", p)
	}

	if parent != "" {
		args = append(args, "-p", parent)
	}

	out, err := gitEnv(ctx, dir, who, args...)

	return strings.TrimSpace(out), err
}

// RestoreWIP gives back the uncommitted work a move carried, in a worktree that was made clean on the branch's tip: wip is the commit
// CommitWIP made over that tip, and carries this move's trailer. The files become its tree, as changes nobody committed: an edited
// file is edited, a deleted one is deleted, a new one is untracked, and nothing is staged (the index is HEAD's). The branch is not
// moved. A worktree whose HEAD is not the parent of wip, a wip that is not one commit over it with this move's trailer, and a worktree
// that is not clean are refused, and nothing is changed.
func (t *Tree) RestoreWIP(wip, id string) error {
	if !IsSHA(wip) {
		return errors.New("the uncommitted work is a whole commit id")
	}

	ctx := t.ctx()

	head, ok, err := t.revOf(ctx, t.Path, "HEAD")
	if err != nil {
		return err
	}

	parent, pok, err := t.revOf(ctx, t.Path, wip+"^")
	if err != nil {
		return err
	}

	if !ok || !pok || parent != head {
		return fmt.Errorf("the commit %s of the uncommitted work is not over the tip %s of the worktree", short(wip), short(head))
	}

	if second, ok, _ := t.revOf(ctx, t.Path, wip+"^2"); ok {
		return fmt.Errorf("the commit %s of the uncommitted work has a second parent %s", short(wip), short(second))
	}

	if carries, err := CarriesMove(t.Path, wip, id); err != nil || !carries {
		return fmt.Errorf("the commit %s is not one this move made", short(wip))
	}

	if dirty, err := t.Dirty(); err != nil {
		return err
	} else if dirty != "" {
		return fmt.Errorf("the worktree %s is not clean: the work is restored onto a clean one", t.Path)
	}

	// A two-way read-tree: the files go from the tip's tree to wip's (the working tree is clean, so no file of its own is overwritten),
	// then the index goes back to the tip's and the files stay.
	if _, err := gitCtx(ctx, t.Path, "read-tree", "-m", "-u", head, wip); err != nil {
		return err
	}

	_, err = gitCtx(ctx, t.Path, "reset", "-q")

	return err
}

// Push pushes the local branch to the remote named origin under the same name. A remote branch that is not an ancestor of it is
// refused by git, and so is the push.
func Push(repo, branch string) error { return PushContext(context.Background(), repo, branch) }

// PushContext is Push that gives up when ctx ends.
func PushContext(ctx context.Context, repo, branch string) error {
	if err := CheckBranch(branch); err != nil {
		return err
	}

	_, err := netGit(ctx, repo, "push", "origin", "refs/heads/"+branch+":refs/heads/"+branch)

	return err
}

// PushWIP pushes the commit CommitWIP made to origin under the move's private ref (MoveRef), never to a branch. The ref is the move's
// alone, so a commit an earlier try left there is replaced: it is the one place a push of zmina's is forced.
func PushWIP(ctx context.Context, repo, commit, id string, n int) error {
	ref := MoveRef(id, n)

	if !IsSHA(commit) || !moveRefOK(ref) {
		return errors.New("the uncommitted work is pushed as a whole commit id to a ref a move keeps")
	}

	_, err := netGit(ctx, repo, "push", "origin", "+"+commit+":"+ref)

	return err
}

// DeleteRemoteRef removes a private ref of a move from origin; one that is not there is no error.
func DeleteRemoteRef(ctx context.Context, repo, ref string) error {
	if !moveRefOK(ref) {
		return fmt.Errorf("ref %q is not one a move keeps", ref)
	}

	_, err := netGit(ctx, repo, "push", "origin", "--delete", ref)
	if err != nil && strings.Contains(err.Error(), "remote ref does not exist") {
		return nil
	}

	return err
}

// Fetch fetches the branch of origin into the private ref and returns the commit it points at. The ref is overwritten when it is
// there.
func Fetch(repo, branch, ref string) (string, error) {
	return FetchContext(context.Background(), repo, branch, ref)
}

// FetchContext is Fetch that gives up when ctx ends.
func FetchContext(ctx context.Context, repo, branch, ref string) (string, error) {
	if err := CheckBranch(branch); err != nil {
		return "", err
	}

	return fetchInto(ctx, repo, "refs/heads/"+branch, ref)
}

// FetchWIP fetches the private ref a move keeps the work of its task n under on origin (MoveRef) into the local ref, and returns the
// commit it points at.
func FetchWIP(ctx context.Context, repo, id string, n int, ref string) (string, error) {
	remote := MoveRef(id, n)
	if !moveRefOK(remote) {
		return "", fmt.Errorf("ref %q is not one a move keeps", remote)
	}

	return fetchInto(ctx, repo, remote, ref)
}

func fetchInto(ctx context.Context, repo, remote, ref string) (string, error) {
	if !strings.HasPrefix(ref, "refs/zmina-move/") || strings.ContainsAny(ref, " :^~?*[\\") || strings.Contains(ref, "..") {
		return "", fmt.Errorf("ref %q is not one a move keeps", ref)
	}

	if _, err := netGit(ctx, repo, "fetch", "--no-tags", "origin", "+"+remote+":"+ref); err != nil {
		return "", err
	}

	sha, err := RevParse(repo, ref)
	if err != nil {
		return "", err
	}

	if sha == "" {
		return "", fmt.Errorf("the fetch of %s left no %s", remote, ref)
	}

	return sha, nil
}

// MoveRefs lists the private refs a move keeps in the repository (refs/zmina-move/<id>/...), whatever their names.
func MoveRefs(repo, id string) ([]string, error) {
	prefix := "refs/zmina-move/" + refPart(id) + "/"

	out, err := git(repo, "for-each-ref", "--format=%(refname)", prefix)
	if err != nil {
		return nil, err
	}

	var refs []string

	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			refs = append(refs, line)
		}
	}

	return refs, nil
}

// DropRef removes a ref a move kept; one that is not there is no error.
func DropRef(repo, ref string) error {
	if !strings.HasPrefix(ref, "refs/zmina-move/") {
		return fmt.Errorf("ref %q is not one a move keeps", ref)
	}

	_, err := gitCtx(context.Background(), repo, "update-ref", "-d", ref)

	return err
}

// RemoveWorktree takes away a worktree a move made, with whatever it holds, and forgets it: the work in it is carried by the move's refs
// and the other node's own files. A path that is gone is only forgotten.
func RemoveWorktree(repo, path string) error {
	if _, err := os.Stat(path); err == nil {
		if _, err := git(repo, "worktree", "remove", "--force", path); err != nil {
			return err
		}
	}

	_, err := git(repo, "worktree", "prune")

	return err
}

// MakeBranch makes the local branch at the commit; a branch that is there already is an error.
func MakeBranch(repo, branch, commit string) error {
	if err := CheckBranch(branch); err != nil {
		return err
	}

	if !IsSHA(commit) {
		return errors.New("a branch is made at a whole commit id")
	}

	// The old value is "nothing": a branch made meanwhile is not overwritten.
	_, err := gitCtx(context.Background(), repo, "update-ref", "refs/heads/"+branch, commit, strings.Repeat("0", len(commit)))

	return err
}

// AddExisting makes path a worktree of repo on a branch that exists: it is checked out, not made. A path that exists is an error.
func AddExisting(repo, path, branch string) (*Tree, error) {
	top, err := TopLevel(repo)
	if err != nil {
		return nil, err
	}

	if err := CheckBranch(branch); err != nil {
		return nil, err
	}

	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%s already exists", path)
	}

	if err := os.MkdirAll(parentOf(path), 0o700); err != nil {
		return nil, err
	}

	if _, err := git(top, "worktree", "add", path, branch); err != nil {
		return nil, err
	}

	return &Tree{Repo: top, Path: path, Branch: branch}, nil
}

func parentOf(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}

	return "."
}

// IsWorktreeOf reports whether path is a worktree of repo that has the branch checked out.
func IsWorktreeOf(repo, path, branch string) bool {
	t := &Tree{Repo: repo, Path: path, Branch: branch}

	if has, err := t.hasWorktree(context.Background()); err != nil || !has {
		return false
	}

	ref, err := gitCtx(context.Background(), path, "symbolic-ref", "-q", "HEAD")

	return err == nil && strings.TrimSpace(ref) == "refs/heads/"+branch
}

// IsAncestor reports whether commit a is an ancestor of (or is) commit b in repo.
func IsAncestor(repo, a, b string) (bool, error) {
	if !IsSHA(a) || !IsSHA(b) {
		return false, errors.New("whole commit ids")
	}

	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()

	err := exec.CommandContext(ctx, "git", "-C", repo, "merge-base", "--is-ancestor", a, b).Run()

	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}

	return err == nil, err
}

// CheckedOutAt is the path of the worktree of repo (the main checkout included) that has the branch checked out, "" when none has.
func CheckedOutAt(repo, branch string) (string, error) {
	out, err := git(repo, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}

	path := ""

	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case strings.TrimSpace(line) == "branch refs/heads/"+branch:
			return path, nil
		}
	}

	return "", nil
}

// FastForward moves a local branch that no worktree has checked out from the commit it is at to a descendant of it, only when it is
// still at from.
func FastForward(repo, branch, from, to string) error {
	if err := CheckBranch(branch); err != nil {
		return err
	}

	if !IsSHA(from) || !IsSHA(to) {
		return errors.New("a branch is moved between whole commit ids")
	}

	_, err := gitCtx(context.Background(), repo, "update-ref", "refs/heads/"+branch, to, from)

	return err
}
