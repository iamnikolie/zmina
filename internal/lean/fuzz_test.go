package lean

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// These run inside the read-guard hook of every role and on every file a role reads: a panic or a hang there is a
// role that cannot read. Each target needs no more than "does not panic, and what it returns is sane"; run one for a
// while with
//
//	go test -fuzz=FuzzOutline -fuzztime=30s -run '^$' ./internal/lean
//
// Plain `go test` runs every seed below and every input kept in testdata/fuzz as an ordinary test.

// fuzzExts are the file types whose outline is read by different code: every outliner's own extensions, and a few
// types read by indentation only.
func fuzzExts() []string {
	var exts []string

	for _, o := range outliners {
		exts = append(exts, o.exts[0])
	}

	return append(exts, ".c", ".rb", ".toml", ".html", "")
}

// seeds are small files of the types above, one per language, each with a line or two that make a symbol.
var fuzzSeeds = []string{
	"package p\n\n// F does a thing.\nfunc F(a int) string {\n\treturn \"x\"\n}\n\ntype T struct{ A int }\n\nfunc (t T) M() {}\n",
	"# Title\n\ntext\n\n## Part one\n\n```go\nfunc x() {}\n```\n\n## Part two\n",
	"name: x\nitems:\n  - a: 1\n    b: [1, 2]\n  - a: 2\nnested:\n  deep:\n    key: value\n",
	"{\"a\": {\"b\": [1, 2, {\"c\": null}]}, \"d\": \"e\"}\n",
	"PROCEDURE Save\n  LOCAL x\n  x = 1\nENDPROC\n\nDEFINE CLASS Foo AS Custom\n  PROCEDURE Init\n  ENDPROC\nENDDEFINE\n",
	"CREATE TABLE t (id INT PRIMARY KEY, name TEXT);\n\n-- +goose Up\nALTER TABLE t ADD COLUMN c INT;\n\n-- +goose Down\nDROP TABLE t;\n",
	"export function f(a: number): string {\n  return `${a}`\n}\n\nclass C {\n  m() {}\n}\n\ndescribe('suite', () => {\n  it('works', () => {})\n})\n",
	"import os\n\nclass A:\n    def m(self):\n        pass\n\n@dec\ndef f(x):\n    return x\n",
	"#!/bin/sh\nf() {\n  echo \"$1\"\n}\nf a\n",
	"namespace N {\n  public class C {\n    public void M() { }\n  }\n}\n",
	"fn main() {\n    let x = 1;\n}\n\nstruct S { a: i32 }\n\nimpl S {\n    fn m(&self) {}\n}\n",
	"id,name,score\n1,alex,10\n2,sam,20\n",
	"{\"a\":1}\n{\"a\":2}\n",
	"Sub Main()\n  Dim x\nEnd Sub\n",
	"<?php\nclass A {\n  function f() {}\n}\n",
	"\x00\x01binary\x00",
	"\xff\xfe\x00a\x00",
	"",
}

func addFuzzSeeds(f *testing.F) {
	n := len(fuzzExts())
	for i, s := range fuzzSeeds {
		f.Add(uint8(i%n), []byte(s))
	}
}

func fuzzPath(ext uint8) string {
	exts := fuzzExts()

	return "f" + exts[int(ext)%len(exts)]
}

// FuzzOutline: any bytes under any file type give an outline whose symbols lie inside the file, in valid ranges, and
// the same outline twice.
func FuzzOutline(f *testing.F) {
	addFuzzSeeds(f)

	f.Fuzz(func(t *testing.T, ext uint8, src []byte) {
		path := fuzzPath(ext)
		syms := Outline(path, src)
		lines := countLines(src)

		for _, s := range syms {
			if s.Start < 1 || s.End < s.Start || s.End > lines {
				t.Fatalf("%s: symbol %q has lines %d-%d in a file of %d lines", path, s.Name, s.Start, s.End, lines)
			}

			if s.Depth < 0 {
				t.Fatalf("%s: symbol %q has depth %d", path, s.Name, s.Depth)
			}
		}

		again := Outline(path, src)
		if len(again) != len(syms) {
			t.Fatalf("%s: two outlines of the same bytes differ: %d and %d symbols", path, len(syms), len(again))
		}

		for i := range syms {
			if syms[i] != again[i] {
				t.Fatalf("%s: two outlines of the same bytes differ at %d: %+v and %+v", path, i, syms[i], again[i])
			}
		}

		Lang(path)
		Kind(path, src)
		Decode(src)
	})
}

