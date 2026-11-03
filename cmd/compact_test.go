package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

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
		"  ⏵⏵ bypass permissions on",
	}, "\n")

	assert.Equal(t, "⏺ Done: the fix is in.\n  - tests green\n✻ Baked for 2m\n❯ a draft", compactScreen(screen, 20))
	assert.Equal(t, "✻ Baked for 2m\n❯ a draft", compactScreen(screen, 2))

	dialog := "Do you trust this folder?\n\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm"
	assert.Equal(t, "Do you trust this folder?\n❯ 1. Yes, proceed", compactScreen(dialog, 20),
		"what follows the last ❯ is the footer")
	assert.Equal(t, "a\nb", compactScreen("a\n\n\nb\n", 20), "no input line: only blank lines go")
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
	assert.LessOrEqual(t, utf8.RuneCountInString(res), 2200)
	assert.Contains(t, res, "full: zmina task result T-1 --full\n")

	res, _, err = zm(t, file, "task", "result", "1", "--full")
	require.NoError(t, err)
	assert.Contains(t, res, strings.Repeat("x", 3000))

	q, _, err := zm(t, file, "queue", "ls")
	require.NoError(t, err)
	assert.Contains(t, q, "T-1 result ready (worker-reported): Shipped. (zmina task result T-1)")
	assert.NotRegexp(t, `[0-9a-f]{40}`, q, "no revision hash in the notice")
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
