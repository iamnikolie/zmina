package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/shift"
)

// A claude that shows a question with numbered choices and writes the keys it gets.
const dialogClaude = `#!/bin/sh
d=$(dirname "$0")
printf ' ☐ Release\n│ Ship it?\n❯ 1. Yes\n     all of it\n  2. Only locally\n  3. No\n  4. Type something.\nEnter to select · ↑/↓ to navigate\n'
stty -echo 2>/dev/null
IFS= read -r keys
printf '%s' "$keys" | od -An -c > "$d/keys.log"
sleep 30
`

// The cockpit answers a session's question with one of its choices: the cursor goes there and Enter takes it; a label
// that is not that choice's any more is refused.
func TestChooseAnswersTheQuestionOnScreen(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	dir := filepath.Dir(file)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "claude"), []byte(dialogClaude), 0o700))
	_, _, err := zm(t, file, "up", "dev")
	require.NoError(t, err)
	time.Sleep(500 * time.Millisecond)

	a := &app{crew: file, out: io.Discard, err: io.Discard}
	require.NoError(t, a.withState(func(s *shift.Shift) error {
		require.ErrorContains(t, chooseDialog(s, "dev", "2:Only remotely"), "changed")
		require.ErrorContains(t, chooseDialog(s, "dev", "4:Type something."), "changed")
		return chooseDialog(s, "dev", "3:No")
	}))

	var keys []byte
	require.Eventually(t, func() bool {
		keys, err = os.ReadFile(filepath.Join(dir, "keys.log"))
		return err == nil && len(keys) > 0
	}, 5*time.Second, 100*time.Millisecond)
	assert.Equal(t, 2, strings.Count(string(keys), "["), "two Downs (ESC [ B), then Enter: %s", keys)
}
