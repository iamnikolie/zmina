package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The default views are what an agent reads on every turn after it: they stay small however big the crew or the
// task, and the detail is one flag away. These tests pin the size.

func maxLineRunes(s string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		n = max(n, utf8.RuneCountInString(l))
	}

	return n
}

func TestLsCompactIsSmallAndDetailOnDemand(t *testing.T) {
	about := "a long line on what this seat is for, " + strings.Repeat("x", 80)

	var roles strings.Builder
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&roles, "  r%d:\n    about: %s\n", i, about)
	}

	file := testCrew(t, roles.String())
	_, _, err := zm(t, file, "later", "add", "an idea")
	require.NoError(t, err)

	out, _, err := zm(t, file, "ls")
	require.NoError(t, err)
	assert.LessOrEqual(t, len(out), 300, out)
	assert.NotContains(t, out, "a long line")
	assert.Contains(t, out, "0 up, 13 down")
	assert.Contains(t, out, "\ndown: dispatcher, r1, r10, r11, ")
	assert.Contains(t, out, "\nlater: 1 parked (zmina later ls)\n")

	wide, _, err := zm(t, file, "ls", "--wide")
	require.NoError(t, err)
	assert.Contains(t, wide, "ABOUT")
	assert.Equal(t, 12, strings.Count(wide, "a long line on what"))
	assert.Greater(t, len(wide), 5*len(out), "the compact view saves most of it")

	all, _, err := zm(t, file, "ls", "--all")
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^r12\s+down\s+-\s+-\s+-$`, all)
	assert.NotContains(t, all, "a long line")

	one, _, err := zm(t, file, "ls", "r3")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(one, "a long line on what"))
	assert.Regexp(t, `(?m)^r3\s`, one)

	_, _, err = zm(t, file, "ls", "nobody")
	require.ErrorContains(t, err, `no role "nobody"`)
}

func TestLsCompactMarksDownRoles(t *testing.T) {
	file := testCrew(t, "  dev: {}\n  designer: {}\n")
	_, _, err := zm(t, file, "task", "new", "dev", "fix the login")
	require.NoError(t, err)

	out, _, err := zm(t, file, "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "\ndown: dispatcher, designer, dev (T-1)\n")
}

func TestCompactScreen(t *testing.T) {
	screen := strings.Join([]string{
		"⏺ Done: the fix is in.   ",
		"",
		"  - tests green",
		"",
		"✻ Baked for 2m",
		"──────────────────────── dev ──",
		"❯ a draft",
		"────────────────────────────────",
		"  ~/work (main) [Opus] ctx:44%",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← 1 agent",
	}, "\n")

	assert.Equal(t, "⏺ Done: the fix is in.\n  - tests green\n✻ Baked for 2m\n❯ a draft", compactScreen(screen, 20))
	assert.Equal(t, "✻ Baked for 2m\n❯ a draft", compactScreen(screen, 2))

	inline := strings.Join([]string{
		"⏺ Which one?",
		"❯ 1. the first",
		"  2. the second",
		"────────────────────────────────",
		"❯ ",
		"────────────────────────────────",
		"  [Sonnet] ctx:10%",
		"  ? for shortcuts",
	}, "\n")
	assert.Equal(t, "⏺ Which one?\n❯ 1. the first\n  2. the second\n❯", compactScreen(inline, 20),
		"options above the input box are history; the box's own footer goes")

	draft := "──────\n❯ first line\n  second line\n──────\n  ~/work [Opus]\n  ⏸ plan mode on (shift+tab to cycle)"
	assert.Equal(t, "❯ first line\n  second line", compactScreen(draft, 20), "a draft on two lines stays whole")

	unknown := "──────\n❯ \n──────\n  something we do not know"
	assert.Equal(t, "❯\n  something we do not know", compactScreen(unknown, 20), "a line not known as footer stays")

	assert.Equal(t, "a\nb", compactScreen("a\n\n\nb\n", 20), "no input line: only blank lines go")
}

// A dialog on screen is never cut, whatever its shape and whatever input boxes the history above it holds: the
// question, every option and the confirm hint stay.
func TestCompactScreenKeepsDialogs(t *testing.T) {
	history := []string{
		"────────────────────────────────",
		"❯ previous request",
		"────────────────────────────────",
		"  ~/work (main) [Opus] ctx:44%",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
		"⏺ Working",
	}
	plain := []string{"Do you trust this folder?", "", "❯ 1. Yes, proceed", "  2. No, exit", "", "Enter to confirm"}
	boxed := []string{
		"╭──────────────────────────────╮",
		"│ Allow this command?          │",
		"│ ❯ 1. Yes                     │",
		"│   2. No, and tell Claude why │",
		"╰──────────────────────────────╯",
		"  Esc to cancel",
	}
	ruled := []string{
		"────────────────────────────────",
		" Bash command",
		"   rm -rf build",
		" Do you want to proceed?",
		" ❯ 1. Yes",
		"   2. Yes, and don't ask again",
		"   3. No (esc)",
		"────────────────────────────────",
		"  Esc to cancel · Tab to amend",
	}
	numbered := []string{
		"────────────────────────────────",
		"❯ 1. Keep the old name",
		"  2. Rename it",
		"────────────────────────────────",
		"  Enter to select · ↑/↓ to navigate",
	}
	cat := func(parts ...[]string) string {
		var all []string
		for _, p := range parts {
			all = append(all, p...)
		}

		return strings.Join(all, "\n")
	}

	cases := map[string]struct {
		screen string
		keep   []string
	}{
		"plain":                     {cat(plain), []string{"Do you trust this folder?", "2. No, exit", "Enter to confirm"}},
		"boxed":                     {cat(boxed), []string{"Allow this command?", "2. No, and tell Claude why", "Esc to cancel"}},
		"ruled":                     {cat(ruled), []string{"Do you want to proceed?", "3. No (esc)", "Esc to cancel"}},
		"numbered":                  {cat(numbered), []string{"1. Keep the old name", "2. Rename it", "Enter to select"}},
		"plain after history":       {cat(history, plain), []string{"Do you trust this folder?", "2. No, exit", "Enter to confirm"}},
		"boxed after history":       {cat(history, boxed), []string{"Allow this command?", "2. No, and tell Claude why", "Esc to cancel"}},
		"ruled after history":       {cat(history, ruled), []string{"Do you want to proceed?", "3. No (esc)", "Esc to cancel"}},
		"numbered after history":    {cat(history, numbered), []string{"1. Keep the old name", "2. Rename it", "Enter to select"}},
		"boxed after two histories": {cat(history, history, boxed), []string{"Allow this command?", "2. No, and tell Claude why", "Esc to cancel"}},
		"plain after two histories": {cat(history, history, plain), []string{"Do you trust this folder?", "2. No, exit", "Enter to confirm"}},
		"boxed after a bare history box": {cat([]string{"────", "❯ previous request", "────", "⏺ Working"}, boxed),
			[]string{"Allow this command?", "2. No, and tell Claude why", "Esc to cancel"}},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := compactScreen(c.screen, 40)
			for _, k := range c.keep {
				assert.Contains(t, got, k)
			}
		})
	}
}

func TestTaskShowCompactIsSmall(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	asked := strings.Repeat("Do the long thing with care. ", 70)
	_, _, err := zm(t, file, "task", "new", "dev", asked)
	require.NoError(t, err)

	for i := 0; i < 15; i++ {
		_, _, err = zm(t, file, "task", "set", "1", "--note", fmt.Sprintf("step %d done · left: %s", i, strings.Repeat("more ", 60)))
		require.NoError(t, err)
	}

	body := filepath.Join(t.TempDir(), "result.json")
	require.NoError(t, os.WriteFile(body, []byte(`{"summary":"Shipped. `+strings.Repeat("Detail. ", 200)+`",`+
		`"done":"`+strings.Repeat("x", 3000)+`","remaining":"none","open_questions":"none","artifacts":"none","next_step":"none"}`), 0o600))
	_, _, err = zm(t, file, "task", "result", "1", "--file", body)
	require.NoError(t, err)

	out, _, err := zm(t, file, "task", "show", "1")
	require.NoError(t, err)
	assert.LessOrEqual(t, utf8.RuneCountInString(out), 2400, out)
	assert.LessOrEqual(t, maxLineRunes(out), 330)
	assert.Regexp(t, `asked: Do the long thing.*… \(\+\d+ chars, --full\)\n`, out)
	assert.Contains(t, out, "more: zmina task show T-1 --full · zmina task result T-1\n")

	full, _, err := zm(t, file, "task", "show", "1", "--full")
	require.NoError(t, err)
	assert.Contains(t, full, asked)
	assert.Contains(t, full, strings.Repeat("x", 3000))

	res, _, err := zm(t, file, "task", "result", "1")
	require.NoError(t, err)
	assert.Contains(t, res, "summary:\nShipped. Detail.", "the summary whole")
	assert.LessOrEqual(t, utf8.RuneCountInString(res), 2300, "the full hint carries the crew path, a long temp path here")
	assert.Regexp(t, `full: zmina --crew '[^']+' task result T-1 --revision [0-9a-f]{12} --full\n`, res)

	res, _, err = zm(t, file, "task", "result", "1", "--full")
	require.NoError(t, err)
	assert.Contains(t, res, strings.Repeat("x", 3000))

	q, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.Regexp(t, `T-1 result ready \(worker-reported\): Shipped\. \(zmina task result T-1 --revision [0-9a-f]{12}\)`, q)
	assert.NotRegexp(t, `[0-9a-f]{40}`, q, "the revision is short in the notice")
}

