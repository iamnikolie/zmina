package lean

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCSVSummary(t *testing.T) {
	dir := t.TempDir()
	src := "id;name;price;created;ok\n" +
		"1;alex;1.5;2026-01-02;true\n" +
		"2;\"acme; inc\nsecond line\";2;2026-01-03;false\n" +
		"3;;x;;\n" +
		"4;bo;3\n"
	write(t, dir, "shop.csv", src)

	out := Read([]Target{{Path: "shop.csv"}}, dir, Options{Outline: true})
	assert.Equal(t, "== shop.csv · 6 lines · 111 · csv · summary:\n"+
		"columns (5, separated by \";\", by the first line):\n"+
		"  1 id\tint\n"+
		"  2 name\ttext, 1 empty\n"+
		"  3 price\tnumber 75%, text 25%\n"+
		"  4 created\tdate, 1 empty, 1 missing\n"+
		"  5 ok\tbool, 1 empty, 1 missing\n"+
		"rows: 4 after the header line; 1 of another width (first at line 6)\n"+
		"first rows:\n"+
		"1\tid;name;price;created;ok\n"+
		"2\t1;alex;1.5;2026-01-02;true\n"+
		"3\t2;\"acme; inc\n"+
		"4\tsecond line\";2;2026-01-03;false\n"+
		"5\t3;;x;;\n"+
		"6\t4;bo;3\n"+
		"read rows: zmina read shop.csv:1-50 | zmina grep <regex> shop.csv · all of it: --full\n", out)

	assert.Contains(t, Read([]Target{{Path: "shop.csv"}}, dir, Options{}), "6\t4;bo;3\n", "a small one is read whole")

	big := "a,b\n" + strings.Repeat("1,"+strings.Repeat("x", 400)+"\n", 100)
	p := write(t, dir, "big.csv", big)
	out = Read([]Target{{Path: "big.csv"}}, dir, Options{})
	assert.Contains(t, out, "· csv · summary:\ncolumns (2, separated by \",\", by the first line):\n  1 a\tint\n  2 b\ttext\nrows: 100 after")
	assert.Contains(t, out, "first rows:\n1\ta,b\n2\t1,"+strings.Repeat("x", 298)+"…\n", "a row cut at 300 bytes")
	assert.Contains(t, out, "last row:\n101\t1,")
	assert.Less(t, len(out), 3000)

	d := Guard(HookInput{Cwd: dir, ToolName: "Read", ToolInput: map[string]any{"file_path": p}}, "/opt/zmina", nil)
	assert.Equal(t, Decision{}, d, "the summary shows rows: never the guard's answer")
}

// A TSV has no quoting: a quote is a character, a row is a line.
func TestTSVSummary(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "rights.tsv", "form\tcaption\n\"main\tSave \"all\n\"edit\t\n")

	out := Read([]Target{{Path: "rights.tsv"}}, dir, Options{Outline: true})
	assert.Contains(t, out, "columns (2, separated by tab, by the first line):\n  1 form\ttext\n  2 caption\ttext, 1 empty\n"+
		"rows: 2 after the header line\n")
}

func TestJSONLSummary(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "events.jsonl", `{"id":1,"name":"alex","tags":["a"],"at":"2026-01-02T10:00:00Z"}
{"id":2,"name":"acme","extra":{"k":1}}
not json

{"id":3.5,"name":null}
`)

	out := Read([]Target{{Path: "events.jsonl"}}, dir, Options{Outline: true})
	assert.Contains(t, out, "· jsonl · summary:\nkeys (5, of the objects on each line):\n"+
		"  id\tnumber\n"+
		"  name\ttext, 1 empty\n"+
		"  tags\tarray, 2 missing\n"+
		"  at\tdate, 2 missing\n"+
		"  extra\tobject, 2 missing\n"+
		"rows: 4; 1 not a JSON object (first at line 3)\n"+
		"first rows:\n1\t{\"id\":1,")
}

func TestCSVByteOrderMark(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "x.csv", "\ufeffid,name\n1,alex\n")
	write(t, dir, "x.jsonl", "\ufeff{\"id\":1}\n")

	assert.Contains(t, Read([]Target{{Path: "x.csv"}}, dir, Options{Outline: true}), "\n  1 id\tint\n")
	assert.Contains(t, Read([]Target{{Path: "x.jsonl"}}, dir, Options{Outline: true}), "keys (1, of the objects on each line):\n  id\tint\nrows: 1\n")
}
