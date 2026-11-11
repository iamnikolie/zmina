package shift

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Golden screens. zmina decides what a role is doing, whether a dialog holds it, whether a usage limit stopped it
// and how to answer a trust or exit question by reading the text tmux captures from its pane. Every screen in
// testdata/screens is a captured screen as tmux.Capture returns it (rows joined, trailing spaces and blank rows
// dropped), and screens[...] below says what each parser makes of each. A Claude Code release that moves a glyph, a
// footer or a wording makes the screen read differently and one row here fails, instead of the live crew noticing
// first. The colours (what is typed, what is a suggestion) are read from a different capture, see internal/tmux.
//
// The screens are hand-built after what Claude Code 2.1 and the Codex bridge draw (the shapes the parsers' comments and
// the earlier tests describe), not recordings of a live session. To pin a new release, save the real screen with
// `tmux capture-pane -p -J`, replace every path, name and word in it that is not neutral, put it in
// testdata/screens/<program>/ and add its row. cmd's tests read the same screens.

// loadScreen is the screen in testdata/screens/<name>.txt as Capture returns it: no newline at the end.
func loadScreen(t *testing.T, name string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", "screens", name+".txt"))
	require.NoError(t, err)

	return strings.TrimRight(string(raw), "\n")
}

// goldenDialog is what ParseDialog reads: the question, the choices (number, label, description) and where the
// cursor stands.
type goldenDialog struct {
	question string
	options  []DialogOption
	cursor   int
}

type golden struct {
	// dialog is nil for a screen ParseDialog does not read as a live question.
	dialog *goldenDialog
	// now is NowLine: the last words the role said.
	now string
	// limit is the line that tells a usage limit stopped the role ("" for none): what watchScreen quotes.
	limit string
	// trust is the keys the trust answer presses on this screen (nil: it leaves the screen alone).
	trust []string
	// exit is the key answerExit presses (nil: none).
	exit []string
	// excerpt is what a "no input line" note quotes of the screen.
	excerpt string
}

