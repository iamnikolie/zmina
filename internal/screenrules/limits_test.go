package screenrules

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// row is one override row as the file writes it.
func row(id, engine, sets, match string, extra ...string) string {
	return fmt.Sprintf("  - id: %s\n    engine: %s\n    sets: %s\n    means: m\n    match: {%s}\n%s", id, engine, sets, match,
		strings.Join(extra, ""))
}

// refused says an override file is refused whole, naming the row.
func refused(t *testing.T, body, id, why string) {
	t.Helper()

	got, o := LoadOverrides(Default(), writeOverride(t, "rules:\n"+body))
	require.Error(t, o.Err, "the file is refused")
	assert.Contains(t, o.Err.Error(), `rule "`+id+`"`)
	assert.Contains(t, o.Err.Error(), why)
	assert.Same(t, Default(), got, "the embedded rules stay in force")
}

func accepted(t *testing.T, body string) {
	t.Helper()

	_, o := LoadOverrides(Default(), writeOverride(t, "rules:\n"+body))
	require.NoError(t, o.Err)
}

// An override regexp is bounded: the matcher runs it over every row of every pane at every beat, so a pattern that unrolls
// into a huge program would stall the ticker. A source over the bound, or a program over it once its repeats are unrolled, is
// refused with the row's id.
func TestOverrideRegexpsAreBounded(t *testing.T) {
	refused(t, row("acme.unrolled", "claude", "usage_limit", `regexp: '(?:a?){1000}'`), "acme.unrolled", "program")
	refused(t, row("acme.nested", "claude", "usage_limit", `regexp: '((a{100}){100}){100}'`), "acme.nested", "")
	refused(t, row("acme.long", "claude", "usage_limit", `regexp: '`+strings.Repeat("a", 513)+`'`), "acme.long", "512")
	refused(t, row("acme.longliteral", "claude", "usage_limit", `contains: '`+strings.Repeat("a", 513)+`'`), "acme.longliteral", "512")

	accepted(t, row("acme.fine", "claude", "usage_limit", `regexp: '(?i)(plan|quota) (exhausted|used up){1,3}'`))
	accepted(t, row("acme.longish", "claude", "usage_limit", `regexp: '`+strings.Repeat("a", 512)+`'`))
}

// The embedded rows pass the bounds with room to spare, so the bounds are not set to the rows' own size.
func TestEmbeddedRowsAreWellInsideTheBounds(t *testing.T) {
	most := 0

	for _, r := range Default().Rules() {
		if r.Match.Regexp == "" {
			continue
		}

		assert.LessOrEqual(t, len(r.Match.Regexp), maxRegexpSource/2, r.ID)

		n, err := programSize(r.Match.Regexp)
		require.NoError(t, err, r.ID)
		assert.LessOrEqual(t, n, maxProgram/2, "%s: a program of %d instructions", r.ID, n)

		most = max(most, n)
	}

	t.Logf("the largest embedded program is %d instructions; the bound is %d", most, maxProgram)
}

func TestOverrideRowsAreCapped(t *testing.T) {
	var rows strings.Builder

	for i := range maxOverrideRows {
		rows.WriteString(row(fmt.Sprintf("acme.r%d", i), "claude", "usage_limit", `contains: 'x'`))
	}

	accepted(t, rows.String())

	got, o := LoadOverrides(Default(), writeOverride(t, "rules:\n"+rows.String()+row("acme.one-more", "claude", "usage_limit", `contains: 'x'`)))
	require.Error(t, o.Err)
	assert.Contains(t, o.Err.Error(), fmt.Sprintf("at most %d", maxOverrideRows))
	assert.Same(t, Default(), got)
}

