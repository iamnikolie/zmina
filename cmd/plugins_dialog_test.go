package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
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

	// waitingFor is what the session file says it waits on: "input needed" (a question) or "permission prompt"; since is when the
	// wait began, as the session file has it.
	waitingFor string
	since      time.Time
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

// show puts a dialog on the screen (and the session file says it waits, a new wait each time), or takes it off (the input line
// is back).
func (f *dialogScreenTmux) show(t *testing.T, ns string, dialog bool) {
	t.Helper()

	f.mu.Lock()
	f.input = !dialog

	// A new wait is later than the one before, as the file's own milliseconds say.
	if now := time.Now(); !now.After(f.since) {
		f.since = f.since.Add(time.Millisecond)
	} else {
		f.since = now
	}

	waiting := f.waitingFor
	if waiting == "" {
		waiting = "input needed"
	}

	since := f.since
	f.mu.Unlock()

	status := `"status": "idle"`
	if dialog {
		status = `"status": "waiting", "waitingFor": "` + waiting + `"`
	}

	body := `{"pid": ` + strconv.Itoa(f.pid) + `, "sessionId": "sid-1", ` + status + `, "statusUpdatedAt": ` + strconv.FormatInt(since.UnixMilli(), 10) + `}`
	require.NoError(t, os.MkdirAll(filepath.Join(ns, "sessions"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ns, "sessions", strconv.Itoa(f.pid)+".json"), []byte(body), 0o600))
}

// permission makes the wait a permission prompt of a tool call, as its hook recorded it: the command, written the moment the wait
// began.
func (f *dialogScreenTmux) permission(t *testing.T, crewFile, ns, command string) {
	t.Helper()

	f.mu.Lock()
	f.waitingFor = "permission prompt"
	f.mu.Unlock()

	f.show(t, ns, true)

	input := `{"session_id":"sid-1","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":` + strconv.Quote(command) + `}}`
	require.NoError(t, shift.RecordPermission(crew.Layout{Dir: filepath.Dir(crewFile)}, "dev", []byte(input), f.since))
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
	assert.Regexp(t, `^dev:[0-9a-f]{16}:[0-9a-f]{16}$`, id, "role:fingerprint:one-shot mark")
	assert.Equal(t, "question", ev.Line["prompt"])
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

	// A tap for another question, or with no question named, or with no mark, or from a run that is not the plugin's, presses nothing.
	_, err := runWith(t, file, seams, "choose", "dev", "3", "--dialog", "dev:0000000000000000:"+strings.Split(id, ":")[2], "--by", "pager")
	require.ErrorContains(t, err, "the question on its screen changed: look again")

	_, err = runWith(t, file, seams, "choose", "dev", "3", "--by", "pager")
	require.ErrorContains(t, err, "--by needs --dialog")

	_, err = runWith(t, file, seams, "choose", "dev", "3", "--dialog", strings.Join(strings.Split(id, ":")[:2], ":"), "--by", "pager")
	require.ErrorContains(t, err, "no one-shot mark")

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

	ids := toldIDs(t, log)
	require.Len(t, ids, 3)
	assert.Equal(t, []string{"dialog " + id, "answered " + id}, ids[:2])
	assert.True(t, strings.HasPrefix(ids[2], "dialog "+strings.Join(strings.Split(id, ":")[:2], ":")+":"), "the same question")
	assert.NotEqual(t, "dialog "+id, ids[2], "with a mark of its own: a card of the first does not press the second")
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
	require.ErrorContains(t, err, "not waiting on a question now")
	assert.Len(t, f.pressed(), 1)
}

// A choose run by a role is recorded as that role's, and is no sign that the owner is at the machine; the cockpit's button is the
// owner's own choice, and is noted for the plugins too.
func TestChooseRecordsWhoChose(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: 5m}\n")
	f.show(t, ns, true)
	owner := filepath.Join(filepath.Dir(file), "plugins", "owner.json")

	t.Setenv("ZMINA_ROLE", "dev")

	_, err := runWith(t, file, seams, "choose", "dev", "2")
	require.NoError(t, err)

	o := plugin.ReadOwner(owner)
	assert.True(t, o.Active.IsZero(), "a role's choice does not say the owner is here")
	require.Len(t, o.Chosen, 1)

	for _, c := range o.Chosen {
		assert.Equal(t, "role dev", c.By)
	}

	t.Setenv("ZMINA_PLUGIN", "pager")
	_, err = runWith(t, file, seams, "choose", "dev", "2", "--dialog", "dev:x", "--by", "pager")
	require.ErrorContains(t, err, "a role (dev) does not answer in a plugin's voice")
	t.Setenv("ZMINA_ROLE", "")

	// The cockpit.
	a := &app{crew: file, out: io.Discard, err: io.Discard, shiftSeams: seams}

	var id string

	require.NoError(t, a.readState(func(s *shift.Shift) error {
		id = viewOf(s, "dev").Dialog.ID

		return nil
	}))
	require.NoError(t, a.withState(func(s *shift.Shift) error { return chooseDialog(s, "dev", "3:"+id) }))

	o = plugin.ReadOwner(owner)
	assert.WithinDuration(t, time.Now(), o.Active, time.Minute)

	chosen := plugin.Chosen{}

	for _, c := range o.Chosen {
		if c.N == 3 {
			chosen = c
		}
	}

	assert.Equal(t, "owner", chosen.By)
	assert.Equal(t, "No", chosen.Text)
}

