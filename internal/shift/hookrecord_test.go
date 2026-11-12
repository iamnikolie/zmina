package shift

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/registry"
)

// askedInput is what Claude Code hands a PermissionRequest hook for an AskUserQuestion call.
const askedInput = `{"session_id":"sid-designer","hook_event_name":"PermissionRequest","tool_name":"AskUserQuestion",` +
	`"tool_input":{"questions":[{"question":"Which screen first?","header":"Screen","multiSelect":false,"options":[` +
	`{"label":"Home","description":"the landing page"},{"label":"Talk","description":"the conversation"}]}]}}`

// askedScreen is the question as Claude Code draws it, with its two own choices after the call's.
const askedScreen = " ☐ Screen\n│ Which screen first?\n❯ 1. Home\n     the landing page\n  2. Talk\n     the conversation\n" +
	"  3. Type something.\n  4. Chat about this\nEnter to select · ↑/↓ to navigate · Esc to cancel"

// waitingOn puts a role's session in the registry as waiting on a question since the fixture's now.
func (f fix) waitingOn(role, waitingFor string) {
	f.rg.byPID[f.tm.up[role]] = &registry.Session{Status: "waiting", WaitingFor: waitingFor,
		StatusUpdatedAt: f.now.UnixMilli(), SessionID: "sid-" + role}
	f.tm.drafts[role] = "-"
}

func dialogOf(t *testing.T, f fix, role string) *Dialog {
	t.Helper()

	for _, v := range f.s.Views() {
		if v.Role == role {
			return v.Dialog
		}
	}

	t.Fatalf("no view of %s", role)

	return nil
}

// A PermissionRequest for AskUserQuestion becomes the role's record: the questions as the tool had them, the
// session and the time. Any other call, or input that is not a call, records nothing; the hook is never an error to
// its caller beyond what it returns.
func TestRecordQuestionWritesTheCallsQuestions(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	require.NoError(t, RecordQuestion(l, "designer", []byte(askedInput), at))

	var got AskedQuestion
	require.True(t, readRecord(l.HookQuestion("designer"), &got))
	assert.Equal(t, AskedQuestion{SessionID: "sid-designer", At: at, Questions: []AskedItem{{
		Question: "Which screen first?", Header: "Screen",
		Options: []AskedOption{{Label: "Home", Description: "the landing page"}, {Label: "Talk", Description: "the conversation"}},
	}}}, got)

	info, err := os.Stat(l.HookQuestion("designer"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	entries, err := os.ReadDir(l.Hooks())
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temporary file is left")

	// The next question replaces it.
	later := at.Add(time.Minute)
	require.NoError(t, RecordQuestion(l, "designer", []byte(strings.Replace(askedInput, "Which screen first?", "And then?", 1)), later))
	require.True(t, readRecord(l.HookQuestion("designer"), &got))
	assert.Equal(t, "And then?", got.Questions[0].Question)
	assert.Equal(t, later, got.At)
}

func TestRecordQuestionLeavesEverythingElseAlone(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	for name, tc := range map[string]struct {
		role, input string
	}{
		"another tool":          {"designer", strings.Replace(askedInput, "AskUserQuestion", "Bash", 1)},
		"not json":              {"designer", "not json"},
		"nothing":               {"designer", ""},
		"no session":            {"designer", strings.Replace(askedInput, `"session_id":"sid-designer",`, "", 1)},
		"no questions":          {"designer", `{"session_id":"s","tool_name":"AskUserQuestion","tool_input":{"questions":[]}}`},
		"questions missing":     {"designer", `{"session_id":"s","tool_name":"AskUserQuestion","tool_input":{}}`},
		"no role":               {"", askedInput},
		"a role that is a path": {"../x", askedInput},
		"a role with a slash":   {"a/b", askedInput},
		"a role that is dots":   {"..", askedInput},
	} {
		t.Run(name, func(t *testing.T) {
			l := crew.Layout{Dir: t.TempDir()}

			require.NoError(t, RecordQuestion(l, tc.role, []byte(tc.input), at))
			assert.NoDirExists(t, l.Hooks(), "nothing recorded: not even the directory")
			assert.NoFileExists(t, filepath.Join(filepath.Dir(l.Dir), "x.question.json"))
		})
	}

	t.Run("no crew directory", func(t *testing.T) {
		l := crew.Layout{Dir: filepath.Join(t.TempDir(), "gone")}

		require.Error(t, RecordQuestion(l, "designer", []byte(askedInput), at))
		assert.NoDirExists(t, l.Dir, "no crew is made for it")
	})
}

// The recorded question is the role's dialog while it is the one on screen, whether or not the screen reads: its words
// whole, each choice numbered in the order the tool gave them with its description.
func TestViewShowsTheQuestionItsHookRecorded(t *testing.T) {
	for name, screen := range map[string]string{
		"a screen that does not read": "something Claude Code draws that no parse takes for a question",
		"a screen that does":          askedScreen,
	} {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("designer"))
			f.waitingOn("designer", "input needed")
			f.tm.screen = screen

			require.NoError(t, RecordQuestion(f.s.Crew.Layout(), "designer", []byte(askedInput), *f.now))

			d := dialogOf(t, f, "designer")
			require.NotNil(t, d)
			assert.Equal(t, "Which screen first?", d.Question)
			assert.Equal(t, []DialogOption{{N: 1, Label: "Home", Detail: "the landing page"},
				{N: 2, Label: "Talk", Detail: "the conversation"}}, d.Options)
			assert.Equal(t, 1, d.Cursor)
			assert.NotEmpty(t, d.ID)
		})
	}
}