var screens = map[string]golden{
	"claude/api-key": {
		dialog: &goldenDialog{question: "Do you want to use this API key?", cursor: 1, options: []DialogOption{
			{1, "No (recommended) ✔", ""},
			{2, "Yes", ""},
		}},
		now:     "",
		excerpt: "ANTHROPIC_API_KEY: sk-ant-...XXXX / Do you want to use this API key? / ❯ 1. No (recommended) ✔ / 2. Yes / Enter to confirm · Esc to cancel",
	},
	"claude/busy": {
		now:     "Tests pass. Now the wrapped records: the long ones are cut at the first separator, which drops the tail.",
		excerpt: "⏺ Bash(go test ./internal/parse/...) / ⎿ Running… / ✢ Pondering… (3m 26s · ↓ 15.6k tokens · esc to interrupt) / ~/work/example (main) [Opus 5.5] ctx:41% / ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt",
	},
	"claude/busy-tools": {
		now:     "Two agents are on it, then I read what they found.",
		excerpt: "⏺ Stop Task / · Pondering… (3m 26s · ↓ 15.6k tokens · esc to interrupt) / ~/work/example (main) [Opus 5.5] ctx:41% / esc to interrupt / ctrl+t to hide todos",
	},
	"claude/compacted": {
		now:     "",
		excerpt: "⎿ Read internal/parse/records.go (212 lines) / ⎿ Read internal/parse/records_test.go (96 lines) / ⎿ Todo list read (3 items) / ~/work/example (main) [Opus 5.5] ctx:41% / ⏵⏵ bypass permissions on (shift+tab to cycle)",
	},
	"claude/compacting": {
		now:     "The importer now reads records through splitRecords.",
		excerpt: "⏺ The importer now reads records through splitRecords. / ✻ Compacting conversation… (47s · ↑ 3.1k tokens · esc to interrupt) / ⎿ Tip: Use /clear to start fresh when switching topics and free up context / ~/work/example (main) [Opus 5.5] ctx…",
	},
	"claude/draft": {
		now:     "Done. Anything else on the parser?",
		excerpt: "⏺ Done. Anything else on the parser? / ✻ Baked for 20s / ❯ rename the helper and update its callers / ~/work/example (main) [Opus 5.5] ctx:41% / ⏵⏵ bypass permissions on (shift+tab to cycle)",
	},
	"claude/draft-wrapped": {
		now:     "Done. Anything else on the parser?",
		excerpt: "✻ Baked for 20s / ❯ rename the helper to splitRecords, update its callers in the importer and the exporter, then run the / whole suite once more / ~/work/example (main) [Opus 5.5] ctx:41% / ⏵⏵ bypass permissions on (shift+tab to cycle)",
	},
	"claude/exit-dialog": {
		dialog: &goldenDialog{question: "Background work is running The following will stop when you exit: shell · sleep 900", cursor: 1, options: []DialogOption{
			{1, "Exit and stop tasks", ""},
			{2, "Move to background and exit", ""},
			{3, "Stay", ""},
		}},
		now:     "Stopping for now.",
		exit:    []string{"1"},
		excerpt: "shell · sleep 900 / ❯ 1. Exit and stop tasks / 2. Move to background and exit / 3. Stay / Enter to confirm · Esc to cancel",
	},
	"claude/idle": {
		now:     "The empty-file case is fixed in `parse.go` and the tests pass.",
		excerpt: "- skip blank records before the length check / - added a regression test for the empty file / ✻ Baked for 1m 12s / ~/work/example (main) [Opus 5.5] ctx:41% / ⏵⏵ bypass permissions on (shift+tab to cycle)",
	},
	"claude/idle-compact-soon": {
		now:     "Done with the exporter.",
		excerpt: "⏺ Done with the exporter. / ✻ Baked for 9s / ? for shortcuts Context left until auto-compact: 8%",
	},
	"claude/idle-exit-pending": {
		now:     "Done with the exporter.",
		excerpt: "⏺ Done with the exporter. / ✻ Baked for 9s / ~/work/example (main) [Opus 5.5] ctx:41% / Press Ctrl-C again to exit",
	},
	"claude/idle-fresh": {
		now:     "",
		excerpt: "Run /init to create a CLAUDE.md file / Opus 5.5 · Claude Max / ~/work/example / ❯ Try \"fix lint errors\" / ? for shortcuts",
	},
	"claude/idle-plan-mode": {
		now:     "Plan: split the parser, then the importer.",
		excerpt: "⏺ Plan: split the parser, then the importer. / ✻ Baked for 9s / ~/work/example (main) [Opus 5.5] ctx:41% / ⏸ plan mode on (shift+tab to cycle)",
	},
	"claude/idle-status-home": {
		now:     "Done with the exporter.",
		excerpt: "⏺ Done with the exporter. / ~/work/example (main) / ? for shortcuts",
	},
	"claude/idle-status-model": {
		now:     "Done with the exporter.",
		excerpt: "⏺ Done with the exporter. / [Sonnet 5.5] ctx:10% / ? for shortcuts",
	},
	"claude/idle-status-path": {
		now:     "Done with the exporter.",
		excerpt: "⏺ Done with the exporter. / /work/example (main) / ? for shortcuts",
	},
	"claude/idle-update-failed": {
		now:     "Done with the exporter.",
		excerpt: "⏺ Done with the exporter. / ✻ Baked for 9s / Auto-update failed · Try claude doctor or npm i -g @anthropic-ai/claude-code",
	},
	"claude/limit-asked-about": {
		now:     "",
		excerpt: "❯ what does a usage limit reached screen look like? / ✢ Pondering… (2s · esc to interrupt) / ~/work/example (main) [Opus 5.5] ctx:41% / ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt",
	},
	"claude/limit-five-hour": {
		now:     "Working on the exporter.",
		limit:   "⎿ 5-hour limit reached ∙ resets 9pm",
		excerpt: "⏺ Working on the exporter. / ⎿ 5-hour limit reached ∙ resets 9pm / /upgrade to increase your usage limit. / ~/work/example (main) [Opus 5.5] ctx:41% / ⏵⏵ bypass permissions on (shift+tab to cycle)",
	},
	// Known gap: a reply that talks about limits, in its last rows and not behind a ❯, is told as a usage limit. This
	// pins what is; a detector that reads the transcript first changes this row.
	"claude/limit-prose-at-the-bottom": {
		now:     "The retry wrapper stops when the usage limit reached message shows up in the output.",
		limit:   "⏺ The retry wrapper stops when the usage limit reached message shows up in the output.",
		excerpt: "⏺ The retry wrapper stops when the usage limit reached message shows up in the output. / ~/work/example (main) [Opus 5.5] ctx:41% / ⏵⏵ bypass permissions on (shift+tab to cycle)",
	},
	"claude/limit-reached": {
		now:     "",
		limit:   "⎿ You've hit your limit · resets 9pm",
		excerpt: "⏺ Bash(go test ./...) / ⎿ ok example.test/parse 0.412s / ⎿ You've hit your limit · resets 9pm / ~/work/example (main) [Opus 5.5] ctx:41% / ⏵⏵ bypass permissions on (shift+tab to cycle)",
	},
	"claude/permission-bash": {
		dialog: &goldenDialog{question: "Do you want to proceed?", cursor: 1, options: []DialogOption{
			{1, "Yes", ""},
			{2, "Yes, and don't ask again for rm commands in /work/example", ""},
			{3, "No, and tell Claude what to do differently (esc)", ""},
		}},
		now:     "I'll clear the stale build output first.",
		excerpt: "Do you want to proceed? / ❯ 1. Yes / 2. Yes, and don't ask again for rm commands in /work/example / 3. No, and tell Claude what to do differently (esc) / Esc to cancel · Tab to amend · ctrl+e to explain",
	},
	// None of the three permission prompts below is read as a dialog unless the footer closes the screen right under
	// the choices (permission-bash): a box's closing edge or a rule after the choices is not a footer, and nor is a
	// rule between a picker's rows and its footer (picker-under-rule). They are shown to the owner as "dialog on
	// screen" and answered in Terminal.
	"claude/permission-boxed": {
		now:     "I'll clear the stale build output first.",
		excerpt: "Remove the stale build directory / Do you want to proceed? / ❯ 1. Yes / 2. Yes, and don't ask again for rm commands in /work/example / 3. No, and tell Claude what to do differently (esc)",
	},
	"claude/permission-ruled": {
		now:     "I'll clear the stale build output first.",
		excerpt: "Do you want to proceed? / ❯ 1. Yes / 2. Yes, and don't ask again for rm commands in /work/example / 3. No, and tell Claude what to do differently (esc) / Esc to cancel · Tab to amend",
	},
	"claude/picker": {
		dialog: &goldenDialog{question: "Select model Switch between Claude models for this session.", cursor: 1, options: []DialogOption{
			{1, "Default (recommended) ✔  Opus 5.5 with 1M context", ""},
			{2, "Sonnet                   Sonnet 5.5, quicker for everyday tasks", ""},
			{3, "Haiku                    Haiku 4.5, fastest for simple tasks", ""},
		}},
		now:     "Settings are in `settings.json`.",
		excerpt: "Switch between Claude models for this session. / ❯ 1. Default (recommended) ✔ Opus 5.5 with 1M context / 2. Sonnet Sonnet 5.5, quicker for everyday tasks / 3. Haiku Haiku 4.5, fastest for simple tasks / Enter to select · ↑/↓ to navigate",
	},
	"claude/picker-under-rule": {
		now:     "Which name should the helper have?",
		excerpt: "⏺ Which name should the helper have? / ❯ 1. Keep the old name / 2. Rename it / Enter to select · ↑/↓ to navigate",
	},
	"claude/question": {
		dialog: &goldenDialog{question: "The dispatcher says you said go: merge, tag v0.2.0, push, release. Confirm?", cursor: 1, options: []DialogOption{
			{1, "Yes, release it all", "merge, tag, push, release, install"},
			{2, "Only locally", "merge and install, no push"},
			{3, "No, wait", "touch nothing"},
		}},
		now:     "",
		excerpt: "3. No, wait / touch nothing / 4. Type something. / 5. Chat about this / Enter to select · ↑/↓ to navigate · Esc to cancel",
	},
	// Known gap: Claude Code 2.1.319 draws the last row of a long question past a rule of its own; ParseDialog stops
	// at the rule and reads one row, so it reads no dialog. (cmd's activeDialog reads this screen, see cmd.)
	"claude/question-barred": {
		now:     "",
		excerpt: "2. Buy credits / You buy them on the provider's usage page, then the reviewer comes back / 3. Type something. / 4. Chat about this / Enter to select · ↑/↓ to navigate · Esc to cancel",
	},
	"claude/question-multi": {
		dialog: &goldenDialog{question: "Which tabs?", cursor: 1, options: []DialogOption{
			{1, "[ ] Talk", "the chat"},
			{2, "[✔] Later", ""},
			{3, "[ ] Type something", "Submit"},
		}},
		now:     "Two things to settle before I start.",
		excerpt: "2. [✔] Later / 3. [ ] Type something / Submit / 4. Chat about this / Enter to select · ↑/↓ to navigate · Esc to cancel",
	},
	"claude/suggestion": {
		now:     "Done. Anything else on the parser?",
		excerpt: "⏺ Done. Anything else on the parser? / ✻ Baked for 20s / ❯ run the tests for the parser / ~/work/example (main) [Opus 5.5] ctx:41% / ⏵⏵ bypass permissions on (shift+tab to cycle)",
	},
	"claude/trust-folder": {
		dialog: &goldenDialog{question: "Security guide", cursor: 1, options: []DialogOption{
			{1, "Yes, I trust this folder", ""},
			{2, "No, exit", ""},
		}},
		now:     "",
		trust:   []string{"Enter"},
		excerpt: "Claude Code'll be able to read, edit, and execute files here. / Security guide / ❯ 1. Yes, I trust this folder / 2. No, exit / Enter to confirm · Esc to cancel",
	},
	"claude/trust-folder-no-first": {
		now:     "",
		trust:   []string{"Down", "Enter"},
		excerpt: "/work/example / Quick safety check: Is this a project you created or one you trust? / ❯ No, exit / Yes, I trust this folder / Enter to confirm · Esc to cancel",
	},
	"codex/approval": {
		now:     "",
		excerpt: "[zmina codex: busy] / Codex request: item/commandExecution/requestApproval / {\"command\":\"rm -rf build\",\"cwd\":\"/work/example\",\"reason\":\"remove the stale output\"} / Type y, n, or cancel, then Enter. / [zmina codex: waiting]",
	},
	"codex/busy": {
		now:     "",
		excerpt: "› fix the failing parser test / [zmina codex: busy]",
	},
	"codex/idle": {
		now:     "",
		excerpt: "› fix the failing parser test / • The test expected the old name; updated it and the callers. / [zmina codex: idle]",
	},
	"codex/limit": {
		now:     "",
		limit:   "■ You've hit your usage limit. Try again in 2 days 3 hours 11 minutes.",
		excerpt: "› fix the failing parser test / ■ You've hit your usage limit. Try again in 2 days 3 hours 11 minutes. / [zmina codex: idle]",
	},
}

