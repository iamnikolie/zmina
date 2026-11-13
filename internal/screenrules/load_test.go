package screenrules

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A row of kind builtin names logic that stays in Go: the embedded tables list it, an override file has no way to make one.
func TestOverrideCannotAddABuiltinRow(t *testing.T) {
	refused(t, "  - id: acme.builtin\n    engine: claude\n    kind: builtin\n    means: m\n", "acme.builtin", "builtin")
	refused(t, "  - id: acme.builtin-off\n    kind: builtin\n    disabled: true\n", "acme.builtin-off", "builtin")

	// A row that is the embedded builtin's id is refused as before.
	refused(t, "  - id: claude.input.line\n    engine: claude\n    kind: builtin\n    means: m\n", "claude.input.line", "builtin")

	// kind: match is the default and may be said.
	accepted(t, row("acme.explicit", "claude", "usage_limit", `contains: 'x'`, "    kind: match\n"))
}

// Only the first YAML document of a file used to be read, the rest silently dropped: a file of two is refused.
func TestOverrideWithASecondDocumentIsRefused(t *testing.T) {
	body := "rules:\n" + row("acme.first", "claude", "usage_limit", `contains: 'x'`)

	for name, second := range map[string]string{
		"rules":  "---\nrules:\n" + row("acme.second", "claude", "usage_limit", `contains: 'y'`),
		"empty":  "---\n",
		"scalar": "---\nnope\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, o := LoadOverrides(Default(), writeOverride(t, body+second))
			require.Error(t, o.Err)
			assert.Contains(t, o.Err.Error(), "document")
			assert.Same(t, Default(), got)
		})
	}

	// A document marker before the one document is not a second.
	accepted(t, row("acme.marked", "claude", "usage_limit", `contains: 'x'`))

	_, o := LoadOverrides(Default(), writeOverride(t, "---\n"+body))
	require.NoError(t, o.Err)
}

// An override reads no anchors, aliases or merge keys: a few lines of them can name a tree of billions of nodes, and nothing in a
// file of rows needs one.
func TestOverrideWithAnAliasIsRefusedCheaply(t *testing.T) {
	bomb := "rules: []\nbomb:\n  a: &a [x, x, x, x, x, x, x, x, x]\n"

	prev := "a"

	for _, n := range []string{"b", "c", "d", "e", "f", "g", "h", "i"} {
		bomb += "  " + n + ": &" + n + " [*" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev + ", *" + prev +
			", *" + prev + ", *" + prev + ", *" + prev + "]\n"
		prev = n
	}

	for name, body := range map[string]string{
		"bomb":   bomb,
		"anchor": "rules:\n  - &r\n    id: acme.a\n    engine: claude\n    sets: usage_limit\n    means: m\n    match: {contains: x}\n",
		"alias": "rules:\n  - id: acme.a\n    engine: claude\n    sets: usage_limit\n    means: &m same\n    match: {contains: x}\n" +
			"  - id: acme.b\n    engine: claude\n    sets: usage_limit\n    means: *m\n    match: {contains: y}\n",
		"merge": "base: &b {engine: claude, sets: usage_limit, means: m}\nrules:\n  - id: acme.a\n    <<: *b\n    match: {contains: x}\n",
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			got, o := LoadOverrides(Default(), writeOverride(t, body))
			require.Error(t, o.Err)
			assert.Contains(t, o.Err.Error(), "alias")
			assert.Same(t, Default(), got)
			assert.Less(t, time.Since(start), 2*time.Second, "refused without expanding it")
		})
	}
}

// An override file is a few rows: 64 KiB is more than enough, and a larger one is refused unread.
func TestOverrideFileSizeIsCapped(t *testing.T) {
	pad := "# " + strings.Repeat("x", 100) + "\n"
	header := "rules: []\n"
	room := 64 << 10

	// Exactly the cap is read.
	body := header + strings.Repeat(pad, (room-len(header))/len(pad))
	body += strings.Repeat("#", room-len(body))
	require.Len(t, body, room)

	_, o := LoadOverrides(Default(), writeOverride(t, body))
	require.NoError(t, o.Err)

	// One byte over is not.
	got, o := LoadOverrides(Default(), writeOverride(t, body+"#"))
	require.Error(t, o.Err)
	assert.Contains(t, o.Err.Error(), "65536")
	assert.Same(t, Default(), got)
}

