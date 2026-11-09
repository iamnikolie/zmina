package lean

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const goSrc = `package acme

import (
	"fmt"
	"strings"
)

// Shop sells things.
type Shop struct {
	Name string
}

const (
	A = 1
	B = 2
)

// Open opens the shop.
func (s *Shop) Open(day string) error {
	fmt.Println(strings.ToUpper(day))

	return nil
}

func Close() { fmt.Println("closed") }
`

const mdSrc = "# Guide\n\nIntro.\n\n## Install\n\nRun it.\n\n```sh\n# not a heading\n```\n\n### From source\n\nBuild.\n\n## Usage\n\nUse it.\n"

func write(t *testing.T, dir, name, body string) string {
	t.Helper()

	p := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(body), 0o644))

	return p
}

func TestGoOutline(t *testing.T) {
	syms := Outline("shop.go", []byte(goSrc))

	var got []string
	for _, s := range syms {
		got = append(got, fmt.Sprintf("%d-%d %s | %s", s.Start, s.End, s.Name, s.Label))
	}

	assert.Equal(t, []string{
		"3-6 imports | imports (2)",
		"8-11 Shop | type Shop struct",
		"13-16 A,B | const ( A, B )",
		"18-23 Shop.Open | func (s *Shop) Open(day string) error",
		"25-25 Close | func Close()",
	}, got, "a range starts at its doc comment; a one-line func shows no body")
}

func TestMarkdownOutline(t *testing.T) {
	syms := Outline("guide.md", []byte(mdSrc))

	var got []string
	for _, s := range syms {
		got = append(got, fmt.Sprintf("%d-%d %d %s", s.Start, s.End, s.Depth, s.Name))
	}

	assert.Equal(t, []string{"1-19 0 Guide", "5-16 1 Install", "13-16 2 From source", "17-19 1 Usage"}, got,
		"a # inside a fence is no heading; a section runs to the next heading of its level or above")
}

func TestYAMLAndJSONOutline(t *testing.T) {
	roles := "roles:\n"
	for i := range 30 {
		roles += fmt.Sprintf("  r%d:\n    keep: always\n", i)
	}

	y := Outline("crew.yaml", []byte("namespace: /ns\n"+roles+"timezone: UTC\n"))
	require.GreaterOrEqual(t, len(y), 33)
	assert.Equal(t, "namespace", y[0].Name)
	assert.Equal(t, Symbol{Start: 2, End: 62, Depth: 0, Name: "roles", Label: "roles:"}, y[1])
	assert.Equal(t, "roles.r0", y[2].Name, "the keys under a big top key")
	assert.Equal(t, "timezone", y[len(y)-1].Name)

	j := Outline("p.json", []byte("{\n  \"name\": \"acme\",\n  \"deps\": {\n    \"a\": \"1\",\n    \"b\": \"2\"\n  },\n  \"list\": [{\"x\": 1}]\n}\n"))

	var got []string
	for _, s := range j {
		got = append(got, fmt.Sprintf("%d-%d %s", s.Start, s.End, s.Name))
	}

	assert.Equal(t, []string{"2-2 name", "3-6 deps", "4-4 deps.a", "5-6 deps.b", "7-8 list"}, got)
	assert.Nil(t, Outline("bad.json", []byte("{nope")))
}

func TestParseTarget(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", goSrc)
	write(t, dir, "d.md", mdSrc)
	write(t, dir, "odd:name.txt", "x")

	assert.Equal(t, Target{Path: "a.go"}, ParseTarget("a.go", dir))
	assert.Equal(t, Target{Path: "a.go", Symbol: "Shop.Open"}, ParseTarget("a.go:Shop.Open", dir))
	assert.Equal(t, Target{Path: "a.go", From: 3, To: 9}, ParseTarget("a.go:3-9", dir))
	assert.Equal(t, Target{Path: "a.go", From: 12}, ParseTarget("a.go:12-", dir))
	assert.Equal(t, Target{Path: "a.go", From: 30, To: 70}, ParseTarget("a.go:40", dir), "the lines around 40")
	assert.Equal(t, Target{Path: "d.md", Heading: "From source"}, ParseTarget("d.md#From source", dir))
	assert.Equal(t, Target{Path: "odd:name.txt"}, ParseTarget("odd:name.txt", dir), "a path that exists wins")
}

func TestReadSmallWholeBigOutline(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", goSrc)

	out := Read([]Target{{Path: "a.go"}}, dir, Options{})
	assert.Contains(t, out, "== a.go · 25 lines")
	assert.Contains(t, out, "19\tfunc (s *Shop) Open(day string) error {\n", "small: whole, numbered")

	out = Read([]Target{{Path: "a.go"}}, dir, Options{Max: 450})
	assert.Contains(t, out, "outline (lines, size):")
	assert.Contains(t, out, "18-23\tfunc (s *Shop) Open(day string) error\t")
	assert.Contains(t, out, "read a piece: zmina read a.go:Name")
	assert.NotContains(t, out, "fmt.Println(strings")

	out = Read([]Target{{Path: "a.go"}}, dir, Options{Max: 450, Full: true})
	assert.Contains(t, out, "fmt.Println(strings.ToUpper(day))", "--full: no cap, no outline")
}

