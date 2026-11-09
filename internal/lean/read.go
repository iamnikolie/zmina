package lean

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DefaultMax is a call's output cap in characters: about 3k tokens, near what one more turn costs to re-read the
// context (design/READER.md: below this, an outline and a second read cost more than the read itself).
const DefaultMax = 12000

// Options shape one read call.
type Options struct {
	Max     int  // total characters for every target of the call; 0: DefaultMax
	Full    bool // no cap, no outline: the file as it is
	Outline bool // the outline only
	// Via names the command the guard replaced; the output says so, once, at its head.
	Via string
}

// Target is one thing to read: a whole file, a symbol (path:Name), a section (path#Heading) or lines (path:120-180,
// path:412-, path:120 for the lines around 120).
type Target struct {
	Path    string
	Symbol  string
	Heading string
	From    int // 0: none
	To      int // 0: to the end
}

var rangeRe = regexp.MustCompile(`^(\d+)(?:-(\d*))?$`)

// ParseTarget reads one argument. A path that exists as written wins over any reading of ':' or '#' in it.
func ParseTarget(arg, dir string) Target {
	if exists(abs(arg, dir)) {
		return Target{Path: arg}
	}

	if i := strings.LastIndex(arg, ":"); i > 0 && exists(abs(arg[:i], dir)) {
		t := Target{Path: arg[:i]}

		rest := arg[i+1:]
		if m := rangeRe.FindStringSubmatch(rest); m != nil {
			a, _ := strconv.Atoi(m[1])

			switch {
			case !strings.Contains(rest, "-"):
				t.From, t.To = max(1, a-10), a+30
			case m[2] == "":
				t.From = a
			default:
				b, _ := strconv.Atoi(m[2])
				t.From, t.To = a, b
			}

			return t
		}

		t.Symbol = rest

		return t
	}

	if i := strings.Index(arg, "#"); i > 0 && exists(abs(arg[:i], dir)) {
		return Target{Path: arg[:i], Heading: arg[i+1:]}
	}

	return Target{Path: arg}
}

func abs(p, dir string) string {
	if filepath.IsAbs(p) || dir == "" {
		return p
	}

	return filepath.Join(dir, p)
}

func exists(p string) bool {
	_, err := os.Stat(p)

	return err == nil
}

// Read renders the targets into one output under one cap. dir resolves relative paths. The second value is the
// number of characters printed (for the meter).
func Read(targets []Target, dir string, o Options) string {
	max := o.Max
	if max <= 0 {
		max = DefaultMax
	}

	if o.Full {
		max = 1 << 40
	}

	var b strings.Builder

	if o.Via != "" {
		fmt.Fprintf(&b, "(read-guard: `%s` would print over %s characters; this is zmina read. The raw output: command %s)\n",
			clip(o.Via, 80), kchars(DefaultMax), clip(o.Via, 80))
	}

	for i, t := range targets {
		left := max - b.Len()
		if left <= 200 && i > 0 {
			var rest []string
			for _, r := range targets[i:] {
				rest = append(rest, r.String())
			}

			fmt.Fprintf(&b, "… not shown (cap %s): %s; raise it with --max\n", kchars(max), strings.Join(rest, " "))

			break
		}

		readOne(&b, t, dir, o, left)
	}

	return b.String()
}

func (t Target) String() string {
	switch {
	case t.Symbol != "":
		return t.Path + ":" + t.Symbol
	case t.Heading != "":
		return t.Path + "#" + t.Heading
	case t.From > 0 && t.To > 0:
		return fmt.Sprintf("%s:%d-%d", t.Path, t.From, t.To)
	case t.From > 0:
		return fmt.Sprintf("%s:%d-", t.Path, t.From)
	}

	return t.Path
}

// File is a read file with what the reader knows of it.
type File struct {
	Path  string
	Src   []byte
	Lines []string // without their newlines
	Kind  string   // "", "binary", "generated (…)"
}

// MaxFile is the biggest file zmina read loads: a bigger one (a log of gigabytes) is read in part with sed or tail.
const MaxFile = 8 << 20

// Load reads a regular file of at most MaxFile bytes and tells what kind it is. A device, a FIFO or a bigger file is
// refused before a byte is read: /dev/zero would never end.
func Load(path string) (*File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}

	if info.Size() > MaxFile {
		return nil, fmt.Errorf("%.1f MB, over zmina read's %d MB: read a part with sed -n or tail", float64(info.Size())/(1<<20), MaxFile>>20)
	}

	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	f := &File{Path: path, Src: src, Kind: Kind(path, src)}
	if f.Kind != "binary" {
		f.Lines = splitLines(src)
	}

	return f, nil
}

