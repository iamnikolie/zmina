package crew

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const handWritten = `# the crew of acme
name: acme   # its name
namespace: /ns
home: alpha
nodes:
  alpha: {prefix: al-}
  beta: {prefix: be-}
roles:
    dispatcher: {}
    dev:
        about: builds the form   # a comment of the owner's
    ops: {place: alpha}
`

// The edit of a move is a pure function of the file: the same file gives the same bytes, wherever it is made, so two nodes that hold one
// file hold one file after it. The comments stay, the role's place is written (also when it is home), and it says what it did once.
func TestPlaceAfterIsOneEditOnEqualFiles(t *testing.T) {
	a, err := PlaceAfter([]byte(handWritten), "dev", "beta")
	require.NoError(t, err)

	b, err := PlaceAfter([]byte(handWritten), "dev", "beta")
	require.NoError(t, err)
	assert.Equal(t, string(a), string(b))

	assert.Contains(t, string(a), "# the crew of acme")
	assert.Contains(t, string(a), "# a comment of the owner's")
	assert.Contains(t, string(a), "place: beta")

	// Again: the file is the result already.
	c, err := PlaceAfter(a, "dev", "beta")
	require.NoError(t, err)
	assert.Equal(t, string(a), string(c), "a move made twice is the same file")

	// Back to home: written as place: alpha, not removed.
	home, err := PlaceAfter(a, "dev", "alpha")
	require.NoError(t, err)
	assert.Contains(t, string(home), "place: alpha")
	assert.NotContains(t, string(home), "place: beta")

	// A role with nothing under it.
	bare, err := PlaceAfter([]byte("home: alpha\nnodes: {alpha: {}, beta: {}}\nroles:\n  dispatcher:\n  dev:\n"), "dev", "beta")
	require.NoError(t, err)
	assert.Contains(t, string(bare), "place: beta")

	_, err = PlaceAfter([]byte(handWritten), "nope", "beta")
	require.ErrorContains(t, err, `no role "nope"`)

	_, err = PlaceAfter([]byte("- a\n- b\n"), "dev", "beta")
	require.Error(t, err)
}

func TestMovePlaceReplacesOnlyTheFileItWasBasedOn(t *testing.T) {
	path := putDir(t, handWritten)
	base := SumOf([]byte(handWritten))

	// Another file than the one the move was made from: nothing is written.
	_, _, _, err := MovePlace(path, "dev", "beta", strings.Repeat("a", 64), "mesh alpha", "zmina move")

	var be *BaseError
	require.True(t, errors.As(err, &be), "%v", err)

	raw, _ := os.ReadFile(path)
	assert.Equal(t, handWritten, string(raw))

	saved, sum, wrote, err := MovePlace(path, "dev", "beta", base, "mesh alpha", "zmina move")
	require.NoError(t, err)
	assert.True(t, wrote)
	assert.NotEmpty(t, saved.Versions, "what it replaced is kept in the history")

	raw, _ = os.ReadFile(path)
	assert.Equal(t, sum, SumOf(raw))

	want, err := PlaceAfter([]byte(handWritten), "dev", "beta")
	require.NoError(t, err)
	assert.Equal(t, string(want), string(raw), "the file is what PlaceAfter said it would be")

	// The same move again, on the file it made: nothing is written, whatever the base.
	_, sum2, wrote, err := MovePlace(path, "dev", "beta", base, "mesh alpha", "zmina move")
	require.NoError(t, err)
	assert.False(t, wrote)
	assert.Equal(t, sum, sum2)

	// A move to a node the file does not list does not load, so it is not written.
	_, _, _, err = MovePlace(path, "dev", "gamma", sum, "mesh alpha", "zmina move")
	require.NoError(t, err, "an unknown place is a warning, as in any file: the role runs nowhere")
}
