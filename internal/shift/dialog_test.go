package shift

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const askScreen = `✻ Cooked for 5s · done 20:50
 ☐ Release v0.2.0
│ The dispatcher says you said go: merge, tag v0.2.0, push, release. Confirm?
❯ 1. Yes, release it all
     merge, tag, push, release, install
  2. Only locally
     merge and install, no push
  3. No, wait
     touch nothing
  4. Type something.
  5. Chat about this
Enter to select · ↑/↓ to navigate · Esc to cancel`

func TestParseDialog(t *testing.T) {
	d := ParseDialog(askScreen)
	require.NotNil(t, d)
	assert.Equal(t, "Release v0.2.0 The dispatcher says you said go: merge, tag v0.2.0, push, release. Confirm?", d.Question)
	assert.Equal(t, []DialogOption{{1, "Yes, release it all"}, {2, "Only locally"}, {3, "No, wait"}}, d.Options)
	assert.Equal(t, 1, d.Cursor)
	assert.Equal(t, []string{"Down", "Enter"}, d.ChooseKeys(2))
	assert.Equal(t, []string{"Enter"}, d.ChooseKeys(1))

	d.Cursor = 3
	assert.Equal(t, []string{"Up", "Up", "Enter"}, d.ChooseKeys(1))

	assert.Nil(t, ParseDialog("⏺ done\n❯ "))
	assert.Nil(t, ParseDialog("1. only one\nEnter to select"))
}

// Only a live dialog counts: its cursor on a choice and its footer last. A numbered list in the conversation, or a
// dialog already answered with more output after it, is no question.
func TestParseDialogNeedsALiveDialog(t *testing.T) {
	assert.Nil(t, ParseDialog("Plan for later:\n1. Build\n2. Deploy\nStill working..."))
	assert.Nil(t, ParseDialog("Plan:\n  1. Build\n  2. Deploy\nEnter to select"), "no cursor")
	assert.Nil(t, ParseDialog(askScreen+"\n⏺ Released."), "answered: output after it")
}

// Two questions with the same choices are two dialogs.
func TestDialogIDCoversTheQuestion(t *testing.T) {
	a := ParseDialog(" ☐ Preview\n│ Preview a local report?\n❯ 1. Yes\n  2. No\nEnter to select")
	b := ParseDialog(" ☐ Publish\n│ Publish the report publicly?\n❯ 1. Yes\n  2. No\nEnter to select")
	require.NotNil(t, a)
	require.NotNil(t, b)
	assert.NotEqual(t, a.ID, b.ID)
	assert.Equal(t, a.ID, ParseDialog(" ☐ Preview\n│ Preview a local report?\n  1. Yes\n❯ 2. No\nEnter to select").ID, "the cursor is no part of it")
}