// FuzzRead: reading a file of any bytes, whole, as an outline, by a symbol or a heading its own outline names, and
// by a range, returns text within a cap and never panics.
func FuzzRead(f *testing.F) {
	addFuzzSeeds(f)

	dir := f.TempDir()

	f.Fuzz(func(t *testing.T, ext uint8, src []byte) {
		path := filepath.Join(dir, fuzzPath(ext))
		if err := os.WriteFile(path, src, 0o600); err != nil {
			t.Skip(err)
		}

		const max = 4000

		targets := [][]Target{{{Path: path}}, {{Path: path, From: 2, To: 5}}, {{Path: path, From: 3}}}
		for _, s := range firstSymbols(path, src, 3) {
			targets = append(targets, []Target{{Path: path, Symbol: s.Name}}, []Target{{Path: path, Heading: s.Name}})
		}

		for _, ts := range targets {
			for _, o := range []Options{{Max: max}, {Max: max, Outline: true}, {Max: max, Via: "cat x"}} {
				out := Read(ts, dir, o)

				// The cap is on what is shown; a header and a footer come on top of it.
				if len(out) > 4*max+4096 {
					t.Fatalf("%v with cap %d printed %d characters", ts, o.Max, len(out))
				}

				if !utf8.ValidString(out) && utf8.Valid(src) {
					t.Fatalf("%v printed invalid UTF-8 of a valid file", ts)
				}
			}
		}
	})
}

func firstSymbols(path string, src []byte, n int) []Symbol {
	syms := Outline(path, src)

	return syms[:min(n, len(syms))]
}

// FuzzParseTarget: any argument gives a target, and printing it gives text.
func FuzzParseTarget(f *testing.F) {
	dir := f.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600); err != nil {
		f.Fatal(err)
	}

	for _, s := range []string{"a.go", "a.go:Name", "a.go:12", "a.go:12-30", "a.go:40-", "a.go#Heading", "a.go:#Heading",
		"a.go:Recv.Method", "missing.go:5", ":", "#", "::", "a.go:-", "a.go:0-0", "a.go:99999999999999999999", ""} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, arg string) {
		tg := ParseTarget(arg, dir)
		_ = tg.String()

		if tg.From < 0 || tg.To < 0 {
			t.Fatalf("%q: negative line %d-%d", arg, tg.From, tg.To)
		}
	})
}

