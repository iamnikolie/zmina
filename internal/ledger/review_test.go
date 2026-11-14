package ledger

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/task"
)

// reviewFixture is a ledger with auto_review on for two rounds and a task with a worktree, T-1, held by dev.
func reviewFixture(t *testing.T) (*Ledger, *fakeTrees, task.Task) {
	t.Helper()

	l, trees := fixture(t, "")
	l.Dispatcher = "boss"
	l.AutoReview = 2

	tk, err := l.New(NewTask{Role: "dev", Asked: "fix the login", Repo: "api", Worktree: true})
	require.NoError(t, err)

	return l, trees, tk
}

// madeReview makes a review task of id in a round, as the loop does.
func madeReview(t *testing.T, l *Ledger, of string, round int) task.Task {
	t.Helper()

	rv, err := l.New(NewTask{Role: "reviewer", Asked: "review " + of, Review: &task.Review{Of: of, Round: round}})
	require.NoError(t, err)

	return rv
}

// A task a role closes as done is stamped as due its review, in the same write as the closing.
func TestARoleClosingAWorktreeTaskStampsItsReviewDue(t *testing.T) {
	l, _, tk := reviewFixture(t)

	got, err := l.Set(tk.ID, Change{State: task.Done, By: "dev"})
	require.NoError(t, err)
	require.NotNil(t, got.Closed.Review)
	assert.Equal(t, task.ReviewDue{Round: 1}, *got.Closed.Review)

	read, err := l.Store.Get(tk.ID)
	require.NoError(t, err)
	assert.Equal(t, task.ReviewDue{Round: 1}, *read.Closed.Review, "it is in the file")
}

func TestNoReviewIsDueExceptForARolesCloseOfAWorktreeTask(t *testing.T) {
	l, _, tk := reviewFixture(t)

	plain, err := l.New(NewTask{Role: "dev", Asked: "no worktree"})
	require.NoError(t, err)

	rv := madeReview(t, l, tk.ID, 1)

	for name, c := range map[string]struct {
		id string
		by string
	}{
		"the owner's hand": {tk.ID, Hand},
		"the dispatcher":   {tk.ID, "boss"},
		"no worktree":      {plain.ID, "dev"},
		"a review task":    {rv.ID, "reviewer"},
	} {
		change := Change{State: task.Done, By: c.by}
		if c.id == rv.ID {
			change.Verdict = task.VerdictAccept
		}

		got, err := l.Set(c.id, change)
		require.NoError(t, err, name)
		require.NotNil(t, got.Closed, name)
		assert.Nil(t, got.Closed.Review, name)

		// Back to where it was for the next.
		_, err = l.Set(c.id, Change{State: task.Open, By: "boss"})
		require.NoError(t, err, name)
	}

	l.AutoReview = 0

	got, err := l.Set(tk.ID, Change{State: task.Done, By: "dev"})
	require.NoError(t, err)
	assert.Nil(t, got.Closed.Review, "with the key off nothing is stamped")
}

// The rounds are the reviews made of a task: when they are used up the close is as it always was.
func TestNoReviewIsDueOncePastTheRounds(t *testing.T) {
	l, _, tk := reviewFixture(t)

	for round := 1; round <= 2; round++ {
		got, err := l.Set(tk.ID, Change{State: task.Done, By: "dev"})
		require.NoError(t, err)
		require.NotNil(t, got.Closed.Review, "round %d", round)
		assert.Equal(t, round, got.Closed.Review.Round)

		rv := madeReview(t, l, tk.ID, round)
		_, err = l.Set(rv.ID, Change{State: task.Done, By: "reviewer", Verdict: task.VerdictReject, Note: "no"})
		require.NoError(t, err)

		// Reopened, as the loop does it.
		_, err = l.Set(tk.ID, Change{State: task.Open, By: "zmina", Note: "again"})
		require.NoError(t, err)
	}

	got, err := l.Set(tk.ID, Change{State: task.Done, By: "dev"})
	require.NoError(t, err)
	assert.Nil(t, got.Closed.Review)
}