func TestReadSymbolSectionRange(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", goSrc)
	write(t, dir, "d.md", mdSrc)

	out := Read([]Target{{Path: "a.go", Symbol: "Open"}, {Path: "d.md", Heading: "install"}}, dir, Options{})
	assert.Contains(t, out, "18-23 func (s *Shop) Open(day string) error\n18\t// Open opens the shop.\n")
	assert.Contains(t, out, "5-16 ## Install\n5\t## Install\n", "a heading by any case, with its subsections")
	assert.Contains(t, out, "15\tBuild.")
	assert.NotContains(t, out, "Use it.")

	out = Read([]Target{{Path: "d.md", Heading: "Instal guide"}}, dir, Options{})
	assert.Contains(t, out, `"Instal guide" not found; near: Guide`)

	big := strings.Repeat("a line of text that is long enough\n", 2000)
	write(t, dir, "big.log", big)

	out = Read([]Target{{Path: "big.log", From: 100, To: 1900}}, dir, Options{})
	assert.LessOrEqual(t, len(out), DefaultMax)
	assert.Regexp(t, `… cut at line \d+ of 2,000; next: zmina read big\.log:\d+-1900\n$`, out)

	out = Read([]Target{{Path: "big.log"}}, dir, Options{})
	assert.Contains(t, out, "no outline for this type; its start:")
	assert.Regexp(t, `next: zmina read big\.log:\d+-\n$`, out)
}

func TestReadKindsAndCap(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.sum", "x v1 h1:abc\n")
	write(t, dir, "bin.dat", "a\x00b")
	write(t, dir, "a.go", goSrc)

	out := Read([]Target{{Path: "go.sum"}, {Path: "bin.dat"}, {Path: "nope.go"}}, dir, Options{})
	assert.Contains(t, out, "== go.sum · 1 lines · 12 · generated (lock, minified or generated file), not shown")
	assert.Contains(t, out, "== bin.dat · 0 lines · 3 · binary, not shown")
	assert.Contains(t, out, "== nope.go · no such file")

	out = Read([]Target{{Path: "go.sum", From: 1}}, dir, Options{})
	assert.Contains(t, out, "1\tx v1 h1:abc", "a range reads a generated file")

	out = Read([]Target{{Path: "a.go"}, {Path: "a.go", Symbol: "Close"}, {Path: "a.go", Symbol: "Shop"}}, dir, Options{Max: 500})
	assert.Contains(t, out, "… not shown (cap 500): ")

	out = Read([]Target{{Path: "a.go", Symbol: "Close"}}, dir, Options{Via: "cat a.go"})
	assert.True(t, strings.HasPrefix(out, "(read-guard: `cat a.go` would print over 12.0k characters; this is zmina read. The raw output: command cat a.go)\n"))
}

func TestGrep(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", goSrc)
	write(t, dir, "d.md", mdSrc)
	write(t, dir, "node_modules/x.js", "Open")
	write(t, dir, ".hidden/x.go", "Open")

	out, err := Grep("Open|Use", nil, dir, GrepOptions{})
	require.NoError(t, err)
	assert.Equal(t, "a.go (2)\n"+
		"  func (s *Shop) Open(day string) error · 18-23 (2)\n"+
		"    18: // Open opens the shop.\n"+
		"    19: func (s *Shop) Open(day string) error {\n"+
		"d.md (1)\n"+
		"  ## Usage · 17-19 (1)\n"+
		"    19: Use it.\n"+
		"— 3 hits in 2 files; shown 3\n", out, "skips node_modules and hidden directories")

	out, err = Grep("fmt", []string{"a.go"}, dir, GrepOptions{PerFile: 1})
	require.NoError(t, err)
	assert.Contains(t, out, "— 3 hits in 1 files; shown 1; 2 more in the files above (--per-file 4)")

	out, err = Grep("nothing here", nil, dir, GrepOptions{Fixed: true})
	require.NoError(t, err)
	assert.Equal(t, "no match for \"nothing here\"\n", out)

	_, err = Grep("(", nil, dir, GrepOptions{})
	require.Error(t, err)

	for i := range 40 {
		write(t, dir, fmt.Sprintf("many/f%02d.txt", i), strings.Repeat("needle in a haystack line\n", 3))
	}

	out, err = Grep("needle", []string{"many"}, dir, GrepOptions{Max: 600})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(out), 900)
	assert.Contains(t, out, "not shown, over the 600 budget: many/f")
}

