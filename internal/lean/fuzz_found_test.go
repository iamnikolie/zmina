package lean

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Found by FuzzOutline: keys that share a line, and a declaration the parser cut short, ended before they began, so a
// read of the key showed "1-0" and no lines.
func TestOutlineRangesNeverEndBeforeTheyStart(t *testing.T) {
	cases := map[string]string{
		"f.json": "{\"a\": {\"b\": 1}, \"d\": \"e\"}\n",
		"f.yaml": "{a: 1, b: {c: 2}, d: 3}\n",
		"f.go":   "package a\nfunc",
	}

	for path, src := range cases {
		t.Run(path, func(t *testing.T) {
			syms := Outline(path, []byte(src))
			require.NotEmpty(t, syms)

			for _, s := range syms {
				assert.GreaterOrEqual(t, s.End, s.Start, "%+v", s)
			}
		})
	}
}

func TestReadAKeyOnTheSameLineAsTheNext(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "one.json", "{\"a\": [1, 2], \"b\": 2}\n")

	out := Read([]Target{{Path: "one.json", Symbol: "a"}}, dir, Options{})
	assert.Contains(t, out, "1-1 \"a\":")
	assert.Contains(t, out, "1\t{\"a\": [1, 2], \"b\": 2}", "the line the key is on is shown")
}

// Found by FuzzParseTarget: a line number too long for an int wrapped around into a negative range.
func TestParseTargetLineNumbersTooBigForAnInt(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", goSrc)

	for _, arg := range []string{"a.go:99999999999999999999", "a.go:1-99999999999999999999", "a.go:99999999999999999999-",
		"a.go:99999999999999999999-99999999999999999999", "a.go:9223372036854775807"} {
		t.Run(arg, func(t *testing.T) {
			tg := ParseTarget(arg, dir)
			assert.Equal(t, "a.go", tg.Path)
			assert.GreaterOrEqual(t, tg.From, 0)
			assert.GreaterOrEqual(t, tg.To, 0)
			assert.NotPanics(t, func() { Read([]Target{tg}, dir, Options{}) })
		})
	}

	tg := ParseTarget("a.go:99999999999999999999", dir)
	assert.Equal(t, maxLine-10, tg.From)
	assert.Equal(t, maxLine+30, tg.To, "the lines around a line past the end of the file: none")
	assert.Equal(t, Target{Path: "a.go", From: 1, To: maxLine}, ParseTarget("a.go:1-99999999999999999999", dir))
}
