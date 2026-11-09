package lean

import (
	"strings"
	"unicode/utf8"
)

// braceLine is what a scan of a brace language (TypeScript, JavaScript, Rust) knows of one line.
type braceLine struct {
	depth int    // brackets ( [ { open at the line's start
	end   int    // open at its end
	min   int    // fewest open anywhere in it
	in    bool   // it starts inside a comment, a string or a template that spans lines
	code  string // its code, trimmed: a string's or a regexp's text emptied, comments dropped
}

// braceScan reads a brace language line by line, knowing its strings and comments: in JavaScript and TypeScript
// quotes that close on their line, template literals with ${…} inside, regexp literals, and JSX text (a quote with
// no partner on its line, as in <p>Don't</p>, is text); in Rust strings over lines, raw strings r#"…"#, byte and
// char literals, lifetimes and nested block comments. sure: every bracket closed by its own kind, nothing open at
// the end, and no line where a declaration at column 0 had to reset the count.
func braceScan(lines []string, rust bool) (out []braceLine, sure bool) {
	var (
		stack   []byte // ( [ { ; ` a template; $ a ${ inside one
		comment int    // nested /* */ open
		str     bool   // a Rust string open over lines
		hashes  int    // its closing quote's hashes; -1: an ordinary string with escapes
		prev    byte   // the last code character written, across lines (JS: regexp or division)
		word    string // the last word written
	)

	sure = true
	out = make([]braceLine, len(lines))

	for li, l := range lines {
		bl := &out[li]
		bl.depth = len(stack)
		bl.in = comment > 0 || str || len(stack) > 0 && stack[len(stack)-1] == '`'

		// A declaration at column 0 is at the top: a count that says otherwise went wrong above (a regexp or JSX
		// read as something else). Start again from it.
		if !bl.in && len(stack) > 0 && topStart(l, rust) {
			stack, bl.depth, sure = stack[:0], 0, false
		}

		if li == 0 && strings.HasPrefix(l, "#!") && !strings.HasPrefix(l, "#![") {
			bl.end, bl.min = len(stack), len(stack)
			continue
		}

		var code strings.Builder

		emit := func(s string) {
			code.WriteString(s)

			if t := strings.TrimRight(s, " \t"); t != "" {
				prev = t[len(t)-1]
				if wordByte(prev) {
					word += s
				} else {
					word = ""
				}
			}
		}

		low := len(stack)
		i := 0

		for i < len(l) {
			switch {
			case comment > 0:
				switch {
				case strings.HasPrefix(l[i:], "*/"):
					comment--
					i += 2
				case rust && strings.HasPrefix(l[i:], "/*"):
					comment++
					i += 2
				default:
					i++
				}

				continue
			case str:
				if j, ok := rustStringEnd(l, i, hashes); ok {
					str = false
					i = j

					emit(`"`)
				} else {
					i = len(l)
				}

				continue
			case len(stack) > 0 && stack[len(stack)-1] == '`':
				switch {
				case l[i] == '\\':
					i += 2
				case l[i] == '`':
					stack = stack[:len(stack)-1]
					i++

					emit("`")
				case strings.HasPrefix(l[i:], "${"):
					stack = append(stack, '$')
					i += 2

					emit("${")
				default:
					i++
				}

				continue
			}

			c := l[i]

			switch {
			case c == '/' && i+1 < len(l) && l[i+1] == '/':
				i = len(l)
			case c == '/' && i+1 < len(l) && l[i+1] == '*':
				comment++
				i += 2
			case c == '"' && rust:
				if j, ok := rustStringEnd(l, i+1, -1); ok {
					i = j
				} else {
					str, hashes, i = true, -1, len(l)
				}

				emit(`""`)
			case rust && (c == 'r' || c == 'b' || c == 'c') && (i == 0 || !wordByte(l[i-1])) && rawStart(l[i:]) > 0:
				n := rawStart(l[i:]) // past the opening quote
				h := strings.Count(l[i:i+n], "#")

				if j, ok := rustStringEnd(l, i+n, h); ok {
					i = j
				} else {
					str, hashes, i = true, h, len(l)
				}

				emit(`""`)
			case c == '\'' && rust:
				i = rustQuote(l, i)

				emit("''")
			case c == '\'' || c == '"':
				if j := closesOnLine(l, i, c); j > 0 {
					i = j

					emit(string([]byte{c, c}))
				} else {
					i++ // JSX text: Don't
				}
			case c == '`' && !rust:
				stack = append(stack, '`')
				i++

				emit("`")
			case c == '/' && !rust && regexpHere(l, i, prev, word):
				if j := regexpEnd(l, i); j > 0 {
					i = j

					emit("/r/")
				} else {
					i++

					emit("/")
				}
			case c == '(' || c == '[' || c == '{':
				stack = append(stack, c)
				i++

				emit(string(c))
			case c == ')' || c == ']' || c == '}':
				if len(stack) == 0 {
					sure = false
				} else {
					if !closes(stack[len(stack)-1], c) {
						sure = false
					}

					stack = stack[:len(stack)-1]
				}

				low = min(low, len(stack))
				i++

				emit(string(c))
			default:
				i++

				emit(string(c))
			}
		}

		bl.end, bl.min, bl.code = len(stack), low, strings.TrimSpace(code.String())
	}

	return out, sure && len(stack) == 0 && comment == 0 && !str
}

