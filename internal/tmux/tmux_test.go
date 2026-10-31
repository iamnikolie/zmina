package tmux

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The client against a real tmux server of its own.
func server(t *testing.T) *Client {
	t.Helper()

	c, err := New("zmina-test-" + strconv.FormatInt(time.Now().UnixNano(), 36))
	if err != nil {
		t.Skip(err)
	}

	t.Cleanup(func() { _, _ = c.run("kill-server") })

	return c
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()

	for range 50 {
		if cond() {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatal("condition not met")
}

func TestTypeKeepsDashesAndSemicolons(t *testing.T) {
	c := server(t)
	require.NoError(t, c.Start("r", t.TempDir(), nil, []string{"cat"}))

	require.NoError(t, c.Type("r", "- a bullet;"))
	require.NoError(t, c.Type("r", "--flag-like"))

	eventually(t, func() bool {
		out, _ := c.Capture("r", 10)
		return strings.Count(out, "- a bullet;") == 2 && strings.Count(out, "--flag-like") == 2
	})
}

// Type sends no control byte: cat -v would show a C-u as ^U and an ESC as ^[.
func TestTypeSendsNoControlBytes(t *testing.T) {
	c := server(t)
	require.NoError(t, c.Start("r", t.TempDir(), nil, []string{"cat", "-v"}))

	require.NoError(t, c.Type("r", "a\x15!b\x1b[Zc\rd"))

	eventually(t, func() bool {
		out, _ := c.Capture("r", 10)
		return strings.Count(out, "a!b[Zc d") == 2
	})

	out, _ := c.Capture("r", 10)
	assert.NotContains(t, out, "^U")
	assert.NotContains(t, out, "^[")
}

func TestOnlyTheRolePaneCounts(t *testing.T) {
	c := server(t)
	require.NoError(t, c.Start("r", t.TempDir(), nil, []string{"cat"}))
	assert.True(t, c.Has("r"))

	pid, err := c.PanePID("r")
	require.NoError(t, err)
	assert.Positive(t, pid)

	// The owner opens a second window while attached and leaves it: it closes, the role is untouched.
	_, err = c.run("new-window", "-t", "=r:", "--", "sh", "-c", "exit 0")
	require.NoError(t, err)
	eventually(t, func() bool {
		out, _ := c.run("list-windows", "-t", "=r", "-F", "#{window_index}")
		return strings.Count(strings.TrimSpace(out), "\n") == 0
	})
	assert.True(t, c.Has("r"))
	assert.False(t, c.Dead("r"))

	// A second window that stays and is current: commands still aim at the role's pane.
	_, err = c.run("new-window", "-t", "=r:", "--", "sleep", "30")
	require.NoError(t, err)

	pid2, err := c.PanePID("r")
	require.NoError(t, err)
	assert.Equal(t, pid, pid2)

	// The role's process ends: its window stays, dead, with its screen.
	require.NoError(t, c.Type("r", "last words"))
	require.NoError(t, c.Keys("r", "C-d"))
	eventually(t, func() bool { return c.Dead("r") })
	assert.False(t, c.Has("r"))

	out, err := c.Capture("r", 3)
	require.NoError(t, err)
	assert.Contains(t, out, "last words", "the last output sits right above the dead-pane line")
	assert.Contains(t, out, "Pane is dead")

	require.NoError(t, c.Kill("r"))
	assert.False(t, c.Has("r") || c.Dead("r"))
}

func TestKeepScreensOnOlderSession(t *testing.T) {
	c := server(t)

	// A session started the old way, without remain-on-exit.
	_, err := c.run("new-session", "-d", "-s", "old", "--", "cat")
	require.NoError(t, err)
	require.NoError(t, c.KeepScreens("old"))

	// cat may not read yet when the first C-d arrives: press it until it ends.
	eventually(t, func() bool {
		_ = c.Keys("old", "C-d")

		return c.Dead("old")
	})

	p, err := c.Probe("old")
	require.NoError(t, err)
	assert.Equal(t, Dead, p.State, "its screen is kept now")
}

func TestProbeTellsAbsentFromUnknown(t *testing.T) {
	c := server(t)

	p, err := c.Probe("nobody")
	require.NoError(t, err, "no server at all is plain absence")
	assert.Equal(t, Absent, p.State)

	require.NoError(t, c.Start("r", t.TempDir(), nil, []string{"cat"}))

	p, err = c.Probe("nobody")
	require.NoError(t, err)
	assert.Equal(t, Absent, p.State)

	broken := &Client{Bin: "/usr/bin/false", Socket: c.Socket}
	_, err = broken.Probe("r")
	assert.Error(t, err, "tmux that fails for another reason is not absence")
}

func TestAttached(t *testing.T) {
	c := server(t)

	_, err := c.run("new-session", "-d", "-s", "role", "--", "cat")
	require.NoError(t, err)

	attached, err := c.Attached("role")
	require.NoError(t, err)
	assert.False(t, attached, "started detached")

	_, err = c.Attached("gone")
	assert.Error(t, err)
}

func TestPrompt(t *testing.T) {
	c := server(t)

	screens := map[string]string{
		"empty":       `printf '❯ asked before\n───\n❯ \n───\n  status\n'`,
		"draft":       `printf '❯ asked before\n───\n\033[39m❯ \033[38;5;231mhalf a thought\033[39m\n───\n'`,
		"placeholder": `printf '───\n❯ \033[2mTry "fix lint errors"\033[0m\n───\n'`,
		"multiline":   `printf '───\n❯ \n  second line\n───\n'`,
		"nbsp":        `printf '───\n\033[39m❯\302\240\033[2mTry "how does <filepath> work?"\033[0m\n───\n'`,
		"dialog":      `printf 'Do you trust this folder?\n 1. Yes\n 2. No\n'`,
		"trust":       `printf '───\n Accessing workspace:\n\n ❯ No, exit\n   Yes, I trust this folder\n\n Enter to confirm\n'`,
	}
	for name, script := range screens {
		_, err := c.run("new-session", "-d", "-s", name, "--", "sh", "-c", script+"; cat")
		require.NoError(t, err)
	}

	want := map[string]struct {
		draft string
		ok    bool
	}{"empty": {"", true}, "draft": {"half a thought", true}, "placeholder": {"", true},
		"multiline": {"second line", true}, "nbsp": {"", true}, "dialog": {"", false}, "trust": {"", false}}

	for name, w := range want {
		eventually(t, func() bool {
			out, _ := c.Capture(name, 5)

			return strings.TrimSpace(out) != ""
		})

		draft, ok, err := c.Prompt(name)
		require.NoError(t, err, name)
		assert.Equal(t, w.draft, draft, name)
		assert.Equal(t, w.ok, ok, name)
	}
}

func TestProbeNeverTakesAPrefix(t *testing.T) {
	c := server(t)

	_, err := c.run("new-session", "-d", "-s", "dev3", "--", "cat")
	require.NoError(t, err)

	p, err := c.Probe("dev")
	require.NoError(t, err)
	assert.Equal(t, Absent, p.State, "dev is down although dev3 runs")

	p, err = c.Probe("dev3")
	require.NoError(t, err)
	assert.Equal(t, Up, p.State)
}