func TestLogAndListsAreOneLineEach(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	long := strings.Repeat("a very long ask that goes on and on ", 20)
	_, _, err := zm(t, file, "task", "new", "dev", long)
	require.NoError(t, err)
	_, _, err = zm(t, file, "later", "add", long, "--note", long)
	require.NoError(t, err)
	_, _, err = zm(t, file, "todo", "add", long+"\n\n"+long, "--title", "a short title")
	require.NoError(t, err)
	_, _, err = zm(t, file, "send", "--when-idle", "dev", long)
	require.NoError(t, err)

	for _, args := range [][]string{{"log"}, {"later", "ls"}, {"todo", "ls"}, {"queue", "ls"}} {
		out, _, err := zm(t, file, args...)
		require.NoError(t, err)
		assert.LessOrEqual(t, maxLineRunes(out), 180, "%v: %s", args, out)

		full, _, err := zm(t, file, append(args, "--full")...)
		require.NoError(t, err)
		assert.Contains(t, full, strings.TrimSpace(long), "%v --full", args)
	}

	out, _, err := zm(t, file, "todo", "ls")
	require.NoError(t, err)
	assert.Contains(t, out, "a short title")
}

func TestPacketAsksForAOneLineReport(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	_, _, err := zm(t, file, "task", "new", "dev", "fix the login")
	require.NoError(t, err)

	out, _, err := zm(t, file, "task", "handoff", "1", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, "task result T-1 --file")
	assert.Contains(t, out, "that tells the dispatcher")
	assert.Contains(t, out, "never the result again")
}

