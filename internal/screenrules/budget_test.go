package screenrules

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A row of an override file is held to a smaller program than an embedded one, and the file as a whole to a budget: what one
// tick spends on a screen is every row's program over every line, so a file of many rows that each pass is bounded too.
func TestOverrideProgramsAreBoundedPerRowAndInAll(t *testing.T) {
	// 243 instructions a row: inside the row's bound.
	accepted(t, row("acme.fine", "claude", "usage_limit", `regexp: '(?:.?){120}Z'`))

	// 402: more than a row of an override may have, though an embedded row may be that large.
	refused(t, row("acme.big", "claude", "usage_limit", `regexp: '(?:a?){200}'`), "acme.big", "program")

	var rows strings.Builder

	for i := range 8 {
		rows.WriteString(row(fmt.Sprintf("acme.r%d", i), "claude", "usage_limit", `regexp: '(?:.?){120}Z'`))
	}

	// Eight of them are 1944 instructions: inside the budget of the file.
	accepted(t, rows.String())

	// A ninth is past it, and the file is refused whole, naming the row that crossed it.
	rows.WriteString(row("acme.r8", "claude", "usage_limit", `regexp: '(?:.?){120}Z'`))
	refused(t, rows.String(), "acme.r8", "in all")
}

// A literal costs nothing against the budget, and neither does a row that only disables.
func TestOverrideBudgetCountsRegexpProgramsOnly(t *testing.T) {
	var rows strings.Builder

	for i := range 40 {
		rows.WriteString(row(fmt.Sprintf("acme.lit%d", i), "claude", "usage_limit", `contains: 'x'`))
	}

	rows.WriteString("  - id: claude.peek.hint-todos\n    disabled: true\n")
	accepted(t, rows.String())
}

// What an accepted override file may cost one beat on one screen is bounded: every row of the most the file may list, each
// reading the lines of the longest screen the readers capture.
func TestWorstAcceptedOverrideReadsAScreenInTime(t *testing.T) {
	var rows strings.Builder

	// The overridable flags, the worst row spread over them: every row is read once a line by its own reader.
	effects := []string{UsageLimit, SayMark, ToolCall, SayCont, SayStop, FooterHint, FooterOption, FooterModel}

	for i, effect := range effects {
		rows.WriteString(row(fmt.Sprintf("acme.worst%d", i), "claude", effect, `regexp: '(?:.?){120}Z'`))
	}

	tb, o := LoadOverrides(Default(), writeOverride(t, "rules:\n"+rows.String()))
	require.NoError(t, o.Err)
	require.Equal(t, len(effects), o.Count)

	screen := strings.Repeat(strings.Repeat("a", 200)+"\n", MaxLastLines)

	tries := 5
	if raceBuild {
		tries = 1
	}

	start := time.Now()

	for range tries {
		tb.Scan(Claude, UsageLimit, screen)

		for _, line := range strings.Split(screen, "\n") {
			for _, effect := range effects[1:] {
				tb.Test(Claude, effect, line)
			}
		}
	}

	took := time.Since(start) / time.Duration(tries)
	t.Logf("one screen of %d lines of 200 characters: %v", MaxLastLines, took)

	// Generous, a bound on the order of the work and not a measurement: a fraction of a second on a laptop.
	if !raceBuild {
		assert.Less(t, took, time.Second, "a screen read with the largest accepted file")
	}
}

// A line is matched on its first 512 characters: a regexp is never run over more than that, whatever the pane's width. The
// hit keeps the line whole.
func TestLongLinesAreCutForMatching(t *testing.T) {
	tb, err := NewTable([]Rule{
		{ID: "acme.limit.re", Engine: Claude, Sets: UsageLimit, Means: "m", Match: Match{Regexp: `(Z+)$`}},
	})
	require.NoError(t, err)

	_, ok := tb.Test(Claude, UsageLimit, strings.Repeat("a", 500)+"ZZ")
	assert.True(t, ok, "inside the 512 characters the end of the line is the line's end")

	_, ok = tb.Test(Claude, UsageLimit, strings.Repeat("a", 600)+"ZZ")
	assert.False(t, ok, "past them nothing is read: the end of what is read is a, not Z")

	long := strings.Repeat("é", 511) + "Z" + strings.Repeat("é", 500)
	tb, err = NewTable([]Rule{
		{ID: "acme.limit.z", Engine: Claude, Sets: UsageLimit, Means: "m", Match: Match{Contains: "Z"}},
	})
	require.NoError(t, err)

	h, ok := tb.Test(Claude, UsageLimit, long)
	assert.True(t, ok, "the 512th character is read: characters, not bytes")
	assert.Equal(t, long, h.Line, "the hit keeps the line as it stands")

	_, ok = tb.Test(Claude, UsageLimit, strings.Repeat("é", 512)+"Z")
	assert.False(t, ok, "the 513th is not")

	// Each line of a screen is cut, not the screen: a hit on the last line of 80 is read.
	hits := tb.Scan(Claude, UsageLimit, strings.Repeat(strings.Repeat("a", 600)+"\n", 3)+"x Z")
	require.Len(t, hits, 1)
	assert.Equal(t, "x Z", hits[0].Line)

	// A text-scope row reads each of its lines to 512 characters too.
	text, err := NewTable([]Rule{
		{ID: "claude.trust.q", Engine: Claude, Sets: TrustQuestion, Means: "m", Match: Match{Contains: "ZZ"}, Region: Region{LastLines: 10}},
	})
	require.NoError(t, err)

	assert.Empty(t, text.Scan(Claude, TrustQuestion, strings.Repeat("a", 600)+"ZZ"), "the ZZ is past the 512th character of its line")

	hits = text.Scan(Claude, TrustQuestion, strings.Repeat("a", 600)+"\nZZ")
	require.Len(t, hits, 1, "and a short line under it is a line of its own, read whole")
	assert.Equal(t, "ZZ", hits[0].Line)
}