// A done task goes back to open through Set, which takes its closing and so its review off: the loop's reject relies on it.
func TestReopeningADoneTaskClearsItsClosing(t *testing.T) {
	l, _, tk := reviewFixture(t)

	_, err := l.Set(tk.ID, Change{State: task.Done, By: "dev", Note: "shipped"})
	require.NoError(t, err)

	got, err := l.Set(tk.ID, Change{State: task.Open, By: "zmina", Note: "review rejected this"})
	require.NoError(t, err)
	assert.Equal(t, task.Open, got.State)
	assert.Nil(t, got.Closed)
	assert.Equal(t, "review rejected this", got.Note)

	read, err := l.Store.Get(tk.ID)
	require.NoError(t, err)
	assert.Nil(t, read.Closed)

	// Its next close is stamped again, for the next round.
	again, err := l.Set(tk.ID, Change{State: task.Done, By: "dev"})
	require.NoError(t, err)
	assert.Equal(t, 1, again.Closed.Review.Round, "no review was made in between, so it is still round 1")
}

// Closing a done task again, to say more, keeps the review it is due, and with it the review it names.
func TestDoneAgainKeepsTheReviewDue(t *testing.T) {
	l, _, tk := reviewFixture(t)

	_, err := l.Set(tk.ID, Change{State: task.Done, By: "dev"})
	require.NoError(t, err)

	read, _ := l.Store.Get(tk.ID)
	read.Closed.Review.Task = "T-9"
	require.NoError(t, l.Store.Put(read))

	got, err := l.Set(tk.ID, Change{State: task.Done, By: "boss", Note: "merged"})
	require.NoError(t, err)
	assert.Equal(t, "boss", got.Closed.By)
	require.NotNil(t, got.Closed.Review)
	assert.Equal(t, task.ReviewDue{Round: 1, Task: "T-9"}, *got.Closed.Review)

	_, err = l.Set(tk.ID, Change{State: task.Done, By: "dev"})
	require.ErrorIs(t, err, ErrAlreadyDone)
}

func TestAReviewTaskClosesOnAVerdict(t *testing.T) {
	l, _, tk := reviewFixture(t)
	rv := madeReview(t, l, tk.ID, 1)

	_, err := l.Set(rv.ID, Change{State: task.Done, By: "reviewer"})
	require.ErrorIs(t, err, ErrNoVerdict)
	assert.ErrorContains(t, err, "--verdict")

	got, _ := l.Store.Get(rv.ID)
	assert.Equal(t, task.Open, got.State)

	_, err = l.Set(rv.ID, Change{State: task.Done, By: "reviewer", Verdict: "maybe"})
	require.ErrorIs(t, err, ErrVerdict)

	_, err = l.Set(rv.ID, Change{State: task.Done, By: "reviewer", Verdict: task.VerdictReject})
	require.ErrorIs(t, err, ErrVerdict, "a reject says what is wrong")

	got, err = l.Set(rv.ID, Change{State: task.Done, By: "reviewer", Verdict: task.VerdictReject, Note: "the token is logged"})
	require.NoError(t, err)
	assert.Equal(t, task.VerdictReject, got.Review.Verdict)
	assert.Nil(t, got.Closed.Unchecked)

	read, _ := l.Store.Get(rv.ID)
	assert.Equal(t, task.VerdictReject, read.Review.Verdict, "it is in the file")
}

func TestARejectWithASavedResultNeedsNoNote(t *testing.T) {
	l, _, tk := reviewFixture(t)
	rv := madeReview(t, l, tk.ID, 1)

	// Closing the task checks only that a result is there, as it does for criteria.
	saved, _ := l.Store.Get(rv.ID)
	saved.Result = &record.Ref{Revision: "r1"}
	require.NoError(t, l.Store.Put(saved))

	got, err := l.Set(rv.ID, Change{State: task.Done, By: "reviewer", Verdict: task.VerdictReject})
	require.NoError(t, err)
	assert.Equal(t, task.VerdictReject, got.Review.Verdict)
}

