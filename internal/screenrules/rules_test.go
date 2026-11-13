package screenrules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbeddedTablesLoad(t *testing.T) {
	tb, err := Load()
	require.NoError(t, err)

	assert.Len(t, tb.IDs(), len(tb.Rules()), "every id is used once")

	engines := map[string]int{}

	for _, r := range tb.Rules() {
		engines[r.Engine]++

		assert.NotEmpty(t, r.Means, r.ID)
		assert.True(t, strings.HasPrefix(r.ID, r.Engine+"."), "%s is in the %s table", r.ID, r.Engine)
	}

	assert.Positive(t, engines[Claude])
	assert.Positive(t, engines[Codex])
}

// Every flag a reader looks for has a row in the Claude table, and the Codex table has the limit notice: a reader that
// finds none would read nothing, silently.
func TestEveryFlagTheCodeReadsHasARow(t *testing.T) {
	tb := Default()

	for _, effect := range Effects() {
		assert.True(t, tb.Has(Claude, effect), "no claude row sets %s", effect)
		assert.NotEmpty(t, EffectDoc(effect), effect)
	}

	assert.True(t, tb.Has(Codex, UsageLimit))
}

// The builtin ids the code names are the builtin rows of the tables, and no builtin row is left unnamed.
func TestBuiltinRowsAreTheOnesTheCodeNames(t *testing.T) {
	named := []string{BuiltinInputLine, BuiltinBridgeMode, BuiltinNowLine, BuiltinDialogParse, BuiltinTrustAnswer,
		BuiltinPeekFooter, BuiltinChatDialog, BuiltinExcerpt}

	var have []string
	for _, r := range Default().Builtins() {
		have = append(have, r.ID)

		assert.Equal(t, KindBuiltin, r.Kind)
		assert.Empty(t, r.Sets, r.ID)
	}

	assert.ElementsMatch(t, named, have)
}

func TestMatcherReadsLiteralPrefixAndRegexp(t *testing.T) {
	tb, err := build([]Rule{
		{ID: "claude.t.contains", Engine: Claude, Sets: FooterHint, Means: "m", Match: Match{Contains: "esc to"}},
		{ID: "claude.t.prefix", Engine: Claude, Sets: SayMark, Means: "m", Match: Match{Prefix: "⏺", Trim: true}},
		{ID: "claude.t.re", Engine: Claude, Sets: ExitDialog, Means: "m", Match: Match{Regexp: `(\d)\. Exit`}},
		{ID: "claude.t.lower", Engine: Claude, Sets: TrustYes, Means: "m", Match: Match{Contains: "YES", Lower: true}},
		{ID: "claude.t.skip", Engine: Claude, Sets: UsageLimit, Means: "m", Match: Match{Contains: "limit", Trim: true, SkipPrefix: "❯"}},
	})
	require.NoError(t, err)

	_, ok := tb.Test(Claude, FooterHint, "  hold esc to interrupt")
	assert.True(t, ok)
	_, ok = tb.Test(Claude, FooterHint, "ESC TO")
	assert.False(t, ok, "a literal is case sensitive unless the row lower-cases")

	h, ok := tb.Test(Claude, SayMark, "   ⏺  said this")
	require.True(t, ok)
	assert.Equal(t, "  said this", h.Rest, "the rest is what follows the prefix in the trimmed text")
	assert.Equal(t, "   ⏺  said this", h.Line, "the hit keeps the text as it was given")

	h, ok = tb.Test(Claude, ExitDialog, "2. Exit and stop")
	require.True(t, ok)
	assert.Equal(t, []string{"2. Exit", "2"}, h.Sub)

	_, ok = tb.Test(Claude, TrustYes, "Yes, Proceed")
	assert.True(t, ok, "lower: the text and the literal are lower-cased")

	_, ok = tb.Test(Claude, UsageLimit, "  hit a limit")
	assert.True(t, ok)
	_, ok = tb.Test(Claude, UsageLimit, "  ❯ hit a limit")
	assert.False(t, ok, "skip_prefix is tried after the trim")

	_, ok = tb.Test(Codex, UsageLimit, "hit a limit")
	assert.False(t, ok, "a row of the other engine does not read it")
}

