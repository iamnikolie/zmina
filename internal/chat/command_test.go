package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseCommandTakesOnlyTheWholeRecord(t *testing.T) {
	name, args, ok := ParseCommand("<command-name>/effort</command-name>\n  <command-message>effort</command-message>\n  <command-args>high</command-args>")
	assert.True(t, ok)
	assert.Equal(t, []string{"/effort", "high"}, []string{name, args})

	name, args, ok = ParseCommand("<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>the diff</command-args>")
	assert.True(t, ok, "the elements in another order")
	assert.Equal(t, []string{"/review", "the diff"}, []string{name, args})

	for _, text := range []string{
		"/compact",
		"see <command-name>/compact</command-name> here",
		"<command-name>/compact</command-args>",
		"<command-message>compact</command-message>",
		"<command-name>compact</command-name>",
	} {
		_, _, ok := ParseCommand(text)
		assert.False(t, ok, text)
	}
}

func TestParseLocalOutputDropsEscapesAndHooks(t *testing.T) {
	out, failed, ok := ParseLocalOutput("<local-command-stdout>\x1b[2mCompacted (ctrl+o to see full summary)\x1b[22m\n" +
		"\x1b[2mPreCompact [command hook 1] completed successfully\x1b[22m\n\x1b[2mPostCompact [command hook 2] failed: exit 1\x1b[22m\n" +
		"\x1b[2mPreCompact [bash \"/home/alex/hooks/save.sh\"] completed successfully\x1b[22m\n" +
		"\x1b[2mPreCompact [bash \"/home/alex/hooks/note.sh\"] failed with exit code 2\x1b[22m</local-command-stdout>")
	assert.True(t, ok)
	assert.False(t, failed)
	assert.Equal(t, "Compacted (ctrl+o to see full summary)", out)

	// A line of the command's own that only looks bracketed stays.
	out, _, _ = ParseLocalOutput("<local-command-stdout>Set model to [Opus] for this session</local-command-stdout>")
	assert.Equal(t, "Set model to [Opus] for this session", out)

	out, failed, ok = ParseLocalOutput("<local-command-stderr>no such model</local-command-stderr>")
	assert.True(t, ok)
	assert.True(t, failed)
	assert.Equal(t, "no such model", out)

	_, _, ok = ParseLocalOutput("<local-command-stdout>a</local-command-stderr>")
	assert.False(t, ok)
}

func TestStripANSI(t *testing.T) {
	assert.Equal(t, "dim bold link title", StripANSI("\x1b[2mdim\x1b[22m \x1b[1;31mbold\x1b[0m \x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\ \x1b]0;t\x07title"))
	assert.Equal(t, "plain [2m stays", StripANSI("plain [2m stays"))
	assert.Equal(t, "lone ", StripANSI("lone \x1b"))
}

func TestCommandEventWords(t *testing.T) {
	assert.Equal(t, "Context compacted", CommandEvent("/compact", "", "Compacted", false).Summary)
	assert.Equal(t, "/model · Set model to Opus", CommandEvent("/model", "", "Set model to Opus", false).Summary)
	e := CommandEvent("/context", "all", "## Context Usage\n\n| a | b |", false)
	assert.Equal(t, "/context all · Context Usage", e.Summary)
	assert.NotEmpty(t, e.Detail)
	assert.Equal(t, "failed", CommandEvent("/compact", "", "Error: nothing to compact", true).Status)
	assert.Equal(t, "/compact · Error: nothing to compact", CommandEvent("/compact", "", "Error: nothing to compact", true).Summary)
}
