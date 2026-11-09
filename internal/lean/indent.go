package lean

import (
	"regexp"
	"strings"
)

// indentOutline is the outline of a file no outliner of its own reads: Python, TypeScript and JavaScript, Java,
// C#, Visual Basic, Swift, PHP, shell, SQL, CSS, pretty-printed XML and HTML. A block is a line whose next code line
// is indented deeper; it runs while the lines stay deeper, through closing lines at its own indent (}, ), End Sub,
// else:, </tag>). The top-level blocks are listed, and the blocks inside a big one, or inside one that wraps the whole
// file (a class, a namespace) one level further. A block starts at the comments and decorators right above it; its
// name is what a target names it by: Class.method below the top. A statement over several lines (a signature, a
// call, a list: brackets left open, a line ending in " _" or a backslash) counts as its first line.
func indentOutline(src []byte) []Symbol {
	lines := splitLines(src)
	if len(lines) == 0 {
		return nil
	}

	in := codeLines(lines)
	if len(in) == 0 {
		return nil
	}

	var out []Symbol

	var walk func(from, to, depth int, parent string)
	walk = func(from, to, depth int, parent string) {
		for k := from; k < to; k++ {
			c := in[k]

			// A line that starts by closing something, or a bare brace, opens no block of its own.
			if closer(c.text) || c.text == "{" {
				continue
			}

			// A block, or a statement of many lines (CREATE TABLE x ( … ), a big literal).
			end := blockEnd(in, k, to)
			if end == k && c.last-c.n < 5 {
				continue
			}

			name := blockName(c.text)
			if parent != "" {
				name = parent + "." + name
			}

			start := headStart(lines, c.n)
			last := in[end].last
			out = append(out, Symbol{Start: start, End: last, Depth: depth, Name: name, Label: clip(c.text, 110)})

			// Inside a class, a namespace, an element, or a block that is most of the file: its members. Inside a
			// function: nothing (its ifs and loops are no outline).
			span := last - start + 1
			if span > 40 && depth < 3 && (container(c.text) || span*5 >= len(lines)*4) {
				walk(k+1, end+1, depth+1, name)
			}

			k = end
		}
	}
	walk(0, len(in), 0, "")

	return out
}

// codeLine is a statement that counts for blocks: not blank, not only a comment, not inside a multi-line string.
type codeLine struct {
	n      int // its first line, 1-based
	last   int // its last line
	indent int // tabs as 4
	text   string
}

// maxStatement is the most lines one statement joins: past it a bracket left open in a string or a regexp no longer
// joins lines.
const maxStatement = 60

var quotedRe = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'`)

// container: a block whose members make the outline (a class, a namespace, an element), not a function's body.
func container(t string) bool {
	if strings.HasPrefix(t, "<") || strings.HasPrefix(t, "@media") || strings.HasPrefix(t, "@supports") {
		return true
	}

	for _, w := range strings.Fields(t) {
		w = strings.ToLower(w)
		if modifiers[w] {
			continue
		}

		switch w {
		case "class", "interface", "namespace", "module", "struct", "impl", "object", "trait", "extension",
			"protocol", "record", "structure", "actor", "enum":
			return true
		}

		return false
	}

	return false
}