// closes: c closes what top opened (a } closes a template's ${ too).
func closes(top, c byte) bool {
	switch c {
	case ')':
		return top == '('
	case ']':
		return top == '['
	}

	return top == '{' || top == '$'
}

// wordByte: c can be part of a word in code ($ too).
func wordByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// closesOnLine is the index past the quote that closes the one at i on its line, 0 when none does.
func closesOnLine(l string, i int, q byte) int {
	for j := i + 1; j < len(l); j++ {
		switch l[j] {
		case '\\':
			j++
		case q:
			return j + 1
		}
	}

	return 0
}

// topStart: the line, at column 0, starts a declaration only the top level holds.
func topStart(l string, rust bool) bool {
	if l == "" || l[0] == ' ' || l[0] == '\t' {
		return false
	}

	w := firstWord(l)
	if rust {
		switch w {
		case "fn", "pub", "impl", "struct", "enum", "trait", "mod", "use", "macro_rules":
			return true
		}

		return false
	}

	switch w {
	case "export", "import", "function", "class", "interface":
		return !strings.HasSuffix(strings.TrimSpace(l), ",")
	}

	return false
}

// regexpHere: a slash at i starts a regexp, not a division: after an operator, an opening bracket, a comma, a
// keyword or nothing. A slash after < closes a JSX tag, and /> after an attribute's {…} ends one.
func regexpHere(l string, i int, prev byte, word string) bool {
	if i > 0 && l[i-1] == '<' || prev == '}' && i+1 < len(l) && l[i+1] == '>' {
		return false
	}

	switch word {
	case "return", "typeof", "case", "do", "else", "in", "of", "new", "delete", "void", "throw", "yield", "await":
		return true
	}

	return prev == 0 || strings.IndexByte("(,=:[!&|?{};+-*%<>~^", prev) >= 0
}

// regexpEnd is the index past a regexp literal's closing slash and flags on its line, 0 when it has none there.
func regexpEnd(l string, i int) int {
	class := false

	for j := i + 1; j < len(l); j++ {
		switch c := l[j]; {
		case c == '\\':
			j++
		case c == '[':
			class = true
		case c == ']':
			class = false
		case c == '/' && !class:
			j++
			for j < len(l) && wordByte(l[j]) {
				j++
			}

			return j
		}
	}

	return 0
}

