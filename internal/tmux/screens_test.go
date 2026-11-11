package tmux

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Golden panes. zmina decides "is there an input line, and what is typed in it" from the pane tmux captures with its
// colours kept (`tmux capture-pane -p -e`): the input line is a ❯ row right under a rule, a typed draft is text that is
// neither dim (SGR 2) nor reversed (SGR 7, the cursor). These tests feed Input and Prompt recorded-shaped panes
// through a stand-in for the tmux binary, so they need no tmux, and pin what each returns. When a Claude Code release
// moves the rule, the glyph or the colours, the pane below it stops reading the same and one of these fails.
//
// The panes in testdata/panes are hand-built after what Claude Code 2.1 draws (the shapes the parsers' comments and the
// earlier tests describe), not recordings of a live session. To pin a new release, save the real pane with
//
//	tmux -L <socket> capture-pane -p -e -t <pane> > testdata/panes/<name>.ansi
//
// after replacing every path, name and word in it that is not neutral, and add its row to wantInput.

// fakeTmux is a tmux binary that answers the two calls a screen read makes: list-panes (one tagged, live pane) and
// capture-pane (the file named by FAKE_TMUX_PANE, whatever the flags). Every call's arguments after the socket are
// appended to calls.log, returned as the second value's path.
func fakeTmux(t *testing.T) (*Client, string) {
	t.Helper()

	dir := t.TempDir()
	bin, log := filepath.Join(dir, "tmux"), filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\nshift 3\necho \"$*\" >> '" + log + "'\ncase \"$1\" in\n" +
		"list-panes) printf '%%1\\t1\\t0\\t4242\\n' ;;\n" +
		"capture-pane) cat \"$FAKE_TMUX_PANE\" ;;\n*) echo \"fake tmux: $1\" >&2; exit 1 ;;\nesac\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o700))

	return &Client{Bin: bin, Socket: "fake"}, log
}

// captureCalls are the capture-pane calls the fake has seen, one string each.
func captureCalls(t *testing.T, log string) []string {
	t.Helper()

	raw, err := os.ReadFile(log)
	require.NoError(t, err)

	var out []string

	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.HasPrefix(l, "capture-pane") {
			out = append(out, l)
		}
	}

	return out
}

// showing makes the fake's pane the file at path.
func showing(t *testing.T, path string) {
	t.Helper()

	abs, err := filepath.Abs(path)
	require.NoError(t, err)

	t.Setenv("FAKE_TMUX_PANE", abs)
}

type inputRead struct {
	draft, suggestion string
	ok                bool
}

// wantInput is what Input reads from each pane in testdata/panes.
var wantInput = map[string]inputRead{
	// Claude Code, input line up.
	"claude-empty.ansi":       {"", "", true},
	"claude-busy.ansi":        {"", "", true},
	"claude-compacting.ansi":  {"", "", true},
	"claude-suggestion.ansi":  {"", "run the tests for the parser", true},
	"claude-placeholder.ansi": {"", `Try "fix lint errors"`, true},
	"claude-draft.ansi":       {"rename the helper and update its callers", "", true},
	"claude-draft-plain.ansi": {"half a thought", "", true},
	// A draft that wraps is read by its first row; one that starts on a second row by that row.
	"claude-draft-wrapped.ansi":     {"rename the helper to splitRecords, update its callers in the importer and the exporter, then run the", "", true},
	"claude-draft-after-break.ansi": {"second line of a draft", "", true},

	// Claude Code, a dialog instead of the input line: its ❯ stands on an answer and has no rule right above it.
	"claude-permission.ansi": {"", "", false},
	"claude-question.ansi":   {"", "", false},
	"claude-trust.ansi":      {"", "", false},

	// Codex bridge: an input line only under its idle marker; an older one above a busy or waiting marker is no prompt.
	"codex-idle.ansi":     {"", "", true},
	"codex-busy.ansi":     {"", "", false},
	"codex-approval.ansi": {"", "", false},

	// Nothing of Claude Code's.
	"shell.ansi": {"", "", false},

	// A numbered picker whose first row sits right under a rule, with no title row between, reads as a typed draft
	// "1. Keep the old name": Input has no guard against a row that is a numbered option (cmd's footer cut has one).
	// This pins what is, not what should be: change it together with Input.
	"claude-picker-under-rule.ansi": {"1. Keep the old name", "", true},
}

func TestInputReadsEveryGoldenPane(t *testing.T) {
	files, err := filepath.Glob("testdata/panes/*.ansi")
	require.NoError(t, err)

	var have []string
	for _, f := range files {
		have = append(have, filepath.Base(f))
	}

	var want []string
	for name := range wantInput {
		want = append(want, name)
	}

	sort.Strings(want)
	require.Equal(t, want, have, "every pane in testdata/panes has a row in wantInput, and every row a pane")

	c, _ := fakeTmux(t)

	for _, name := range have {
		t.Run(name, func(t *testing.T) {
			showing(t, filepath.Join("testdata", "panes", name))

			draft, suggestion, ok, err := c.Input("r")
			require.NoError(t, err)

			w := wantInput[name]
			assert.Equal(t, w.draft, draft, "draft")
			assert.Equal(t, w.suggestion, suggestion, "suggestion")
			assert.Equal(t, w.ok, ok, "an input line is on the screen")

			// Prompt is Input without the suggestion.
			pdraft, pok, err := c.Prompt("r")
			require.NoError(t, err)
			assert.Equal(t, w.draft, pdraft)
			assert.Equal(t, w.ok, pok)
		})
	}
}

