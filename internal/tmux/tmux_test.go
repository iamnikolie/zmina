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

	t.Cleanup(func() { KillServer(c.Socket) })

	return c
}

// eventuallyWithin is how long eventually waits for what a real tmux server and the process in its pane are to show: as long as a
// machine busy with other work could need. A run that is not slow does not wait it out, cond is asked again every poll.
const eventuallyWithin = 30 * time.Second

// eventually asks cond until it holds, and fails the test when it does not within eventuallyWithin.
func eventually(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(eventuallyWithin)

	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", eventuallyWithin)
		}

		time.Sleep(25 * time.Millisecond)
	}
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

// A line break goes as Ctrl-J (the key that breaks a line in Claude Code's input) and long text in pieces, every
// character of it arriving in order: cat shows each line as it comes.
func TestTypeLinesBreaksLinesAndTypesLongTextInPieces(t *testing.T) {
	c := server(t)
	require.NoError(t, c.Start("r", t.TempDir(), nil, []string{"cat", "-v"}))

	long := strings.Repeat("слово ", 70) + "end"
	require.NoError(t, c.TypeLines("r", "first line\r\nsecond line\n"+long))

	eventually(t, func() bool {
		out, _ := c.Capture("r", 40)
		return strings.Count(out, "first line") == 2 && strings.Count(out, "second line") == 2 && strings.Count(strings.ReplaceAll(out, "\n", ""), "end") == 2
	})

	out, _ := c.Capture("r", 40)
	assert.NotContains(t, out, "^M", "no carriage return typed")
	assert.Len(t, pieces(long, typePiece), 3, "423 characters: three pieces")
}

// Type keeps to one line: a break in its text is a space, for an input where Ctrl-J would submit part of it.
func TestTypeFlattensLineBreaks(t *testing.T) {
	c := server(t)
	require.NoError(t, c.Start("r", t.TempDir(), nil, []string{"cat", "-v"}))

	require.NoError(t, c.Type("r", "first\nsecond"))

	eventually(t, func() bool {
		out, _ := c.Capture("r", 40)
		return strings.Count(out, "first second") == 2
	})
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
		"cursor":      `printf '───\n\033[39m❯\302\240\033[7mп\033[27m\033[2mодними dev-сервер\033[0m\n───\n'`,
		"typed":       `printf '───\n\033[39m❯\302\240закрывай\033[7m \033[27m\n───\n'`,
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
		"multiline": {"second line", true}, "nbsp": {"", true}, "cursor": {"", true}, "typed": {"закрывай", true},
		"dialog": {"", false}, "trust": {"", false}}

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

// What names a plugin's run, or holds the cockpit's credential, never reaches a role through the tmux server's environment.
func TestCleanEnvKeepsAPluginsNameAndTheCockpitsSecretsFromRoles(t *testing.T) {
	env := CleanEnv([]string{"PATH=/bin", "ZMINA_PLUGIN=pager", "ZMINA_PLUGIN_EVENT=ask", "ZMINA_UI_TOKEN=x", "ZMINA_ROLE=dev",
		"ZMINA_CREW=/c/crew.yaml"}, nil)

	assert.Equal(t, []string{"PATH=/bin", "ZMINA_CREW=/c/crew.yaml"}, env)
}

// Sessions lists each session of the server once and says which of them zmina started (a pane carrying its tag): one opened by
// hand on the same server is listed untagged, a second window does not list its session twice, a session whose process ended
// stays on the list, and a server that is not there has no sessions.
func TestSessionsListsEachSessionOnceAndSaysWhichZminaStarted(t *testing.T) {
	c := server(t)
	c.Owner = OwnerTag("/crews/dev/crew.yaml")

	none, err := c.Sessions()
	require.NoError(t, err, "no server is no sessions, not a failure")
	assert.Empty(t, none)

	require.NoError(t, c.Start("dev", t.TempDir(), nil, []string{"cat"}))
	require.NoError(t, c.Start("dev#2", t.TempDir(), nil, []string{"cat"}))
	require.NoError(t, c.Start("gone", t.TempDir(), nil, []string{"true"}))

	_, err = c.run("new-session", "-d", "-s", "by-hand", "--", "cat")
	require.NoError(t, err)

	_, err = c.run("new-window", "-t", "=dev:", "--", "cat")
	require.NoError(t, err)

	eventually(t, func() bool { return c.Dead("gone") })

	got, err := c.Sessions()
	require.NoError(t, err)
	assert.ElementsMatch(t, []SessionInfo{
		{Name: "dev", Tagged: true},
		{Name: "dev#2", Tagged: true},
		{Name: "gone", Tagged: true},
		{Name: "by-hand", Tagged: false},
	}, got)

	require.NoError(t, c.Kill("dev#2"))

	got, err = c.Sessions()
	require.NoError(t, err)
	assert.Len(t, got, 3)
	assert.NotContains(t, got, SessionInfo{Name: "dev#2", Tagged: true})
}