// rawStart is how many bytes of s open a Rust raw or prefixed string (r#", br", b", c"), 0 when s opens none.
func rawStart(s string) int {
	i := 0
	if s[0] == 'b' || s[0] == 'c' {
		i++
	}

	raw := i < len(s) && s[i] == 'r'
	if raw {
		i++
	}

	for raw && i < len(s) && s[i] == '#' {
		i++
	}

	if i == 0 || i >= len(s) || s[i] != '"' || (!raw && i != 1) {
		return 0
	}

	return i + 1
}

// rustStringEnd is the index past the closing quote of a string whose text starts at i: hashes < 0 an ordinary one
// (escapes), else a raw one closed by " and that many #. ok false: it goes on past the line.
func rustStringEnd(l string, i, hashes int) (int, bool) {
	for j := i; j < len(l); j++ {
		switch {
		case l[j] == '\\' && hashes < 0:
			j++
		case l[j] == '"' && (hashes < 0 || strings.HasPrefix(l[j+1:], strings.Repeat("#", hashes))):
			return j + 1 + max(hashes, 0), true
		}
	}

	return len(l), false
}

// rustQuote is the index past a char literal ('a', '\n', '\u{1F600}', b'x') at i, or past the quote of a lifetime
// or a label ('a, 'static).
func rustQuote(l string, i int) int {
	if i+1 < len(l) && l[i+1] == '\\' {
		if j := strings.IndexByte(l[i+2:], '\''); j >= 0 {
			return i + 2 + j + 1
		}

		return len(l)
	}

	if i+1 < len(l) {
		if _, n := utf8.DecodeRuneInString(l[i+1:]); i+1+n < len(l) && l[i+1+n] == '\'' {
			return i + 1 + n + 1
		}
	}

	return i + 1
}

// braceStatements are the statements at depth d from line from to line to (0-based, inclusive): each its first and
// last line. A line that starts by closing the block is none; list: a comma ends a statement (an object's members).
func braceStatements(bl []braceLine, from, to, d int, list bool) [][2]int {
	var out [][2]int

	for i := from; i <= to; i++ {
		b := bl[i]
		if b.in || b.depth != d || b.code == "" || strings.IndexByte(")]}", b.code[0]) >= 0 {
			continue
		}

		e := braceStatementEnd(bl, i, to, d, list)
		out = append(out, [2]int{i, e})
		i = e
	}

	return out
}

// braceStatementEnd is the last line of the statement at line i: where the brackets it opened are closed and the
// line ends it (not an operator, a comma or => at its end; the next line not going on with . | && ? : where { ->).
func braceStatementEnd(bl []braceLine, i, to, d int, list bool) int {
	for j := i; j <= to; j++ {
		if bl[j].min < d {
			return max(j-1, i)
		}

		if bl[j].end != d || j+1 < len(bl) && bl[j+1].in {
			continue
		}

		if braceEnds(bl, j, to, list) {
			return j
		}
	}

	return to
}

var (
	goesOnSuffix = []string{"=>", "=", ",", "(", "[", "+", "-", "*", "/", "%", "&", "|", "^", "?", ":", ".", "<",
		"!", "~", " extends", " implements", " where", " as", " in", " of", " new", " ->"}
	goesOnPrefix = []string{".", "?", ":", "|", "&", "+", "*", "/", "=", "{", ">", "->", "where", "extends", "implements",
		"as ", ")", "]"}
)

func braceEnds(bl []braceLine, j, to int, list bool) bool {
	t := bl[j].code
	if t == "" || t == "where" {
		return false
	}

	for _, s := range goesOnSuffix {
		switch {
		case !strings.HasSuffix(t, s), list && s == ",", s == "-" && strings.HasSuffix(t, "--"),
			s == "+" && strings.HasSuffix(t, "++"):
			continue
		}

		return false
	}

	for k := j + 1; k <= to && k < len(bl); k++ {
		if bl[k].in || bl[k].code == "" {
			continue
		}

		n := bl[k].code
		for _, p := range goesOnPrefix {
			if strings.HasPrefix(n, p) && (p != "{" || bl[k].depth == bl[j].end) {
				return false
			}
		}

		break
	}

	return true
}