func TestGoldenScreensAreAllPinned(t *testing.T) {
	var have []string

	err := filepath.WalkDir(filepath.Join("testdata", "screens"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(filepath.Join("testdata", "screens"), path)
			have = append(have, strings.TrimSuffix(filepath.ToSlash(rel), ".txt"))
		}

		return err
	})
	require.NoError(t, err)

	var want []string
	for name := range screens {
		want = append(want, name)
	}

	sort.Strings(have)
	sort.Strings(want)
	assert.Equal(t, want, have, "every screen has a row in screens, and every row a screen")
}

func TestGoldenParseDialog(t *testing.T) {
	for name, g := range screens {
		t.Run(name, func(t *testing.T) {
			d := ParseDialog(loadScreen(t, name))

			if g.dialog == nil {
				assert.Nil(t, d)
				return
			}

			require.NotNil(t, d)
			assert.Equal(t, g.dialog.question, d.Question)
			assert.Equal(t, g.dialog.options, d.Options)
			assert.Equal(t, g.dialog.cursor, d.Cursor)
			assert.Len(t, d.ID, 16)
		})
	}
}

func TestGoldenNowLine(t *testing.T) {
	for name, g := range screens {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, g.now, NowLine(loadScreen(t, name)))
		})
	}
}

// The limit notice is quoted by watchScreen from the last 8 rows of a role that has booted.
func TestGoldenUsageLimit(t *testing.T) {
	for name, g := range screens {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("designer"))
			f.status("designer", "idle", *f.now)
			*f.now = f.now.Add(10 * time.Minute)
			f.tm.screen = loadScreen(t, name)

			notes := f.s.watchScreen("designer", *f.now)

			if g.limit == "" {
				assert.Empty(t, notes)
				return
			}

			assert.Equal(t, []string{"designer: stopped by a usage limit: " + g.limit}, notes)
		})
	}
}