func TestGuardBash(t *testing.T) {
	dir := t.TempDir()
	big := write(t, dir, "big.go", goSrc+strings.Repeat("// filler line for size\n", 800))
	write(t, dir, "small.go", goSrc)
	write(t, dir, ".hidden/big.go", goSrc+strings.Repeat("// filler line for size\n", 800))
	out := write(t, t.TempDir(), "big.go", goSrc+strings.Repeat("// filler line for size\n", 800))

	bash := func(cmd string) Decision {
		return Guard(HookInput{Cwd: dir, PermissionMode: "bypassPermissions", ToolName: "Bash",
			ToolInput: map[string]any{"command": cmd}}, "/opt/zmina", nil)
	}

	d := bash("cat big.go")
	assert.Equal(t, "rewrite", d.Kind)
	assert.Equal(t, "/opt/zmina read --via 'cat big.go' big.go", d.Command)
	assert.Equal(t, big, d.Path)

	d = bash("cd " + dir + " && sed -n '1,700p' big.go")
	assert.Equal(t, "cd "+dir+" && /opt/zmina read --via 'sed -n 1,700p big.go' big.go:1-700", d.Command)
	assert.Equal(t, big, d.Path)

	d = bash("cat " + big)
	assert.Equal(t, big, d.Path, "an absolute target")

	assert.True(t, strings.HasSuffix(bash("tail -n 800 big.go").Command, "' big.go:26-825"))
	assert.True(t, strings.HasSuffix(bash("head -700 big.go").Command, "' big.go:1-700"))

	for _, pass := range []string{"cat small.go", "cat big.go | grep x", "cat big.go > /tmp/x", "sed -n 1,5p big.go",
		"head big.go", "cat $F", "command cat big.go", "cat nope.go", "cat -A big.go", "cat *.go", "sed -i s/a/b/ big.go",
		"cat big.go; ls", "tail -f big.go", "cat .hidden/big.go", "cat " + out, "head -n 5 /dev/zero",
		"head -n 20 /dev/urandom"} {
		assert.Equal(t, Decision{}, bash(pass), pass)
	}

	assert.Equal(t, Decision{}, Guard(HookInput{Cwd: dir, PermissionMode: "bypassPermissions", ToolName: "Bash",
		ToolInput: map[string]any{"command": "cat big.go"}}, "", nil), "no executable known: nothing to rewrite to")

	for _, mode := range []string{"", "default", "acceptEdits", "plan", "auto"} {
		assert.Equal(t, Decision{}, Guard(HookInput{Cwd: dir, PermissionMode: mode, ToolName: "Bash",
			ToolInput: map[string]any{"command": "cat big.go"}}, "/opt/zmina", nil), "a rewritten command would wait for approval: "+mode)
	}
}

func TestGuardRead(t *testing.T) {
	dir := t.TempDir()
	filler := strings.Repeat("// filler line for size\n", 800)
	big := write(t, dir, "big.go", goSrc+filler)
	small := write(t, dir, "small.go", goSrc)
	img := write(t, dir, "pic.png", strings.Repeat("x", 20000))
	notes := write(t, dir, "notes.txt", strings.Repeat("SECRET_TOKEN=abc\n", 1000))
	env := write(t, dir, ".config/app.yaml", "a: 1\n"+strings.Repeat("# filler\n", 3000))
	log := write(t, dir, "app.log", strings.Repeat("a log line\n", 3000))
	outside := write(t, t.TempDir(), "big.go", goSrc+filler)
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "link.go")))
	require.NoError(t, os.Symlink(filepath.Dir(env), filepath.Join(dir, "conf")))
	require.NoError(t, os.Symlink(big, filepath.Join(dir, "inside.go")))

	read := func(in map[string]any) Decision {
		return Guard(HookInput{Cwd: dir, ToolName: "Read", ToolInput: in}, "/opt/zmina", nil)
	}

	d := read(map[string]any{"file_path": big})
	assert.Equal(t, "outline", d.Kind)
	assert.Contains(t, d.Reason, "read-guard: this file is 19.5k characters, so here is its outline")
	assert.Contains(t, d.Reason, "18-23\tfunc (s *Shop) Open(day string) error")
	assert.NotContains(t, d.Reason, "filler line", "names and ranges, never the file's lines")

	assert.Equal(t, "outline", read(map[string]any{"file_path": "big.go"}).Kind, "relative to the session's directory")
	assert.Equal(t, "outline", read(map[string]any{"file_path": filepath.Join(dir, "inside.go")}).Kind, "a link that stays inside")

	for name, in := range map[string]map[string]any{
		"limit":                  {"file_path": big, "limit": 50.0},
		"offset":                 {"file_path": big, "offset": 10.0},
		"small":                  {"file_path": small},
		"image":                  {"file_path": img},
		"a .txt with no outline": {"file_path": notes},
		"hidden":                 {"file_path": env},
		"no outline type":        {"file_path": log},
		"outside the cwd":        {"file_path": outside},
		"a link to outside":      {"file_path": filepath.Join(dir, "link.go")},
		"a link to a hidden dir": {"file_path": filepath.Join(dir, "conf", "app.yaml")},
		"a device":               {"file_path": "/dev/zero"},
	} {
		assert.Equal(t, Decision{}, read(in), name)
	}

	assert.Equal(t, Decision{}, Guard(HookInput{ToolName: "Read", ToolInput: map[string]any{"file_path": big}}, "/opt/zmina", nil),
		"no cwd: not sure what is the session's")
	assert.Equal(t, Decision{}, Guard(HookInput{Cwd: dir, ToolName: "Edit", ToolInput: map[string]any{"file_path": big}}, "/opt/zmina", nil))
}

