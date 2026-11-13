package shift

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/screenrules"
)

// The rules table reads a screen exactly as the code that kept every marker in a Go literal did. screen_legacy_test.go
// holds that code, copied from before the change; each test here runs a screen through both and asserts the same
// answer — on every golden screen (and every cut of one), and on random panes built from the markers and from the
// golden rows. The golden tests (screens_golden_test.go) are unchanged and pin the same corpus to their expected values.

// corpusScreens is every golden screen by name, read from testdata/screens.
func corpusScreens(t testing.TB) map[string]string {
	t.Helper()

	out := map[string]string{}
	root := filepath.Join("testdata", "screens")

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(root, path)
		out[strings.TrimSuffix(filepath.ToSlash(rel), ".txt")] = strings.TrimRight(string(raw), "\n")

		return nil
	})
	require.NoError(t, err)

	return out
}

// sameReading asserts that every reader of a screen answers as it did before the table.
func sameReading(t testing.TB, label, text string) {
	t.Helper()

	if want, got := legacyNowLine(text), NowLine(text); want != got {
		t.Errorf("%s: NowLine %q, was %q\n%s", label, got, want, text)
	}

	if want, got := legacyParseDialog(text), ParseDialog(text); !assert.ObjectsAreEqual(want, got) {
		t.Errorf("%s: ParseDialog %+v, was %+v\n%s", label, got, want, text)
	}

	// The limit notice is read from the bottom rows tmux captures (the rows' own count): what the old code saw of a pane is
	// its last limitLines rows.
	bottom := screenrules.Tail(text, limitLines)
	want := legacyLimitLine(bottom)

	for _, engine := range []string{screenrules.Claude, screenrules.Codex} {
		if got := limitLine(engine, text); want != got {
			t.Errorf("%s: limitLine(%s) %q, was %q\n%s", label, engine, got, want, text)
		}

		if got := limitLine(engine, bottom); want != got {
			t.Errorf("%s: limitLine(%s) of the bottom rows %q, was %q\n%s", label, engine, got, want, bottom)
		}
	}

	if want, got := legacyTrustKeys(text), trustKeys(text); !assert.ObjectsAreEqual(want, got) {
		t.Errorf("%s: trustKeys %v, was %v\n%s", label, got, want, text)
	}

	wantKey, wantOK := legacyExitKey(text)
	if gotKey, gotOK := exitKey(text); wantKey != gotKey || wantOK != gotOK {
		t.Errorf("%s: exitKey %q %v, was %q %v\n%s", label, gotKey, gotOK, wantKey, wantOK, text)
	}
}

// Every golden screen, and every cut of one (its first n rows, its last n rows), reads as before. The corpus is the same
// set the golden tests pin to their expected values.
func TestScreenRulesReadTheGoldenCorpusAsTheOldCodeDid(t *testing.T) {
	corpus := corpusScreens(t)
	require.GreaterOrEqual(t, len(corpus), 36, "the corpus is every screen under testdata/screens: claude and codex")

	var names []string
	for name := range corpus {
		names = append(names, name)
	}

	sort.Strings(names)

	cuts := 0

	for _, name := range names {
		text := corpus[name]
		sameReading(t, name, text)

		rows := strings.Split(text, "\n")
		for n := 1; n < len(rows); n++ {
			sameReading(t, fmt.Sprintf("%s (first %d rows)", name, n), strings.Join(rows[:n], "\n"))
			sameReading(t, fmt.Sprintf("%s (last %d rows)", name, n), strings.Join(rows[len(rows)-n:], "\n"))

			cuts += 2
		}
	}

	t.Logf("%d screens, %d cuts of them, each through six readers", len(names), cuts)
}

// The golden screens still read to the expected values the golden tests pin, through the table's readers directly.
func TestScreenRulesReadTheGoldenCorpusToItsExpectedValues(t *testing.T) {
	for name, g := range screens {
		text := loadScreen(t, name)

		assert.Equal(t, g.now, NowLine(text), name)
		assert.Equal(t, g.limit, limitLine(screenrules.Claude, text), name)
		assert.Equal(t, g.trust, trustKeys(text), name)

		key, ok := exitKey(text)
		assert.Equal(t, g.exit != nil, ok, name)

		if ok {
			assert.Equal(t, g.exit, []string{key}, name)
		}
	}
}

