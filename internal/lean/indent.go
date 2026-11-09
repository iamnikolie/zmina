package lean

import (
	"regexp"
	"strings"
)

// indentOutline is the outline of a file no outliner of its own reads: Python, TypeScript and JavaScript, Java,
// C#, Visual Basic, Swift, PHP, shell, SQL, CSS, pretty-printed XML and HTML; ext is the file's extension. A block is a line whose next code line
// is indented deeper; it runs while the lines stay deeper, through closing lines at its own indent (}, ), End Sub,
// else:, </tag>). The top-level blocks are listed, and the blocks inside a big one, or inside one that wraps the whole
// file one level further; a class's or a namespace's members whatever its size, unless the whole outline would pass
// the cap (fitMembers). A block starts at the comments and decorators right above it; its name is what a target names
// it by: Class.method below the top. A statement over several lines (a signature, a
// call, a list: brackets left open, a backslash at its end, " _" in Visual Basic) counts as its first line. Imports
// in a row are one entry, "imports (n)", when they take five lines or more.
func indentOutline(src []byte, ext string) []Symbol {
	lines := splitLines(src)
	if len(lines) == 0 {
		return nil
	}

	in, _ := codeLines(lines, vbExts[ext])
	if len(in) == 0 {
		return nil
	}

	var out []Symbol

	var walk func(from, to, depth int, parent string)
	walk = func(from, to, depth int, parent string) {
		head := 0 // the first line of the decorators over lines above the next block

		for k := from; k < to; k++ {
			c := in[k]

			// A line that starts by closing something, a bare brace or a docstring opens no block of its own.
			if closer(c.text) || c.text == "{" || docString(c.text) {
				continue
			}

			// A decorator over lines (@pytest.mark.parametrize(…)) heads the block below it.
			if strings.HasPrefix(c.text, "@") && k+1 < to && in[k+1].indent == c.indent {
				if head == 0 {
					head = headStart(lines, c.n)
				}

				continue
			}

			first := head
			head = 0

			if isImport(c.text) {
				n, end := 0, k
				for j := k; j < to && in[j].indent == c.indent && isImport(in[j].text); j = end + 1 {
					n, end = n+1, blockEnd(in, j, to)
				}

				if start, last := headStart(lines, c.n), in[end].last; last-start >= 4 {
					out = append(out, Symbol{Start: start, End: last, Depth: depth, Name: "imports",
						Label: "imports (" + itoa(n) + ")"})
				}

				k = end

				continue
			}

			// A block, or a statement of many lines (CREATE TABLE x ( … ), a big literal), or a declaration on one
			// line (def f(): ...).
			end := blockEnd(in, k, to)
			if end == k && c.last-c.n < 5 && !declLine(c.text) {
				continue
			}

			// Definitions under a top-level condition (if sys.platform == "win32":, try: … except ImportError:) are
			// listed as the top level's.
			if depth == 0 && end > k && strings.HasSuffix(c.text, ":") && condWords[firstWord(c.text)] && holdsDecl(in, k+1, end) {
				walk(k+1, end+1, depth, parent)

				k = end

				continue
			}

			name := blockName(c.text)
			if parent != "" {
				name = parent + "." + name
			}

			start := headStart(lines, c.n)
			if first > 0 {
				start = first
			}

			last := in[end].last
			out = append(out, Symbol{Start: start, End: last, Depth: depth, Name: name, Label: clip(c.text, 110)})

			// Inside a class, a namespace (whatever its size), a big element or a block that is most of the file:
			// its members. Inside a function: nothing (its ifs and loops are no outline).
			span := last - start + 1
			code := container(c.text) && !strings.HasPrefix(c.text, "<") && !strings.HasPrefix(c.text, "@")
			if depth < 3 && end > k && (code || span > 40 && (container(c.text) || span*5 >= len(lines)*4)) {
				walk(k+1, end+1, depth+1, name)
			}

			k = end
		}
	}
	walk(0, len(in), 0, "")

	return fitMembers(out)
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

// vbExts are Visual Basic's: " _" at a line's end goes on to the next line, and a word with a colon alone on a line
// is a label inside a procedure (at column 0 too), not a statement.
var vbExts = map[string]bool{".vb": true, ".bas": true, ".cls": true, ".frm": true, ".ctl": true, ".vbs": true,
	".dsr": true}

// codeLines are the file's statements; closed: no multi-line string or here-document is left open at the end. vb:
// Visual Basic's line continuation and labels.
func codeLines(lines []string, vb bool) (_ []codeLine, closed bool) {
	var (
		out     []codeLine
		quote   string // the open multi-line string's delimiter: """, ''' or `
		heredoc string // the open here-document's closing word
		open    int    // ( and [ the statement has left open so far
		cont    bool   // the statement goes on to the next line
		tail    string // this code line, for the next
		prev    string // the code line before this one
	)

	for i, l := range lines {
		t := strings.TrimSpace(l)

		if heredoc != "" {
			if len(out) > 0 {
				out[len(out)-1].last = i + 1
			}

			if hereEnds(t, heredoc) {
				heredoc = ""
			}

			continue
		}

		inside := quote != ""
		if !inside && (t == "" || isComment(t) || vb && vbLabel.MatchString(t)) {
			continue // a quote in a comment opens no string
		}

		if !vb { // Visual Basic has no multi-line strings; "" is a quote inside one
			quote = stepQuotes(t, quote)
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
		cont = vb && strings.HasSuffix(t, " _") || strings.HasSuffix(t, `\`)

		if quote == "" {
			heredoc = hereStart(t, lines[i+1:])
		}
	}

	return out, quote == "" && heredoc == ""
}

// stepQuotes is the multi-line string open after line t (three double or single quotes, a backtick), given the one
// open before it: a quote inside a string that closes on the line ('"""') or after a comment mark (# or // after a
// space) opens none.
func stepQuotes(t, quote string) string {
	for i := 0; i < len(t); {
		if quote != "" {
			j := strings.Index(t[i:], quote)
			if j < 0 {
				return quote
			}

			i, quote = i+j+len(quote), ""

			continue
		}

		switch r := t[i:]; {
		case strings.HasPrefix(r, `"""`) || strings.HasPrefix(r, `'''`):
			quote, i = r[:3], i+3
		case r[0] == '`':
			quote, i = "`", i+1
		case r[0] == '"' && i > 0 && t[i-1] == '@' || r[0] == '"' && i > 1 && t[i-2] == '@' && t[i-1] == '$':
			i = verbatimEnd(t, i) // C#'s @"…" with "" inside
		case r[0] == '"' || r[0] == '\'':
			if j := closesOnLine(t, i, r[0]); j > 0 {
				i = j
			} else {
				i++
			}
		case (r[0] == '#' || strings.HasPrefix(r, "//")) && (i == 0 || t[i-1] == ' ' || t[i-1] == '\t'):
			return ""
		default:
			i++
		}
	}

	return quote
}

// verbatimEnd is the index past the quote that closes a C# verbatim string opened at i ("" inside is a quote).
func verbatimEnd(t string, i int) int {
	for j := i + 1; j < len(t); j++ {
		if t[j] == '"' {
			if j+1 < len(t) && t[j+1] == '"' {
				j++
				continue
			}

			return j + 1
		}
	}

	return len(t)
}

var docRe = regexp.MustCompile(`^[rRuUbBfF]{0,2}(?:"""|''')`)

// docString: the statement is a string on its own (a Python docstring).
func docString(t string) bool { return docRe.MatchString(t) }

// pySure: every triple-quoted string closes, so no docstring's text is read as code.
func pySure(src []byte) bool {
	_, closed := codeLines(splitLines(src), false)
	return closed
}

var (
	vbLabel = regexp.MustCompile(`^[\p{L}_][\p{L}\p{N}_]*:\s*(?:'.*)?$`)
	hereRe  = regexp.MustCompile(`<<<?[-~]?\s*(?:'([A-Za-z_]\w*)'|"([A-Za-z_]\w*)"|([A-Z_][A-Z0-9_]*)\b)`)
)

// hereStart is the closing word of a here-document the line opens (shell <<EOF, <<-'EOF', PHP <<<EOT, Ruby <<~SQL),
// "" when it opens none: a word in capitals or quoted, closed by a line of its own further down (so "x << MAX" in C
// opens nothing).
func hereStart(t string, rest []string) string {
	if !strings.Contains(t, "<<") {
		return ""
	}

	m := hereRe.FindStringSubmatch(t)
	if m == nil {
		return ""
	}

	w := m[1] + m[2] + m[3]
	for _, l := range rest {
		if hereEnds(strings.TrimSpace(l), w) {
			return w
		}
	}

	return ""
}

// hereEnds: the line closes a here-document opened with word w (PHP lets ; , or ) follow it).
func hereEnds(t, w string) bool {
	return t == w || len(t) > len(w) && strings.HasPrefix(t, w) && strings.ContainsRune(";,)", rune(t[len(w)]))
}

var importRe = regexp.MustCompile(`^(?:import\b|from\s+\S+\s+import\b|use\s+[\w\\:{]|using\s+[\w.]+(?:\s*=\s*[\w.<>]+)?\s*;|` +
	`(?i:imports)\s+\w|require(?:_once)?\b|include(?:_once)?\b|(?:const|let|var)\s+[\w{}\s,:]+=\s*require\()`)

// isImport: the statement imports (import, from … import, use, using, Imports, require).
func isImport(t string) bool { return importRe.MatchString(t) }

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

var condWords = map[string]bool{"if": true, "try:": true, "with": true}

// declLine: the statement declares a function or a class (def, class, function, fn, func, Sub …) after its modifiers.
func declLine(t string) bool {
	for _, w := range strings.Fields(t) {
		w = strings.ToLower(w)
		if modifiers[w] {
			continue
		}

		switch strings.TrimSuffix(w, ":") {
		case "def", "class", "function", "fn", "func", "sub", "procedure":
			return true
		}

		return false
	}

	return false
}

// holdsDecl: a statement of the block's first level, from code line a to b, declares a function or a class.
func holdsDecl(in []codeLine, a, b int) bool {
	for j := a; j <= b; j++ {
		if in[j].indent == in[a].indent && declLine(in[j].text) {
			return true
		}
	}

	return false
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