// Two results of one task whose summaries start the same are two notices, each naming its own revision, and the
// command in each opens that revision rather than the latest.
func TestResultRevisionsWithTheSameGistAreTwoNotices(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	_, _, err := zm(t, file, "task", "new", "dev", "fix it")
	require.NoError(t, err)

	body := filepath.Join(t.TempDir(), "result.json")
	for _, done := range []string{"first pass", "second pass"} {
		require.NoError(t, os.WriteFile(body, []byte(`{"summary":"Fixed. More words.","done":"`+done+`",`+
			`"remaining":"none","open_questions":"none","artifacts":"none","next_step":"none"}`), 0o600))
		_, _, err = zm(t, file, "task", "result", "1", "--file", body)
		require.NoError(t, err)
	}

	q, _, err := zm(t, file, "queue", "ls", "--full")
	require.NoError(t, err)
	revs := regexp.MustCompile(`T-1 result ready \(worker-reported\): Fixed\. \(zmina task result T-1 --revision ([0-9a-f]{12})\)`).
		FindAllStringSubmatch(q, -1)
	require.Len(t, revs, 2, q)
	assert.NotEqual(t, revs[0][1], revs[1][1])

	seen := map[string]bool{}
	for _, r := range revs {
		out, _, err := zm(t, file, "task", "result", "1", "--revision", r[1])
		require.NoError(t, err)
		assert.Contains(t, out, "result: "+r[1])
		for _, done := range []string{"first pass", "second pass"} {
			if strings.Contains(out, "done: "+done) {
				seen[done] = true
			}
		}
	}
	assert.Len(t, seen, 2, "each notice opens its own revision")
}

// task result --revision <old> with a long section prints a full hint that opens <old> in this crew, not the latest.
func TestHistoricalResultHintKeepsRevision(t *testing.T) {
	for _, kind := range []string{"result", "checkpoint"} {
		t.Run(kind, func(t *testing.T) {
			file := testCrew(t, "  dev: {}\n")
			_, _, err := zm(t, file, "task", "new", "dev", "fix it")
			require.NoError(t, err)

			body := filepath.Join(t.TempDir(), "record.json")
			var revs []string
			for _, done := range []string{"OLD " + strings.Repeat("o", 400), "NEW " + strings.Repeat("n", 400)} {
				require.NoError(t, os.WriteFile(body, []byte(`{"summary":"Step.","done":"`+done+`",`+
					`"remaining":"none","open_questions":"none","artifacts":"none","next_step":"none"}`), 0o600))
				out, _, err := zm(t, file, "task", kind, "1", "--file", body)
				require.NoError(t, err)
				revs = append(revs, strings.Fields(out)[2])
			}

			old, _, err := zm(t, file, "task", kind, "1", "--revision", revs[0][:10])
			require.NoError(t, err)
			m := regexp.MustCompile(`full: zmina --crew (\S+) task ` + kind + ` T-1 --revision ([0-9a-f]+) --full\n`).FindStringSubmatch(old)
			require.NotNil(t, m, old)
			assert.Equal(t, quoteCommandArg(file), m[1], "the hint names this crew")
			assert.True(t, strings.HasPrefix(revs[0], m[2]), "the hint names the revision shown")

			full, _, err := zm(t, file, "task", kind, "1", "--revision", m[2], "--full")
			require.NoError(t, err)
			assert.Contains(t, full, "OLD "+strings.Repeat("o", 400))
			assert.NotContains(t, full, "NEW ")
		})
	}
}

