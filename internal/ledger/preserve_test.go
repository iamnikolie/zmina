package ledger

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/worktree"
)

// droppedFixture is a ledger with three tasks that hold work, closed: a dropped one with unmerged commits and uncommitted
// changes, a dropped one that is merged and clean, and a done one with unmerged commits (force never takes a done task's).
func droppedFixture(t *testing.T) (l *Ledger, trees *fakeTrees, unmerged, clean, done task.Task, events *[]string) {
	t.Helper()

	l, trees = fixture(t, "")

	var log []string

	l.Log = func(role, event, id, detail string) { log = append(log, role+" "+event+" "+id+" "+detail) }

	var err error

	unmerged, err = l.New(NewTask{Role: "dev", Asked: "unmerged", Repo: "api", Worktree: true})
	require.NoError(t, err)
	clean, err = l.New(NewTask{Role: "dev", Asked: "clean", Repo: "api", Worktree: true})
	require.NoError(t, err)
	done, err = l.New(NewTask{Role: "dev", Asked: "done", Repo: "api", Worktree: true})
	require.NoError(t, err)

	trees.merged[clean.Branch] = true
	trees.kept = map[string]worktree.Preserved{unmerged.Branch: {
		Branches:  []worktree.KeptBranch{{Name: "zmina-preserve/t-1-0123abcd", Tip: "0123abcd", Commits: 2}},
		Dirty:     "refs/zmina-preserve-dirty/t-1-4567ef89",
		DirtyOver: "0123abcd",
		Index:     "refs/zmina-preserve-dirty/t-1-index-89abcdef",
		IndexOver: "0123abcd",
	}}

	for _, id := range []string{unmerged.ID, clean.ID} {
		_, err = l.Set(id, Change{State: task.Dropped, By: "dev"})
		require.NoError(t, err)
	}

	_, err = l.Set(done.ID, Change{State: task.Done, Force: true, By: "hand"})
	require.NoError(t, err)

	// The changes are there once the task is closed: a close refuses a worktree that has some.
	trees.dirty[unmerged.Worktree] = " M a\n?? b"

	return l, trees, unmerged, clean, done, &log
}

func collectedBy(got []Collected) map[string]Collected {
	out := map[string]Collected{}
	for _, c := range got {
		out[c.ID] = c
	}

	return out
}

// A forced collection keeps what the removal would lose in git first, before anything is removed, and says where: the
// work of the dropped task that holds some, nothing for one that holds none, and nothing at all for a done task, which force
// does not take.
func TestForcedGCKeepsTheWorkBeforeRemovingIt(t *testing.T) {
	l, trees, unmerged, clean, done, events := droppedFixture(t)

	got, err := l.GCWith(context.Background(), GCOptions{Force: true})
	require.NoError(t, err)

	by := collectedBy(got)
	assert.True(t, by[unmerged.ID].Removed)
	assert.True(t, by[clean.ID].Removed)
	assert.NotContains(t, by[unmerged.ID].Kept, "could not")

	saved := by[unmerged.ID].Preserved
	require.Len(t, saved.Branches, 1)
	assert.Equal(t, "zmina-preserve/t-1-0123abcd", saved.Branches[0].Name)
	assert.Equal(t, "refs/zmina-preserve-dirty/t-1-4567ef89", saved.Dirty)
	assert.Equal(t, unmerged.Worktree, by[unmerged.ID].Path)
	assert.Equal(t, unmerged.Repo, by[unmerged.ID].Repo)
	assert.True(t, by[clean.ID].Preserved.Empty(), "nothing to keep: as it was")

	// Kept first, under the task's id as a lower case name, and only then removed.
	assert.Equal(t, []string{
		"preserve " + unmerged.Branch + " as " + strings.ToLower(unmerged.ID), "remove " + unmerged.Branch,
		"preserve " + clean.Branch + " as " + strings.ToLower(clean.ID), "remove " + clean.Branch,
	}, trees.calls)
	assert.NotContains(t, strings.Join(trees.calls, "\n"), done.Branch, "force never takes a done task's work")

	assert.Contains(t, strings.Join(*events, "\n"), "dev task "+unmerged.ID+" work kept before removal: 2 commit(s) on branch "+
		"zmina-preserve/t-1-0123abcd; uncommitted changes as refs/zmina-preserve-dirty/t-1-4567ef89; what its index holds as "+
		"refs/zmina-preserve-dirty/t-1-index-89abcdef")
	assert.NotContains(t, strings.Join(*events, "\n"), "work kept before removal: "+clean.ID)
}

// A removal whose work could not be kept does not happen, and says how to remove it anyway; --no-preserve removes it
// without keeping anything. A collection that is not forced, and a dry run, keep and remove nothing here.
func TestForcedGCRefusesWhenTheWorkCannotBeKept(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    GCOptions
		failing bool
		removed bool
		calls   int
		kept    string
	}{
		{name: "kept, so removed", opts: GCOptions{Force: true}, removed: true, calls: 2},
		{name: "could not be kept", opts: GCOptions{Force: true}, failing: true, calls: 2,
			kept: "its work could not be kept first (no room in the repository), so nothing was removed; --no-preserve removes it anyway"},
		{name: "could not be kept, and no preserve", opts: GCOptions{Force: true, NoPreserve: true}, failing: true, removed: true},
		{name: "not forced: it is never removed", opts: GCOptions{}, kept: "uncommitted changes in"},
		{name: "a dry run", opts: GCOptions{Force: true, DryRun: true}, failing: true, removed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, trees, unmerged, clean, _, _ := droppedFixture(t)

			if tc.failing {
				trees.preserveErr = errString("no room in the repository")
			}

			got, err := l.GCWith(context.Background(), tc.opts)
			require.NoError(t, err)

			c := collectedBy(got)[unmerged.ID]
			assert.Equal(t, tc.removed, c.Removed, "%+v", c)
			assert.Contains(t, c.Kept, tc.kept)

			preserves := 0

			for _, call := range trees.calls {
				if strings.HasPrefix(call, "preserve ") {
					preserves++
				}
			}

			assert.Equal(t, tc.calls, preserves)

			if tc.opts.DryRun {
				assert.Empty(t, trees.removed, "a dry run removes nothing")
				assert.True(t, c.WouldPreserve)
			}

			if !tc.removed {
				assert.NotContains(t, trees.removed, unmerged.Branch)
			}

			if tc.failing && tc.opts.Force && !tc.opts.NoPreserve && !tc.opts.DryRun {
				// Nothing is removed, the clean one's work too: whatever cannot be kept stays where it is.
				assert.NotContains(t, trees.removed, clean.Branch)
			}
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }
