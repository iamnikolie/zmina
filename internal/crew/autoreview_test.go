package crew

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const autoReviewRoles = "namespace: /ns\nroles:\n  dispatcher:\n  dev:\n  reviewer:\n"

const autoReviewRoutes = "routes:\n  - review: {first: reviewer, again: [dev]}\n  - pool: {seats: [dev]}\n"

// auto_review is off unless the crew file names it; on has two rounds.
func TestAutoReviewKey(t *testing.T) {
	c, err := Load(write(t, autoReviewRoutes+autoReviewRoles))
	require.NoError(t, err)
	assert.Zero(t, c.AutoReview.Rounds)
	assert.Zero(t, c.AutoReviewRounds(), "off unless the file says so")

	for body, want := range map[string]int{
		"auto_review: on\n":              2,
		"auto_review: true\n":            2,
		"auto_review: off\n":             0,
		"auto_review: false\n":           0,
		"auto_review: {rounds: 1}\n":     1,
		"auto_review:\n  rounds: 5\n":    5,
		"auto_review: {}\n":              2,
		"auto_review: {rounds: \"3\"}\n": 3,
	} {
		c, err := Load(write(t, body+autoReviewRoutes+autoReviewRoles))
		require.NoError(t, err, body)
		assert.Equal(t, want, c.AutoReviewRounds(), body)
		assert.Empty(t, c.Warnings, body)
	}
}

func TestAutoReviewRefusesWhatItCannotRead(t *testing.T) {
	for _, body := range []string{
		"auto_review: maybe\n",
		"auto_review: {rounds: 0}\n",
		"auto_review: {rounds: 6}\n",
		"auto_review: {rounds: many}\n",
		"auto_review: {round: 2}\n",
		"auto_review: [on]\n",
	} {
		_, err := Load(write(t, body+autoReviewRoutes+autoReviewRoles))
		assert.ErrorContains(t, err, "auto_review", body)
	}
}

// With no review rule in the routes there is nobody to pick the reviewer: the key does nothing.
func TestAutoReviewIsInertWithoutAReviewRule(t *testing.T) {
	for _, routes := range []string{"", "routes:\n  - pool: {seats: [dev]}\n"} {
		c, err := Load(write(t, "auto_review: on\n"+routes+autoReviewRoles))
		require.NoError(t, err)
		assert.Equal(t, 2, c.AutoReview.Rounds, "the key is read")
		assert.Zero(t, c.AutoReviewRounds(), "and does nothing")

		_, ok := c.ReviewRule()
		assert.False(t, ok)
	}

	c, err := Load(write(t, "auto_review: on\n"+autoReviewRoutes+autoReviewRoles))
	require.NoError(t, err)

	rule, ok := c.ReviewRule()
	require.True(t, ok)
	assert.Equal(t, "reviewer", rule.First)
	assert.Equal(t, []string{"dev"}, rule.Again)
}
