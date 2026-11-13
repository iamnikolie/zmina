package cmd

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/shift"
)

// The rules table reads a pane for peek's footer cut and for the browser chat's dialog as the code that kept every
// marker in a Go literal did. screen_legacy_test.go holds that code, copied from before the change; each test here runs a
// screen through both and asserts the same answer. The expected values of the golden tests (screens_golden_test.go) are
// unchanged.

// corpus is every golden screen of internal/shift, by name.
func corpus(t testing.TB) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(screensDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(screensDir, path)
		out[strings.TrimSuffix(filepath.ToSlash(rel), ".txt")] = strings.TrimRight(string(raw), "\n")

		return nil
	})
	require.NoError(t, err)

	return out
}

// sameScreenReading asserts that peek's cut and the chat's dialog read a screen as they did before the table.
func sameScreenReading(t testing.TB, label, text string) {
	t.Helper()

	rows := strings.Split(text, "\n")

	if want, got := legacyFooterAt(rows), footerAt(rows); want != got {
		t.Errorf("%s: footerAt %d, was %d\n%s", label, got, want, text)
	}

	for _, n := range []int{5, 40, 1000} {
		if want, got := legacyCompactScreen(text, n), compactScreen(text, n); want != got {
			t.Errorf("%s: compactScreen(%d)\n%s\nwas\n%s\nof\n%s", label, n, got, want, text)
		}
	}

	if want, got := legacyDialogLines(text), dialogLines(text); fmt.Sprint(want) != fmt.Sprint(got) {
		t.Errorf("%s: dialogLines %q, was %q", label, got, want)
	}

	// The question asked of the screen: the one ParseDialog reads, and each row of it.
	questions := []string{"Do you want to proceed?"}
	if d := (&shift.Shift{}).ParseDialog(text); d != nil {
		questions = append(questions, d.Question)
	}

	questions = append(questions, rows...)

	for _, q := range questions {
		wantRows, wantOK := legacyActiveDialog(text, q)
		gotRows, gotOK := activeDialog(text, q)

		if wantOK != gotOK || fmt.Sprintf("%+v", wantRows) != fmt.Sprintf("%+v", gotRows) {
			t.Errorf("%s: activeDialog(%q) %+v %v, was %+v %v\n%s", label, q, gotRows, gotOK, wantRows, wantOK, text)
		}
	}
}

func TestScreenRulesReadPeekAndChatAsTheOldCodeDid(t *testing.T) {
	screens := corpus(t)
	require.GreaterOrEqual(t, len(screens), 36)

	var names []string
	for name := range screens {
		names = append(names, name)
	}

	sort.Strings(names)

	cuts := 0

	for _, name := range names {
		text := screens[name]
		sameScreenReading(t, name, text)

		rows := strings.Split(text, "\n")
		for n := 1; n < len(rows); n++ {
			sameScreenReading(t, fmt.Sprintf("%s (first %d rows)", name, n), strings.Join(rows[:n], "\n"))
			sameScreenReading(t, fmt.Sprintf("%s (last %d rows)", name, n), strings.Join(rows[len(rows)-n:], "\n"))

			cuts += 2
		}
	}

	t.Logf("%d screens, %d cuts of them", len(names), cuts)
}

var screenMarkers = []string{
	"❯", "❯ ", "❯ 1. Yes", "  2. No", "1. Yes", "   3) three", "│ 1. boxed │", "┃ 2. heavy", "╭────╮", "╰────╯", "│ Which one? │", "▏ left bar",
	"────────────", "───", "━━━━━", "═══", "── label ──", "  ~/work/example (main)", "  /work/example (main)", "  [Opus 5.5] ctx:41%", "  [gpt-6]", "  [ ]",
	"  ⏵⏵ bypass permissions on (shift+tab to cycle)", "  ⏸ plan mode on", "  ? for shortcuts", "  esc to interrupt", "  Context left until auto-compact: 8%",
	"  Auto-update failed · Try claude doctor", "  Press Ctrl-C again to exit", "  ctrl+t to hide todos", "  ctrl+o to expand", "⏵⏵", "? for shortcuts",
	"Enter to select · ↑/↓ to navigate", "Esc to cancel", "  [ ] 2. Later", "  [✔] 3. Done", "1. [ ] Talk", "2. [✔] Later", "text with 1. number inside",
	"", "", "   ", "\t", "☐ Which tabs?", "Do you want to proceed?", "  detail row",
}

func randomPane(r *rand.Rand, rows []string) string {
	n := 1 + r.Intn(24)
	out := make([]string, 0, n)

	for range n {
		var row string

		switch r.Intn(5) {
		case 0, 1:
			row = rows[r.Intn(len(rows))]
		case 2:
			row = rows[r.Intn(len(rows))]
			row = row[:r.Intn(len(row)+1)]
		case 3:
			row = strings.Repeat(" ", r.Intn(5)) + screenMarkers[r.Intn(len(screenMarkers))]
		default:
			row = screenMarkers[r.Intn(len(screenMarkers))]
		}

		out = append(out, row)
	}

	return strings.Join(out, "\n")
}

func corpusRows(t testing.TB) []string {
	t.Helper()

	seen := map[string]bool{}

	var rows []string

	for _, text := range corpus(t) {
		for _, row := range strings.Split(text, "\n") {
			if !seen[row] {
				seen[row] = true
				rows = append(rows, row)
			}
		}
	}

	sort.Strings(rows)

	return rows
}

// Random panes read as before: a fixed seed, so a failure is the same every run.
func TestScreenRulesReadRandomPanesForPeekAndChatAsTheOldCodeDid(t *testing.T) {
	rows := corpusRows(t)
	r := rand.New(rand.NewSource(20261113))

	for i := range 5000 {
		sameScreenReading(t, fmt.Sprintf("random pane %d", i), randomPane(r, rows))

		if t.Failed() {
			return
		}
	}
}
