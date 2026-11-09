package lean

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// GrepOptions shape one grep call.
type GrepOptions struct {
	Max        int    // total characters; 0: 4000
	PerFile    int    // hits shown a file; 0: 5
	IgnoreCase bool   // -i
	Fixed      bool   // -F: the pattern is a plain string
	Glob       string // -g: only files whose base name matches
	// Count (-c): no hit lines, the number a file and, in a file with an outline, a section.
	Count bool
}

// skipDirs are never walked into unless named as a root.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true,
	".next": true, "target": true, "__pycache__": true, ".venv": true, "venv": true}

const (
	grepMaxFile  = 2 << 20
	grepMaxFiles = 20000
)

type hit struct {
	line int
	text string
}

// Grep searches the roots (files or directories) and writes hits grouped by file and by the function or section
// they sit in, within a budget, then what it left out and how to see it.
func Grep(pattern string, roots []string, dir string, o GrepOptions) (string, error) {
	if o.Max <= 0 {
		o.Max = 4000
	}

	if o.PerFile <= 0 {
		o.PerFile = 5
	}

	expr := pattern
	if o.Fixed {
		expr = regexp.QuoteMeta(pattern)
	}

	if o.IgnoreCase {
		expr = "(?i)" + expr
	}

	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("pattern: %w (Go regexp syntax; -F for a plain string)", err)
	}

	if len(roots) == 0 {
		roots = []string{"."}
	}

	var (
		files   []string
		walked  int
		stopped bool
	)

	for _, r := range roots {
		root := abs(r, dir)

		info, err := os.Stat(root)
		if err != nil {
			return "", fmt.Errorf("%s: %w", r, err)
		}

		if !info.IsDir() {
			files = append(files, r)
			continue
		}

		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}

			if d.IsDir() {
				if p != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
					return filepath.SkipDir
				}

				return nil
			}

			walked++
			if walked > grepMaxFiles {
				stopped = true
				return filepath.SkipAll
			}

			if o.Glob != "" {
				if ok, _ := filepath.Match(o.Glob, d.Name()); !ok {
					return nil
				}
			}

			rel, _ := filepath.Rel(dir, p)
			if dir == "" || strings.HasPrefix(rel, "..") {
				rel = p
			}

			files = append(files, rel)

			return nil
		})
	}

	sort.Strings(files)

	var (
		b          strings.Builder
		total      int
		inFiles    int
		shown      int
		cutFiles   []string
		budgetHit  bool
		moreInFile int
		counts     []countFile
	)

	for _, name := range files {
		p := abs(name, dir)

		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || info.Size() > grepMaxFile {
			continue // a FIFO in the tree would block a read forever
		}

		src, err := os.ReadFile(p)
		if err != nil || Kind(p, src) == "binary" {
			continue
		}

		src, _ = Decode(src) // a cp1251 file matches and shows as text

		var hits []hit

		for i, l := range splitLines(src) {
			if re.MatchString(l) {
				hits = append(hits, hit{i + 1, l})
			}
		}

		if len(hits) == 0 {
			continue
		}

		total += len(hits)
		inFiles++

		if o.Count {
			counts = append(counts, countFile{name, len(hits), countSections(p, src, hits)})
			continue
		}

		if budgetHit {
			cutFiles = append(cutFiles, name)
			continue
		}

		block := grepBlock(name, p, src, hits, o.PerFile)
		if b.Len()+len(block) > o.Max {
			budgetHit = true

			cutFiles = append(cutFiles, name)

			continue
		}

		b.WriteString(block)

		n := min(len(hits), o.PerFile)
		shown += n
		moreInFile += len(hits) - n
	}

	if total == 0 {
		return fmt.Sprintf("no match for %q\n", pattern), nil
	}

	var bare int // files whose sections did not fit

	if o.Count {
		cutFiles, bare = writeCounts(&b, counts, o.Max)
		fmt.Fprintf(&b, "— %d hits in %d files", total, inFiles)

		if bare > 0 {
			fmt.Fprintf(&b, "; sections of %d files left out (-c on one file shows them)", bare)
		}
	} else {
		fmt.Fprintf(&b, "— %d hits in %d files; shown %d", total, inFiles, shown)
	}

	if moreInFile > 0 {
		fmt.Fprintf(&b, "; %d more in the files above (--per-file %d)", moreInFile, o.PerFile*4)
	}

	if len(cutFiles) > 0 {
		fmt.Fprintf(&b, "; not shown, over the %s budget: %s (--max %d, or grep one of them)", kchars(o.Max),
			listFew(cutFiles, 6), o.Max*3)
	}

	if stopped {
		fmt.Fprintf(&b, "; stopped after %d files: name a narrower path", grepMaxFiles)
	}

	b.WriteString("\n")

	return b.String(), nil
}