// The owner and the dispatcher close a review with no verdict; a role cannot, and the closing says what was bypassed.
func TestOnlyTheOwnerAndTheDispatcherCloseAReviewWithoutAVerdict(t *testing.T) {
	l, _, tk := reviewFixture(t)
	rv := madeReview(t, l, tk.ID, 1)

	_, err := l.Set(rv.ID, Change{State: task.Done, By: "reviewer", Force: true})
	require.ErrorIs(t, err, ErrCheckForce)

	got, err := l.Set(rv.ID, Change{State: task.Done, By: "boss", Force: true})
	require.NoError(t, err)
	assert.Empty(t, got.Review.Verdict)
	require.NotNil(t, got.Closed.Unchecked)
	assert.Equal(t, "boss", got.Closed.Unchecked.By)
	assert.Equal(t, "zmina task done "+rv.ID+" --verdict accept|reject", got.Closed.Unchecked.Run)
}

func TestAVerdictIsForAReviewTaskAndStaysTheOneGiven(t *testing.T) {
	l, _, tk := reviewFixture(t)

	_, err := l.Set(tk.ID, Change{State: task.Done, By: "dev", Verdict: task.VerdictAccept})
	require.ErrorIs(t, err, ErrVerdict)
	assert.ErrorContains(t, err, "is for a review task")

	_, err = l.Set(tk.ID, Change{State: task.Open, Verdict: task.VerdictAccept})
	require.Error(t, err, "a verdict goes with a close")

	got, _ := l.Store.Get(tk.ID)
	assert.Equal(t, task.Open, got.State)

	rv := madeReview(t, l, tk.ID, 1)
	_, err = l.Set(rv.ID, Change{State: task.Done, By: "reviewer", Verdict: task.VerdictAccept})
	require.NoError(t, err)

	_, err = l.Set(rv.ID, Change{State: task.Done, By: "reviewer", Verdict: task.VerdictAccept})
	require.ErrorIs(t, err, ErrAlreadyDone, "the same verdict again is nothing")

	_, err = l.Set(rv.ID, Change{State: task.Done, By: "reviewer", Verdict: task.VerdictReject, Note: "wait"})
	require.ErrorIs(t, err, ErrVerdict)
	assert.ErrorContains(t, err, "accept already")
}

// A done task whose review is still to come keeps its worktree through a collection: the reviewer reads it.
func TestGCLeavesTheWorktreeOfATaskWhoseReviewIsPending(t *testing.T) {
	l, trees, tk := reviewFixture(t)
	trees.merged[tk.Branch] = true

	_, err := l.Set(tk.ID, Change{State: task.Done, By: "dev"})
	require.NoError(t, err)

	got, err := l.GC(true, false)
	require.NoError(t, err)
	assert.Empty(t, got, "the review is not made yet")

	rv := madeReview(t, l, tk.ID, 1)
	read, _ := l.Store.Get(tk.ID)
	read.Closed.Review.Task = rv.ID
	require.NoError(t, l.Store.Put(read))

	got, err = l.GC(true, false)
	require.NoError(t, err)
	assert.Empty(t, got, "the review is under way")

	_, err = l.Set(rv.ID, Change{State: task.Done, By: "reviewer", Verdict: task.VerdictAccept, Note: "ok"})
	require.NoError(t, err)

	got, err = l.GC(true, false)
	require.NoError(t, err)
	require.Len(t, got, 1, "the review is over: the merged work goes as any")
	assert.Equal(t, tk.ID, got[0].ID)

	// With the key off nothing waits on a review.
	l.AutoReview = 0
	read.Closed.Review.Task = ""
	require.NoError(t, l.Store.Put(read))

	got, err = l.GC(true, false)
	require.NoError(t, err)
	assert.Len(t, got, 1)
}
