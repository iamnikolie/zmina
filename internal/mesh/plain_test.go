package mesh

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPlainDropsWhatATerminalActsOn(t *testing.T) {
	for in, want := range map[string]string{
		"a plain line":                   "a plain line",
		"Привіт, світе":                  "Привіт, світе",
		"clear \x1b[2J\x1b[Hscreen":      "clear [2J[Hscreen",
		"title \x1b]0;pwned\x07 done":    "title ]0;pwned done",
		"up\rovertype":                   "up overtype",
		"two\nlines\tand a tab":          "two lines and a tab",
		"del\x7fete and \x00 nul":        "delete and  nul",
		"c1 \u009b31m red":               "c1 31m red",
		"bidi \u202eevil\u202c text":     "bidi evil text",
		"zero\u200bwidth":                "zerowidth",
		"bad \xff\xfe bytes":             "bad ? bytes",
		"a sum 0123abcd and a path /x/y": "a sum 0123abcd and a path /x/y",
	} {
		assert.Equal(t, want, Plain(in), in)
	}
}

func TestPlainTextKeepsLinesAndTabs(t *testing.T) {
	assert.Equal(t, "- old\n+\tnew\n context [31mred\n", PlainText("- old\r\n+\tnew\n context \x1b[31mred\n"))
}

type plainFields struct {
	Why    string            `json:"why"`
	Tasks  []plainTask       `json:"tasks"`
	Taken  map[string]string `json:"taken"`
	Raw    []byte            `json:"raw"`
	Ptr    *plainTask        `json:"ptr"`
	Count  int               `json:"count"`
	hidden string
}

type plainTask struct{ ID, Note string }

func TestPlainAllCleansEveryStringOfAStruct(t *testing.T) {
	f := plainFields{
		Why:    "a\x1b[2Jb",
		Tasks:  []plainTask{{ID: "T-1\x1b", Note: "ok"}, {ID: "T-2", Note: "x\x07y"}},
		Taken:  map[string]string{"T-1\x1b[": "beta/\x1b[2JT-9", "T-3": "beta/T-4"},
		Raw:    []byte("keep\x1b"),
		Ptr:    &plainTask{ID: "\x1b[31mT", Note: "n"},
		Count:  3,
		hidden: "h\x1b",
	}

	PlainAll(&f)

	assert.Equal(t, "a[2Jb", f.Why)
	assert.Equal(t, []plainTask{{ID: "T-1", Note: "ok"}, {ID: "T-2", Note: "xy"}}, f.Tasks)
	assert.Equal(t, map[string]string{"T-1[": "beta/[2JT-9", "T-3": "beta/T-4"}, f.Taken)
	assert.Equal(t, []byte("keep\x1b"), f.Raw, "bytes are the printer's to make plain")
	assert.Equal(t, &plainTask{ID: "[31mT", Note: "n"}, f.Ptr)
	assert.Equal(t, 3, f.Count)
	assert.Equal(t, "h\x1b", f.hidden, "a field only this package sees is left")
}