func TestHigherPriorityIsTriedFirst(t *testing.T) {
	tb, err := build([]Rule{
		{ID: "claude.low", Engine: Claude, Sets: FooterHint, Means: "m", Match: Match{Contains: "a"}, Priority: 1},
		{ID: "claude.same", Engine: Claude, Sets: FooterHint, Means: "m", Match: Match{Contains: "a"}, Priority: 5},
		{ID: "claude.first", Engine: Claude, Sets: FooterHint, Means: "m", Match: Match{Contains: "a"}, Priority: 5},
	})
	require.NoError(t, err)

	h, ok := tb.Test(Claude, FooterHint, "a")
	require.True(t, ok)
	assert.Equal(t, "claude.same", h.Rule.ID, "of one priority the file order decides")
}

func TestScanReadsEachRowsRegion(t *testing.T) {
	screen := strings.Join([]string{
		"limit one", "noise", "limit two", "noise", "noise", "❯ limit in the box", "noise tail",
	}, "\n")

	tb, err := build([]Rule{
		{ID: "claude.bottom", Engine: Claude, Sets: UsageLimit, Means: "m", Match: Match{Contains: "limit"}, Region: Region{LastLines: 3}},
	})
	require.NoError(t, err)

	hits := tb.Scan(Claude, UsageLimit, screen)
	require.Len(t, hits, 1, "only the last three rows are read")
	assert.Equal(t, 5, hits[0].N)
	assert.Equal(t, "❯ limit in the box", hits[0].Line)
	assert.Equal(t, 3, tb.Window(Claude, UsageLimit, 99), "the reader captures what the row reads")
	assert.Equal(t, 99, tb.Window(Claude, ExitDialog, 99), "no row names a count: the reader's own")

	whole, err := build([]Rule{
		{ID: "claude.whole", Engine: Claude, Sets: UsageLimit, Means: "m", Match: Match{Contains: "limit"}},
	})
	require.NoError(t, err)
	assert.Len(t, whole.Scan(Claude, UsageLimit, screen), 3, "no region: every row it was given")

	text, err := build([]Rule{
		{ID: "claude.text", Engine: Claude, Sets: ExitDialog, Means: "m", Match: Match{Regexp: `(\d)\. Exit and stop`}, Scope: ScopeText,
			Region: Region{LastLines: 2}},
	})
	require.NoError(t, err)

	hits = text.Scan(Claude, ExitDialog, "1. Exit and stop\nx\n2. Exit and stop")
	require.Len(t, hits, 1)
	assert.Equal(t, "2. Exit and stop", hits[0].Line)
	assert.Equal(t, -1, hits[0].N, "a text-scope row has no row of its own")
	assert.Equal(t, "2", hits[0].Sub[1])
}

func TestScanReadsTheInputBox(t *testing.T) {
	screen := strings.Join([]string{
		"❯ not a box: no rule above", "words", "──────────", "❯ draft with TODO", "  second row TODO", "──────────", "footer TODO",
	}, "\n")

	tb, err := build([]Rule{
		{ID: "claude.box", Engine: Claude, Sets: FooterHint, Means: "m", Match: Match{Contains: "TODO"}, Region: Region{InputBox: true}},
	})
	require.NoError(t, err)

	var got []string
	for _, h := range tb.Scan(Claude, FooterHint, screen) {
		got = append(got, h.Line)
	}

	assert.Equal(t, []string{"❯ draft with TODO", "  second row TODO"}, got, "the rows from the ❯ under a rule to the next rule")
	assert.Empty(t, tb.Scan(Claude, FooterHint, "no box\nTODO"), "no input box on the screen: nothing to read")
}

func TestStripRemovesWhatTheRowsMatch(t *testing.T) {
	assert.Equal(t, "Which one?", Default().Strip(Claude, ChatFrame, "│ Which one? │"))
	assert.Equal(t, "plain", Default().Strip(Claude, ChatFrame, "plain"))
}

func TestTail(t *testing.T) {
	assert.Equal(t, "c\nd", Tail("a\nb\nc\nd", 2))
	assert.Equal(t, "a\nb", Tail("a\nb", 5))
	assert.Equal(t, "a\nb", Tail("a\nb", 0))
}