// sharedStart writes a record file whose name starts as rev does for n hex digits and then differs: a synthetic
// index entry, so prefix handling is tested without a real hash collision.
func sharedStart(t *testing.T, stateDir, kind, id, rev string, n int, fill byte) string {
	t.Helper()
	other := rev[:n] + strings.Repeat(string(fill), 64-n)
	require.NotEqual(t, rev, other)
	dir := filepath.Join(stateDir, kind+"s", id)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, other+".json"), []byte("{}"), 0o600))
	return other
}

func stateDirOf(t *testing.T, file string) string {
	t.Helper()
	c, err := crew.Load(file)
	require.NoError(t, err)
	return c.StateDir()
}

// The full hint's revision is as long as it takes to name one record: a file sharing the first 12 hex digits
// makes it longer, and the printed command opens the record shown.
func TestFullHintDisambiguatesItsPrefix(t *testing.T) {
	for _, kind := range []string{"result", "checkpoint"} {
		t.Run(kind, func(t *testing.T) {
			file := testCrew(t, "  dev: {}\n")
			_, _, err := zm(t, file, "task", "new", "dev", "fix it")
			require.NoError(t, err)

			body := filepath.Join(t.TempDir(), "record.json")
			require.NoError(t, os.WriteFile(body, []byte(`{"summary":"Step.","done":"`+strings.Repeat("d", 400)+`",`+
				`"remaining":"none","open_questions":"none","artifacts":"none","next_step":"none"}`), 0o600))
			out, _, err := zm(t, file, "task", kind, "1", "--file", body)
			require.NoError(t, err)
			rev := strings.Fields(out)[2]
			sharedStart(t, stateDirOf(t, file), kind, "T-1", rev, 16, otherHex(rev[16]))

			shown, _, err := zm(t, file, "task", kind, "1", "--revision", rev)
			require.NoError(t, err)
			m := regexp.MustCompile(`--revision ([0-9a-f]+) --full\n`).FindStringSubmatch(shown)
			require.NotNil(t, m, shown)
			assert.Greater(t, len(m[1]), 16, "longer than the shared start")

			full, _, err := zm(t, file, "task", kind, "1", "--revision", m[1], "--full")
			require.NoError(t, err)
			assert.Contains(t, full, strings.Repeat("d", 400))
		})
	}
}

// otherHex is a hex digit other than b.
func otherHex(b byte) byte {
	if b == 'f' {
		return 'e'
	}
	return 'f'
}

// A prefix two records share in the task a checkpoint was carried from is refused, even when the new task has
// one record that starts the same: the answer is never a different record than the one asked for.
func TestCarriedAmbiguityIsRefused(t *testing.T) {
	file := testCrew(t, "  dev: {}\n")
	_, _, err := zm(t, file, "task", "new", "dev", "prepare")
	require.NoError(t, err)
	body := filepath.Join(t.TempDir(), "checkpoint.json")
	require.NoError(t, os.WriteFile(body, []byte(`{"summary":"Half.","done":"some","remaining":"rest",`+
		`"open_questions":"none","artifacts":"none","next_step":"go on"}`), 0o600))
	out, _, err := zm(t, file, "task", "checkpoint", "1", "--file", body)
	require.NoError(t, err)
	rev := strings.Fields(out)[2]
	_, _, err = zm(t, file, "task", "park", "1", "--note", "rest")
	require.NoError(t, err)
	_, _, err = zm(t, file, "later", "take", "1", "dev")
	require.NoError(t, err)

	base := stateDirOf(t, file)
	sharedStart(t, base, "checkpoint", "T-1", rev, 12, otherHex(rev[12]))
	sharedStart(t, base, "checkpoint", "T-2", rev, 8, otherHex(rev[8]))

	for _, prefix := range []string{rev[:8], rev[:12]} {
		_, _, err = zm(t, file, "task", "checkpoint", "2", "--revision", prefix)
		require.ErrorContains(t, err, "give more of it", prefix)
	}

	got, _, err := zm(t, file, "task", "checkpoint", "2", "--revision", rev)
	require.NoError(t, err)
	assert.Contains(t, got, "checkpoint: "+rev)
	assert.Contains(t, got, "source task: T-1")
}