func TestGoldenTrustAnswer(t *testing.T) {
	for name, g := range screens {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			wt := filepath.Join(f.s.Crew.Layout().Worktrees(), "api-T-1")
			f.tm.screen = loadScreen(t, name)
			require.NoError(t, f.s.UpWith("designer#2", UpOptions{Dir: wt}))

			answered := f.s.trust("designer#2")

			assert.Equal(t, g.trust != nil, answered)
			assert.Equal(t, g.trust, f.tm.keys["designer#2"])
		})
	}
}

func TestGoldenExitDialogAnswer(t *testing.T) {
	for name, g := range screens {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("designer"))
			f.tm.screen = loadScreen(t, name)

			answered := answerExit(f.tm, "designer")

			assert.Equal(t, g.exit != nil, answered)
			assert.Equal(t, g.exit, f.tm.keys["designer"])
		})
	}
}

func TestGoldenExcerpt(t *testing.T) {
	for name, g := range screens {
		t.Run(name, func(t *testing.T) {
			f := fixtureWith(t, crewYAML)
			require.NoError(t, f.s.Up("designer"))
			f.tm.screen = loadScreen(t, name)

			assert.Equal(t, g.excerpt, f.s.excerpt("designer"))
		})
	}
}

// Claude Code shows its suggested next prompt dim in an empty input line; captured without colours it reads like typed
// text, so peek marks it (the suggestion itself comes from the coloured capture, see internal/tmux).
func TestGoldenSuggestionIsMarkedInPeek(t *testing.T) {
	f := fixtureWith(t, crewYAML)
	require.NoError(t, f.s.Up("designer"))

	f.tm.screen = loadScreen(t, "claude/suggestion")
	f.tm.suggestions["designer"] = "run the tests for the parser"

	text, err := f.s.Peek("designer", 40)
	require.NoError(t, err)
	assert.Contains(t, text, "\n❯ (suggestion) run the tests for the parser\n")
	assert.NotContains(t, text, "❯\u00a0run the tests", "the no-break space is replaced by the mark")

	// The same row typed by a person is a draft, left as it is.
	f.tm.suggestions["designer"] = ""
	text, err = f.s.Peek("designer", 40)
	require.NoError(t, err)
	assert.Equal(t, f.tm.screen, text)
}