// FuzzGuardBash: any command string is let go, or rewritten to one plain call of the zmina executable, never to
// anything else. A command that has no cat, sed, head or tail in front is always let go.
func FuzzGuardBash(f *testing.F) {
	dir := f.TempDir()
	big := strings.Repeat("a line of text that makes the file big enough to guard\n", 400)

	for _, name := range []string{"big.txt", "big.go", "my file.md", "it's.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(big), 0o600); err != nil {
			f.Fatal(err)
		}
	}

	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		f.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "sub", "big.txt"), []byte(big), 0o600); err != nil {
		f.Fatal(err)
	}

	for _, s := range []string{
		"cat big.txt", "cat -n big.go", "cat big.txt big.go", "cat 'my file.md'", `cat "my file.md"`, "cat sub/big.txt",
		"sed -n 1,400p big.txt", "sed -n 5,9p big.go", "head -300 big.txt", "head -n 300 big.go", "tail -n300 big.txt",
		"cd sub && cat big.txt", "cd '/tmp' && cat x", `cd "sub" && head -50 big.txt`, "cat big.txt | head", "cat $HOME/x",
		"cat 'it's.txt'", "cat ../x", "cat /etc/passwd", "cat -", "cat", "", "   ", "ls -la", "echo cat big.txt",
		"cat big.txt; rm -rf x", "cat big.txt && echo", "cat `id`", "sed -n 1,2p", "head -", "tail -n", "cat \"unterminated",
		"cat 'unterminated", "cd", "cd && cat big.txt", "cat big.txt\nrm x",
	} {
		f.Add(s)
	}

	const self = "/usr/local/bin/zmina"

	f.Fuzz(func(t *testing.T, cmd string) {
		d := guardBash(cmd, dir, self, nil)

		switch d.Kind {
		case "":
			if d.Command != "" || d.Reason != "" {
				t.Fatalf("%q: let go, but with %+v", cmd, d)
			}
		case "rewrite":
			rest := cdPrefix.ReplaceAllString(d.Command, "")

			words, ok := plainWords(rest)
			if !ok || len(words) < 4 || words[0] != self || words[1] != "read" || words[2] != "--via" {
				t.Fatalf("%q: rewritten to something that is not one plain zmina read: %q", cmd, d.Command)
			}

			if d.Path == "" || d.Size <= 0 || d.What == "" {
				t.Fatalf("%q: a rewrite without what the meter needs: %+v", cmd, d)
			}
		default:
			t.Fatalf("%q: unknown decision %q", cmd, d.Kind)
		}

		first := strings.Fields(cmd)
		if len(first) == 0 || !slices.Contains([]string{"cat", "sed", "head", "tail", "cd"}, first[0]) {
			if d.Kind != "" {
				t.Fatalf("%q: a command with no read verb in front is rewritten to %q", cmd, d.Command)
			}
		}
	})
}

// FuzzGuard: the whole hook input as JSON decodes into the guard's input and is answered, for any tool call.
func FuzzGuard(f *testing.F) {
	dir := f.TempDir()
	big := strings.Repeat("func f() {}\n", 2000)

	if err := os.WriteFile(filepath.Join(dir, "big.go"), []byte("package p\n\n"+big), 0o600); err != nil {
		f.Fatal(err)
	}

	for _, tool := range []string{"Read", "Bash", "Edit", "", "mcp__x__y"} {
		for _, mode := range []string{"bypassPermissions", "default", ""} {
			f.Add(tool, mode, "big.go", "cat big.go", 0, 0)
		}
	}

	f.Add("Read", "bypassPermissions", "big.go", "", 10, 50)
	f.Add("Read", "bypassPermissions", "", "", 0, 0)
	f.Add("Read", "bypassPermissions", "../../etc/passwd", "", 0, 0)
	f.Add("Read", "bypassPermissions", "sub/../big.go", "", 0, 0)

	f.Fuzz(func(t *testing.T, tool, mode, path, cmd string, offset, limit int) {
		in := HookInput{Cwd: dir, PermissionMode: mode, ToolName: tool, ToolInput: map[string]any{"file_path": path, "command": cmd}}
		if offset > 0 {
			in.ToolInput["offset"] = offset
		}

		if limit > 0 {
			in.ToolInput["limit"] = limit
		}

		d := Guard(in, "/usr/local/bin/zmina", func() []string { return []string{dir} })

		if d.Kind != "" && d.Kind != "rewrite" && d.Kind != "outline" {
			t.Fatalf("unknown decision %q", d.Kind)
		}

		if d.Kind == "outline" && (offset > 0 || limit > 0) {
			t.Fatalf("a Read with offset or limit was answered with an outline")
		}

		if d.Kind != "" && mode != "bypassPermissions" && tool == "Bash" {
			t.Fatalf("a Bash call was rewritten in permission mode %q", mode)
		}
	})
}