func TestReadNeverLoopsOrHangs(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "one.txt", strings.Repeat("x", 13000)+"\n"+strings.Repeat(strings.Repeat("y", 99)+"\n", 100))
	write(t, dir, "app.min.js", strings.Repeat("var a=1;", 3000))

	out := Read([]Target{{Path: "one.txt"}}, dir, Options{})
	assert.Contains(t, out, "1\t"+strings.Repeat("x", LineMax)+"… [+11,000 chars: zmina read one.txt:1-1 --full]\n")
	assert.NotContains(t, out, "next: zmina read one.txt:1-", "never the same command again")
	assert.LessOrEqual(t, len(out), DefaultMax)

	out = Read([]Target{{Path: "one.txt", From: 1}}, dir, Options{})
	assert.Contains(t, out, "--full")

	out = Read([]Target{{Path: "app.min.js"}}, dir, Options{})
	assert.Contains(t, out, "generated (lock, minified or generated file), not shown; --full reads it")

	out = Read([]Target{{Path: "/dev/zero"}}, dir, Options{})
	assert.Contains(t, out, "not a regular file")

	require.NoError(t, syscallMkfifo(filepath.Join(dir, "pipe.txt")))

	got, err := Grep("x", nil, dir, GrepOptions{})
	require.NoError(t, err, "a FIFO in the tree is skipped, not read")
	assert.Contains(t, got, "one.txt (1)")
}

func TestParseTargetColonBeforeHash(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "x", "x")
	write(t, dir, "x#y.md", "# Y\n")

	assert.Equal(t, Target{Path: "x#y.md", From: 1, To: 5}, ParseTarget("x#y.md:1-5", dir), "the guard's sed target")

	only := t.TempDir()
	write(t, only, "x", "x")
	assert.Equal(t, Target{Path: "x", Heading: "y.md"}, ParseTarget("x#y.md", only))
}

func syscallMkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }

// A Markdown file of long table rows is text with headings, never "minified"; its rows are cut on display.
func TestLongRowMarkdownIsRead(t *testing.T) {
	dir := t.TempDir()

	var b strings.Builder

	b.WriteString("# Decisions\n\n")

	for i := range 120 {
		fmt.Fprintf(&b, "## D-%d\n\n| a | %s |\n\n", i, strings.Repeat("cell ", 600))
	}

	write(t, dir, "DECISIONS.md", b.String())
	write(t, dir, "notes.prg", strings.Repeat("x := '"+strings.Repeat("y", 700)+"'\n", 30))
	write(t, dir, "app.js", strings.Repeat("var a=1;", 3000)+"\n"+strings.Repeat("var b=2;", 3000)+"\n")
	write(t, dir, "data.json", "["+strings.Repeat(`{"a":"`+strings.Repeat("z", 900)+`"},`, 30)+"{}]\n")

	out := Read([]Target{{Path: "DECISIONS.md"}}, dir, Options{})
	assert.Contains(t, out, "· markdown · outline (lines, size):")
	assert.Contains(t, out, "\t## D-0\t")

	out = Read([]Target{{Path: "DECISIONS.md", Heading: "D-3"}}, dir, Options{})
	assert.Contains(t, out, "… [+1,008 chars: zmina read DECISIONS.md:17-17 --full]")

	out = Read([]Target{{Path: "DECISIONS.md", From: 17, To: 17}}, dir, Options{Full: true})
	assert.NotContains(t, out, "chars: zmina read", "--full: the row whole")

	out = Read([]Target{{Path: "notes.prg"}}, dir, Options{})
	assert.NotContains(t, out, "minified", "30 lines of 700: a legacy source, not a bundle")

	assert.Contains(t, Read([]Target{{Path: "app.js"}}, dir, Options{}), "generated (minified: long lines)")
	assert.Contains(t, Read([]Target{{Path: "data.json"}}, dir, Options{}), "generated (minified: long lines)")
}

// Legacy sources in Windows-1251 read, outline and grep as Cyrillic, not mojibake.
func TestCP1251(t *testing.T) {
	cp := func(s string) string {
		var out []byte

		for _, r := range s {
			switch {
			case r < 0x80:
				out = append(out, byte(r))
			case r >= 0x0410 && r <= 0x044F:
				out = append(out, byte(0xC0+r-0x0410))
			case r == 'Ё':
				out = append(out, 0xA8)
			case r == 'ё':
				out = append(out, 0xB8)
			case r == '№':
				out = append(out, 0xB9)
			}
		}

		return string(out)
	}

	dir := t.TempDir()
	write(t, dir, "costgroup.prg", cp("* Группы затрат\n\nFUNCTION Main()\n   && Резерв\n   ? \"Ёлка №1\"\nRETURN\n"))
	write(t, dir, "guide.md", cp("# Руководство\n\n## Установка\n\nЗапустить.\n"))

	got, enc := Decode([]byte(cp("&& Резерв")))
	assert.Equal(t, "&& Резерв", string(got))
	assert.Equal(t, "cp1251", enc)

	got, enc = Decode([]byte("plain ascii and UTF-8: Резерв"))
	assert.Equal(t, "plain ascii and UTF-8: Резерв", string(got))
	assert.Empty(t, enc)

	got, enc = Decode([]byte{'c', 'a', 'f', 0xE9, ' ', 0x80, 0x81, 0x82, 0x83})
	assert.Equal(t, "caf\uFFFD \uFFFD", string(got), "mostly not letters: not taken for cp1251 (a bad run is one U+FFFD)")
	assert.Empty(t, enc)

	got, enc = Decode(append([]byte("Привет, это обычный текст в UTF-8.\n"), 0xFF))
	assert.Equal(t, "Привет, это обычный текст в UTF-8.\n\uFFFD", string(got), "UTF-8 with a stray bad byte stays UTF-8")
	assert.Empty(t, enc)

	got, enc = Decode([]byte("Caf\xe9 cr\xe8me br\xfbl\xe9e \xe0 la fran\xe7aise"))
	assert.Empty(t, enc, "Latin-1 accents sit alone between ASCII: not cp1251")
	assert.Equal(t, "Caf\uFFFD cr\uFFFDme br\uFFFDl\uFFFDe \uFFFD la fran\uFFFDaise", string(got))

	got, enc = Decode([]byte(cp("Резерв на складе в цехе и у поставщика")))
	assert.Equal(t, "cp1251", enc, "one-letter words are few next to the runs of longer ones")
	assert.Equal(t, "Резерв на складе в цехе и у поставщика", string(got))

	out := Read([]Target{{Path: "costgroup.prg", From: 4, To: 5}}, dir, Options{})
	assert.Contains(t, out, "· cp1251 · 4-5\n4\t   && Резерв\n5\t   ? \"Ёлка №1\"\n")

	out = Read([]Target{{Path: "guide.md", Heading: "Установка"}}, dir, Options{})
	assert.Contains(t, out, "3-5 ## Установка\n3\t## Установка", "the outline of a cp1251 file is readable")

	out, err := Grep("Резерв", nil, dir, GrepOptions{})
	require.NoError(t, err)
	assert.Contains(t, out, "costgroup.prg (1)\n  FUNCTION Main() · 3-6 (1)\n    4: && Резерв\n", "a cp1251 .prg has its outline")
}

