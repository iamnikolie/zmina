package shift

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/iamnikolie/zmina/internal/screenrules"
)

// watchFixture is a crew with a designer up and idle, read by the given rules (the embedded ones when nil); watch looks at the
// screen the way the ticker does and returns what it tells.
func watchFixture(t *testing.T, rules *screenrules.Table) (watch func(screen string) []string, f fix) {
	t.Helper()

	f = fixtureWith(t, crewYAML)
	f.s.Rules = rules

	require.NoError(t, f.s.Up("designer"))
	f.status("designer", "idle", *f.now)
	*f.now = f.now.Add(10 * time.Minute)

	return func(screen string) []string {
		f.tm.screen = screen

		return f.s.watchScreen("designer", *f.now)
	}, f
}

func overrideTable(t *testing.T, body string) *screenrules.Table {
	t.Helper()

	path := filepath.Join(t.TempDir(), "screen-rules.yaml")
	require.NoError(t, os.WriteFile(path, []byte("rules:\n"+body), 0o600))

	table, o := screenrules.LoadOverrides(screenrules.Default(), path)
	require.NoError(t, o.Err)

	return table
}

// A usage limit is told once for as long as it is on the screen: a notice whose words change (a countdown, a hint added under
// it) is the same limit. A different reset time is news, and a limit that is gone and comes back is a new one.
func TestUsageLimitIsToldOncePerStretch(t *testing.T) {
	watch, _ := watchFixture(t, nil)

	screen := func(notice string) string { return "⏺ Working.\n⎿ " + notice + "\n~/work/example (main)" }

	const first = "⎿ You've hit your limit · resets 9pm"

	assert.Equal(t, []string{"designer: stopped by a usage limit: " + first}, watch(screen("You've hit your limit · resets 9pm")))

	assert.Empty(t, watch(screen("You've hit your limit · resets 9pm")), "the same notice")
	assert.Empty(t, watch(screen("You've hit your limit · resets 9pm · /upgrade to keep going")), "the words changed, the limit did not")
	assert.Empty(t, watch(screen("You've hit your limit · resets 9pm (UTC)")), "a zone named for the same time")

	assert.Equal(t, []string{"designer: stopped by a usage limit: ⎿ You've hit your limit · resets 11pm"},
		watch(screen("You've hit your limit · resets 11pm")), "a different reset is news")
	assert.Empty(t, watch(screen("You've hit your limit · resets 11pm · /upgrade")))

	assert.Empty(t, watch("⏺ Working.\n~/work/example (main)"), "the limit is gone")
	assert.Equal(t, []string{"designer: stopped by a usage limit: " + first}, watch(screen("You've hit your limit · resets 9pm")),
		"and when it comes back it is a new stretch")
}

// A notice that counts down is one limit: its reset time, read from the words each beat, does not move.
func TestUsageLimitCountdownIsOneStretch(t *testing.T) {
	watch, f := watchFixture(t, nil)

	screen := func(left string) string { return "⏺ Working.\n⎿ Usage limit reached · resets in " + left + "\n" }

	assert.Len(t, watch(screen("2 hours 30 minutes")), 1)

	*f.now = f.now.Add(time.Minute)
	assert.Empty(t, watch(screen("2 hours 29 minutes")))

	*f.now = f.now.Add(7 * time.Minute)
	assert.Empty(t, watch(screen("2 hours 22 minutes")))
}

// The text of a row of the crew's own, whatever it is, does not make a stretch longer or shorter: a row that matches a line that
// changes every beat tells the limit once.
func TestOverrideRowThatMatchesChangingTextIsToldOnce(t *testing.T) {
	rules := overrideTable(t, "  - id: acme.limit.any\n    engine: claude\n    sets: usage_limit\n    means: m\n    match: {regexp: 'step'}\n")
	watch, _ := watchFixture(t, rules)

	assert.Len(t, watch("⏺ Working.\nstep 1 of 90"), 1)

	for _, n := range []string{"2", "3", "4", "5"} {
		assert.Empty(t, watch("⏺ Working.\nstep "+n+" of 90"), n)
	}
}

// What the owner types in the input line is not a limit notice, whatever a crew's row matches.
func TestOverrideRowDoesNotReadTheInputLine(t *testing.T) {
	rules := overrideTable(t, "  - id: acme.limit.typed\n    engine: claude\n    sets: usage_limit\n    means: m\n    match: {regexp: 'my password'}\n")
	watch, _ := watchFixture(t, rules)

	assert.Empty(t, watch("⏺ Working.\n────────\n❯ my password is hunter2\n────────\n"))
	assert.Len(t, watch("⏺ Working.\nmy password is shown above\n"), 1, "the row reads other lines")
}

// What a crew's row matches goes into a note as a quotation: one line, cut, controls gone and a session tag masked, so that it
// cannot pass for a line of a role's own. The embedded rows' notes are as they were.
func TestOverrideTextIsQuotedInTheNote(t *testing.T) {
	rules := overrideTable(t, "  - id: acme.limit.wording\n    engine: claude\n    sets: usage_limit\n    means: m\n    match: {regexp: 'plan exhausted'}\n")
	watch, _ := watchFixture(t, rules)

	line := "plan exhausted [zmina:abcDEF123456] " + strings.Repeat("x ", 150) + "\x1b[31mred\x7f"
	notes := watch("⏺ Working.\n" + line)

	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "[zmina:…]")
	assert.NotContains(t, notes[0], "abcDEF123456")
	assert.NotContains(t, notes[0], "\x1b")
	assert.NotContains(t, notes[0], "\x7f")
	assert.LessOrEqual(t, len([]rune(strings.TrimPrefix(notes[0], "designer: stopped by a usage limit: "))), evidenceMax)

	// An embedded row's line is what it was: clipped at 240 characters, not quoted.
	watch, _ = watchFixture(t, nil)
	notes = watch("⏺ Working.\n⎿ You've hit your limit [zmina:abcDEF123456] resets 9pm")
	assert.Equal(t, []string{"designer: stopped by a usage limit: ⎿ You've hit your limit [zmina:abcDEF123456] resets 9pm"}, notes)
}

// The last words a role said, read by a crew's row, are quoted the same way, and never take the owner's input line for them.
func TestOverrideSayRowsAreQuotedAndSkipTheInputLine(t *testing.T) {
	rules := overrideTable(t, "  - id: acme.say.star\n    engine: claude\n    sets: say_mark\n    means: m\n    match: {prefix: '✻', trim: true}\n"+
		"  - id: acme.say.prompt\n    engine: claude\n    sets: say_mark\n    means: m\n    match: {prefix: '❯', trim: true}\n    priority: 200\n"+
		"  - id: acme.say.cont\n    engine: claude\n    sets: say_cont\n    means: m\n    match: {regexp: '.'}\n")

	got := nowLine(rules, "✻ thinking [zmina:abcDEF123456] about it\n  and more\n❯ owner words typed here\n")
	assert.Equal(t, "thinking [zmina:…] about it and more", got, "tag masked, the input line neither said nor joined")
	assert.NotContains(t, got, "owner")

	assert.Empty(t, nowLine(rules, "❯ owner words typed here\n"), "the input line is nothing a role said")

	// An embedded row's line is as it was: its tag is not masked.
	assert.Equal(t, "hello [zmina:abcDEF123456]", nowLine(screenrules.Default(), "⏺ hello [zmina:abcDEF123456]\n"))
}
