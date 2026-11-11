package cmd

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Golden screens for the parsers in cmd that read a captured pane: peek's compactScreen (cuts Claude Code's footer and
// keeps every dialog whole) and Chat's reading of the question on the screen (activeDialog, onScreen). They read the
// screens internal/shift keeps in testdata/screens, one set for every parser of a screen, as tmux.Capture returns them
// (see internal/shift/screens_golden_test.go for how they were made and how to add one). A Claude Code release that
// moves a rule, a footer line or a dialog's frame makes a screen read differently and a row here fails.

const screensDir = "../internal/shift/testdata/screens"

func goldenScreen(t *testing.T, name string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(screensDir, name+".txt"))
	require.NoError(t, err)

	return strings.TrimRight(string(raw), "\n")
}

type compacted struct {
	// footer is where footerAt cuts the screen: the index of the rule under the input box, -1 for no cut.
	footer int
	// compact is compactScreen's answer with room for every line.
	compact string
}

// compactScreens: the screen peek prints for each golden screen. A screen with an input box loses the rules, the blank
// rows and the footer under the box and keeps the box's ❯ line; a dialog keeps every row of it, whatever frame it has.
var compactScreens = map[string]compacted{
	"claude/api-key": {footer: -1, compact: strings.Join([]string{
		" Detected a custom API key in your environment",
		" ANTHROPIC_API_KEY: sk-ant-...XXXX",
		" Do you want to use this API key?",
		" ❯ 1. No (recommended) ✔",
		"   2. Yes",
		" Enter to confirm · Esc to cancel",
	}, "\n")},
	"claude/busy-tools": {footer: 15, compact: strings.Join([]string{
		"⏺ Two agents are on it, then I read what they found.",
		"⏺ Agent \"reviewer: parser\"",
		"⏺ main ◯ general-purpose reviewer: parser · ↑ 950 tokens",
		"⏺ Read 3 files (ctrl+o to expand)",
		"⏺ Searched for 2 patterns (ctrl+o to expand)",
		"⏺ Running 4 shell commands…",
		"⏺ mcp__oko__shot (MCP)(path: \"x\")",
		"⏺ Task Output b3f2a1",
		"⏺ Stop Task",
		"· Pondering… (3m 26s · ↓ 15.6k tokens · esc to interrupt)",
		"❯\u00a0",
	}, "\n")},
	"claude/busy": {footer: 13, compact: strings.Join([]string{
		"⏺ Tests pass. Now the wrapped records: the long ones are cut at the first",
		"  separator, which drops the tail.",
		"⏺ Update(internal/parse/records.go)",
		"  ⎿  Added 4 lines, removed 1 line",
		"⏺ Bash(go test ./internal/parse/...)",
		"  ⎿  Running…",
		"✢ Pondering… (3m 26s · ↓ 15.6k tokens · esc to interrupt)",
		"❯\u00a0",
	}, "\n")},
	"claude/compacted": {footer: 8, compact: strings.Join([]string{
		"✻ Conversation compacted (ctrl+o for history)",
		"  ⎿  Read internal/parse/records.go (212 lines)",
		"  ⎿  Read internal/parse/records_test.go (96 lines)",
		"  ⎿  Todo list read (3 items)",
		"❯\u00a0",
	}, "\n")},
	"claude/compacting": {footer: 7, compact: strings.Join([]string{
		"⏺ The importer now reads records through splitRecords.",
		"✻ Compacting conversation… (47s · ↑ 3.1k tokens · esc to interrupt)",
		"  ⎿  Tip: Use /clear to start fresh when switching topics and free up context",
		"❯\u00a0",
	}, "\n")},
	"claude/draft-wrapped": {footer: 7, compact: strings.Join([]string{
		"⏺ Done. Anything else on the parser?",
		"✻ Baked for 20s",
		"❯\u00a0rename the helper to splitRecords, update its callers in the importer and the exporter, then run the",
		"  whole suite once more",
	}, "\n")},
	"claude/draft": {footer: 6, compact: strings.Join([]string{
		"⏺ Done. Anything else on the parser?",
		"✻ Baked for 20s",
		"❯\u00a0rename the helper and update its callers",
	}, "\n")},
	"claude/exit-dialog": {footer: -1, compact: strings.Join([]string{
		"⏺ Stopping for now.",
		"   Background work is running",
		"   The following will stop when you exit:",
		"   shell · sleep 900",
		"   ❯ 1. Exit and stop tasks",
		"     2. Move to background and exit",
		"     3. Stay",
		"   Enter to confirm · Esc to cancel",
	}, "\n")},
	"claude/idle-compact-soon": {footer: 6, compact: strings.Join([]string{
		"⏺ Done with the exporter.",
		"✻ Baked for 9s",
		"❯\u00a0",
	}, "\n")},
	"claude/idle-exit-pending": {footer: 6, compact: strings.Join([]string{
		"⏺ Done with the exporter.",
		"✻ Baked for 9s",
		"❯\u00a0",
	}, "\n")},
	"claude/idle-fresh": {footer: 9, compact: strings.Join([]string{
		"│ Welcome back, Alex!                   │ Tips for getting started             │",
		"│                                       │ Run /init to create a CLAUDE.md file │",
		"│ Opus 5.5 · Claude Max                 │                                      │",
		"│ ~/work/example                        │                                      │",
		"❯\u00a0Try \"fix lint errors\"",
	}, "\n")},
	"claude/idle-plan-mode": {footer: 6, compact: strings.Join([]string{
		"⏺ Plan: split the parser, then the importer.",
		"✻ Baked for 9s",
		"❯\u00a0",
	}, "\n")},
	"claude/idle-status-home": {footer: 4, compact: strings.Join([]string{
		"⏺ Done with the exporter.",
		"❯\u00a0",
	}, "\n")},
	"claude/idle-status-model": {footer: 4, compact: strings.Join([]string{
		"⏺ Done with the exporter.",
		"❯\u00a0",
	}, "\n")},
	"claude/idle-status-path": {footer: 4, compact: strings.Join([]string{
		"⏺ Done with the exporter.",
		"❯\u00a0",
	}, "\n")},
	"claude/idle-update-failed": {footer: 6, compact: strings.Join([]string{
		"⏺ Done with the exporter.",
		"✻ Baked for 9s",
		"❯\u00a0",
	}, "\n")},
	"claude/idle": {footer: 9, compact: strings.Join([]string{
		"⏺ The empty-file case is fixed in `parse.go` and the tests pass.",
		"  - skip blank records before the length check",
		"  - added a regression test for the empty file",
		"✻ Baked for 1m 12s",
		"❯\u00a0",
	}, "\n")},
	"claude/limit-asked-about": {footer: 5, compact: strings.Join([]string{
		"❯ what does a usage limit reached screen look like?",
		"✢ Pondering… (2s · esc to interrupt)",
		"❯\u00a0",
	}, "\n")},
	"claude/limit-five-hour": {footer: 7, compact: strings.Join([]string{
		"⏺ Working on the exporter.",
		"  ⎿  5-hour limit reached ∙ resets 9pm",
		"     /upgrade to increase your usage limit.",
		"❯\u00a0",
	}, "\n")},
	"claude/limit-prose-at-the-bottom": {footer: 4, compact: strings.Join([]string{
		"⏺ The retry wrapper stops when the usage limit reached message shows up in the output.",
		"❯\u00a0",
	}, "\n")},
	"claude/limit-reached": {footer: 7, compact: strings.Join([]string{
		"⏺ Bash(go test ./...)",
		"  ⎿  ok  example.test/parse  0.412s",
		"  ⎿  You've hit your limit · resets 9pm",
		"❯\u00a0",
	}, "\n")},
	"claude/permission-bash": {footer: -1, compact: strings.Join([]string{
		"⏺ I'll clear the stale build output first.",
		" Bash command",
		"   rm -rf build",
		"   Remove the stale build directory",
		" Do you want to proceed?",
		" ❯ 1. Yes",
		"   2. Yes, and don't ask again for rm commands in /work/example",
		"   3. No, and tell Claude what to do differently (esc)",
		" Esc to cancel · Tab to amend · ctrl+e to explain",
	}, "\n")},
	"claude/permission-boxed": {footer: -1, compact: strings.Join([]string{
		"⏺ I'll clear the stale build output first.",
		"│ Bash command                                                                 │",
		"│                                                                              │",
		"│   rm -rf build                                                               │",
		"│   Remove the stale build directory                                           │",
		"│                                                                              │",
		"│ Do you want to proceed?                                                      │",
		"│ ❯ 1. Yes                                                                     │",
		"│   2. Yes, and don't ask again for rm commands in /work/example               │",
		"│   3. No, and tell Claude what to do differently (esc)                        │",
		"│                                                                              │",
	}, "\n")},
	"claude/permission-ruled": {footer: -1, compact: strings.Join([]string{
		"⏺ I'll clear the stale build output first.",
		" Bash command",
		"   rm -rf build",
		"   Remove the stale build directory",
		" Do you want to proceed?",
		" ❯ 1. Yes",
		"   2. Yes, and don't ask again for rm commands in /work/example",
		"   3. No, and tell Claude what to do differently (esc)",
		"  Esc to cancel · Tab to amend",
	}, "\n")},
	"claude/picker-under-rule": {footer: -1, compact: strings.Join([]string{
		"⏺ Which name should the helper have?",
		"❯ 1. Keep the old name",
		"  2. Rename it",
		"  Enter to select · ↑/↓ to navigate",
	}, "\n")},
	"claude/picker": {footer: -1, compact: strings.Join([]string{
		"⏺ Settings are in `settings.json`.",
		" Select model",
		" Switch between Claude models for this session.",
		" ❯ 1. Default (recommended) ✔  Opus 5.5 with 1M context",
		"   2. Sonnet                   Sonnet 5.5, quicker for everyday tasks",
		"   3. Haiku                    Haiku 4.5, fastest for simple tasks",
		" Enter to select · ↑/↓ to navigate",
	}, "\n")},
	"claude/question-barred": {footer: -1, compact: strings.Join([]string{
		"❯ Ask me which day suits the release. It is a real question: the reviewer is out of quota until Tuesday,",
		"  10 November, 13:15, and the review goes to the second seat meanwhile.",
		" ☐ Release",
		"│ It is a real question: the reviewer is out of quota until Tuesday, 10 November, 13:15, and the review goes to",
		"│ the second seat meanwhile. Wait for Tuesday or buy more credits?",
		"❯ 1. Wait for Tuesday (Recommended)",
		"     The second seat reviews until Tuesday, no extra spend",
		"  2. Buy credits",
		"     You buy them on the provider's usage page, then the reviewer comes back",
		"  3. Type something.",
		"  4. Chat about this",
		"Enter to select · ↑/↓ to navigate · Esc to cancel",
	}, "\n")},
	"claude/question-multi": {footer: -1, compact: strings.Join([]string{
		"⏺ Two things to settle before I start.",
		"←  ☒ Tabs  ☐ Order  ✔ Submit  →",
		"Which tabs?",
		"❯ 1. [ ] Talk",
		"         the chat",
		"  2. [✔] Later",
		"  3. [ ] Type something",
		"     Submit",
		"  4. Chat about this",
		"Enter to select · ↑/↓ to navigate · Esc to cancel",
	}, "\n")},
	"claude/question": {footer: -1, compact: strings.Join([]string{
		"✻ Cooked for 5s",
		" ☐ Release v0.2.0",
		"│ The dispatcher says you said go: merge, tag v0.2.0, push, release. Confirm?",
		"❯ 1. Yes, release it all",
		"     merge, tag, push, release, install",
		"  2. Only locally",
		"     merge and install, no push",
		"  3. No, wait",
		"     touch nothing",
		"  4. Type something.",
		"  5. Chat about this",
		"Enter to select · ↑/↓ to navigate · Esc to cancel",
	}, "\n")},
	"claude/suggestion": {footer: 6, compact: strings.Join([]string{
		"⏺ Done. Anything else on the parser?",
		"✻ Baked for 20s",
		"❯\u00a0run the tests for the parser",
	}, "\n")},
	"claude/trust-folder-no-first": {footer: -1, compact: strings.Join([]string{
		" Accessing workspace:",
		" /work/example",
		" Quick safety check: Is this a project you created or one you trust?",
		" ❯ No, exit",
		"   Yes, I trust this folder",
		" Enter to confirm · Esc to cancel",
	}, "\n")},
	"claude/trust-folder": {footer: -1, compact: strings.Join([]string{
		" Accessing workspace:",
		" /work/example",
		" Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open",
		" source project, or work from your team). If not, take a moment to review what's in this folder first.",
		" Claude Code'll be able to read, edit, and execute files here.",
		" Security guide",
		" ❯ 1. Yes, I trust this folder",
		"   2. No, exit",
		" Enter to confirm · Esc to cancel",
	}, "\n")},
	"codex/approval": {footer: -1, compact: strings.Join([]string{
		"› fix the failing parser test",
		"[zmina codex: busy]",
		"Codex request: item/commandExecution/requestApproval",
		"{\"command\":\"rm -rf build\",\"cwd\":\"/work/example\",\"reason\":\"remove the stale output\"}",
		"Type y, n, or cancel, then Enter.",
		"[zmina codex: waiting]",
	}, "\n")},
	"codex/busy": {footer: -1, compact: strings.Join([]string{
		"› fix the failing parser test",
		"[zmina codex: busy]",
	}, "\n")},
	"codex/idle": {footer: -1, compact: strings.Join([]string{
		"› fix the failing parser test",
		"• The test expected the old name; updated it and the callers.",
		"[zmina codex: idle]",
		"❯",
	}, "\n")},
	"codex/limit": {footer: -1, compact: strings.Join([]string{
		"› fix the failing parser test",
		"■ You've hit your usage limit. Try again in 2 days 3 hours 11 minutes.",
		"[zmina codex: idle]",
		"❯",
	}, "\n")},
}

