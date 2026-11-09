// Package lean reads files the way an agent should pay for them: a small file whole, a big one as an outline with
// line ranges and sizes, then the one function, section or range it needs; grep with a budget, hits grouped by the
// function or section they sit in; and the guard that turns a big cat, sed or Read into such a read.
package lean

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Symbol is one entry of an outline: a Go declaration, a Markdown section, a YAML or JSON key. Lines are 1-based and
// inclusive; Depth 0 is the top level.
type Symbol struct {
	Start, End int
	Depth      int
	// Name is what a target names it by: Recv.Method, Func, Type, a heading's text, a key (parent.key below the top).
	Name string
	// Label is how the outline shows it: a Go signature, "## Heading", "key:".
	Label string
}

// outliner reads one type of file into its outline, or writes its summary (a table: what a row holds, how many, the
// first). A file of any other type is outlined by its indentation (indentOutline); one without blocks shows its
// start, and ranges and the cap work on every file.
type outliner struct {
	lang  string
	exts  []string
	parse func(src []byte, lines int) []Symbol
	// summary, for a type without an outline, is what zmina read shows of a big file instead. It shows the file's
	// rows, so the read guard never answers with it.
	summary func(b *strings.Builder, f *File, path, header string, left int)
	// hint is how the outline footer names a piece of this type (after the path).
	hint string
}

// whole adapts an outliner that needs no line count.
func whole(parse func(src []byte) []Symbol) func(src []byte, lines int) []Symbol {
	return func(src []byte, _ int) []Symbol { return parse(src) }
}

var outliners = []outliner{
	{lang: "go", exts: []string{".go"}, parse: whole(goOutline), hint: ":Name (Func, Type.Method)"},
	{lang: "markdown", exts: []string{".md", ".markdown", ".mdx", ".txt"}, parse: whole(mdOutline), hint: "#Heading"},
	{lang: "yaml", exts: []string{".yaml", ".yml"}, parse: yamlOutline, hint: ":key (a.b below the top)"},
	{lang: "json", exts: []string{".json"}, parse: jsonOutline, hint: ":key (a.b below the top)"},
	{lang: "foxpro", exts: []string{".prg"}, parse: whole(foxOutline), hint: ":Name (PROCEDURE, FUNCTION, Class.Method)"},
	{lang: "sql", exts: []string{".sql", ".pgsql", ".psql", ".mysql", ".ddl"}, parse: whole(sqlOutline),
		hint: ":name (every statement on a table or object; Up, Down)"},
	{lang: "csv", exts: []string{".csv"}, summary: writeCSVSummary},
	{lang: "tsv", exts: []string{".tsv", ".tab"}, summary: writeCSVSummary},
	{lang: "jsonl", exts: []string{".jsonl", ".ndjson"}, summary: writeJSONLSummary},
}

func outlinerFor(path string) *outliner {
	ext := strings.ToLower(filepath.Ext(path))
	for i := range outliners {
		if slices.Contains(outliners[i].exts, ext) {
			return &outliners[i]
		}
	}

	return nil
}

// Lang names the outliner for a path: go, markdown, yaml, json, foxpro, sql, csv, tsv, jsonl, or "" (outlined by
// indentation).
func Lang(path string) string {
	if o := outlinerFor(path); o != nil {
		return o.lang
	}

	return ""
}

// Outline is the file's outline by its type, by indentation for a type without an outliner of its own; nil when it
// has none or cannot be parsed. A text file without headings that holds FoxPro blocks (a form exported as text) gets
// the FoxPro outline.
func Outline(path string, src []byte) []Symbol {
	o := outlinerFor(path)
	if o == nil {
		return indentOutline(src)
	}

	if o.parse == nil {
		return nil
	}

	out := o.parse(src, countLines(src))
	if len(out) == 0 && o.lang == "markdown" && foxEnds.Match(src) {
		out = foxOutline(src)
	}

	return out
}

// foxEnds finds a line that ends a FoxPro block: the sign that a text file is FoxPro code.
var foxEnds = regexp.MustCompile(`(?im)^\s*(ENDPROC|ENDFUNC|ENDDEFINE)\s*$`)

func countLines(src []byte) int {
	n := bytes.Count(src, []byte("\n"))
	if len(src) > 0 && src[len(src)-1] != '\n' {
		n++
	}

	return n
}

// goOutline lists imports, types, funcs, methods, and const/var blocks; a range starts at its doc comment. A file
// with syntax errors still gives what the parser recovered.
func goOutline(src []byte) []Symbol {
	fset := token.NewFileSet()

	f, _ := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if f == nil {
		return nil
	}

	line := func(p token.Pos) int { return fset.Position(p).Line }
	head := func(from, to token.Pos) string {
		a, b := fset.Position(from).Offset, fset.Position(to).Offset
		if a < 0 || b > len(src) || a >= b {
			return ""
		}

		s := string(src[a:b])
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[:i]
		}

		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "{"))

		return clip(s, 110)
	}

	var out []Symbol

	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			start := d.Pos()
			if d.Doc != nil {
				start = d.Doc.Pos()
			}

			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				name = recvName(d.Recv.List[0].Type) + "." + name
			}

			to := d.End()
			if d.Body != nil {
				to = d.Body.Lbrace
			}

			out = append(out, Symbol{Start: line(start), End: line(d.End()), Name: name, Label: head(d.Pos(), to)})
		case *ast.GenDecl:
			start := d.Pos()
			if d.Doc != nil {
				start = d.Doc.Pos()
			}

			switch {
			case d.Tok == token.IMPORT:
				out = append(out, Symbol{Start: line(start), End: line(d.End()), Name: "imports",
					Label: "imports (" + itoa(len(d.Specs)) + ")"})
			case d.Lparen.IsValid() && len(d.Specs) > 1:
				var names []string

				for _, s := range d.Specs {
					names = append(names, specNames(s)...)
				}

				label := d.Tok.String() + " ( " + clip(strings.Join(names, ", "), 90) + " )"
				out = append(out, Symbol{Start: line(start), End: line(d.End()), Name: strings.Join(names, ","), Label: label})

				if d.Tok == token.TYPE {
					for _, s := range d.Specs {
						ts := s.(*ast.TypeSpec)
						out = append(out, Symbol{Start: line(ts.Pos()), End: line(ts.End()), Depth: 1, Name: ts.Name.Name,
							Label: "type " + head(ts.Pos(), ts.End())})
					}
				}
			default:
				for _, s := range d.Specs {
					names := specNames(s)
					if len(names) == 0 {
						continue
					}

					label := d.Tok.String() + " " + head(s.Pos(), s.End())
					out = append(out, Symbol{Start: line(start), End: line(d.End()), Name: names[0], Label: label})
				}
			}
		}
	}

	return out
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvName(t.X)
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	case *ast.Ident:
		return t.Name
	}

	return "?"
}