func splitLines(src []byte) []string {
	s := string(src)
	s = strings.TrimSuffix(s, "\n")

	if s == "" {
		return nil
	}

	return strings.Split(s, "\n")
}

var generatedNames = regexp.MustCompile(`(^|/)(package-lock\.json|pnpm-lock\.yaml|yarn\.lock|go\.sum|Cargo\.lock|` +
	`poetry\.lock|composer\.lock|Gemfile\.lock|[^/]+\.lock|[^/]+\.min\.(js|css)|[^/]+\.map|[^/]+\.pb\.go|[^/]+_pb2?\.py)$`)

// Kind is "binary", "generated (<why>)", or "" for a file worth reading.
func Kind(path string, src []byte) string {
	head := src[:min(len(src), 8000)]
	if bytes.IndexByte(head, 0) >= 0 {
		return "binary"
	}

	if generatedNames.MatchString(filepath.ToSlash(path)) {
		return "generated (lock, minified or generated file)"
	}

	if bytes.Contains(head, []byte("Code generated")) && bytes.Contains(head, []byte("DO NOT EDIT")) {
		return "generated (Code generated … DO NOT EDIT)"
	}

	if len(src) > 5000 {
		lines := bytes.Count(src, []byte("\n")) + 1
		if len(src)/lines > 500 {
			return "generated (minified: long lines)"
		}
	}

	return ""
}

func readOne(b *strings.Builder, t Target, dir string, o Options, left int) {
	p := abs(t.Path, dir)

	f, err := Load(p)
	if err != nil {
		fmt.Fprintf(b, "== %s · %s\n", t.Path, errText(err))
		return
	}

	header := fmt.Sprintf("== %s · %s lines · %s", t.Path, thousands(len(f.Lines)), kchars(len(f.Src)))
	if l := Lang(p); l != "" {
		header += " · " + l
	}

	if f.Kind == "binary" {
		fmt.Fprintf(b, "%s · binary, not shown\n", header)
		return
	}

	whole := t.Symbol == "" && t.Heading == "" && t.From == 0

	if f.Kind != "" && whole && !o.Full {
		how := "a range (" + t.Path + ":1-80) or --full reads it"
		if strings.Contains(f.Kind, "minified") {
			how = "--full reads it"
		}

		fmt.Fprintf(b, "%s · %s, not shown; %s\n", header, f.Kind, how)

		return
	}

	switch {
	case t.Symbol != "" || t.Heading != "":
		readSymbol(b, f, t, header, left)
	case t.From > 0:
		to := t.To
		if to == 0 || to > len(f.Lines) {
			to = len(f.Lines)
		}

		fmt.Fprintf(b, "%s · %d-%d\n", header, t.From, to)
		writeLines(b, f, t.Path, t.From, to, left)
	case o.Outline:
		writeOutline(b, f, t.Path, header, left)
	case o.Full || len(f.Src) <= left-len(header)-8*len(f.Lines)-40:
		fmt.Fprintln(b, header)
		writeLines(b, f, t.Path, 1, len(f.Lines), left)
	default:
		writeOutline(b, f, t.Path, header, left)
	}
}

func errText(err error) string {
	if os.IsNotExist(err) {
		return "no such file"
	}

	return err.Error()
}