// grepBlock is one file's hits: under the function or section each sits in (with how many hits it holds), numbered,
// clipped to one short line.
func grepBlock(name, path string, src []byte, hits []hit, perFile int) string {
	syms := Outline(path, src)
	in := sectionCounts(syms, hits)

	var b strings.Builder

	fmt.Fprintf(&b, "%s (%d)\n", name, len(hits))

	last := ""

	for i, h := range hits {
		if i == perFile {
			break
		}

		if s, ok := enclosing(syms, h.line); ok {
			if s.Label != last {
				fmt.Fprintf(&b, "  %s · %d-%d (%d)\n", s.Label, s.Start, s.End, in[s.Start])
				last = s.Label
			}
		} else if last != "" {
			last = ""
		}

		fmt.Fprintf(&b, "    %d: %s\n", h.line, clip(strings.TrimSpace(h.text), 160))
	}

	return b.String()
}

// countRows is the most section rows grep -c shows a file.
const countRows = 12

// countFile is one file of grep -c: its count and its section rows.
type countFile struct {
	name     string
	n        int
	sections string
}

// writeCounts writes a line a file, then each file's section rows while they fit: the files' counts come first. It
// returns the files left out (their lines over the budget) and how many files went without their sections.
func writeCounts(b *strings.Builder, files []countFile, budget int) (cut []string, bare int) {
	lines := make([]string, len(files))
	room := budget

	for i, f := range files {
		lines[i] = fmt.Sprintf("%s %d\n", f.name, f.n)
		room -= len(lines[i])
	}

	for i, f := range files {
		if b.Len()+len(lines[i]) > budget || len(cut) > 0 {
			cut = append(cut, f.name)
			continue
		}

		b.WriteString(lines[i])

		if f.sections == "" {
			continue
		}

		if len(f.sections) <= room {
			b.WriteString(f.sections)
			room -= len(f.sections)
		} else {
			bare++
		}
	}

	return cut, bare
}

// countSections is the count in each section of the file that holds a hit, in the file's order; "" for a file
// without an outline.
func countSections(path string, src []byte, hits []hit) string {
	syms := Outline(path, src)
	if len(syms) == 0 {
		return ""
	}

	var b strings.Builder

	in := sectionCounts(syms, hits)

	var (
		rows, outside, restN, restHits int
		seen                           = map[int]bool{}
	)

	for _, h := range hits {
		s, ok := enclosing(syms, h.line)
		if !ok {
			outside++
			continue
		}

		if seen[s.Start] {
			continue
		}

		seen[s.Start] = true

		if rows == countRows {
			restN++
			restHits += in[s.Start]

			continue
		}

		rows++

		fmt.Fprintf(&b, "  %d\t%s · %d-%d\n", in[s.Start], s.Label, s.Start, s.End)
	}

	if restN > 0 {
		fmt.Fprintf(&b, "  %d\tin %d more sections\n", restHits, restN)
	}

	if outside > 0 {
		fmt.Fprintf(&b, "  %d\toutside any section\n", outside)
	}

	return b.String()
}

// sectionCounts is how many hits each innermost section holds, by its start line.
func sectionCounts(syms []Symbol, hits []hit) map[int]int {
	in := map[int]int{}

	for _, h := range hits {
		if s, ok := enclosing(syms, h.line); ok {
			in[s.Start]++
		}
	}

	return in
}

// enclosing is the innermost outline entry holding the line (the deepest one that starts last).
func enclosing(syms []Symbol, line int) (Symbol, bool) {
	var (
		best Symbol
		ok   bool
	)

	for _, s := range syms {
		if s.Start <= line && line <= s.End && (!ok || s.Depth >= best.Depth) {
			best, ok = s, true
		}
	}

	return best, ok
}

func listFew(list []string, n int) string {
	if len(list) <= n {
		return strings.Join(list, ", ")
	}

	return strings.Join(list[:n], ", ") + fmt.Sprintf(" and %d more", len(list)-n)
}