func specNames(s ast.Spec) []string {
	switch s := s.(type) {
	case *ast.TypeSpec:
		return []string{s.Name.Name}
	case *ast.ValueSpec:
		var out []string
		for _, n := range s.Names {
			out = append(out, n.Name)
		}

		return out
	}

	return nil
}

// mdOutline lists ATX headings outside fenced code; a section runs to the next heading of its level or above.
func mdOutline(src []byte) []Symbol {
	lines := strings.Split(string(src), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	var out []Symbol

	fence := ""

	for i, l := range lines {
		t := strings.TrimSpace(l)
		if fence != "" {
			if strings.HasPrefix(t, fence) {
				fence = ""
			}

			continue
		}

		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = t[:3]
			continue
		}

		if !strings.HasPrefix(l, "#") {
			continue
		}

		level := len(l) - len(strings.TrimLeft(l, "#"))
		if level > 6 || len(l) == level || l[level] != ' ' {
			continue
		}

		text := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(l[level:]), "#"))
		out = append(out, Symbol{Start: i + 1, Depth: level - 1, Name: text, Label: l[:level] + " " + clip(text, 100)})
	}

	for i := range out {
		out[i].End = len(lines)

		for _, n := range out[i+1:] {
			if n.Depth <= out[i].Depth {
				out[i].End = n.Start - 1
				break
			}
		}
	}

	// A text file without headings has no outline (its .txt may be anything).
	return out
}

// yamlOutline lists the top keys, and the keys under a big one.
func yamlOutline(src []byte, lines int) []Symbol {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}

	var out []Symbol

	var walk func(m *yaml.Node, depth, end int, prefix string)
	walk = func(m *yaml.Node, depth, end int, prefix string) {
		for i := 0; i+1 < len(m.Content); i += 2 {
			k, v := m.Content[i], m.Content[i+1]

			stop := end
			if i+2 < len(m.Content) {
				stop = m.Content[i+2].Line - 1
			}

			name := prefix + k.Value
			out = append(out, Symbol{Start: k.Line, End: stop, Depth: depth, Name: name, Label: clip(k.Value, 80) + ":"})

			if depth == 0 && v.Kind == yaml.MappingNode && stop-k.Line > 40 {
				walk(v, 1, stop, name+".")
			}
		}
	}
	walk(doc.Content[0], 0, lines, "")

	return out
}

// jsonOutline lists an object's top keys, and the keys under a big one, by scanning the bytes: a string right after
// "{" or "," inside an object is a key.
func jsonOutline(src []byte, lines int) []Symbol {
	if !json.Valid(src) {
		return nil
	}

	var (
		out   []Symbol
		stack []byte // container kinds: '{' or '['
		owner []int  // per container: index in out of the key it is the value of, or -1
		line  = 1
		want  bool // the next string in this object is a key
		last  = map[int]int{}
	)

	closeKey := func(depth, at int) {
		if i, ok := last[depth]; ok {
			out[i].End = at
			delete(last, depth)
		}
	}

	for i := 0; i < len(src); i++ {
		c := src[i]
		switch c {
		case '\n':
			line++
		case '{', '[':
			o := -1
			if i2, ok := last[len(stack)]; ok {
				o = i2
			}

			stack = append(stack, c)
			owner = append(owner, o)
			want = c == '{'
		case '}', ']':
			depth := len(stack)
			closeKey(depth, line)

			if depth == 1 && c == '}' {
				return out
			}

			stack, owner = stack[:depth-1], owner[:depth-1]
			want = false
		case ',':
			want = len(stack) > 0 && stack[len(stack)-1] == '{'
		case '"':
			j := i + 1
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' {
					j++
				}

				j++
			}

			if want && len(stack) <= 2 {
				var key string
				_ = json.Unmarshal(src[i:j+1], &key)

				depth := len(stack)
				closeKey(depth, line-1)

				name := key
				if depth == 2 {
					if o := owner[1]; o >= 0 {
						name = out[o].Name + "." + key
					} else {
						want = false
						i = j

						continue
					}
				}

				out = append(out, Symbol{Start: line, End: lines, Depth: depth - 1, Name: name, Label: strconvQuote(clip(key, 80)) + ":"})
				last[depth] = len(out) - 1
			}

			want = false
			i = j
		}
	}

	return out
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}

	return string(r[:n-1]) + "…"
}

func itoa(n int) string { return strconv.Itoa(n) }