// writeLines writes lines from..to numbered, stopping before the cap with the command that goes on.
func writeLines(b *strings.Builder, f *File, path string, from, to, left int) {
	if from < 1 {
		from = 1
	}

	start := b.Len()

	for n := from; n <= to && n <= len(f.Lines); n++ {
		line := f.Lines[n-1]
		if n == from && len(line)+8 > left-120 {
			// One line longer than the room: a pointer to the same line again would loop.
			room := max(left-240, 80)
			fmt.Fprintf(b, "%d\t%s…\n… line %d is %s characters, cut; all of it: zmina read %s --full\n", n,
				clipBytes(line, room), n, thousands(len(line)), path)

			return
		}

		if b.Len()-start+len(line)+8 > left-120 {
			fmt.Fprintf(b, "… cut at line %d of %s; next: zmina read %s:%d-%s\n", n-1, thousands(len(f.Lines)), path, n,
				endOf(to, len(f.Lines)))

			return
		}

		b.WriteString(strconv.Itoa(n))
		b.WriteByte('\t')
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

// clipBytes cuts s to at most n bytes on a rune boundary.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}

	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}

func endOf(to, lines int) string {
	if to >= lines {
		return ""
	}

	return strconv.Itoa(to)
}

// writeOutline writes the outline with each entry's size, then how to read a piece. A file with no outline gets its
// first lines instead.
func writeOutline(b *strings.Builder, f *File, path, header string, left int) {
	writeSyms(b, f, Outline(f.Path, f.Src), path, header, left)
}

// writeSyms writes an outline already made; with none, the file's first lines.
func writeSyms(b *strings.Builder, f *File, syms []Symbol, path, header string, left int) {
	if len(syms) == 0 {
		fmt.Fprintf(b, "%s · no outline for this type; its start:\n", header)
		writeLines(b, f, path, 1, len(f.Lines), left)

		return
	}

	fmt.Fprintf(b, "%s · outline (lines, size):\n", header)

	start := b.Len()

	for i, s := range syms {
		row := fmt.Sprintf("%s%d-%d\t%s\t%s\n", strings.Repeat("  ", s.Depth), s.Start, s.End, s.Label, kchars(span(f, s)))
		if b.Len()-start+len(row) > left-200 {
			fmt.Fprintf(b, "… %d more entries; next: zmina read %s:%d-\n", len(syms)-i, path, s.Start)
			break
		}

		b.WriteString(row)
	}

	fmt.Fprintf(b, "read a piece: zmina read %s%s | %s:120-180 · all of it: --full\n", path, pieceHint(Lang(f.Path)), path)
}

func pieceHint(lang string) string {
	switch lang {
	case "go":
		return ":Name (Func, Type.Method)"
	case "markdown":
		return "#Heading"
	case "yaml", "json":
		return ":key (a.b below the top)"
	}

	return ""
}

func span(f *File, s Symbol) int {
	n := 0
	for i := s.Start; i <= s.End && i <= len(f.Lines); i++ {
		n += len(f.Lines[i-1]) + 1
	}

	return n
}

// readSymbol writes the symbols or sections the target names: exact name first, then case-insensitive, then a
// suffix (Method finds Type.Method) or, for a heading, a part of it. A miss lists the near names.
func readSymbol(b *strings.Builder, f *File, t Target, header string, left int) {
	syms := Outline(f.Path, f.Src)
	want := t.Symbol
	if t.Heading != "" {
		want = strings.TrimSpace(strings.TrimLeft(t.Heading, "# "))
	}

	found := match(syms, want, t.Heading != "")
	if len(found) == 0 {
		fmt.Fprintf(b, "%s · %q not found", header, want)

		if near := nearNames(syms, want); len(near) > 0 {
			fmt.Fprintf(b, "; near: %s", strings.Join(near, ", "))
		}

		fmt.Fprintf(b, "; the outline: zmina read %s --outline\n", t.Path)

		return
	}

	start := b.Len()

	for i, s := range found {
		if i > 0 && b.Len()-start > left-300 {
			fmt.Fprintf(b, "… %d more matches for %q: name one by its lines (%s:%d-%d)\n", len(found)-i, want, t.Path, s.Start, s.End)
			break
		}

		fmt.Fprintf(b, "%s · %d-%d %s\n", header, s.Start, s.End, s.Label)
		writeLines(b, f, t.Path, s.Start, s.End, left-(b.Len()-start))
	}
}

func match(syms []Symbol, want string, heading bool) []Symbol {
	tries := []func(s Symbol) bool{
		func(s Symbol) bool { return s.Name == want },
		func(s Symbol) bool { return strings.EqualFold(s.Name, want) },
		func(s Symbol) bool {
			return strings.HasSuffix(strings.ToLower(s.Name), "."+strings.ToLower(want)) ||
				slices.Contains(strings.Split(s.Name, ","), want)
		},
	}
	if heading {
		tries = append(tries, func(s Symbol) bool { return strings.Contains(strings.ToLower(s.Name), strings.ToLower(want)) })
	}

	for _, try := range tries {
		var out []Symbol

		for _, s := range syms {
			if try(s) {
				out = append(out, s)
			}
		}

		if len(out) > 0 {
			return out
		}
	}

	return nil
}

func nearNames(syms []Symbol, want string) []string {
	w := strings.ToLower(want)

	var out []string

	for _, s := range syms {
		n := strings.ToLower(s.Name)
		if strings.Contains(n, w) || strings.Contains(w, n) || sharesWord(n, w) {
			out = append(out, s.Name)
		}

		if len(out) == 8 {
			break
		}
	}

	return out
}

func sharesWord(a, b string) bool {
	for _, x := range strings.FieldsFunc(a, notWord) {
		if len(x) < 4 {
			continue
		}

		if slices.Contains(strings.FieldsFunc(b, notWord), x) {
			return true
		}
	}

	return false
}

func notWord(r rune) bool {
	return r <= ' ' || strings.ContainsRune(".,:;#()[]{}/-_*`'\"", r)
}

func kchars(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}

	return strconv.FormatFloat(float64(n)/1000, 'f', 1, 64) + "k"
}

func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}

	return s
}
