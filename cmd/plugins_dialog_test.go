package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/plugin"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// A permission prompt as Claude Code draws it, with a choice that asks for words.
const permissionScreen = ` ☐ Release
│ Ship it?
❯ 1. Yes
     all of it
  2. Only locally
  3. No
  4. Type something.
Enter to select · ↑/↓ to navigate
`

// dialogScreenTmux is a tmux whose role dev stands at a screen: a dialog, or its input line; it records the keys pressed.
type dialogScreenTmux struct {
	shift.Tmux
	pid int

	mu     sync.Mutex
	screen string
	input  bool
	keys   [][]string
}

func (f *dialogScreenTmux) Probe(name string) (tmux.Pane, error) {
	if name == "dev" {
		return tmux.Pane{State: tmux.Up, PID: f.pid}, nil
	}

	return tmux.Pane{}, nil
}

func (f *dialogScreenTmux) Prompt(string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return "", f.input, nil
}

func (f *dialogScreenTmux) Capture(string, int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.screen, nil
}

func (f *dialogScreenTmux) KeepScreens(string) error      { return nil }
func (f *dialogScreenTmux) Attached(string) (bool, error) { return false, nil }

func (f *dialogScreenTmux) Keys(_ string, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.keys = append(f.keys, keys)

	return nil
}

// show puts a dialog on the screen (and the session file says it waits), or takes it off (the input line is back).
func (f *dialogScreenTmux) show(t *testing.T, ns string, dialog bool) {
	t.Helper()

	f.mu.Lock()
	f.input = !dialog
	f.mu.Unlock()

	status := `"status": "idle"`
	if dialog {
		status = `"status": "waiting", "waitingFor": "permission prompt"`
	}

	body := `{"pid": ` + strconv.Itoa(f.pid) + `, "sessionId": "sid-1", ` + status + `, "statusUpdatedAt": 1}`
	require.NoError(t, os.MkdirAll(filepath.Join(ns, "sessions"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ns, "sessions", strconv.Itoa(f.pid)+".json"), []byte(body), 0o600))
}

func (f *dialogScreenTmux) pressed() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([][]string(nil), f.keys...)
}

// dialogWorld is a crew whose role dev stands at a dialog, and the seam that shows it to the commands.
func dialogWorld(t *testing.T, plugins string) (file string, f *dialogScreenTmux, seams func(s *shift.Shift), ns string) {
	t.Helper()

	file = pluginCrew(t, "  dev: {}\n", plugins)
	ns = filepath.Join(filepath.Dir(file), "ns")
	f = &dialogScreenTmux{pid: 4545, screen: permissionScreen}
	seams = func(s *shift.Shift) { s.Tmux, s.Reg = f, &registry.Registry{Namespace: ns} }

	return file, f, seams, ns
}

// runWith runs a command with the seams, as zm does without them.
func runWith(t *testing.T, file string, seams func(s *shift.Shift), args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer

	a := &app{out: &out, err: &bytes.Buffer{}, shiftSeams: seams}
	root := newRoot(a)
	root.SetArgs(append([]string{"--crew", file}, args...))
	err := root.Execute()

	return out.String(), err
}

// A question on a role's screen goes to the plugin once it has stood a minute (so what the role settles itself never pages),
// once; with the exact argv that answer it. Answered through the plugin, it is told as chosen, by the plugin.
func TestPluginsTellOfADialogAfterItStoodAMinute(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	f.show(t, ns, true)

	// The beats before the owner's tap are a few minutes back.
	now := time.Now().Add(-5 * time.Minute)

	deliver(t, file, now, seams)
	deliver(t, file, now.Add(30*time.Second), seams)
	assert.Empty(t, told(t, log), "it has not stood long enough")

	deliver(t, file, now.Add(time.Minute), seams)
	deliver(t, file, now.Add(2*time.Minute), seams)
	require.Len(t, told(t, log), 1, "once")

	ev := told(t, log)[0]
	assert.Equal(t, "dialog", ev.Event)
	id := ev.Line["id"].(string)
	assert.Regexp(t, `^dev:[0-9a-f]{16}$`, id)
	assert.Equal(t, "dev", ev.Line["role"])
	assert.Contains(t, ev.Line["question"], "Ship it?")
	require.Len(t, ev.Line["options"], 3, "the choice that asks for words is not offered")
	assert.Equal(t, map[string]any{"n": float64(3), "text": "No"}, ev.Line["options"].([]any)[2])

	opts := ev.Line["answer"].(map[string]any)["options"].([]any)
	third := opts[2].(map[string]any)["argv"].([]any)
	assert.Equal(t, []any{"zmina", "--crew", file, "choose", "dev", "3", "--dialog", id, "--by", "pager"}, third)

	// The plugin runs that argv, with ZMINA_PLUGIN set.
	args := []string{}
	for _, w := range third[3:] {
		args = append(args, w.(string))
	}

	t.Setenv("ZMINA_PLUGIN", "pager")

	// A tap for another question, or with no question named, or from a run that is not the plugin's, presses nothing.
	_, err := runWith(t, file, seams, "choose", "dev", "3", "--dialog", "dev:0000000000000000", "--by", "pager")
	require.ErrorContains(t, err, "the question on its screen changed: look again")

	_, err = runWith(t, file, seams, "choose", "dev", "3", "--by", "pager")
	require.ErrorContains(t, err, "--by needs --dialog")

	t.Setenv("ZMINA_PLUGIN", "")
	_, err = runWith(t, file, seams, args...)
	require.ErrorContains(t, err, "ZMINA_PLUGIN=pager is not set")
	assert.Empty(t, f.pressed(), "nothing pressed")

	t.Setenv("ZMINA_PLUGIN", "pager")
	out, err := runWith(t, file, seams, args...)
	require.NoError(t, err)
	assert.Equal(t, "dev · chose 3: No\n", out)
	assert.Equal(t, [][]string{{"Down", "Down", "Enter"}}, f.pressed())

	// The screen moves on: the plugin hears it was chosen, and by whom.
	f.show(t, ns, false)
	deliver(t, file, time.Now().Add(time.Minute), seams)
	deliver(t, file, time.Now().Add(2*time.Minute), seams)

	require.Equal(t, []string{"dialog " + id, "answered " + id}, toldIDs(t, log))
	end := told(t, log)[1].Line
	assert.Equal(t, "dialog", end["for"])
	assert.Equal(t, "chosen", end["how"])
	assert.Equal(t, "plugin pager", end["by"])
	assert.Equal(t, "No", end["text"])

	out, _, err = zm(t, file, "log", "--full")
	require.NoError(t, err)
	assert.Contains(t, out, "chose")
	assert.Contains(t, out, "3: No (answered through plugin pager)")
}