const foxSrc = `* Main program
DO Setup

*-- Opens the books.
* Two lines of header.
PROCEDURE OpenBooks(tcYear)
   LOCAL lcSql
   TEXT TO lcSql NOSHOW
      PROCEDURE NotABlock
   ENDTEXT
   func = 5 && a variable, not a block
   DO Work WITH ;
      "PROCEDURE NotEither"
ENDPROC

func CloseBooks
   RETURN .T.


DEFINE CLASS Ledger AS Custom
   nYear = 0

   * Posts one entry.
   PROTECTED PROCEDURE Post(tnSum)
      RETURN tnSum
   ENDPROC

   procedure Command1.Click
      THIS.Post(1)
ENDDEFINE

Defi Clas Report as Form
   PROC Init
ENDDEF
`

func TestFoxProOutline(t *testing.T) {
	syms := Outline("books.prg", []byte(foxSrc))

	type row = Symbol

	got := syms

	assert.Equal(t, []row{
		{4, 14, 0, "OpenBooks", "PROCEDURE OpenBooks(tcYear)"},
		{16, 17, 0, "CloseBooks", "func CloseBooks"},
		{20, 30, 0, "Ledger", "DEFINE CLASS Ledger AS Custom"},
		{23, 26, 1, "Ledger.Post", "PROTECTED PROCEDURE Post(tnSum)"},
		{28, 29, 1, "Ledger.Command1.Click", "procedure Command1.Click"},
		{32, 34, 0, "Report", "Defi Clas Report as Form"},
		{33, 33, 1, "Report.Init", "PROC Init"},
	}, got)

	dir := t.TempDir()
	write(t, dir, "books.prg", foxSrc)

	out := Read([]Target{ParseTarget("books.prg:command1.click", dir)}, dir, Options{})
	assert.Contains(t, out, "· 28-29 procedure Command1.Click\n28\t   procedure Command1.Click\n")

	out = Read([]Target{ParseTarget("books.prg:Post", dir)}, dir, Options{})
	assert.Contains(t, out, "23\t   * Posts one entry.\n", "a method by its name alone, with its header comment")

	out = Read([]Target{{Path: "books.prg"}}, dir, Options{Outline: true})
	assert.Contains(t, out, "· foxpro · outline (lines, size):\n4-14\tPROCEDURE OpenBooks(tcYear)\t")
	assert.Contains(t, out, "zmina read books.prg:Name (PROCEDURE, FUNCTION, Class.Method)")
}

// path:#Heading is read as the section, with a line that gives the form.
func TestReadColonHeading(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "d.md", mdSrc)

	tg := ParseTarget("d.md:#From source", dir)
	assert.Equal(t, Target{Path: "d.md", Heading: "From source", Colon: true}, tg)

	out := Read([]Target{tg}, dir, Options{})
	assert.Contains(t, out, "(read as d.md#From source; a section is path#Heading, without the colon)\n")
	assert.Contains(t, out, "13-16 ### From source\n")
}

func TestGrepCount(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", goSrc)
	write(t, dir, "d.md", mdSrc)
	write(t, dir, "n.txt", "it\nit\n")
	write(t, dir, "big.md", "it before\n"+func() string {
		var b strings.Builder
		for i := range 14 {
			fmt.Fprintf(&b, "## S%02d\nit\nit\n", i)
		}

		return b.String()
	}())

	out, err := Grep("fmt|Use|it", nil, dir, GrepOptions{Count: true})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "a.go 3\n"+
		"  1\timports (2) · 3-6\n"+
		"  1\tfunc (s *Shop) Open(day string) error · 18-23\n"+
		"  1\tfunc Close() · 25-25\n"+
		"big.md 29\n"+
		"  2\t## S00 · 2-4\n"), out)
	assert.Contains(t, out, "  2\t## S11 · 35-37\n  4\tin 2 more sections\n  1\toutside any section\n"+
		"d.md 2\n  1\t## Install · 5-16\n  1\t## Usage · 17-19\n"+
		"n.txt 2\n"+
		"— 36 hits in 4 files\n", "a file without an outline: its count alone")
	assert.NotContains(t, out, "S12")

	// The files' counts come first; sections only while they fit.
	out, err = Grep("it", nil, dir, GrepOptions{Count: true, Max: 200})
	require.NoError(t, err)
	assert.Equal(t, "big.md 29\nd.md 2\n  1\t## Install · 5-16\n  1\t## Usage · 17-19\nn.txt 2\n"+
		"— 33 hits in 3 files; sections of 1 files left out (-c on one file shows them)\n", out)
}

