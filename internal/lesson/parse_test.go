package lesson

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pilotText = `# Lessons from the shift

Some words before the first block are left out.
Why: this line is not in a block either.

### Run make test before every push
Why: CI is strict and a red pipeline costs a round trip.
Applies to: every seat
In a file already: AGENTS.md

### Quote zsh globs
Why: an unquoted glob that matches nothing aborts the whole command.
Applies to: this role
In a file already: no

### The staging box resets on Mondays
Why: data written there on a Friday is gone by the next week.
Applies to: one task
In a file already: -

### Ask before touching the lockfile
Why: two seats once edited it at the same time.
Applies to: not sure
`

func TestParseMarkdownSeveralBlocks(t *testing.T) {
	got, err := ParseMarkdown(pilotText)
	require.NoError(t, err)
	require.Len(t, got, 4)

	assert.Equal(t, "Run make test before every push", got[0].Rule)
	assert.Equal(t, "CI is strict and a red pipeline costs a round trip.", got[0].Why)
	assert.Equal(t, Every, got[0].Applies)
	assert.Equal(t, "AGENTS.md", got[0].InFile)

	assert.Equal(t, "Quote zsh globs", got[1].Rule)
	assert.Equal(t, Role, got[1].Applies)
	assert.Empty(t, got[1].InFile, "no is no file")

	assert.Equal(t, Task, got[2].Applies)
	assert.Empty(t, got[2].InFile, "- is no file")

	assert.Equal(t, "Ask before touching the lockfile", got[3].Rule)
	assert.Equal(t, Unsure, got[3].Applies)
	assert.Empty(t, got[3].InFile, "a missing key is no file")

	for _, l := range got {
		assert.Empty(t, l.ID, "the store gives the id")
		assert.Empty(t, l.State)
		assert.True(t, l.At.IsZero())
	}
}

func TestParseMarkdownMultiLineWhy(t *testing.T) {
	got, err := ParseMarkdown(`### Pin the Go version
Why: a bump to 1.25 slipped in with a dependency
and broke the build on the older toolchain.

It happened twice:
- once in the spring
- once last week
Applies to: every seat
`)
	require.NoError(t, err)
	require.Len(t, got, 1)
	want := "a bump to 1.25 slipped in with a dependency\nand broke the build on the older toolchain.\n\n" +
		"It happened twice:\n- once in the spring\n- once last week"
	assert.Equal(t, want, got[0].Why, "line breaks stay, the ends are trimmed")
}

func TestParseMarkdownWhyOnTheNextLine(t *testing.T) {
	got, err := ParseMarkdown("### Rule\nWhy:\n  the reason starts below the key  \nApplies to: all\n")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "the reason starts below the key", got[0].Why)
}

func TestParseMarkdownWhyEndsAtTheNextKeyOrHeading(t *testing.T) {
	got, err := ParseMarkdown(`### One
Applies to: task
Why: first
second
In a file already: docs/a.md
this line follows a key that is not Why and is left out
### Two
Why: other
Applies to: role
`)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "first\nsecond", got[0].Why)
	assert.Equal(t, "docs/a.md", got[0].InFile)
	assert.Equal(t, "other", got[1].Why)
}

func TestParseMarkdownKeysInAnyCase(t *testing.T) {
	got, err := ParseMarkdown("### Rule\nWHY: because\nAPPLIES TO: This Role\nin a file already: `docs/x.md`\n")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "because", got[0].Why)
	assert.Equal(t, Role, got[0].Applies)
	assert.Equal(t, "docs/x.md", got[0].InFile, "backticks around a path are dropped")
}

func TestParseMarkdownAppliesVariants(t *testing.T) {
	for in, want := range map[string]Applies{
		"every seat": Every, "Every": Every, "all": Every,
		"this role": Role, "role": Role, "one role": Role,
		"one task": Task, "TASK": Task,
		"not sure": Unsure, "unsure": Unsure,
	} {
		got, err := ParseMarkdown("### r\nWhy: w\nApplies to: " + in + "\n")
		require.NoError(t, err, in)
		require.Len(t, got, 1, in)
		assert.Equal(t, want, got[0].Applies, in)
	}

	got, err := ParseMarkdown("### r\nWhy: w\nApplies: every seat\n")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, Every, got[0].Applies, "Applies without to is read too")
}

