package tmux

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedTmux is a tmux binary that answers list-panes with one live pane and send-keys by mode, one of:
// "ok", "list-hangs" (list-panes gets no answer), "first-refused" (the first send-keys exits 1), "first-hangs" (the
// first send-keys gets no answer), "second-refused" (the second send-keys exits 1).
func scriptedTmux(t *testing.T, mode string) *Client {
	t.Helper()

	dir := t.TempDir()
	// Shell builtins only, so that a start costs little and the Timeout below can be short; a hang is an exec of sleep, so
	// that the call's kill ends it and its output pipe closes.
	script := `#!/bin/sh
dir=${0%/*}
read mode < "$dir/mode"
for a in "$@"; do
  case "$a" in list-panes|send-keys) sub=$a ;; esac
done
if [ "$sub" = list-panes ]; then
  [ "$mode" = list-hangs ] && exec sleep 5
  printf '%%1\t@zmina\t0\t4242\n'
  exit 0
fi
n=0
[ -f "$dir/n" ] && read n < "$dir/n"
n=$((n + 1))
echo $n > "$dir/n"
case "$mode:$n" in
  first-refused:1|second-refused:2) echo "can't find pane: %1" >&2; exit 1 ;;
  first-hangs:1) exec sleep 5 ;;
esac
exit 0
`
	bin := filepath.Join(dir, "tmux")
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mode"), []byte(mode), 0o600))

	old := Timeout
	Timeout = time.Second
	t.Cleanup(func() { Timeout = old })

	return &Client{Bin: bin, Socket: "scripted"}
}

// A failure before any key of the line was sent, or one tmux itself gave for the first, is an ErrNotTyped: nothing of the
// line reached the pane, so it may be tried again.
func TestTypeSaysWhenNothingWasTyped(t *testing.T) {
	t.Run("the role has no session", func(t *testing.T) {
		c := server(t)

		err := c.Type("nobody", "a line")

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNotTyped)
	})

	t.Run("the pane could not be asked for, not even in time", func(t *testing.T) {
		err := scriptedTmux(t, "list-hangs").Type("r", "a line")

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNotTyped)
		assert.Contains(t, err.Error(), "no answer in", "its own words stay")
	})

	t.Run("tmux refused the first send-keys", func(t *testing.T) {
		err := scriptedTmux(t, "first-refused").Type("r", "a line")

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNotTyped)
		assert.Contains(t, err.Error(), "can't find pane")
	})
}

// A first send-keys that got no answer may still have run, and one that failed after another went has typed something:
// neither is an ErrNotTyped.
func TestTypeDoesNotSayNothingWasTypedWhenSomethingMayHaveBeen(t *testing.T) {
	t.Run("the first send-keys got no answer", func(t *testing.T) {
		err := scriptedTmux(t, "first-hangs").Type("r", "a line")

		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrNotTyped)
		assert.Contains(t, err.Error(), "no answer in")
	})

	t.Run("a later send-keys failed", func(t *testing.T) {
		err := scriptedTmux(t, "second-refused").Type("r", "a line")

		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrNotTyped)
	})

	t.Run("nothing failed", func(t *testing.T) {
		require.NoError(t, scriptedTmux(t, "ok").Type("r", "a line"))
	})
}