// A question that goes before it stood a minute is never told, nor its end; one that is told and goes with no choice made is gone;
// one that comes back stands anew.
func TestPluginsADialogThatGoesAway(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off, dialog_after: 60s}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now()

	f.show(t, ns, true)
	deliver(t, file, now, seams)
	f.show(t, ns, false) // the role settled it itself
	deliver(t, file, now.Add(30*time.Second), seams)
	deliver(t, file, now.Add(2*time.Minute), seams)
	assert.Empty(t, told(t, log))

	// Back, and stood: told. Then gone by the owner's own hand at the terminal: gone, no one named.
	f.show(t, ns, true)
	deliver(t, file, now.Add(3*time.Minute), seams)
	deliver(t, file, now.Add(4*time.Minute), seams)
	require.Len(t, told(t, log), 1)
	id := told(t, log)[0].Line["id"].(string)

	f.show(t, ns, false)
	deliver(t, file, now.Add(5*time.Minute), seams)
	assert.Equal(t, []string{"dialog " + id, "answered " + id}, toldIDs(t, log))
	assert.Equal(t, "gone", told(t, log)[1].Line["how"])
	assert.Nil(t, told(t, log)[1].Line["by"])

	// The same question again is a new one: told again once it has stood.
	f.show(t, ns, true)
	deliver(t, file, now.Add(6*time.Minute), seams)
	assert.Len(t, told(t, log), 2)
	deliver(t, file, now.Add(7*time.Minute), seams)
	assert.Equal(t, []string{"dialog " + id, "answered " + id, "dialog " + id}, toldIDs(t, log))
}

// The owner's own choose, by hand, needs no --dialog, and says he is at the machine; one that names a dialog is held to it.
func TestChooseByHand(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: 5m}\n")
	f.show(t, ns, true)

	_, err := runWith(t, file, seams, "choose", "dev", "10")
	require.ErrorContains(t, err, "n is the number of a choice")

	_, err = runWith(t, file, seams, "choose", "dev", "x")
	require.Error(t, err)

	_, err = runWith(t, file, seams, "choose", "dev", "5")
	require.ErrorContains(t, err, "changed", "a choice the dialog does not have")

	out, err := runWith(t, file, seams, "choose", "dev", "2")
	require.NoError(t, err)
	assert.Equal(t, "dev · chose 2: Only locally\n", out)
	assert.Equal(t, [][]string{{"Down", "Enter"}}, f.pressed())

	o := plugin.ReadOwner(filepath.Join(filepath.Dir(file), "plugins", "owner.json"))
	assert.WithinDuration(t, time.Now(), o.Active, time.Minute, "a choice in a terminal says he is here")
	require.Len(t, o.Chosen, 1)

	for key, c := range o.Chosen {
		assert.True(t, strings.HasPrefix(key, "dev:"))
		assert.Equal(t, "owner", c.By)
		assert.Equal(t, 2, c.N)
	}

	// Nothing on the screen: nothing pressed.
	f.show(t, ns, false)
	_, err = runWith(t, file, seams, "choose", "dev", "1")
	require.ErrorContains(t, err, "no question on its screen now")
	assert.Len(t, f.pressed(), 1)
}