// names lists an outline as "depth start-end Name".
func names(syms []Symbol) []string {
	var out []string
	for _, s := range syms {
		out = append(out, fmt.Sprintf("%d %d-%d %s", s.Depth, s.Start, s.End, s.Name))
	}

	return out
}

func TestIndentOutline(t *testing.T) {
	py := "import os\n\n\n# Shop sells.\n@dataclass\nclass Shop:\n    name: str\n\n    def open(\n        self,\n        day: str,\n    ) -> None:\n" +
		"        print(day)\n" + strings.Repeat("        pass\n", 40) + "\n    def close(self):\n        \"\"\"Closes.\n\nAt night.\n\"\"\"\n        pass\n\n\n" +
		"ROWS = [\n    1,\n    2,\n    3,\n    4,\n    5,\n]\n\n\ndef main():\n    Shop('x').open('mon')\n"
	assert.Equal(t, []string{
		"0 4-60 Shop", "1 9-53 Shop.open", "1 55-60 Shop.close", "0 63-69 ROWS", "0 72-73 main",
	}, names(Outline("shop.py", []byte(py))), "a decorator and comment head the class; a docstring at column 0 stays inside")

	vb := "Public Class UnionsDC\n\n    Public Function InsertSQL(\n    ByVal ID As Integer,\n    ByVal Name As String) As String\n" +
		"        Dim s As String\n' a comment at column 0\n        Return s\n    End Function\n\n    Public Sub New()\n        x = 1\n    End Sub\nEnd Class\n"
	assert.Equal(t, []string{"0 1-14 UnionsDC", "1 3-9 UnionsDC.InsertSQL", "1 11-13 UnionsDC.New"}, names(Outline("dc.vb", []byte(vb))), "a class's members whatever its size")
	assert.Equal(t, []string{"0 1-54 UnionsDC", "1 3-49 UnionsDC.InsertSQL", "1 51-53 UnionsDC.New"},
		names(Outline("dc.vb", []byte(strings.Replace(vb, "        Return s\n", strings.Repeat("        Return s\n", 41), 1)))),
		"a signature over lines at one indent is one statement; End Function closes it")

	ts := "export const uk = {\n  'common.cancel': 'Cancel',\n  'common.ok': 'OK',\n}\n\nexport function sum(\n  a: number,\n  b: number,\n): number {\n" +
		"  return a + b\n}\n"
	assert.Equal(t, []string{"0 1-4 uk", "0 6-11 sum"}, names(Outline("m.ts", []byte(ts))), "quoted keys are no comments")

	sql := "-- the sheets\nCREATE TABLE IF NOT EXISTS sheets (\n  id BIGINT,\n  name TEXT,\n  a INT,\n  b INT,\n  c INT\n);\n\nCREATE INDEX ix ON sheets (id);\n"
	assert.Equal(t, []string{"0 1-8 sheets"}, names(indentOutline([]byte(sql), ".txt")), "a statement over lines is a block")
	assert.Equal(t, []string{"0 1-8 sheets", "0 10-10 ix"}, names(Outline("x.sql", []byte(sql))), ".sql: by statement")

	css := "#main {\n  color: red;\n}\n\n.btn,\n.link {\n  margin: 0;\n}\n"
	assert.Equal(t, []string{"0 1-3 #main", "0 6-8 .link"}, names(Outline("x.css", []byte(css))), "a selector list: its last line")

	php := "<?php\nclass Account\n{\n    public function fetch($id)\n    {\n        return $id;\n    }\n}\n"
	assert.Equal(t, []string{"0 2-8 Account", "1 4-7 Account.fetch"}, names(Outline("a.php", []byte(php))), "a brace on its own line opens the class")

	xml := "<Report>\n  <DataSets>\n    <DataSet Name=\"Orders\">\n      <Query>x</Query>\n    </DataSet>\n  </DataSets>\n</Report>\n"
	assert.Equal(t, []string{"0 1-7 Report"}, names(Outline("r.rdl", []byte(xml))))

	vue := "<template>\n  <XSlideout\n    :open=\"open\"\n  >\n    <div>hi</div>\n  </XSlideout>\n</template>\n"
	assert.Equal(t, []string{"0 1-7 template"}, names(Outline("c.vue", []byte(vue))))

	big := "<Report>\n  <DataSets>\n    <DataSet Name=\"Orders\">\n" + strings.Repeat("      <Field>x</Field>\n", 45) + "    </DataSet>\n  </DataSets>\n</Report>\n"
	assert.Equal(t, []string{"0 1-51 Report", "1 2-50 Report.DataSets", "2 3-49 Report.DataSets.Orders"}, names(Outline("r.rdl", []byte(big))),
		"elements are containers: their members are listed")

	pyUnder := "from x import (\n    a,\n    b,\n)\nfrom y import c\nfrom z import gettext as _\n\n\nclass Deferred:\n    def name(self):\n        return 1\n"
	assert.Equal(t, []string{"0 1-6 imports", "0 9-11 Deferred", "1 10-11 Deferred.name"}, names(Outline("m.py", []byte(pyUnder))),
		"imports in a row are one entry; \" _\" goes on to the next line only in Visual Basic")

	vbLabel := "Public Sub Send()\n    On Error GoTo send_Err\n    x = 1\nsend_Err:\n    With conn\n        .Close\n    End With\nEnd Sub\n\nPublic Sub Done()\n    y = 2\nEnd Sub\n"
	assert.Equal(t, []string{"0 1-8 Send", "0 10-12 Done"}, names(Outline("m.bas", []byte(vbLabel))), "a label at column 0 stays inside its Sub")

	sh := "usage() {\n  cat <<'EOF'\nUsage: run\n  deploy() {\nEOF\n}\n\nrun() {\n  echo $((1 << 2))\n}\n"
	assert.Equal(t, []string{"0 1-6 usage", "0 8-10 run"}, names(Outline("x.sh", []byte(sh))), "a here-document is no code; << alone opens none")

	shq := "#!/usr/bin/env bash\nlist() {\n  case \"$1\" in\n    *) OUT=\"${1%%\nv*}\" ;;\n  esac\n  echo \"$(which \"$1\") ($(sed -e \"s/\\`|'//g\"))\"\n}\n\n" +
		"if [ -t 1 ]; then\n  is_tty() {\n    true\n  }\nfi\n\nusage() {\n  echo \"Usage: run\n  deploy() {\n\"\n}\n"
	assert.Equal(t, []string{"0 2-8 list", "0 11-13 is_tty", "0 16-20 usage"}, names(Outline("x.sh", []byte(shq))),
		"quotes inside $( ) in a string, a string over lines, a case pattern; a function under a top-level if")
	assert.True(t, shSure([]byte(shq)))
	assert.False(t, shSure([]byte("run() {\n  echo \"open\n}\n")), "a string left open")

	dir := t.TempDir()
	write(t, dir, "shop.py", py)

	out := Read([]Target{ParseTarget("shop.py:close", dir)}, dir, Options{})
	assert.Contains(t, out, "· 55-60 def close(self):\n55\t    def close(self):\n")

	out = Read([]Target{{Path: "shop.py"}}, dir, Options{Outline: true})
	assert.Contains(t, out, "read a piece: zmina read shop.py:Name (def, class, Class.method)")

	write(t, dir, "shop.rb", "class Shop\n  def open\n  end\nend\n")
	out = Read([]Target{{Path: "shop.rb"}}, dir, Options{Outline: true})
	assert.Contains(t, out, "read a piece: zmina read shop.rb:Name (a function, class or block by its name; Class.method below the top)")

	deco := "\"\"\"Shop.\n\nclass Fake:\n\"\"\"\n\n\n@pytest.mark.parametrize(\n    \"day\",\n    [\"mon\", \"tue\"],\n)\ndef test_open(day):\n    assert day\n"
	assert.Equal(t, []string{"0 7-12 test_open"}, names(Outline("t.py", []byte(deco))),
		"a docstring is no block; a decorator over lines heads the def")
	assert.True(t, pySure([]byte(deco)))
	assert.False(t, pySure([]byte("def f():\n    \"\"\"Open.\n    return 1\n")), "a docstring left open")

	stubs := "import sys\n\nif sys.platform == \"win32\":\n    def home() -> str:\n        return \"C:\"\nelse:\n    def home() -> str:\n        return \"/\"\n\n\n" +
		"@overload\ndef get(key: str) -> str: ...\n@overload\ndef get(key: int) -> int: ...\n\nclass Box:\n    size: int\n    def open(self) -> None: ...\n"
	assert.Equal(t, []string{"0 4-5 home", "0 7-8 home", "0 11-12 get", "0 13-14 get", "0 16-18 Box", "1 18-18 Box.open"},
		names(Outline("s.pyi", []byte(stubs))), "defs under a top-level if, and one-line defs, are listed")

	quoted := "SEP = '\"\"\"'  # three quotes\nEND = '\"\"\"'\n\n\ndef f():\n    \"\"\"Doc.\n\nclass Fake:\n    \"\"\"\n    return 1\n"
	assert.Equal(t, []string{"0 5-10 f"}, names(Outline("q.py", []byte(quoted))), "quotes inside a string open nothing")
	assert.True(t, pySure([]byte(quoted)))

	vbq := "Public Function Esc(s As String) As String\n    Esc = \"say \"\"\" & s & \"\"\"\" ' a ` and a \"\"\"\nEnd Function\n\nPublic Sub Done()\n    y = 2\nEnd Sub\n"
	assert.Equal(t, []string{"0 1-3 Esc", "0 5-7 Done"}, names(Outline("m.bas", []byte(vbq))), "Visual Basic's \"\" opens no string over lines")

	cs := "class A\n{\n    string q = @\"say \"\"\"\" \";\n    void B()\n    {\n    }\n}\n"
	assert.Equal(t, []string{"0 1-7 A"}, names(Outline("a.cs", []byte(cs))), "C#'s @\"…\"\" opens no string over lines")
}

