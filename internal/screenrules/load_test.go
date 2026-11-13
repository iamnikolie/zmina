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