func TestParseMarkdownInFileVariants(t *testing.T) {
	for in, want := range map[string]string{
		"no": "", "No": "", "none": "", "NONE": "", "-": "", "": "", "   ": "",
		"AGENTS.md": "AGENTS.md", "internal/crew/crew.go": "internal/crew/crew.go", "  skills/x/SKILL.md  ": "skills/x/SKILL.md",
		"notes": "notes",
	} {
		got, err := ParseMarkdown("### r\nWhy: w\nApplies to: all\nIn a file already: " + in + "\n")
		require.NoError(t, err, "%q", in)
		require.Len(t, got, 1, "%q", in)
		assert.Equal(t, want, got[0].InFile, "%q", in)
	}
}

func TestParseMarkdownErrorsNameTheLine(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"no why", "### r\nApplies to: all\n", "line 1: no Why"},
		{"empty why", "### r\nWhy:\nApplies to: all\n", "line 1: no Why"},
		{"no applies", "### r\nWhy: w\n", "line 1: no Applies to"},
		{"empty applies", "### r\nWhy: w\nApplies to:\n", "line 1: no Applies to"},
		{"bad applies", "### r\nWhy: w\nApplies to: whoever\n", `line 1: applies to "whoever"`},
		{"no rule", "### \nWhy: w\nApplies to: all\n", "line 1: no rule"},
		{"bare heading", "###\nWhy: w\nApplies to: all\n", "line 1: no rule"},
		{"why twice", "### r\nWhy: a\nWhy: b\nApplies to: all\n", "line 1: Why twice"},
		{"applies twice", "### r\nWhy: a\nApplies to: all\nApplies to: role\n", "line 1: Applies to twice"},
		{"file twice", "### r\nWhy: a\nApplies to: all\nIn a file already: a\nIn a file already: b\n", "line 1: In a file already twice"},
		{
			"the heading's line, after other text",
			"intro\n\n### ok\nWhy: w\nApplies to: all\n\n\n### broken\nApplies to: task\n",
			"line 8: no Why",
		},
		{"crlf counts the same", "a\r\n### r\r\nWhy: w\r\n", "line 2: no Applies to"},
	} {
		got, err := ParseMarkdown(tc.text)
		require.Error(t, err, tc.name)
		assert.Contains(t, err.Error(), tc.want, tc.name)
		assert.Nil(t, got, tc.name)
	}
}

func TestParseMarkdownAllOrNothing(t *testing.T) {
	text := pilotText + "\n### One more without a reason\nApplies to: all\n"

	got, err := ParseMarkdown(text)
	require.Error(t, err)
	assert.Nil(t, got, "four good blocks are not returned with a bad fifth")

	at := slices.Index(strings.Split(text, "\n"), "### One more without a reason")
	require.Positive(t, at)
	assert.Contains(t, err.Error(), fmt.Sprintf("line %d: no Why", at+1), "the heading's line, counted from 1")
}

func TestParseMarkdownFirstErrorWins(t *testing.T) {
	_, err := ParseMarkdown("### a\nApplies to: all\n### b\nWhy: w\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line 1:")
	assert.NotContains(t, err.Error(), "line 3")
}

func TestParseMarkdownNoBlocks(t *testing.T) {
	for _, text := range []string{"", "\n\n", "just a paragraph\nWhy: not in a block\n", "## Not a lesson\n#### nor this\n"} {
		got, err := ParseMarkdown(text)
		require.NoError(t, err, "%q", text)
		assert.Empty(t, got, "%q", text)
	}
}

func TestParseMarkdownWhatIsAHeading(t *testing.T) {
	got, err := ParseMarkdown(`### First
Why: kept
#### a sub-heading is part of the reason
###not a heading
Applies to: all
`)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "kept\n#### a sub-heading is part of the reason\n###not a heading", got[0].Why)

	got, err = ParseMarkdown("   ### indented a little\nWhy: w\nApplies to: all\n")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "indented a little", got[0].Rule)
}

func TestParseMarkdownHorizontalRuleEndsTheWhy(t *testing.T) {
	got, err := ParseMarkdown("### a\nWhy: because\n---\nApplies to: all\n\n### b\nWhy: also\n___\nApplies to: role\n")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "because", got[0].Why)
	assert.Equal(t, "also", got[1].Why)
}

func TestParseMarkdownCRLF(t *testing.T) {
	got, err := ParseMarkdown("### Rule\r\nWhy: line one\r\nline two\r\nApplies to: every seat\r\nIn a file already: none\r\n")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Rule", got[0].Rule)
	assert.Equal(t, "line one\nline two", got[0].Why)
	assert.Equal(t, Every, got[0].Applies)
	assert.Empty(t, got[0].InFile)
}

func TestParseMarkdownRuleKeepsItsText(t *testing.T) {
	got, err := ParseMarkdown("###   Why: is not a key in a heading  \nWhy: w\nApplies to: all\n")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Why: is not a key in a heading", got[0].Rule)
}
