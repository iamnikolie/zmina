package shift

import (
	"os"
	"strings"
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

// A permission prompt as Claude Code draws it: the command is above the question, and the same words follow every time.
const permissionScreen = " Bash command\n\n   rm -rf build\n   clean up\n\nDo you want to proceed?\n❯ 1. Yes\n" +
	"  2. Yes, and don't ask again for rm commands in /work/example\n  3. No, and tell Claude what to do differently (esc)\n" +
	"Enter to select · ↑/↓ to navigate · Esc to cancel"

// A PermissionRequest for a tool call becomes the role's permission record: the tool, what it was asked to do (cut), and a sum of
// the whole input. AskUserQuestion has its own record, and anything that is not a call records nothing.
func TestRecordPermissionWritesTheCall(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	require.NoError(t, RecordPermission(l, "dev", permissionInput("sid-dev", "rm -rf build"), at))

	var got AskedPermission

	require.True(t, readRecord(l.HookPermission("dev"), &got))
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
	require.True(t, readRecord(l.HookPermission("dev"), &got))
	assert.Equal(t, first, got.InputSum)

	require.NoError(t, RecordPermission(l, "dev", permissionInput("sid-dev", "rm -rf ~/x"), at))
	require.True(t, readRecord(l.HookPermission("dev"), &got))
	assert.NotEqual(t, first, got.InputSum)

	// What is not a shell call is said by its path or its input; a long one is cut, its sum is of all of it.
	require.NoError(t, RecordPermission(l, "dev", []byte(`{"session_id":"s","tool_name":"Write","tool_input":{"file_path":"/work/example/a.txt","content":"`+strings.Repeat("x", 5000)+`"}}`), at))
	require.True(t, readRecord(l.HookPermission("dev"), &got))
	assert.Equal(t, "Write", got.Tool)
	assert.Equal(t, "/work/example/a.txt", got.Input)

	require.NoError(t, RecordPermission(l, "dev", []byte(`{"session_id":"s","tool_name":"mcp__x__send","tool_input":{"to":"`+strings.Repeat("y", 3000)+`"}}`), at))
	require.True(t, readRecord(l.HookPermission("dev"), &got))
	assert.Len(t, []rune(got.Input), permissionInputMax, "cut")

	// A command with a control character in it is one clean line.
	require.NoError(t, RecordPermission(l, "dev", permissionInput("s", "echo a\x1b[2J\nrm -rf /"), at))
	require.True(t, readRecord(l.HookPermission("dev"), &got))
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

	// Another command under the very same screen is another dialog.
	require.NoError(t, RecordPermission(f.s.Crew.Layout(), "designer", permissionInput("sid-designer", "rm -rf ~/x"), *f.now))

	two := dialogOf(t, f, "designer")
	require.NotNil(t, two)
	assert.Equal(t, "rm -rf ~/x", two.Subject)
	assert.NotEqual(t, one.ID, two.ID, "two commands never share an id")

	// The press reads the screen again and must come to the same id.
	assert.Equal(t, two.ID, f.s.DialogOn("designer", permissionScreen).ID)
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
