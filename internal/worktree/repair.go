package worktree

import (
	"context"
	"strings"
)

// Repair runs `git worktree repair` in repo, the main checkout, and returns what git said (nothing, when no link
// was broken). After the repository was moved, the linked worktrees that stayed where they were point at its old
// place; this makes them point at the new one. It deletes nothing. It ends with ctx, and each git call is bounded
// by Timeout as every other one is.
func Repair(ctx context.Context, repo string) (string, error) {
	out, err := gitCtx(ctx, repo, "worktree", "repair")

	return strings.TrimSpace(out), err
}
