package lean

import (
	"regexp"
	"strings"
)

// rustOutline lists a Rust file: use as one entry; fn, struct, enum, union, type, const, static, macro_rules!;
// trait, impl and inline mod with their items (an impl's fn is Type.method); a top-level macro call over lines by
// its first argument (a test macro's name). A fn's body is no outline.
func rustOutline(src []byte) []Symbol { return braceOutline(src, true, rustItem) }

func rustSure(src []byte) bool { return braceSure(src, true) }

var (
	rustVisRe   = regexp.MustCompile(`^pub(?:\s*\([^)]*\))?\s+`)
	rustNameRe  = regexp.MustCompile(`^(fn|struct|enum|union|trait|type|const|static(?:\s+mut)?|mod|macro_rules!)\s*([\p{L}_][\p{L}\p{N}_]*)`)
	rustMacroRe = regexp.MustCompile(`^([\p{L}_][\p{L}\p{N}_:]*)!\s*[({\[]\s*([\p{L}_][\p{L}\p{N}_]*)?`)
	rustPathRe  = regexp.MustCompile(`^[\p{L}_][\p{L}\p{N}_]*(?:::[\p{L}_][\p{L}\p{N}_]*)*`)
)

// rustItem says what one Rust statement is.
func rustItem(code, _ string, kind, span int) braceItem {
	switch {
	case strings.HasPrefix(code, "#!["):
		return braceItem{skip: true}
	case strings.HasPrefix(code, "#["):
		return braceItem{attr: true}
	case strings.HasPrefix(rustVisRe.ReplaceAllString(code, ""), "use ") || strings.HasPrefix(code, "extern crate "):
		return braceItem{imp: true}
	}

	rest := rustStrip(rustVisRe.ReplaceAllString(code, ""))

	if strings.HasPrefix(rest, "impl") && (len(rest) == 4 || !wordByte(rest[4])) {
		return braceItem{name: rustImplName(rest[4:]), walk: kindClass, always: true}
	}

	if strings.HasPrefix(rest, `extern ""`) || rest == "extern {" || strings.HasPrefix(rest, "extern {") {
		return braceItem{name: "extern", walk: kindClass, always: true}
	}

	if m := rustNameRe.FindStringSubmatch(rest); m != nil {
		it := braceItem{name: m[2]}

		switch m[1] {
		case "trait":
			it.walk, it.always = kindClass, true
		case "mod":
			if strings.Contains(rest, "{") {
				it.walk, it.always = kindTop, true
			}
		}

		return it
	}

	if m := rustMacroRe.FindStringSubmatch(rest); m != nil && kind == kindTop && span >= 2 {
		if m[2] != "" {
			return braceItem{name: m[2]}
		}

		return braceItem{name: m[1]}
	}

	return braceItem{skip: true}
}

// rustStrip drops the modifiers before an item's kind: default, async, unsafe, auto, const and extern "C" before fn.
func rustStrip(s string) string {
	for {
		w := firstWord(s)
		next := firstWord(strings.TrimSpace(s[len(w):]))

		switch {
		case w == "default" || w == "async" || w == "unsafe" || w == "auto":
		case w == "const" && (next == "fn" || next == "unsafe" || next == "async" || next == "extern"):
		case w == "extern" && next == `""` && firstWord(strings.TrimSpace(s[len(w)+3:])) == "fn":
			s = strings.TrimSpace(s[len(w)+3:])
			continue
		case w == "extern" && next == "fn":
		default:
			return s
		}

		s = strings.TrimSpace(s[len(w):])
	}
}

// rustImplName is the type an impl block is for: impl<T> Trait<X> for &'a mut Type<T> where … is Type.
func rustImplName(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "<") {
		s = strings.TrimSpace(s[angleEnd(s):])
	}

	// The type follows " for " outside angle brackets; without one the impl is the type's own.
	depth := 0

	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			depth++
		case '>':
			depth--
		case '{':
			i = len(s)
		case ' ':
			if depth == 0 && strings.HasPrefix(s[i:], " for ") {
				s = s[i+5:]
				i = len(s)
			}
		}
	}

	for {
		t := strings.TrimLeft(s, "&!( ")
		t = strings.TrimPrefix(t, "mut ")
		t = strings.TrimPrefix(t, "dyn ")

		if strings.HasPrefix(t, "'") || strings.HasPrefix(t, "''") {
			t = strings.TrimLeft(t, "'")
			t = strings.TrimLeft(t, "abcdefghijklmnopqrstuvwxyz_0123456789")
		}

		if t == s {
			break
		}

		s = t
	}

	p := rustPathRe.FindString(s)
	if i := strings.LastIndex(p, "::"); i >= 0 {
		p = p[i+2:]
	}

	if p == "" {
		return "impl"
	}

	return p
}

// angleEnd is the index past the > that closes the < at s[0] (len(s) when none does).
func angleEnd(s string) int {
	depth := 0

	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			depth++
		case '>':
			if i > 0 && s[i-1] == '-' {
				continue // -> inside: Fn() -> T
			}

			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}

	return len(s)
}