// A permission prompt is told to a plugin only with the call its hook recorded: the screen says "Do you want to proceed?" and not
// of what, and a plugin shown that would be asked to approve blind. With the record, the event carries the tool and the command,
// and two commands never share an id.
func TestPluginsTellOfAPermissionPromptOnlyWithItsSubject(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now().Add(-30 * time.Minute)

	// No record: it stands for as long as it likes, and is never told.
	f.mu.Lock()
	f.waitingFor = "permission prompt"
	f.mu.Unlock()
	f.show(t, ns, true)

	for _, m := range []int{0, 1, 2, 10} {
		deliver(t, file, now.Add(time.Duration(m)*time.Minute), seams)
	}

	assert.Empty(t, told(t, log), "a permission prompt with no hook record is not told: no blind approvals")

	// The hook's record of the call, of this very wait: now it is.
	f.permission(t, file, ns, "rm -rf build")
	deliver(t, file, now.Add(11*time.Minute), seams)
	deliver(t, file, now.Add(12*time.Minute), seams)
	require.Len(t, told(t, log), 1)

	ev := told(t, log)[0].Line
	assert.Equal(t, "permission", ev["prompt"])
	assert.Equal(t, "Bash", ev["tool"])
	assert.Equal(t, "rm -rf build", ev["subject"], "what is being approved")
	assert.Contains(t, ev["question"], "Ship it?")

	first := ev["id"].(string)

	// Another command under the very same words: another dialog, told as one; the old card is closed.
	f.permission(t, file, ns, "rm -rf ~/x")
	deliver(t, file, now.Add(13*time.Minute), seams)
	deliver(t, file, now.Add(14*time.Minute), seams)
	deliver(t, file, now.Add(15*time.Minute), seams)

	ids := toldIDs(t, log)
	require.Len(t, ids, 3, "%v", ids)
	assert.Equal(t, "answered "+first, ids[1], "the first card is closed")

	second := told(t, log)[2]
	assert.Equal(t, "dialog", second.Event)
	assert.Equal(t, "rm -rf ~/x", second.Line["subject"])
	assert.NotEqual(t, first, second.Line["id"])
	assert.NotEqual(t, strings.Join(strings.Split(first, ":")[:2], ":"), strings.Join(strings.Split(second.Line["id"].(string), ":")[:2], ":"),
		"the fingerprint is of the command too: two commands never share one")

	// The first card's press, now: the screen asks about another command.
	t.Setenv("ZMINA_PLUGIN", "pager")

	_, err := runWith(t, file, seams, "choose", "dev", "1", "--dialog", first, "--by", "pager")
	require.ErrorContains(t, err, "the question on its screen changed: look again")
	assert.Empty(t, f.pressed(), "a stale card presses nothing")

	// The second card's press, at once.
	out, err := runWith(t, file, seams, "choose", "dev", "1", "--dialog", second.Line["id"].(string), "--by", "pager")
	require.NoError(t, err)
	assert.Equal(t, "dev · chose 1: Yes\n", out)
	assert.Len(t, f.pressed(), 1)
}