// braceItem is what one statement of a brace language is in the outline.
type braceItem struct {
	name   string
	skip   bool // not listed
	attr   bool // a decorator or attribute: it heads the statement after it
	imp    bool // an import: imports in a row are one entry
	walk   int  // its body holds members of this kind (0: none)
	always bool // members listed whatever its size
}

// braceOutline lists a brace language's declarations: the top level's, and the members of the containers the rules
// name (a class, a namespace, an impl, a test suite). A range starts at the comments, decorators and attributes right
// above its first line and ends with its statement. Imports in a row are one entry, "imports (n)".
func braceOutline(src []byte, rust bool, item func(code, line string, kind, span int) braceItem) []Symbol {
	lines := splitLines(src)
	if len(lines) == 0 {
		return nil
	}

	bl, _ := braceScan(lines, rust)

	var out []Symbol

	var walk func(from, to, d, depth int, parent string, kind int)
	walk = func(from, to, d, depth int, parent string, kind int) {
		sts := braceStatements(bl, from, to, d, kind == kindObject)
		head := -1

		for si := 0; si < len(sts); si++ {
			st := sts[si]
			it := item(bl[st[0]].code, strings.TrimSpace(lines[st[0]]), kind, st[1]-st[0]+1)

			if it.attr {
				if head < 0 {
					head = st[0]
				}

				continue
			}

			first := st[0]
			if head >= 0 {
				first, head = head, -1
			}

			if it.skip {
				continue
			}

			start := headStart(lines, first+1)

			if it.imp {
				n, last := 1, st[1]
				for si+1 < len(sts) && item(bl[sts[si+1][0]].code, "", kind, 0).imp {
					si++
					n, last = n+1, sts[si][1]
				}

				out = append(out, Symbol{Start: start, End: last + 1, Depth: depth, Name: "imports",
					Label: "imports (" + itoa(n) + ")"})

				continue
			}

			name := it.name
			if parent != "" {
				name = parent + "." + name
			}

			out = append(out, Symbol{Start: start, End: st[1] + 1, Depth: depth, Name: name,
				Label: clip(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(lines[st[0]]), "{")), 110)})

			if it.walk == 0 || st[1] == st[0] || !it.always && st[1]-st[0] < 40 {
				continue
			}

			// The body: the brackets the statement leaves open at the end of the line that opens it.
			for ob := st[0]; ob < st[1]; ob++ {
				if bl[ob].end > d && !bl[ob+1].in {
					walk(ob+1, st[1], bl[ob].end, depth+1, name, it.walk)

					break
				}
			}
		}
	}
	walk(0, len(lines)-1, 0, 0, "", kindTop)

	return fitMembers(out)
}

const (
	kindTop = iota + 1
	kindClass
	kindObject
	kindSuite
)

// fitMembers drops the members of small containers (40 lines or less) when the whole outline would not fit the
// default cap, so the top level stays whole.
func fitMembers(syms []Symbol) []Symbol {
	size := 0
	for _, s := range syms {
		size += len(s.Label) + 2*s.Depth + 24
	}

	if size <= DefaultMax-400 {
		return syms
	}

	var out []Symbol

	small, dropped := map[string]bool{}, map[string]bool{}

	for _, s := range syms {
		if s.Depth > 0 {
			if p := s.Name[:max(strings.LastIndexByte(s.Name, '.'), 0)]; small[p] || dropped[p] {
				dropped[s.Name] = true
				continue
			}
		}

		small[s.Name] = s.End-s.Start < 40
		out = append(out, s)
	}

	return out
}

// braceSure: the scan closed every bracket by its kind and had to reset nothing; the read guard answers only then.
func braceSure(src []byte, rust bool) bool {
	_, sure := braceScan(splitLines(src), rust)
	return sure
}