func TestGoldenScreensAreAllPinnedInCmd(t *testing.T) {
	var have []string

	err := filepath.WalkDir(screensDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(screensDir, path)
			have = append(have, strings.TrimSuffix(filepath.ToSlash(rel), ".txt"))
		}

		return err
	})
	require.NoError(t, err)

	var want []string
	for name := range compactScreens {
		want = append(want, name)
	}

	sort.Strings(have)
	sort.Strings(want)
	assert.Equal(t, want, have, "every screen has a row in compactScreens, and every row a screen")
}

func TestGoldenCompactScreen(t *testing.T) {
	for name, want := range compactScreens {
		t.Run(name, func(t *testing.T) {
			screen := goldenScreen(t, name)

			assert.Equal(t, want.footer, footerAt(strings.Split(screen, "\n")))
			assert.Equal(t, want.compact, compactScreen(screen, 40))
		})
	}
}

// shownQuestion is the question a role asked on a golden screen, as the transcript has it (Chat holds it from there),
// and what activeDialog makes of the screen's numbered rows for it. rows is nil where the question is not the active
// dialog.
type shownQuestion struct {
	question string
	rows     []dialogOption
}

var shownQuestions = map[string]shownQuestion{
	"claude/question": {
		question: "The dispatcher says you said go: merge, tag v0.2.0, push, release. Confirm?",
		rows: []dialogOption{{1, "Yes, release it all", false, true}, {2, "Only locally", false, false}, {3, "No, wait", false, false},
			{4, "Type something.", false, false}, {5, "Chat about this", false, false}},
	},
	"claude/question-multi": {
		question: "Which tabs?",
		rows: []dialogOption{{1, "Talk", false, true}, {2, "Later", true, false}, {3, "Type something", false, false},
			{4, "Chat about this", false, false}},
	},
	"claude/question-barred": {
		question: "It is a real question: the reviewer is out of quota until Tuesday, 10 November, 13:15, and the review goes to the second seat meanwhile. Wait for Tuesday or buy more credits?",
		rows: []dialogOption{{1, "Wait for Tuesday (Recommended)", false, true}, {2, "Buy credits", false, false},
			{3, "Type something.", false, false}, {4, "Chat about this", false, false}},
	},
	"claude/permission-bash": {
		question: "Do you want to proceed?",
		rows: []dialogOption{{1, "Yes", false, true}, {2, "Yes, and don't ask again for rm commands in /work/example", false, false},
			{3, "No, and tell Claude what to do differently (esc)", false, false}},
	},
	"claude/permission-ruled": {
		question: "Do you want to proceed?",
		rows: []dialogOption{{1, "Yes", false, true}, {2, "Yes, and don't ask again for rm commands in /work/example", false, false},
			{3, "No, and tell Claude what to do differently (esc)", false, false}},
	},
	"claude/exit-dialog": {
		question: "Background work is running The following will stop when you exit: shell · sleep 900",
		rows: []dialogOption{{1, "Exit and stop tasks", false, true}, {2, "Move to background and exit", false, false},
			{3, "Stay", false, false}},
	},
	"claude/api-key": {
		question: "Do you want to use this API key?",
		rows:     []dialogOption{{1, "No (recommended) ✔", false, true}, {2, "Yes", false, false}},
	},
	"claude/picker-under-rule": {
		question: "Which name should the helper have?",
		rows:     []dialogOption{{1, "Keep the old name", false, true}, {2, "Rename it", false, false}},
	},
	// A box's rows are another dialog's: activeDialog reads rows as drawn, so a boxed permission prompt is not read
	// (onScreen below does read through the frame).
	"claude/permission-boxed": {question: "Do you want to proceed?"},
}