// What an answer names is the screen's fingerprint, so `choose` keeps working on the screen's dialog: where the screen
// reads as the same question the view carries the screen's id and cursor; where it does not read, the id is one no
// screen has, and nothing can be answered by it.
func TestRecordedQuestionKeepsTheScreensFingerprintForAnAnswer(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.waitingOn("designer", "input needed")
	require.NoError(t, RecordQuestion(f.s.Crew.Layout(), "designer", []byte(askedInput), *f.now))

	f.tm.screen = strings.Replace(askedScreen, "❯ 1. Home", "  1. Home", 1)
	f.tm.screen = strings.Replace(f.tm.screen, "  2. Talk", "❯ 2. Talk", 1)

	onScreen := ParseDialog(f.tm.screen)
	require.NotNil(t, onScreen)
	require.Equal(t, 2, onScreen.Cursor)

	d := dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.Equal(t, onScreen.ID, d.ID, "the id an answer names is the screen's")
	assert.Equal(t, 2, d.Cursor)

	f.tm.screen = "nothing a parse takes for a question"
	alone := dialogOf(t, f, "designer")
	require.NotNil(t, alone)
	assert.NotEqual(t, onScreen.ID, alone.ID)
	assert.Equal(t, d.Options, alone.Options)
}

// A call with several questions is shown by its first, and says there are more: Claude Code asks them one after
// another and a Dialog holds one. The hook speaks once, so once the screen has moved on to the next question the screen
// is shown, not the first one again.
func TestRecordedQuestionsOfSeveralAreShownByTheFirst(t *testing.T) {
	two := strings.Replace(askedInput, `}]}]}}`, `}]},{"question":"Which tabs?","options":[{"label":"Talk"},{"label":"Later"}]}]}}`, 1)

	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.waitingOn("designer", "input needed")
	require.NoError(t, RecordQuestion(f.s.Crew.Layout(), "designer", []byte(two), *f.now))

	f.tm.screen = askedScreen

	d := dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.Equal(t, "Which screen first? (and 1 more after this one: Which tabs?)", d.Question)
	assert.Len(t, d.Options, 2)

	// The owner answered the first: the screen asks the second.
	f.tm.screen = " ☐ Tabs\n│ Which tabs?\n❯ 1. Talk\n  2. Later\n  3. Type something.\n  4. Chat about this\nEnter to select · Esc to cancel"

	d = dialogOf(t, f, "designer")
	require.NotNil(t, d)
	assert.Equal(t, "Tabs Which tabs?", d.Question, "the screen's question, not the first one again")
	assert.Equal(t, []string{"Talk", "Later"}, []string{d.Options[0].Label, d.Options[1].Label})
}

