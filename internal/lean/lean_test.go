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
		"  func (s *Shop) Open(day string) error · 18-23\n"+
		"    18: // Open opens the shop.\n"+
		"    19: func (s *Shop) Open(day string) error {\n"+
		"d.md (1)\n"+
		"  ## Usage · 17-19\n"+
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
	assert.Contains(t, out, "costgroup.prg (1)\n    4: && Резерв\n")
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

	read := Guard(HookInput{Cwd: session, ToolName: "Read", ToolInput: map[string]any{"file_path": big}}, "/opt/zmina", rootsOf)
	assert.Equal(t, "outline", read.Kind, "a Read into the worktree is answered with the outline")

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
