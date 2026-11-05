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