// markers are the pieces of a screen the readers look for, and near misses of them.
var markers = []string{
	"⏺ ", "⏺ Bash(go test ./...)", "⏺ Read 3 files (ctrl+o to expand)", "⏺ Done. Anything else?", "⏺", "  ⎿ Running…", "⎿",
	"  wrapped words", "    deeper", "❯", "❯ 1. Yes", "❯ run the tests", "  2. No", "1. Yes, I trust this folder", "2. No, exit",
	"Do you trust the files in this folder?", "Enter to select · ↑/↓ to navigate", "Esc to cancel", "Enter to confirm · Esc to cancel",
	"☐ Which tabs?", "│ Which one? │", "  detail under a choice", "3. Type something.", "4. Chat about this",
	"3. Exit and stop tasks", "5. Exit and stop tasks", "You've hit your limit · resets 9pm", "5-hour limit reached ∙ resets 9pm",
	"usage limit reached", "Usage Limit Reached", "limit will reset at 3pm", "out of extra usage", "/rate-limit-options",
	"hit your usage limit", "  /upgrade to increase your usage limit.", "───────────────", "────", "~/work/example (main) [Opus 5.5]",
	"? for shortcuts", "esc to interrupt", "[zmina codex: idle]", "[zmina codex: busy]", "", "", "   ", "trust", "TRUST", "yes", "Yes",
	"yeſ", "İ", "\t", "\r", "\x1b[2m", "no, exit", "YES, PROCEED", "  ❯ No, exit", "9. nine", "10. ten", "0. zero", "[ ] one", "[✔] two",
}

// randomScreen builds a pane from rows of the golden screens, cuts and mutations of them, and the markers.
func randomScreen(r *rand.Rand, rows []string) string {
	n := 1 + r.Intn(30)
	out := make([]string, 0, n)

	for range n {
		var row string

		switch r.Intn(6) {
		case 0, 1:
			row = rows[r.Intn(len(rows))]
		case 2:
			row = rows[r.Intn(len(rows))]
			row = row[:r.Intn(len(row)+1)]
		case 3:
			row = strings.Repeat(" ", r.Intn(7)) + markers[r.Intn(len(markers))]
		case 4:
			row = markers[r.Intn(len(markers))] + " " + rows[r.Intn(len(rows))]
		default:
			row = markers[r.Intn(len(markers))]
		}

		out = append(out, row)
	}

	return strings.Join(out, "\n")
}

func corpusRows(t testing.TB) []string {
	t.Helper()

	seen := map[string]bool{}

	var rows []string

	for _, text := range corpusScreens(t) {
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
func TestScreenRulesReadRandomPanesAsTheOldCodeDid(t *testing.T) {
	rows := corpusRows(t)
	r := rand.New(rand.NewSource(20261113))

	const panes = 20000

	for i := range panes {
		sameReading(t, fmt.Sprintf("random pane %d", i), randomScreen(r, rows))

		if t.Failed() {
			return
		}
	}
}

// FuzzScreenRules is the same assertion on whatever the fuzzer builds, seeded with every golden screen (go test runs the
// seeds; go test -fuzz=FuzzScreenRules explores).
func FuzzScreenRules(f *testing.F) {
	for _, text := range corpusScreens(f) {
		f.Add(text)
	}

	for _, m := range markers {
		f.Add(m)
	}

	f.Fuzz(func(t *testing.T, text string) {
		sameReading(t, "fuzz", text)
	})
}

// A row a crew adds is read by the ticker's watch of the screen: a wording the embedded rows do not know is told as a usage
// limit once the crew's screen-rules.yaml says so, and not before.
func TestAddedScreenRuleIsReadByTheWatch(t *testing.T) {
	t.Cleanup(func() { screenrules.Use(nil) })

	told := func() []string {
		f := fixtureWith(t, crewYAML)
		require.NoError(t, f.s.Up("designer"))
		f.status("designer", "idle", *f.now)
		*f.now = f.now.Add(10 * time.Minute)
		f.tm.screen = "⏺ Working.\n⎿ Plan exhausted until 9pm\n~/work/example (main)"

		return f.s.watchScreen("designer", *f.now)
	}

	assert.Empty(t, told())

	path := filepath.Join(t.TempDir(), "screen-rules.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`rules:
  - id: acme.limit.wording
    engine: claude
    sets: usage_limit
    means: a newer wording of the notice
    match: {regexp: "(?i)plan exhausted", trim: true}
    region: {last_lines: 12}
`), 0o600))

	table, o := screenrules.LoadOverrides(screenrules.Default(), path)
	require.NoError(t, o.Err)
	screenrules.Use(table)

	assert.Equal(t, []string{"designer: stopped by a usage limit: ⎿ Plan exhausted until 9pm"}, told())
	assert.Equal(t, 12, screenrules.Active().Window(screenrules.Claude, screenrules.UsageLimit, limitLines))
}