func codeLines(lines []string) []codeLine {
	var (
		out   []codeLine
		quote string // the open multi-line string's delimiter: """, ''' or `
		open  int    // ( and [ the statement has left open so far
		cont  bool   // the statement goes on to the next line
		tail  string // this code line, for the next
		prev  string // the code line before this one
	)

	for i, l := range lines {
		t := strings.TrimSpace(l)

		inside := quote != ""
		if !inside && (t == "" || isComment(t)) {
			continue // a quote in a comment opens no string
		}

		for _, q := range []string{`"""`, `'''`, "`"} {
			if quote != "" && quote != q {
				continue
			}

			if strings.Count(t, q)%2 == 1 {
				if quote == "" {
					quote = q
				} else {
					quote = ""
				}
			}
		}

		if inside || t == "" || isComment(t) {
			if inside && len(out) > 0 {
				out[len(out)-1].last = i + 1
			}

			continue
		}

		bare := quotedRe.ReplaceAllString(t, `""`)
		delta := strings.Count(bare, "(") + strings.Count(bare, "[") - strings.Count(bare, ")") - strings.Count(bare, "]")

		// A brace on a line of its own (class Foo / {) opens the block of the statement above it.
		allman := strings.HasPrefix(t, "{") && len(out) > 0 && indentOf(l) == out[len(out)-1].indent &&
			!strings.HasSuffix(tail, ",") && !strings.HasSuffix(tail, ";") && !strings.HasSuffix(tail, "}")
		prev, tail = tail, t

		// An open bracket joins the next line when it is deeper, closes the bracket, or follows "(", "[" or ",".
		joins := cont || open > 0 && len(out) > 0 && (indentOf(l) > out[len(out)-1].indent ||
			strings.HasPrefix(t, ")") || strings.HasPrefix(t, "]") ||
			strings.HasSuffix(prev, "(") || strings.HasSuffix(prev, "[") || strings.HasSuffix(prev, ","))

		if (joins || allman) && len(out) > 0 && i+1-out[len(out)-1].n < maxStatement {
			out[len(out)-1].last = i + 1
		} else {
			out = append(out, codeLine{n: i + 1, last: i + 1, indent: indentOf(l), text: t})
			open = 0
		}

		open = max(open+delta, 0)
		cont = strings.HasSuffix(t, " _") || strings.HasSuffix(t, `\`)
	}

	return out
}

func indentOf(l string) int {
	n := 0

	for _, r := range l {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}

	return n
}

// isComment: the line is only a comment. The markers that are code too in some languages are a comment only when
// the line is not that code: "#id {" and "* {" are CSS, "'key': value" is a string.
func isComment(t string) bool {
	for _, p := range []string{"//", "/*", "--", "<!--", "REM "} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}

	switch {
	case strings.HasPrefix(t, "#[") || strings.HasPrefix(t, "#{") || strings.HasPrefix(t, "#!") && len(t) > 2 && t[2] != ' ':
		return false
	case strings.HasPrefix(t, "#") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "%"):
		return !strings.HasSuffix(t, "{") && !strings.HasSuffix(t, ",") && !strings.HasPrefix(t, "*=")
	case strings.HasPrefix(t, "'"):
		return !quotedKeyRe.MatchString(t)
	}

	return false
}

var quotedKeyRe = regexp.MustCompile(`^'(?:[^'\\]|\\.)*'\s*(?:[:,;)\]}+.=]|$)`)

// blockEnd is the index of the last code line of the block that line k opens, k itself when it opens none.
func blockEnd(in []codeLine, k, to int) int {
	if k+1 >= to || in[k+1].indent <= in[k].indent {
		return k
	}

	level, end := in[k].indent, k

	for j := k + 1; j < to; j++ {
		c := in[j]
		if c.indent > level {
			end = j
			continue
		}

		if c.indent == level && closer(c.text) {
			end = j
			continue
		}

		break
	}

	return end
}

var closerRe = regexp.MustCompile(`^(?i:[})\]]|/?>|</|end\b|end[a-z]+\b|else\b|elif\b|elseif\b|except\b|finally\b|catch\b|next\b|loop\b|wend\b|until\b|fi\b|done\b|esac\b|\?>)`)

func closer(t string) bool { return closerRe.MatchString(t) }

// headStart is where a block opening at line n begins: the comment, decorator and attribute lines right above it.
func headStart(lines []string, n int) int {
	for n > 1 {
		if t := strings.TrimSpace(lines[n-2]); t == "" || !heading(t) {
			break
		}

		n--
	}

	return n
}

// heading: a line that belongs to the declaration below it (a comment, a decorator, an attribute).
func heading(t string) bool {
	return isComment(t) || strings.HasPrefix(t, "@") || strings.HasPrefix(t, "#[") ||
		strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") || strings.HasPrefix(t, "<") && strings.HasSuffix(t, "> _")
}

// modifiers come before what a declaration is; they are no part of its name.
var modifiers = map[string]bool{"export": true, "default": true, "async": true, "public": true, "private": true,
	"protected": true, "internal": true, "static": true, "final": true, "abstract": true, "override": true,
	"virtual": true, "readonly": true, "sealed": true, "partial": true, "open": true, "fileprivate": true,
	"friend": true, "shared": true, "overrides": true, "overridable": true, "mustoverride": true, "notinheritable": true,
	"declare": true, "unsafe": true, "pub": true, "extern": true, "inline": true, "local": true, "global": true,
	"data": true, "suspend": true, "mutating": true, "convenience": true, "required": true}

// kinds are the words a declaration's name follows.
var kinds = map[string]bool{"def": true, "class": true, "function": true, "function*": true, "func": true, "fn": true,
	"fun": true, "interface": true, "type": true, "enum": true, "struct": true, "trait": true, "impl": true,
	"module": true, "namespace": true, "object": true, "sub": true, "property": true, "const": true, "let": true,
	"var": true, "procedure": true, "record": true, "extension": true, "protocol": true, "actor": true, "table": true,
	"view": true, "index": true, "trigger": true, "schema": true, "event": true,
	"operator": true, "structure": true, "union": true, "contract": true, "library": true, "package": true}

var (
	identRe   = regexp.MustCompile(`[\p{L}_$][\p{L}\p{N}_$]*`)
	beforeRe  = regexp.MustCompile(`([\p{L}_$][\p{L}\p{N}_$.]*)\s*(?:<[^()]*>)?\s*\(`)
	tagRe     = regexp.MustCompile(`^<([\w:.-]+)`)
	nameAttrs = regexp.MustCompile(`\b(?:Name|name|id|key)\s*=\s*"([^"]+)"`)
	sqlNameRe = regexp.MustCompile(`(?i)^create\s+(?:or\s+replace\s+)?(?:temp(?:orary)?\s+)?(?:materialized\s+)?(?:unique\s+)?` +
		`(?:table|view|function|procedure|index|trigger|type|schema|sequence)\s+(?:if\s+not\s+exists\s+)?([\w."\x60\[\]]+)`)
)

// blockName is the name a target finds a block by: a declaration's name, an XML element's name or tag, a CSS
// selector, else the block's first word.
func blockName(t string) string {
	if m := sqlNameRe.FindStringSubmatch(t); m != nil {
		return strings.Trim(m[1], "\"`[]")
	}

	if strings.HasPrefix(t, "<") {
		if m := nameAttrs.FindStringSubmatch(t); m != nil {
			return m[1]
		}

		if m := tagRe.FindStringSubmatch(t); m != nil {
			return m[1]
		}
	}

	if i := strings.Index(t, "{"); i > 0 && !strings.ContainsAny(t[:i], "(=") && !identStart(t) {
		return strings.TrimSpace(t[:i]) // a CSS selector
	}

	words := strings.Fields(t)
	for i := 0; i < len(words); i++ {
		w := strings.ToLower(words[i])
		if modifiers[w] {
			continue
		}

		if kinds[w] && i+1 < len(words) {
			if id := identRe.FindString(words[i+1]); id != "" {
				return id
			}
		}

		break
	}

	if m := beforeRe.FindStringSubmatch(t); m != nil && !controlWord(m[1]) {
		return m[1]
	}

	if id := identRe.FindString(t); id != "" {
		return id
	}

	return clip(t, 40)
}

func identStart(t string) bool {
	w := strings.ToLower(firstWord(t))
	return kinds[w] || modifiers[w]
}

func controlWord(w string) bool {
	switch strings.ToLower(w) {
	case "if", "for", "while", "switch", "catch", "with", "foreach", "elseif", "return", "function":
		return true
	}

	return false
}