// A FoxPro form exported as text: each object is a class, its methods the procedures in it.
func TestFoxProFormText(t *testing.T) {
	form := "FORM: price\n\n===== OBJECT =====\nOBJNAME: Dataenvironment\nCLASS: dataenvironment\nMETHODS:\n===== END OBJECT =====\n\n" +
		"===== OBJECT =====\nOBJNAME: PRICE\nCLASS: kform\nMETHODS:\nPROCEDURE mylock\nThisForm.x = 1\nENDPROC\nPROCEDURE Init\nDODEFAULT()\nENDPROC\n===== END OBJECT =====\n"

	assert.Equal(t, []string{"0 3-7 Dataenvironment", "0 9-19 PRICE", "1 13-15 PRICE.mylock", "1 16-18 PRICE.Init"},
		names(Outline("price.txt", []byte(form))))
	assert.Empty(t, Outline("notes.txt", []byte("Procedure for the night shift\nfunction keys are F1-F12\n")),
		"prose without ENDPROC is no FoxPro")

	dir := t.TempDir()
	write(t, dir, "price.txt", form)

	out := Read([]Target{ParseTarget("price.txt:mylock", dir)}, dir, Options{})
	assert.Contains(t, out, "· foxpro · 13-15 PROCEDURE mylock\n")
}

