package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/tmux"
)

const permissionAsk = `{"session_id":"s-1","hook_event_name":"PermissionRequest","tool_name":"AskUserQuestion",` +
	`"tool_input":{"questions":[{"question":"Ship it?","header":"Release","multiSelect":false,"options":[` +
	`{"label":"Yes","description":"all of it"},{"label":"No"}]}]},"permission_suggestions":[]}`

// runHook runs a hook command with its input on stdin and returns what it printed, to stdout and stderr.
func runHook(t *testing.T, crewFile, stdin string, args ...string) (string, string) {
	t.Helper()

	var out, errb bytes.Buffer

	root := newRoot(&app{out: &out, err: &errb})
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"--crew", crewFile}, args...))
	require.NoError(t, root.Execute(), "a hook ends with exit 0 whatever it was given")

	return out.String(), errb.String()
}

// The PermissionRequest hook of a role records the question an AskUserQuestion puts on its screen, in the role's
// file next to the crew file, and says nothing: no decision, nothing a dialog could read as an answer.
func TestPermissionRequestHookRecordsTheQuestionAndSaysNothing(t *testing.T) {
	crewFile := testCrew(t, "")
	layout := crew.Layout{Dir: filepath.Dir(crewFile)}

	out, errOut := runHook(t, crewFile, permissionAsk, "_permission-request", "--role", "dev")
	assert.Empty(t, out, "no allow, no deny, nothing")
	assert.Empty(t, errOut)

	raw, err := os.ReadFile(layout.HookQuestion("dev"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"session_id":"s-1"`)
	assert.Contains(t, string(raw), `"question":"Ship it?"`)
	assert.Contains(t, string(raw), `"label":"Yes","description":"all of it"`)
	assert.Contains(t, string(raw), `"at":"`)
}

// The same hook, for any other tool, records the call as the role's permission record: the tool, what it was asked to do and a
// sum of the whole input — what a plugin is shown of a permission prompt, and what its fingerprint covers.
func TestPermissionRequestHookRecordsTheCallOfAPermissionPrompt(t *testing.T) {
	crewFile := testCrew(t, "")
	layout := crew.Layout{Dir: filepath.Dir(crewFile)}
	call := `{"session_id":"s-1","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"rm -rf build"}}`

	out, errOut := runHook(t, crewFile, call, "_permission-request", "--role", "dev")
	assert.Empty(t, out, "no allow, no deny, nothing")
	assert.Empty(t, errOut)

	raw, err := os.ReadFile(layout.HookPermission("dev"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"session_id":"s-1"`)
	assert.Contains(t, string(raw), `"tool":"Bash"`)
	assert.Contains(t, string(raw), `"input":"rm -rf build"`)
	assert.Contains(t, string(raw), `"input_sum":"`)
	assert.NoFileExists(t, layout.HookQuestion("dev"))
}

// Whatever it is given, the hook returns 0 and prints nothing, and records only an AskUserQuestion of a role, or another tool's
// call as its permission record. A flag this
// zmina does not know — a line of a newer zmina run by this one — is ignored and the rest of the line still runs.
func TestPermissionRequestHookIsSilentAndRecordsNothingElse(t *testing.T) {
	for name, tc := range map[string]struct {
		stdin   string
		args    []string
		records bool
		// permission: another tool's call is the role's permission record, not a question.
		permission bool
	}{
		"another tool":     {strings.Replace(permissionAsk, "AskUserQuestion", "Bash", 1), []string{"--role", "dev"}, false, true},
		"not json":         {"not json", []string{"--role", "dev"}, false, false},
		"nothing":          {"", []string{"--role", "dev"}, false, false},
		"no role":          {permissionAsk, nil, false, false},
		"a path as a role": {permissionAsk, []string{"--role", "../dev"}, false, false},
		"an unknown flag":  {permissionAsk, []string{"--role", "dev", "--nope"}, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			crewFile := testCrew(t, "")
			layout := crew.Layout{Dir: filepath.Dir(crewFile)}

			out, errOut := runHook(t, crewFile, tc.stdin, append([]string{"_permission-request"}, tc.args...)...)
			assert.Empty(t, out)
			assert.Empty(t, errOut)

			switch {
			case tc.records:
				assert.FileExists(t, layout.HookQuestion("dev"))
				assert.NoFileExists(t, layout.HookPermission("dev"))
			case tc.permission:
				assert.FileExists(t, layout.HookPermission("dev"))
				assert.NoFileExists(t, layout.HookQuestion("dev"))
			default:
				assert.NoDirExists(t, layout.Hooks())
			}
		})
	}
}

// An input too long to be a question is read to its end, so Claude Code writes into no closed pipe, and records
// nothing.
func TestPermissionRequestHookReadsALongInput(t *testing.T) {
	crewFile := testCrew(t, "")
	body := strings.Repeat("x", hookInputMax+(1<<20))
	in := strings.NewReader(`{"session_id":"s-1","tool_name":"Write","tool_input":{"content":"` + body + `"}}`)

	var out, errb bytes.Buffer

	root := newRoot(&app{out: &out, err: &errb})
	root.SetIn(in)
	root.SetArgs([]string{"--crew", crewFile, "_permission-request", "--role", "dev"})
	require.NoError(t, root.Execute())

	assert.Zero(t, in.Len(), "stdin read to its end")
	assert.Empty(t, out.String()+errb.String())
	assert.NoDirExists(t, filepath.Join(filepath.Dir(crewFile), "hooks"))
}

// A crew whose directory is not there gets no directory made by a hook.
func TestPermissionRequestHookMakesNoCrew(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone", "crew.yaml")

	out, errOut := runHook(t, gone, permissionAsk, "_permission-request", "--role", "dev")
	assert.Empty(t, out+errOut)
	assert.NoDirExists(t, filepath.Dir(gone))
}

// choiceTmux is a role's tmux with a question dialog on its screen, the keys pressed kept.
type choiceTmux struct {
	shift.Tmux
	pid    int
	screen string
	keys   []string
}

func (c *choiceTmux) Probe(string) (tmux.Pane, error) {
	return tmux.Pane{State: tmux.Up, PID: c.pid}, nil
}
func (c *choiceTmux) Prompt(string) (string, bool, error) { return "", false, nil }
func (c *choiceTmux) Capture(string, int) (string, error) { return c.screen, nil }
func (c *choiceTmux) KeepScreens(string) error            { return nil }
func (c *choiceTmux) Attached(string) (bool, error)       { return false, nil }
func (c *choiceTmux) Keys(_ string, keys ...string) error {
	c.keys = append(c.keys, keys...)
	return nil
}

// `choose` presses its keys by the numbers of the dialog the screen reads: the order of the recorded question is not
// proven to be the screen's numbering, so the record is for showing the question and an answer still needs a screen
// that reads. Where it does, the id the view gives is the one choose accepts; where it does not, nothing is pressed.
func TestChooseStaysOnTheScreenWhenAHookRecordedTheQuestion(t *testing.T) {
	crewFile := testCrew(t, "  dev:\n")
	dir := filepath.Dir(crewFile)
	pid := 4545

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "ns", "sessions"), 0o700))
	// The wait began now; the hook runs within the second, as Claude Code's does.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ns", "sessions", strconv.Itoa(pid)+".json"),
		[]byte(`{"pid": 4545, "sessionId": "s-1", "status": "waiting", "waitingFor": "input needed", "statusUpdatedAt": `+
			strconv.FormatInt(time.Now().UnixMilli(), 10)+`}`), 0o600))

	_, _ = runHook(t, crewFile, permissionAsk, "_permission-request", "--role", "dispatcher")

	const screen = " ☐ Release\n│ Ship it?\n❯ 1. Yes\n     all of it\n  2. No\n  3. Type something.\n  4. Chat about this\n" +
		"Enter to select · ↑/↓ to navigate · Esc to cancel"

	for name, tc := range map[string]struct {
		screen   string
		keys     []string
		question string
	}{
		"a screen that reads":    {screen, []string{"Down", "Enter"}, "Ship it?"},
		"a screen that does not": {"the question, drawn some way no parse reads", nil, "Ship it?"},
	} {
		t.Run(name, func(t *testing.T) {
			mux := &choiceTmux{pid: pid, screen: tc.screen}
			a := &app{crew: crewFile, out: &bytes.Buffer{}, err: &bytes.Buffer{}, shiftSeams: func(s *shift.Shift) {
				s.Tmux, s.Reg = mux, &registry.Registry{Namespace: filepath.Join(dir, "ns")}
			}}

			var id string

			require.NoError(t, a.readState(func(s *shift.Shift) error {
				v := viewOf(s, "dispatcher")
				require.NotNil(t, v)
				require.NotNil(t, v.Dialog, "the recorded question is the view's dialog either way")
				assert.Equal(t, tc.question, v.Dialog.Question)
				assert.Equal(t, "all of it", v.Dialog.Options[0].Detail)
				id = v.Dialog.ID

				return nil
			}))

			require.NoError(t, a.withState(func(s *shift.Shift) error {
				err := chooseDialog(s, "dispatcher", "2:"+id)
				if tc.keys == nil {
					require.ErrorContains(t, err, "no question on its screen now")

					return nil
				}

				return err
			}))

			assert.Equal(t, tc.keys, mux.keys)
		})
	}
}