// The panes are what a real capture-pane writes: SGR sequences, and the no-break space after Claude Code's ❯. A pane
// that lost them (an editor that normalised it) would still read, but would stop testing the colour rules.
func TestGoldenPanesKeepTheirColoursAndNoBreakSpace(t *testing.T) {
	for name, w := range wantInput {
		if !strings.HasPrefix(name, "claude-") || !w.ok {
			continue
		}

		raw, err := os.ReadFile(filepath.Join("testdata", "panes", name))
		require.NoError(t, err)

		assert.Contains(t, string(raw), "\x1b[", name+": colours")

		if name != "claude-draft-plain.ansi" && name != "claude-picker-under-rule.ansi" {
			assert.Contains(t, string(raw), "❯ ", name+": no-break space after the glyph")
		}
	}
}

// The same dim row is a suggestion with its colours and a draft without them: it is the colour that tells them apart.
func TestSuggestionNeedsItsDimColour(t *testing.T) {
	c, _ := fakeTmux(t)

	raw, err := os.ReadFile(filepath.Join("testdata", "panes", "claude-suggestion.ansi"))
	require.NoError(t, err)

	plain := sgr.ReplaceAllString(string(raw), "")
	path := filepath.Join(t.TempDir(), "plain.ansi")
	require.NoError(t, os.WriteFile(path, []byte(plain), 0o600))
	showing(t, path)

	draft, suggestion, ok, err := c.Input("r")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "run the tests for the parser", draft, "colourless, the suggestion reads as typed text")
	assert.Empty(t, suggestion)
}

// A tmux that cannot be asked, or a pane that is not there, is an error and never "no input line".
func TestInputOfNoPaneIsAnError(t *testing.T) {
	broken := &Client{Bin: "/usr/bin/false", Socket: "fake"}

	_, _, ok, err := broken.Input("r")
	assert.Error(t, err)
	assert.False(t, ok)
}

// padLikeTmux is a plain screen the way capture-pane -J returns a pane of the given width and height: every row padded
// with spaces to the width, empty rows under the text up to the height.
func padLikeTmux(text string, width, rows int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, l := range lines {
		if n := width - len([]rune(l)); n > 0 {
			lines[i] = l + strings.Repeat(" ", n)
		}
	}

	for len(lines) < rows {
		lines = append(lines, strings.Repeat(" ", width))
	}

	return strings.Join(lines, "\n") + "\n"
}

func TestCaptureTrimsPaddingAndBlankRowsAndKeepsTheLastLines(t *testing.T) {
	c, log := fakeTmux(t)

	idle, err := os.ReadFile(filepath.Join("testdata", "capture", "idle.txt"))
	require.NoError(t, err)

	text := strings.TrimRight(string(idle), "\n")
	rows := strings.Split(text, "\n")

	path := filepath.Join(t.TempDir(), "pane.txt")
	require.NoError(t, os.WriteFile(path, []byte(padLikeTmux(text, 120, 60)), 0o600))
	showing(t, path)

	all, err := c.Capture("r", 100)
	require.NoError(t, err)
	assert.Equal(t, text, all, "no padding, no blank rows under the text; the no-break space stays")

	last, err := c.Capture("r", 3)
	require.NoError(t, err)
	assert.Equal(t, strings.Join(rows[len(rows)-3:], "\n"), last)

	// -J joins a row the pane wrapped, so a long line is one line; the scrollback is asked for past the n rows wanted.
	calls := captureCalls(t, log)
	require.Len(t, calls, 2)
	assert.Equal(t, "capture-pane -p -J -t %1 -S -300", calls[0])
	assert.Equal(t, "capture-pane -p -J -t %1 -S -203", calls[1])
}

// A pane whose process ended shows "Pane is dead" on its bottom row, under empty rows: it is kept, right under the last
// output, and counts as one of the n lines.
func TestCaptureKeepsTheDeadPaneRowUnderTheLastOutput(t *testing.T) {
	c, _ := fakeTmux(t)

	idle, err := os.ReadFile(filepath.Join("testdata", "capture", "idle.txt"))
	require.NoError(t, err)

	text := strings.TrimRight(string(idle), "\n")
	rows := strings.Split(text, "\n")
	dead := "Pane is dead (status 0, Thu Jan  1 12:00:00 2026)"

	path := filepath.Join(t.TempDir(), "pane.txt")
	require.NoError(t, os.WriteFile(path, []byte(strings.TrimRight(padLikeTmux(text, 120, 59), "\n")+"\n"+dead+"\n"), 0o600))
	showing(t, path)

	got, err := c.Capture("r", 100)
	require.NoError(t, err)
	assert.Equal(t, text+"\n"+dead, got)

	got, err = c.Capture("r", 2)
	require.NoError(t, err)
	assert.Equal(t, rows[len(rows)-1]+"\n"+dead, got)
}

// Input asks for the pane with its colours (-e); without them a draft cannot be told from a suggestion.
func TestInputAsksForTheColours(t *testing.T) {
	c, log := fakeTmux(t)
	showing(t, filepath.Join("testdata", "panes", "claude-empty.ansi"))

	_, _, _, err := c.Input("r")
	require.NoError(t, err)

	calls := captureCalls(t, log)
	require.Len(t, calls, 1)
	assert.Equal(t, "capture-pane -p -e -t %1", calls[0])
}
