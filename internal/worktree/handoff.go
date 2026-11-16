package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// MoveTrailer is the trailer of the commit a move makes of a worktree's uncommitted work: "Zmina-Move: <id>". The node that takes the
// branch undoes that one commit, and only a commit that carries its own move's id.
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
func netGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, NetTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = waitDelay

	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", fmt.Errorf("git %s: no answer in %s", args[0], NetTimeout)
	}

	if err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(out)))
	}

	return strings.TrimSpace(string(out)), nil
}

// Origin is the URL of the repository's remote named origin; empty when it has none.
func Origin(repo string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "git", "-C", repo, "config", "--get", "remote.origin.url").Output()

	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		// git config exits 1 for a key that is not set.
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("git config in %s: %w", repo, err)
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

// CommitWIP commits the uncommitted work of the worktree (everything not ignored, tracked and untracked, found by comparing trees, as
// Preserve finds it) as one commit on the task's branch whose message ends in the trailer of the move id, and leaves the worktree
// clean at it. made is false, and commit the branch's tip as it is, when there was nothing to commit. The commit is made with
// plumbing under zmina's own name, signed by nobody, and no hook runs. It refuses what a tree cannot hold (a nested repository,
// conflicts, work that lives only in the index), a worktree whose HEAD is not the task's branch, and a worktree that is gone.
func (t *Tree) CommitWIP(id string) (commit string, made bool, err error) {
	ctx := t.ctx()

	has, err := t.hasWorktree(ctx)
	if err != nil {
		return "", false, err
	}

	if !has {
		return "", false, fmt.Errorf("the worktree %s is gone: nothing to commit", t.Path)
	}

	ref, err := gitCtx(ctx, t.Path, "symbolic-ref", "-q", "HEAD")
	if err != nil || strings.TrimSpace(ref) != "refs/heads/"+t.Branch {
		return "", false, fmt.Errorf("the worktree %s is not on the task's branch %s: its HEAD is %s", t.Path, t.Branch, strings.TrimSpace(ref))
	}

	head, ok, err := t.revOf(ctx, t.Path, "HEAD")
	if err != nil {
		return "", false, err
	}

	if !ok {
		return "", false, fmt.Errorf("the branch %s has no commit yet: commit once before it is moved", t.Branch)
	}

	snaps, err := t.capture(ctx, refPart(id), head)
	if err != nil {
		return "", false, err
	}

	var files *snapshot

	for _, s := range snaps {
		if s.indexOnly {
			return "", false, fmt.Errorf("the index of %s holds changes the files do not (a file staged and then deleted, or added with force): "+
				"commit or reset them first", t.Path)
		}

		files = s
	}

	if files == nil {
		return head, false, nil
	}

	commit, err = commitTree(ctx, t.Path, head, files.tree, "Work in progress, carried by a move of its role", MoveTrailer+": "+id)
	if err != nil {
		return "", false, err
	}

	// The branch moves only if it is still where it was: a commit made meanwhile is not overwritten.
	if _, err := gitCtx(ctx, t.Repo, "update-ref", "-m", "zmina move "+id, "refs/heads/"+t.Branch, commit, head); err != nil {
		return "", false, err
	}

	// The index still holds the old tip's tree: made to hold the new one, the worktree reads clean and no file is touched.
	if _, err := gitCtx(ctx, t.Path, "reset", "-q"); err != nil {
		return commit, true, fmt.Errorf("the work is committed as %s, but the index could not be brought to it: %w", short(commit), err)
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

// UndoWIP undoes the commit a move made of the worktree's work when it is the worktree's tip and carries this move's trailer: the
// branch goes back to its parent and the files stay where they are, uncommitted (reset --mixed HEAD~1). It reports whether it did.
func (t *Tree) UndoWIP(id string) (bool, error) {
	ctx := t.ctx()

	tip, ok, err := t.revOf(ctx, t.Path, "HEAD")
	if err != nil || !ok {
		return false, err
	}

	carries, err := CarriesMove(t.Path, tip, id)
	if err != nil || !carries {
		return false, err
	}

	if _, err := gitCtx(ctx, t.Path, "reset", "--mixed", "-q", "HEAD~1"); err != nil {
		return false, err
	}

	return true, nil
}

// Push pushes the local branch to the remote named origin under the same name. A remote branch that is not an ancestor of it is
// refused by git, and so is the push.
func Push(repo, branch string) error {
	if err := CheckBranch(branch); err != nil {
		return err
	}

	_, err := netGit(context.Background(), repo, "push", "origin", "refs/heads/"+branch+":refs/heads/"+branch)

	return err
}

// Fetch fetches the branch of origin into the private ref and returns the commit it points at. The ref is overwritten when it is
// there.
func Fetch(repo, branch, ref string) (string, error) {
	if err := CheckBranch(branch); err != nil {
		return "", err
	}

	if !strings.HasPrefix(ref, "refs/zmina-move/") || strings.ContainsAny(ref, " :^~?*[\\") || strings.Contains(ref, "..") {
		return "", fmt.Errorf("ref %q is not one a move keeps", ref)
	}

	if _, err := netGit(context.Background(), repo, "fetch", "--no-tags", "origin", "+refs/heads/"+branch+":"+ref); err != nil {
		return "", err
	}

	sha, err := RevParse(repo, ref)
	if err != nil {
		return "", err
	}

	if sha == "" {
		return "", fmt.Errorf("the fetch of %s left no %s", branch, ref)
	}

	return sha, nil
}

// DropRef removes a ref a move kept; one that is not there is no error.
func DropRef(repo, ref string) error {
	if !strings.HasPrefix(ref, "refs/zmina-move/") {
		return fmt.Errorf("ref %q is not one a move keeps", ref)
	}

	_, err := gitCtx(context.Background(), repo, "update-ref", "-d", ref)

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