// A press made through a plugin is one-shot: the first disarms its mark; a second press, a stale card, one with no mark or another's,
// is refused with its own words and presses nothing, though the very same question is still on the screen.
func TestPluginPressIsOneShot(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n  - {name: other, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	recorder(t, file, "other", "")
	now := time.Now().Add(-10 * time.Minute)

	f.permission(t, file, ns, "make deploy")
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	require.Len(t, told(t, log), 1)

	id := told(t, log)[0].Line["id"].(string)
	t.Setenv("ZMINA_PLUGIN", "pager")

	press := func() error {
		_, err := runWith(t, file, seams, "choose", "dev", "1", "--dialog", id, "--by", "pager")

		return err
	}

	// A mark of another plugin, or a made-up one, is no mark of this.
	_, err := runWith(t, file, seams, "choose", "dev", "1", "--dialog", strings.Join(strings.Split(id, ":")[:2], ":")+":0123456789abcdef", "--by", "pager")
	require.ErrorContains(t, err, "not armed")
	assert.Empty(t, f.pressed())

	require.NoError(t, press(), "the first press")
	assert.Equal(t, [][]string{{"Enter"}}, f.pressed())

	// The very same question is still on the screen (the role has not moved): the same card, tapped again, goes nowhere.
	err = press()
	require.Error(t, err)
	assert.ErrorContains(t, err, "used already")
	assert.ErrorContains(t, err, "a press goes once")
	assert.Len(t, f.pressed(), 1, "a double tap, or a retry of the plugin's own network call, presses once")

	// Keys that could not be sent give the press back.
	g := &failingKeys{dialogScreenTmux: f}
	seamsFailing := func(s *shift.Shift) { s.Tmux, s.Reg = g, &registry.Registry{Namespace: ns} }

	f.permission(t, file, ns, "make deploy 2")
	deliver(t, file, now.Add(2*time.Minute), seams)
	deliver(t, file, now.Add(3*time.Minute), seams)

	var second string

	for _, g := range told(t, log) {
		if g.Event == "dialog" && g.Line["subject"] == "make deploy 2" {
			second = g.Line["id"].(string)
		}
	}

	require.NotEmpty(t, second)

	_, err = runWith(t, file, seamsFailing, "choose", "dev", "1", "--dialog", second, "--by", "pager")
	require.Error(t, err, "the keys could not be sent")

	out, err := runWith(t, file, seams, "choose", "dev", "1", "--dialog", second, "--by", "pager")
	require.NoError(t, err, "the mark was given back")
	assert.Equal(t, "dev · chose 1: Yes\n", out)
}

// failingKeys is a tmux whose keys do not go.
type failingKeys struct{ *dialogScreenTmux }

func (f *failingKeys) Keys(string, ...string) error { return os.ErrClosed }

// A prompt answered and asked again in the same breath, with the very same words and call, is a new wait: the old card is closed in
// the same beat and the new one is told, with a mark of its own, once it has stood.
func TestPluginsAPromptReplacedByItsTwinWithinABeat(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now().Add(-10 * time.Minute)

	f.permission(t, file, ns, "make deploy")
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	require.Len(t, told(t, log), 1)

	first := told(t, log)[0].Line["id"].(string)

	// The same command is asked again: a new wait, the same screen.
	f.permission(t, file, ns, "make deploy")
	deliver(t, file, now.Add(2*time.Minute), seams)

	assert.Equal(t, []string{"dialog " + first, "answered " + first}, toldIDs(t, log), "the old card is closed in the beat")
	assert.Equal(t, "gone", told(t, log)[1].Line["how"])

	deliver(t, file, now.Add(3*time.Minute), seams)

	ids := toldIDs(t, log)
	require.Len(t, ids, 3)
	assert.NotEqual(t, "dialog "+first, ids[2], "a mark of its own")
}

// A question whose choices are ticked is told, and says it needs the terminal: no argv to press one choice of it.
func TestPluginsTellOfAMultipleChoiceWithoutAnArgv(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now().Add(-10 * time.Minute)

	f.screen = " ☐ Tabs\n│ Which tabs?\n❯ 1. ☐ Home\n  2. ☐ Talk\n  3. Type something.\nEnter to select · ↑/↓ to navigate · Esc to cancel"
	f.show(t, ns, true)
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	require.Len(t, told(t, log), 1)

	ev := told(t, log)[0].Line
	assert.Equal(t, true, ev["needs_terminal"])
	assert.Nil(t, ev["answer"])
	assert.Len(t, ev["options"], 2)
}

// A prompt the session does not say what it waits on is not told: it may be a permission.
func TestPluginsDoNotTellOfAPromptThatIsNotSaidToBeAQuestion(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now().Add(-10 * time.Minute)

	f.mu.Lock()
	f.waitingFor = "something else"
	f.mu.Unlock()
	f.show(t, ns, true)

	for _, m := range []int{0, 1, 2} {
		deliver(t, file, now.Add(time.Duration(m)*time.Minute), seams)
	}

	assert.Empty(t, told(t, log))
}