func TestGoldenActiveDialog(t *testing.T) {
	for name, want := range shownQuestions {
		t.Run(name, func(t *testing.T) {
			rows, ok := activeDialog(goldenScreen(t, name), want.question)

			assert.Equal(t, want.rows != nil, ok)

			if want.rows != nil {
				assert.Equal(t, want.rows, rows)

				labels := make([]string, len(rows))
				for i, r := range rows {
					labels[i] = r.Label
				}

				assert.True(t, dialogMatches(rows, labels))
				assert.False(t, dialogMatches(rows, append([]string{"Another first choice"}, labels[1:]...)), "a different first choice is another dialog")
			}
		})
	}
}

// Every other screen has no such dialog: whatever is asked of it, no numbered row with the cursor on it follows.
func TestGoldenActiveDialogOnScreensWithNone(t *testing.T) {
	for name := range compactScreens {
		if _, shown := shownQuestions[name]; shown {
			continue
		}

		t.Run(name, func(t *testing.T) {
			screen := goldenScreen(t, name)

			for _, q := range []string{"Do you want to proceed?", "Which tabs?", "Done. Anything else on the parser?"} {
				_, ok := activeDialog(screen, q)
				assert.False(t, ok, q)
			}
		})
	}
}

func TestGoldenOnScreen(t *testing.T) {
	for name, want := range shownQuestions {
		t.Run(name, func(t *testing.T) {
			screen := goldenScreen(t, name)

			var labels []string
			for _, r := range want.rows {
				labels = append(labels, r.Label)
			}

			assert.True(t, onScreen(screen, want.question, labels...), "the whole question and every option, through the frame")
			assert.False(t, onScreen(screen, want.question+" and more words"), "the whole question, not its start")
			assert.False(t, onScreen(screen, want.question, "An option this dialog does not have"))
		})
	}

	assert.False(t, onScreen(goldenScreen(t, "claude/idle"), "Do you want to proceed?"))
}

// A question is read through the bar Claude Code draws down its left, and through a frame around it, but not through
// words: the bar and the frame are dropped, a bar inside a line stays.
func TestGoldenDialogLines(t *testing.T) {
	screen := goldenScreen(t, "claude/question-barred")
	lines := dialogLines(screen)

	require.Len(t, lines, len(strings.Split(screen, "\n")), "one line for each of the screen's")
	assert.Contains(t, lines, "It is a real question: the reviewer is out of quota until Tuesday, 10 November, 13:15, and the review goes to")
	assert.Contains(t, lines, "the second seat meanwhile. Wait for Tuesday or buy more credits?")

	boxed := dialogLines(goldenScreen(t, "claude/permission-boxed"))
	assert.Contains(t, boxed, "Do you want to proceed?")
	assert.Contains(t, boxed, "❯ 1. Yes")
	assert.Contains(t, boxed, "  3. No, and tell Claude what to do differently (esc)")
}