// A table that does not read fails at load and names the row.
func TestInvalidRowsFailAtLoadNamingTheRow(t *testing.T) {
	good := Rule{ID: "claude.ok", Engine: Claude, Sets: UsageLimit, Means: "m", Match: Match{Contains: "x"}}

	cases := map[string]struct {
		row  Rule
		want string
	}{
		"bad regexp": {Rule{ID: "claude.bad-re", Engine: Claude, Sets: UsageLimit, Means: "m", Match: Match{Regexp: "("}},
			`rule "claude.bad-re": bad regexp`},
		"unknown flag": {Rule{ID: "claude.flag", Engine: Claude, Sets: "no_such", Means: "m", Match: Match{Contains: "x"}},
			`rule "claude.flag": sets "no_such" is no flag`},
		"two matchers": {Rule{ID: "claude.two", Engine: Claude, Sets: UsageLimit, Means: "m", Match: Match{Contains: "x", Prefix: "y"}},
			`rule "claude.two": match has exactly one`},
		"no matcher": {Rule{ID: "claude.none", Engine: Claude, Sets: UsageLimit, Means: "m"},
			`rule "claude.none": match has exactly one`},
		"engine": {Rule{ID: "gemini.x", Engine: "gemini", Sets: UsageLimit, Means: "m", Match: Match{Contains: "x"}},
			`rule "gemini.x": engine is claude or codex`},
		"no means": {Rule{ID: "claude.nomeans", Engine: Claude, Sets: UsageLimit, Match: Match{Contains: "x"}},
			`rule "claude.nomeans": no means`},
		"scope": {Rule{ID: "claude.scope", Engine: Claude, Sets: UsageLimit, Means: "m", Match: Match{Contains: "x"}, Scope: "page"},
			`rule "claude.scope": scope is line or text`},
		"region": {Rule{ID: "claude.region", Engine: Claude, Sets: UsageLimit, Means: "m", Match: Match{Contains: "x"},
			Region: Region{LastLines: 3, InputBox: true}}, `rule "claude.region": region has last_lines and input_box`},
		"groups": {Rule{ID: "claude.groups", Engine: Claude, Sets: DialogOption, Means: "m", Match: Match{Regexp: `(\d)`}},
			`rule "claude.groups": the regexp has 1 groups`},
		"literal where groups are read": {Rule{ID: "claude.lit", Engine: Claude, Sets: ExitDialog, Means: "m", Match: Match{Contains: "x"}},
			`rule "claude.lit": a row that sets exit_dialog is a regexp`},
		"builtin with a matcher": {Rule{ID: "claude.b", Engine: Claude, Kind: KindBuiltin, Means: "m", Match: Match{Contains: "x"}},
			`rule "claude.b": a builtin row names logic that stays in Go`},
		"kind":      {Rule{ID: "claude.k", Engine: Claude, Kind: "magic", Means: "m"}, `rule "claude.k": kind is match or builtin`},
		"no id":     {Rule{Engine: Claude, Sets: UsageLimit, Means: "m", Match: Match{Contains: "x"}}, "no id"},
		"duplicate": {good, `rule "claude.ok": the id is used twice`},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := build([]Rule{good, c.row})
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

func TestEmbeddedFilesAreReadStrictly(t *testing.T) {
	_, err := decode("x.yaml", []byte("rules:\n  - id: claude.x\n    engine: claude\n    sets: usage_limit\n    means: m\n    matches: {contains: a}\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "matches", "a misspelled field is an error, never a row that silently does less")

	rows, err := decode("empty.yaml", nil)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func writeOverride(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "screen-rules.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return path
}

func TestNoOverrideFileChangesNothing(t *testing.T) {
	base := Default()

	got, o := LoadOverrides(base, filepath.Join(t.TempDir(), "screen-rules.yaml"))
	assert.Same(t, base, got)
	assert.NoError(t, o.Err)
	assert.False(t, o.InForce())
	assert.Zero(t, o.Count)
}

func TestOverrideAddsReplacesAndDisablesByID(t *testing.T) {
	base := Default()
	path := writeOverride(t, `
rules:
  - id: acme.limit.wording
    engine: claude
    sets: usage_limit
    means: a new wording of the notice
    match:
      contains: "plan exhausted"
    region:
      last_lines: 12
  - id: claude.peek.hint-update
    engine: claude
    sets: footer_hint
    means: the update notice, as a newer release words it
    match:
      contains: "Update failed"
  - id: claude.peek.hint-todos
    disabled: true
`)

	got, o := LoadOverrides(base, path)
	require.NoError(t, o.Err)
	assert.True(t, o.InForce())
	assert.Equal(t, 3, o.Count)
	assert.Equal(t, []string{"acme.limit.wording"}, o.Added)
	assert.Equal(t, []string{"claude.peek.hint-update"}, o.Replaced)
	assert.Equal(t, []string{"claude.peek.hint-todos"}, o.Disabled)
	assert.NotSame(t, base, got)

	// Added: the new wording is a limit notice, and the capture is as long as the longest row asks.
	_, ok := got.Test(Claude, UsageLimit, "plan exhausted")
	assert.True(t, ok)
	assert.Equal(t, 12, got.Window(Claude, UsageLimit, 8))

	// Replaced: the old wording no longer matches, the new one does.
	_, ok = got.Test(Claude, FooterHint, "Auto-update failed")
	assert.False(t, ok)
	_, ok = got.Test(Claude, FooterHint, "Update failed")
	assert.True(t, ok)

	// Disabled.
	_, ok = got.Test(Claude, FooterHint, "ctrl+t to hide todos")
	assert.False(t, ok)

	// The embedded table itself is untouched: what was put over it is a new table.
	_, ok = base.Test(Claude, FooterHint, "ctrl+t to hide todos")
	assert.True(t, ok)
	_, ok = base.Test(Claude, UsageLimit, "plan exhausted")
	assert.False(t, ok)
}

// A bad file is refused whole: the embedded table stays in force and the error names the row.
func TestBadOverrideFileIsRefusedWhole(t *testing.T) {
	base := Default()

	cases := map[string]struct{ body, want string }{
		"bad regexp": {"rules:\n  - id: acme.re\n    engine: claude\n    sets: usage_limit\n    means: m\n    match: {regexp: '('}\n",
			`rule "acme.re": bad regexp`},
		"builtin":   {"rules:\n  - id: claude.input.line\n    disabled: true\n", `rule "claude.input.line": it is a builtin`},
		"unknown":   {"rules:\n  - id: claude.no.such\n    disabled: true\n", `rule "claude.no.such": disabled, but no embedded rule has that id`},
		"twice":     {"rules:\n  - id: claude.peek.hint-todos\n    disabled: true\n  - id: claude.peek.hint-todos\n    disabled: true\n", `rule "claude.peek.hint-todos": the id is used twice`},
		"field":     {"rules:\n  - id: acme.x\n    engine: claude\n    sets: usage_limit\n    means: m\n    mach: {contains: a}\n", "mach"},
		"not yaml":  {"rules: [", "yaml"},
		"no id":     {"rules:\n  - engine: claude\n", "has no id"},
		"half good": {"rules:\n  - id: claude.peek.hint-todos\n    disabled: true\n  - id: acme.bad\n    engine: claude\n    sets: nope\n    means: m\n    match: {contains: a}\n", `rule "acme.bad": sets "nope"`},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, o := LoadOverrides(base, writeOverride(t, c.body))
			require.Error(t, o.Err)
			assert.Contains(t, o.Err.Error(), c.want)
			assert.Same(t, base, got, "the embedded rules stay in force")
			assert.False(t, o.InForce())

			_, ok := got.Test(Claude, FooterHint, "ctrl+t to hide todos")
			assert.True(t, ok, "not even the good half of a bad file is in force")
		})
	}
}

func TestOverrideFileThatCannotBeReadIsAnError(t *testing.T) {
	dir := t.TempDir()

	got, o := LoadOverrides(Default(), dir)
	require.Error(t, o.Err, "a directory is not a file of rules")
	assert.Same(t, Default(), got)
}