// A row no reader evaluates is refused at load, with the id: it would do nothing and pass for a rule that did.
func TestRowsNoReaderEvaluatesAreRefused(t *testing.T) {
	// An engine's row for a flag only the other engine's readers read.
	refused(t, row("acme.codex-footer", "codex", "footer_hint", `contains: 'x'`), "acme.codex-footer", "Claude screen")
	accepted(t, row("acme.codex-limit", "codex", "usage_limit", `contains: 'x'`))

	// The limit notice is read a line at a time.
	refused(t, row("acme.limit-text", "claude", "usage_limit", `contains: 'x'`, "    scope: text\n"), "acme.limit-text", "scope")

	// Region and scope on flags whose readers ignore them.
	refused(t, row("acme.hint-region", "claude", "footer_hint", `contains: 'x'`, "    region: {last_lines: 5}\n"), "acme.hint-region", "region")
	refused(t, row("acme.hint-scope", "claude", "footer_hint", `contains: 'x'`, "    scope: text\n"), "acme.hint-scope", "scope")
	refused(t, row("acme.say-box", "claude", "say_mark", `prefix: 'x'`, "    region: {input_box: true}\n"), "acme.say-box", "region")

	// (A table built for a test: an override file may not touch the exit question at all.)
	_, err := NewTable([]Rule{{ID: "claude.exit-box", Engine: Claude, Sets: ExitDialog, Means: "m", Match: Match{Regexp: `(\d)\. Exit`},
		Region: Region{InputBox: true}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `rule "claude.exit-box"`)
	assert.Contains(t, err.Error(), "input_box")

	// What a reader does read: the limit notice's rows and box, the capture of the trust and exit questions.
	accepted(t, row("acme.limit-rows", "claude", "usage_limit", `contains: 'x'`, "    region: {last_lines: 10}\n"))
	accepted(t, row("acme.limit-box", "claude", "usage_limit", `contains: 'x'`, "    region: {input_box: true}\n"))
}

// The embedded trust option row names no region: the trust question's rows say how much of the screen is captured, and the
// option row's own count was never read.
func TestEmbeddedTrustOptionRowNamesNoRegion(t *testing.T) {
	r, ok := Default().Rule("claude.trust.option")
	require.True(t, ok)
	assert.Equal(t, Region{}, r.Region)

	q, ok := Default().Rule("claude.trust.question")
	require.True(t, ok)
	assert.Equal(t, 30, q.Region.LastLines)
	assert.Equal(t, 30, Default().Window(Claude, TrustOption, 30), "the capture is the same")
}

// An override reads no further up the screen than the capture the ticker and explain both take, so that the two cannot see
// different rows.
func TestOverrideCannotReadAboveTheCapture(t *testing.T) {
	accepted(t, row("acme.limit-max", "claude", "usage_limit", `contains: 'x'`, fmt.Sprintf("    region: {last_lines: %d}\n", MaxLastLines)))

	refused(t, row("acme.limit-far", "claude", "usage_limit", `contains: 'x'`, fmt.Sprintf("    region: {last_lines: %d}\n", MaxLastLines+1)),
		"acme.limit-far", fmt.Sprintf("%d", MaxLastLines))

	// And no embedded row asks for more.
	for _, r := range Default().Rules() {
		assert.LessOrEqual(t, r.Region.LastLines, MaxLastLines, r.ID)
	}
}

// protectedFlags are the flags whose rows decide keys zmina presses — the exit question's answer, the trust question's cursor
// and yes, a dialog's choices and the browser chat's rows — or gate the dialog they are pressed into. An override changes the
// wording of what zmina says it sees, never what it presses.
var protectedFlags = []string{ExitDialog, TrustQuestion, TrustOption, TrustSelected, TrustYes, DialogOption, DialogFooter,
	DialogFree, DialogTitle, ChatRow, ChatFrame}

func TestOverrideCannotChangeWhichKeysArePressed(t *testing.T) {
	protected := map[string]bool{}
	for _, f := range protectedFlags {
		protected[f] = true
	}

	seen := 0

	for _, r := range Default().Rules() {
		if r.Kind != KindMatch {
			continue
		}

		if !protected[r.Sets] {
			// Wording a human reads, or the peek footer: overridable.
			continue
		}

		seen++

		refused(t, fmt.Sprintf("  - id: %s\n    disabled: true\n", r.ID), r.ID, "keys")
		refused(t, row(r.ID, r.Engine, r.Sets, `contains: 'x'`), r.ID, "keys")
	}

	assert.Equal(t, 13, seen, "every embedded row of a protected flag was tried")

	// Adding a row for such a flag would change the keys as well.
	for _, f := range protectedFlags {
		match := `contains: 'x'`
		if info := effects[f]; info.groups > 0 {
			match = `regexp: '` + strings.Repeat("(x)", info.groups) + `'`
		}

		refused(t, row("acme.added-"+f, "claude", f, match), "acme.added-"+f, "keys")
	}

	// What a human reads, and the peek footer, stay overridable.
	accepted(t, "  - id: claude.limit.notice\n    disabled: true\n")
	accepted(t, "  - id: claude.peek.hint-todos\n    disabled: true\n")
	accepted(t, row("claude.now.tool-call", "claude", "tool_call", `regexp: '^Bash\('`))
}
