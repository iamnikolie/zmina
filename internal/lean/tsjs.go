package lean

import (
	"regexp"
	"strings"
)

// jsOutline lists a TypeScript or JavaScript file: imports as one entry; functions, classes with their methods,
// interfaces, types, enums, namespaces with their members, top-level consts (an arrow function, a React component
// and a big object's methods among them), exports.x = and module.exports =, and test suites (describe, it, test)
// by their titles. A function's body is no outline.
func jsOutline(src []byte) []Symbol { return braceOutline(src, false, jsItem) }

func jsSure(src []byte) bool { return braceSure(src, false) }

var (
	jsModifiers = map[string]bool{"export": true, "default": true, "declare": true, "abstract": true, "public": true,
		"private": true, "protected": true, "static": true, "readonly": true, "override": true, "accessor": true}
	jsIdent    = `[\p{L}_$#][\p{L}\p{N}_$]*`
	jsDeclRe   = regexp.MustCompile(`^(?:async\s+)?(function\s*\*?|class|interface|type|enum|const\s+enum|namespace|module)\s+(` + jsIdent + `|'[^']*'|"[^"]*")`)
	jsVarRe    = regexp.MustCompile(`^(?:const|let|var|using)\s+(?:(` + jsIdent + `)|[{\[]\s*(?:\.\.\.)?(` + jsIdent + `))`)
	jsAssignRe = regexp.MustCompile(`^(` + jsIdent + `(?:\.` + jsIdent + `|\[[^\]]*\])+)\s*=(?:[^=>]|$)`)
	jsCallRe   = regexp.MustCompile(`^(` + jsIdent + `(?:\.` + jsIdent + `)*)(?:\.each\s*\(.*?\))?\s*\(\s*(?:(['"` + "`" + `])(.*?)['"` + "`" + `])?`)
	jsMethodRe = regexp.MustCompile(`^(?:async\s+)?(?:(?:get|set)\s+)?\*?\s*(` + jsIdent + `|\[[^\]]+\]|'[^']*'|"[^"]*")\s*[?!]?\s*(?:<[^(]*>)?\s*\(`)
	jsPropRe   = regexp.MustCompile(`^(` + jsIdent + `|'[^']*'|"[^"]*")\s*[?!]?\s*(?::[^=]*)?=\s*(?:async\b|function\b|\(|` + jsIdent + `\s*=>|<)`)
	jsKeyRe    = regexp.MustCompile(`^(` + jsIdent + `|'[^']*'|"[^"]*")\s*:\s*(?:async\b|function\b|\(|` + jsIdent + `\s*=>)`)
	jsArrowRe  = regexp.MustCompile(`^` + jsIdent + `\s*=>`)
	jsSuites   = map[string]bool{"describe": true, "context": true, "suite": true}
	jsTests    = map[string]bool{"it": true, "test": true, "bench": true, "specify": true}
)