// A row of an override file for a flag whose text reaches a note (the usage-limit notice, the words a role said) never matches
// the input line: what the owner types is not for a note, whatever the row says or leaves out.
func TestOverrideRowsOfNoteFlagsNeverMatchTheInputLine(t *testing.T) {
	// (The embedded rows that read the same lines are taken away, so that it is the crew's rows that answer.)
	body := "  - id: claude.now.wrap-indent\n    disabled: true\n" +
		row("acme.limit", "claude", UsageLimit, `regexp: 'typed'`) +
		row("acme.say", "claude", SayMark, `prefix: '❯'`) +
		row("acme.cont", "claude", SayCont, `contains: 'typed'`) +
		row("acme.stop", "claude", SayStop, `contains: 'typed'`) +
		row("acme.hint", "claude", FooterHint, `contains: 'typed'`)

	tb, o := LoadOverrides(Default(), writeOverride(t, "rules:\n"+body))
	require.NoError(t, o.Err)

	for _, line := range []string{"❯ words typed here", "  ❯ words typed here", "\t❯ typed"} {
		for _, effect := range []string{UsageLimit, SayCont, SayStop} {
			_, ok := tb.Test(Claude, effect, line)
			assert.False(t, ok, "%s: %q", effect, line)
		}

		_, ok := tb.Test(Claude, SayMark, line)
		assert.False(t, ok, "say_mark: %q", line)
	}

	// The same rows read what is on the screen otherwise.
	for _, effect := range []string{UsageLimit, SayCont, SayStop} {
		_, ok := tb.Test(Claude, effect, "⎿ words typed here")
		assert.True(t, ok, effect)
	}

	_, ok := tb.Test(Claude, SayMark, "❯ x")
	assert.False(t, ok)

	// A flag whose text reaches no note is not held to it, and nor are the embedded rows.
	_, ok = tb.Test(Claude, FooterHint, "❯ words typed here")
	assert.True(t, ok, "footer_hint")

	table, err := NewTable([]Rule{{ID: "acme.limit", Engine: Claude, Sets: UsageLimit, Means: "m", Match: Match{Regexp: `typed`}}})
	require.NoError(t, err)

	_, ok = table.Test(Claude, UsageLimit, "❯ words typed here")
	assert.True(t, ok, "a table built for a test keeps its row as it was written")
}

// A row an override file adds or replaces says so on its hits, so that a reader can treat what it matched as a crew's words;
// an embedded row, and a row of a table built for a test, does not, and a file cannot say it.
func TestRowsOfAnOverrideFileAreMarked(t *testing.T) {
	tb, o := LoadOverrides(Default(), writeOverride(t, "rules:\n"+
		row("acme.limit", "claude", UsageLimit, `contains: 'plan exhausted'`)+
		row("claude.peek.hint-update", "claude", FooterHint, `contains: 'Update failed'`)))
	require.NoError(t, o.Err)

	hit, ok := tb.Test(Claude, UsageLimit, "plan exhausted")
	require.True(t, ok)
	assert.True(t, hit.Rule.Override, "added")

	hit, ok = tb.Test(Claude, FooterHint, "Update failed")
	require.True(t, ok)
	assert.True(t, hit.Rule.Override, "replaced")

	hit, ok = tb.Test(Claude, UsageLimit, "You've hit your limit")
	require.True(t, ok)
	assert.False(t, hit.Rule.Override, "an embedded row")

	hit, ok = Default().Test(Claude, UsageLimit, "You've hit your limit")
	require.True(t, ok)
	assert.False(t, hit.Rule.Override)

	got, o := LoadOverrides(Default(), writeOverride(t, "rules:\n"+row("acme.says", "claude", UsageLimit, `contains: 'x'`, "    override: true\n")))
	require.Error(t, o.Err, "override is no field of a row")
	assert.Same(t, Default(), got)
}
