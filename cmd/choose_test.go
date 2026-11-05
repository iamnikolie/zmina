package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
	var id string
	require.NoError(t, a.readState(func(s *shift.Shift) error {
		v := viewOf(s, "dev")
		require.NotNil(t, v)
		require.ErrorContains(t, chooseDialog(s, "dev", "3:x"), "not waiting", "no session says it waits: nothing pressed")

		// The session says it waits on a dialog, as Claude Code's registry does.
		raw, err := json.Marshal(map[string]any{"pid": v.PID, "sessionId": "conv-1", "cwd": dir, "status": "waiting"})
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "ns", "sessions"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "ns", "sessions", strconv.Itoa(v.PID)+".json"), raw, 0o600))

		return nil
	}))
	require.NoError(t, a.readState(func(s *shift.Shift) error {
		v := viewOf(s, "dev")
		require.NotNil(t, v.Dialog)
		id = v.Dialog.ID
		return nil
	}))
	require.NoError(t, a.withState(func(s *shift.Shift) error {
		other := id[:15] + map[bool]string{true: "1", false: "0"}[id[15] == '0']
		require.ErrorContains(t, chooseDialog(s, "dev", "2:"+other), "changed")
		require.ErrorContains(t, chooseDialog(s, "dev", "4:"+id), "changed", "a choice that asks for words is not offered")
		return chooseDialog(s, "dev", "3:"+id)
	}))

	var keys []byte
	require.Eventually(t, func() bool {
		keys, err = os.ReadFile(filepath.Join(dir, "keys.log"))
		return err == nil && len(keys) > 0
	}, 5*time.Second, 100*time.Millisecond)
	assert.Equal(t, 2, strings.Count(string(keys), "["), "two Downs (ESC [ B), then Enter: %s", keys)
}
