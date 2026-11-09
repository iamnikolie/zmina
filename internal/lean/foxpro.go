package lean

import "strings"

// foxOutline lists a FoxPro program's DEFINE CLASS … ENDDEFINE blocks with their methods (Class.Method) and its
// PROCEDURE and FUNCTION blocks. A block starts at the comment lines right above it and runs to its ENDPROC or
// ENDFUNC, else to the next block (blank lines between left out). Keywords take any case and FoxPro's abbreviations
// (PROC, FUNC, DEFI CLAS); comment lines, continued lines and TEXT … ENDTEXT are not read as code.
func foxOutline(src []byte) []Symbol {
	lines := splitLines(src)

	var (
		out   []Symbol
		class = -1 // index in out of the class open, -1: none
		open  = -1 // index in out of the procedure whose end is not known yet
		text  bool // inside TEXT … ENDTEXT
		cont  bool // the line before ended with ';'
	)

	// closeAt ends the open procedure before line n (its header comments and blank lines not included).
	closeAt := func(n int) {
		if open < 0 {
			return
		}

		end := n - 1
		for end > out[open].Start && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}

		out[open].End, open = end, -1
	}

	for i, raw := range lines {
		n := i + 1
		l := strings.TrimSpace(raw)

		// A form exported as text: each object is a class, its methods the procedures in it.
		if name, ok := strings.CutPrefix(l, "OBJNAME:"); ok && !text {
			closeAt(n)

			if class >= 0 {
				out[class].End = n - 1
			}

			start := n
			if n > 1 && strings.HasPrefix(strings.TrimSpace(lines[n-2]), "===== OBJECT") {
				start = n - 1
			}

			out = append(out, Symbol{Start: start, Name: strings.TrimSpace(name), Label: clip(l, 110)})
			class, cont = len(out)-1, false

			continue
		}

		if strings.HasPrefix(l, "===== END OBJECT") && class >= 0 && !text {
			closeAt(n)
			out[class].End, class = n, -1

			continue
		}

		code := foxCode(l)
		wasCont := cont
		cont = strings.HasSuffix(code, ";")

		if wasCont || code == "" {
			continue
		}

		words := strings.Fields(code)
		w0 := strings.ToUpper(words[0])

		if text {
			if foxKeyword(w0, "ENDTEXT", 5) {
				text = false
			}

			continue
		}

		switch {
		case w0 == "TEXT" && (len(words) == 1 || foxTextOption(words[1])):
			text = true
		case foxKeyword(w0, "ENDPROC", 6) || foxKeyword(w0, "ENDFUNC", 5):
			if open >= 0 {
				out[open].End, open = n, -1
			}
		case foxKeyword(w0, "ENDDEFINE", 5):
			if class >= 0 {
				closeAt(n)
				out[class].End, class = n, -1
			}
		case foxKeyword(w0, "DEFINE", 4) && len(words) > 2 && foxKeyword(strings.ToUpper(words[1]), "CLASS", 4):
			name := foxName(words[2])
			if name == "" {
				continue
			}

			start := foxHead(lines, n)
			closeAt(start)

			if class >= 0 { // a class without its ENDDEFINE
				out[class].End = start - 1
			}

			out = append(out, Symbol{Start: start, Name: name, Label: clip(strings.TrimSuffix(code, ";"), 110)})
			class = len(out) - 1
		default:
			k := 0
			for k < len(words)-1 && (strings.EqualFold(words[k], "PROTECTED") || strings.EqualFold(words[k], "HIDDEN")) {
				k++
			}

			kw := strings.ToUpper(words[k])
			if k+1 >= len(words) || !foxKeyword(kw, "PROCEDURE", 4) && !foxKeyword(kw, "FUNCTION", 4) {
				continue
			}

			name := foxName(words[k+1])
			if name == "" {
				continue
			}

			start := foxHead(lines, n)
			closeAt(start)

			s := Symbol{Start: start, Name: name, Label: clip(strings.TrimSuffix(code, ";"), 110)}
			if class >= 0 {
				s.Depth, s.Name = 1, out[class].Name+"."+name
			}

			out = append(out, s)
			open = len(out) - 1
		}
	}

	closeAt(len(lines) + 1)

	if class >= 0 {
		out[class].End = len(lines)
	}

	return out
}

// foxCode is a line without its comment: "" for a comment line (*, &&, NOTE), the code before && otherwise.
func foxCode(l string) string {
	if l == "" || l[0] == '*' || strings.HasPrefix(l, "&&") || foxKeyword(strings.ToUpper(firstWord(l)), "NOTE", 4) {
		return ""
	}

	if i := strings.Index(l, "&&"); i >= 0 {
		l = l[:i]
	}

	return strings.TrimSpace(l)
}

func firstWord(l string) string {
	if f := strings.Fields(l); len(f) > 0 {
		return f[0]
	}

	return ""
}

// foxHead is where a block starting at line n begins: the comment lines right above it.
func foxHead(lines []string, n int) int {
	for n > 1 {
		l := strings.TrimSpace(lines[n-2])
		if l == "" || foxCode(l) != "" {
			break
		}

		n--
	}

	return n
}

// foxKeyword: w (upper case) is the keyword or its abbreviation of at least least letters.
func foxKeyword(w, keyword string, least int) bool {
	return len(w) >= least && strings.HasPrefix(keyword, w)
}

// foxTextOption: the word after TEXT is one of its clauses, not "=" of a variable called text.
func foxTextOption(w string) bool {
	switch strings.ToUpper(w) {
	case "TO", "NOSHOW", "ADDITIVE", "TEXTMERGE", "PRETEXT", "FLAGS":
		return true
	}

	return false
}

// foxName is a block's name: the word up to its parameter list; "" when it is no name (func = 5).
func foxName(w string) string {
	if i := strings.IndexAny(w, "(,"); i >= 0 {
		w = w[:i]
	}

	if w == "" {
		return ""
	}

	c := w[0]
	if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && c < 0x80 {
		return ""
	}

	return w
}