// jsItem says what one statement is. code is its first line with strings emptied; line the same line as written
// (a test's title and a quoted name come from it).
func jsItem(code, line string, kind, span int) braceItem {
	if strings.HasPrefix(code, "@") {
		return braceItem{attr: true}
	}

	switch kind {
	case kindClass:
		return jsMember(code, line)
	case kindObject:
		if m := jsKeyRe.FindStringSubmatch(line); m != nil {
			return braceItem{name: unquote(m[1])}
		}

		if m := jsMethodRe.FindStringSubmatch(line); m != nil && !controlWord(m[1]) {
			return braceItem{name: unquote(m[1])}
		}

		return braceItem{skip: true}
	}

	if strings.HasPrefix(code, "import ") || strings.HasPrefix(code, "import{") || strings.HasPrefix(code, "import type") ||
		strings.HasPrefix(code, "export ") && strings.Contains(code, " from ") && !jsDeclRe.MatchString(stripJS(code)) ||
		jsVarRe.MatchString(code) && strings.Contains(code, "require(") && span <= 3 {
		return braceItem{imp: true}
	}

	rest := stripJS(code)
	def := strings.HasPrefix(code, "export default")

	if m := jsDeclRe.FindStringSubmatch(rest); m != nil {
		it := braceItem{name: unquote(m[2])}

		switch k := strings.Fields(m[1])[0]; k {
		case "class":
			it.walk, it.always = kindClass, true
		case "namespace", "module":
			it.walk, it.always = kindTop, true
		}

		return it
	}

	if strings.HasPrefix(rest, "class") && (len(rest) == 5 || !wordByte(rest[5])) {
		return braceItem{name: "default", walk: kindClass, always: true} // export default class extends …
	}

	if strings.HasPrefix(code, "declare global") {
		return braceItem{name: "global", walk: kindTop, always: true}
	}

	if m := jsVarRe.FindStringSubmatch(rest); m != nil {
		it := braceItem{name: m[1] + m[2]}
		if eq := strings.Index(rest, "="); eq > 0 && strings.HasPrefix(strings.TrimSpace(rest[eq+1:]), "{") {
			it.walk = kindObject
		}

		return it
	}

	if def {
		it := braceItem{name: "default"}
		if strings.HasPrefix(rest, "{") {
			it.walk = kindObject
		}

		return it
	}

	if strings.HasPrefix(code, "export ") {
		return braceItem{skip: true} // export { a, b }
	}

	if m := jsAssignRe.FindStringSubmatch(code); m != nil {
		rhs := strings.TrimSpace(code[len(m[0])-1:])
		if span >= 5 || jsValueIsCode(rhs) || strings.HasSuffix(code, "=") { // a = b = function …
			it := braceItem{name: m[1]}
			if strings.HasPrefix(rhs, "{") {
				it.walk = kindObject
			}

			return it
		}

		return braceItem{skip: true}
	}

	if m := jsCallRe.FindStringSubmatch(line); m != nil && !controlWord(m[1]) {
		callee := m[1]
		if i := strings.IndexByte(callee, '.'); i > 0 && (jsSuites[callee[:i]] || jsTests[callee[:i]]) {
			callee = callee[:i] // describe.only, it.skip, test.each
		}

		switch {
		case jsSuites[callee] && m[3] != "":
			return braceItem{name: m[3], walk: kindSuite, always: true}
		case jsTests[callee] && m[3] != "":
			return braceItem{name: m[3]}
		case kind == kindTop && span >= 5:
			if m[3] != "" {
				return braceItem{name: m[1] + " " + m[3]} // app.get('/users', …)
			}

			return braceItem{name: m[1]}
		}
	}

	return braceItem{skip: true}
}

// jsMember is a class member: a method, a getter or setter, the constructor, a property holding a function.
func jsMember(code, line string) braceItem {
	rest := stripJS(code)
	if strings.HasPrefix(rest, "{") || strings.HasPrefix(rest, "[") && strings.Contains(rest, ":") && !strings.Contains(rest, "(") {
		return braceItem{skip: true} // static { … }, an index signature
	}

	src := stripJS(line)
	if m := jsMethodRe.FindStringSubmatch(src); m != nil && !controlWord(m[1]) {
		return braceItem{name: unquote(m[1])}
	}

	if m := jsPropRe.FindStringSubmatch(src); m != nil {
		return braceItem{name: unquote(m[1])}
	}

	return braceItem{skip: true}
}

// stripJS drops the modifiers before a declaration: export default declare abstract public static readonly ….
func stripJS(s string) string {
	for {
		w := firstWord(s)
		if !jsModifiers[w] || len(s) == len(w) || s[len(w)] != ' ' && s[len(w)] != '\t' {
			return s
		}

		s = strings.TrimSpace(s[len(w):])
	}
}

// jsValueIsCode: an assigned value that is a function, a class, an arrow function or an object.
func jsValueIsCode(v string) bool {
	for _, p := range []string{"function", "async", "class", "(", "{"} {
		if strings.HasPrefix(v, p) {
			return true
		}
	}

	return jsArrowRe.MatchString(v)
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}

	return s
}