// Crews that share a tmux server tell their sessions apart by the value of the tag: a session is Tagged for the crew that started it
// and Foreign for the other; one tagged the old way ("1"), by a client that does not say whose it is, or not at all is neither, and
// no crew may take it for its own. KeepScreens gives the old tag and none to the crew that asks, and leaves another crew's alone.
func TestSessionsTellTheCrewsOfOneServerApart(t *testing.T) {
	a := server(t)
	a.Owner = OwnerTag("/crews/a/crew.yaml")
	b := &Client{Bin: a.Bin, Socket: a.Socket, Owner: OwnerTag("/crews/b/crew.yaml")}
	legacy := &Client{Bin: a.Bin, Socket: a.Socket}

	require.NotEqual(t, a.Owner, b.Owner)
	assert.Equal(t, a.Owner, OwnerTag("/crews/a/crew.yaml"), "the same for every zmina that works the crew")
	assert.Empty(t, OwnerTag(""))

	require.NoError(t, a.Start("a-dev", t.TempDir(), nil, []string{"cat"}))
	require.NoError(t, b.Start("b-dev", t.TempDir(), nil, []string{"cat"}))
	require.NoError(t, legacy.Start("old", t.TempDir(), nil, []string{"cat"}))

	_, err := a.run("new-session", "-d", "-s", "by-hand", "--", "cat")
	require.NoError(t, err)

	// A window of a-dev that b tagged: a mixture, which is nobody's to be sure of.
	_, err = a.run("new-window", "-t", "=a-dev:", "--", "cat")
	require.NoError(t, err)
	_, err = a.run("set-option", "-p", "-t", "=a-dev:{end}.{top-left}", tag, b.Owner)
	require.NoError(t, err)

	seenBy := func(c *Client) map[string]SessionInfo {
		got, err := c.Sessions()
		require.NoError(t, err)

		out := map[string]SessionInfo{}
		for _, s := range got {
			out[s.Name] = s
		}

		return out
	}

	got := seenBy(a)
	assert.Equal(t, SessionInfo{Name: "a-dev"}, got["a-dev"], "a pane of another crew's among its own: nobody's")
	assert.Equal(t, SessionInfo{Name: "b-dev", Foreign: true}, got["b-dev"])
	assert.Equal(t, SessionInfo{Name: "old"}, got["old"], "the old tag proves nothing")
	assert.Equal(t, SessionInfo{Name: "by-hand"}, got["by-hand"])

	// Take the mixed window away: a-dev is a's alone.
	_, err = a.run("kill-pane", "-t", "=a-dev:{end}.{top-left}")
	require.NoError(t, err)

	got = seenBy(a)
	assert.Equal(t, SessionInfo{Name: "a-dev", Tagged: true}, got["a-dev"])
	assert.Equal(t, SessionInfo{Name: "b-dev", Foreign: true}, got["b-dev"])

	got = seenBy(b)
	assert.Equal(t, SessionInfo{Name: "b-dev", Tagged: true}, got["b-dev"])
	assert.Equal(t, SessionInfo{Name: "a-dev", Foreign: true}, got["a-dev"])

	got = seenBy(legacy)
	assert.False(t, got["a-dev"].Tagged || got["b-dev"].Tagged || got["old"].Tagged, "a client that says no crew owns nothing")

	// KeepScreens tags a session of the old kind for the crew that keeps it, and never takes another crew's.
	require.NoError(t, a.KeepScreens("old"))
	assert.Equal(t, SessionInfo{Name: "old", Tagged: true}, seenBy(a)["old"])

	require.NoError(t, b.KeepScreens("a-dev"))
	assert.Equal(t, SessionInfo{Name: "a-dev", Tagged: true}, seenBy(a)["a-dev"], "still a's")
}