// A role works in a task's worktree, away from where its session started: "cd <worktree> && cat big.go" from another
// directory is the session's own read when the worktree is one of the crew's roots, with the hidden-part and link
// checks made against that root.
func TestGuardTaskWorktree(t *testing.T) {
	session := t.TempDir()
	roots := t.TempDir() // the crew's worktrees directory
	wt := filepath.Join(roots, "acme-T-1")
	filler := goSrc + strings.Repeat("// filler line for size\n", 800)
	big := write(t, wt, "big.go", filler)
	write(t, wt, ".git/big.go", filler)

	elsewhere := t.TempDir()
	write(t, elsewhere, "big.go", filler)

	outside := t.TempDir()
	write(t, outside, "secret.go", filler)
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret.go"), filepath.Join(wt, "link.go")))

	asked := 0
	rootsOf := func() []string { asked++; return []string{roots} }

	bash := func(cmd string) Decision {
		return Guard(HookInput{Cwd: session, PermissionMode: "bypassPermissions", ToolName: "Bash",
			ToolInput: map[string]any{"command": cmd}}, "/opt/zmina", rootsOf)
	}

	d := bash("cd " + wt + " && cat big.go")
	assert.Equal(t, "rewrite", d.Kind)
	assert.Equal(t, "cd "+wt+" && /opt/zmina read --via 'cat big.go' big.go", d.Command)
	assert.Equal(t, big, d.Path)

	assert.Equal(t, "rewrite", bash("cat "+big).Kind, "an absolute path into the worktree")

	for name, cmd := range map[string]string{
		"a cd outside the roots":         "cd " + elsewhere + " && cat big.go",
		"a link out of the worktree":     "cd " + wt + " && cat link.go",
		"a hidden part under the root":   "cd " + wt + " && cat .git/big.go",
		"a file outside from a worktree": "cd " + wt + " && cat " + filepath.Join(elsewhere, "big.go"),
	} {
		assert.Equal(t, Decision{}, bash(cmd), name)
	}

	read := Guard(HookInput{Cwd: session, PermissionMode: "bypassPermissions", ToolName: "Read",
		ToolInput: map[string]any{"file_path": big}}, "/opt/zmina", rootsOf)
	assert.Equal(t, "outline", read.Kind, "a Read into the worktree is answered with the outline")

	// Elsewhere Claude Code would ask before reading outside the session's directory, or a deny rule would stop it:
	// no outline goes out past that.
	for _, mode := range []string{"default", "acceptEdits", "plan", ""} {
		assert.Equal(t, Decision{}, Guard(HookInput{Cwd: session, PermissionMode: mode, ToolName: "Read",
			ToolInput: map[string]any{"file_path": big}}, "/opt/zmina", rootsOf), mode)
	}

	// A root that is /, the home directory or above it is no root.
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	for _, r := range []string{"/", home, filepath.Dir(home), "", "relative/dir"} {
		assert.False(t, saneRoot(r), r)
	}

	assert.True(t, saneRoot(roots))

	before := asked
	write(t, session, "big.go", filler)
	assert.Equal(t, "rewrite", bash("cat big.go").Kind)
	assert.Equal(t, before, asked, "a file under the session's directory never asks for the roots")

	assert.Equal(t, Decision{}, Guard(HookInput{Cwd: session, PermissionMode: "bypassPermissions", ToolName: "Bash",
		ToolInput: map[string]any{"command": "cd " + wt + " && cat big.go"}}, "/opt/zmina", nil), "no roots: as before")
}

func TestIsBulk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rate.json")
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

	for i := range BulkPerMinute {
		assert.False(t, IsBulk(path, "s-1", now.Add(time.Duration(i)*time.Second)), i)
	}

	assert.True(t, IsBulk(path, "s-1", now.Add(40*time.Second)), "the 31st in a minute")
	assert.False(t, IsBulk(path, "s-2", now.Add(41*time.Second)), "another session counts apart")
	assert.False(t, IsBulk(path, "s-1", now.Add(61*time.Second)), "a new minute")
	assert.False(t, IsBulk("", "s-1", now))
	assert.False(t, IsBulk(path, "", now))
}