// A record that is not the question on screen is never shown: another session, one older than the wait it would be
// for, a role that is not waiting on a question, a record that does not read. The screen is read as it always was —
// whether it reads as a dialog (the record would have been taken for it: same choices) or not (nothing else shows).
func TestAStaleRecordIsNeverShown(t *testing.T) {
	const unread = "a screen no parse takes for a question"

	// The same choices as the screen, other words: shown if it were taken, so it shows when it is wrongly taken.
	other := strings.Replace(askedInput, "Which screen first?", "Recorded text", 1)

	record := func(f fix, input string, at time.Time) {
		require.NoError(t, RecordQuestion(f.s.Crew.Layout(), "designer", []byte(input), at))
	}

	for name, set := range map[string]func(f fix){
		"another session": func(f fix) {
			record(f, strings.Replace(other, "sid-designer", "sid-before-a-clear", 1), *f.now)
		},
		"written before the wait began": func(f fix) { record(f, other, f.now.Add(-hookSlack-time.Second)) },
		"the role is on a permission prompt": func(f fix) {
			f.waitingOn("designer", "permission prompt")
			record(f, other, *f.now)
		},
		"the file says nothing of what it waits on": func(f fix) {
			f.waitingOn("designer", "")
			record(f, other, *f.now)
		},
		"the role does not wait any more": func(f fix) {
			record(f, other, *f.now)
			f.rg.byPID[f.tm.up["designer"]].Status = "idle"
			f.rg.byPID[f.tm.up["designer"]].WaitingFor = ""
		},
		"the role went on to another turn": func(f fix) {
			record(f, other, *f.now)
			f.rg.byPID[f.tm.up["designer"]].Status = "busy"
		},
		"asked again, the hook not run yet": func(f fix) {
			record(f, other, *f.now)
			*f.now = f.now.Add(time.Minute)
			f.rg.byPID[f.tm.up["designer"]].StatusUpdatedAt = f.now.UnixMilli()
		},
		"the file does not read": func(f fix) {
			require.NoError(t, os.MkdirAll(f.s.Crew.Layout().Hooks(), 0o700))
			require.NoError(t, os.WriteFile(f.s.Crew.Layout().HookQuestion("designer"), []byte(`{"session_id":"sid-des`), 0o600))
		},
		"a record with no question": func(f fix) {
			require.NoError(t, os.MkdirAll(f.s.Crew.Layout().Hooks(), 0o700))
			require.NoError(t, os.WriteFile(f.s.Crew.Layout().HookQuestion("designer"),
				[]byte(`{"session_id":"sid-designer","at":"`+f.now.Format(time.RFC3339)+`","questions":[]}`), 0o600))
		},
		"no file": func(f fix) {},
	} {
		for screen, want := range map[string]*Dialog{unread: nil, askedScreen: ParseDialog(askedScreen)} {
			t.Run(name+" / "+strings.SplitN(screen, "\n", 2)[0], func(t *testing.T) {
				f := fixtureWith(t, crewYAML)
				require.NoError(t, f.s.Up("designer"))
				f.waitingOn("designer", "input needed")
				f.tm.screen = screen

				set(f)
				assert.Equal(t, want, dialogOf(t, f, "designer"), "the screen's own, as before")
			})
		}
	}

	// The same record, current: shown. The cases above fail for the one thing each changes.
	for screen, want := range map[string]string{unread: "Recorded text", askedScreen: "Recorded text"} {
		t.Run("the control: current / "+strings.SplitN(screen, "\n", 2)[0], func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("designer"))
			f.waitingOn("designer", "input needed")
			f.tm.screen = screen

			record(f, other, f.now.Add(-time.Second))

			d := dialogOf(t, f, "designer")
			require.NotNil(t, d)
			assert.Equal(t, want, d.Question)
		})
	}
}

// A role started before the hook existed has no record and no hooks directory: the view reads its screen as before.
func TestNoRecordIsTheScreen(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))
	f.waitingOn("designer", "input needed")
	f.tm.screen = askedScreen

	assert.NoDirExists(t, f.s.Crew.Layout().Hooks())
	assert.Equal(t, ParseDialog(askedScreen), dialogOf(t, f, "designer"))

	f.tm.screen = "nothing"
	assert.Nil(t, dialogOf(t, f, "designer"))
}

// A copy of a role has a record of its own.
func TestRecordOfACopyIsItsOwn(t *testing.T) {
	l := crew.Layout{Dir: t.TempDir()}
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	require.NoError(t, RecordQuestion(l, "designer#2", []byte(askedInput), at))
	assert.FileExists(t, l.HookQuestion("designer#2"))
	assert.NoFileExists(t, l.HookQuestion("designer"))
}

// The PermissionRequest hook is in the settings of a Claude role on its own channel, beside the others: silent, exit 0
// always, a short timeout, a line of its own for each copy; none where no zmina executable is known, and none for a role
// whose lines are typed.
func TestPermissionRequestHookInSettings(t *testing.T) {
	f, _ := nativeFixture(t, crewYAML)

	type settings struct {
		Hooks struct {
			PermissionRequest []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
					Timeout int    `json:"timeout"`
				} `json:"hooks"`
			}
		} `json:"hooks"`
	}

	var got settings
	require.NoError(t, json.Unmarshal([]byte(f.s.roleSettings("designer")), &got))
	assert.Empty(t, got.Hooks.PermissionRequest, "no executable known: no hook")

	f.s.Self = "/opt/zmina"

	require.NoError(t, json.Unmarshal([]byte(f.s.roleSettings("designer")), &got))
	require.Len(t, got.Hooks.PermissionRequest, 1)
	require.Len(t, got.Hooks.PermissionRequest[0].Hooks, 1)
	h := got.Hooks.PermissionRequest[0].Hooks[0]
	assert.Empty(t, got.Hooks.PermissionRequest[0].Matcher, "every tool: the command picks AskUserQuestion out itself")
	assert.Equal(t, "command", h.Type)
	assert.Equal(t, "'/opt/zmina' --crew '"+f.s.Crew.Path+"' _permission-request --role 'designer' >/dev/null 2>&1 || true", h.Command)
	assert.Equal(t, recordTimeout, h.Timeout)

	got = settings{}
	require.NoError(t, json.Unmarshal([]byte(f.s.roleSettings("designer#2")), &got))
	require.Len(t, got.Hooks.PermissionRequest, 1)
	assert.Contains(t, got.Hooks.PermissionRequest[0].Hooks[0].Command, "_permission-request --role 'designer#2'")

	// A role typed to starts with no hooks of zmina's at all.
	plain := fixtureWith(t, crewYAML)
	plain.s.Self = "/opt/zmina"
	argv := strings.Join(plain.s.Command(plain.s.Crew.Roles["harvester"], ""), " ")
	assert.NotContains(t, argv, "_permission-request")
}
