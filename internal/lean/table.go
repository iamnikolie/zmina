package lean

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// A table file (CSV, TSV, JSON Lines) has no outline: its summary says what a row holds, how many there are and how
// they start. The summary shows rows, so it is never what the read guard answers with.

const (
	tableFirst = 5   // first rows shown
	tableCell  = 300 // a shown row is cut at this many bytes
)

// writeCSVSummary writes a delimited file's columns (by its header line) with each one's type and empty count, the
// number of rows, rows of another width, then its first rows and its last.
func writeCSVSummary(b *strings.Builder, f *File, path, header string, left int) {
	sep := csvSeparator(f)

	var (
		cols    []string
		kinds   []colKind
		rows    int
		ragged  int
		raggedL int // the line of the first row of another width
		bad     string
	)

	row := func(rec []string, line int) {
		if cols == nil {
			cols = append([]string(nil), rec...)
			kinds = make([]colKind, len(cols))

			return
		}

		rows++

		if len(rec) != len(cols) {
			if ragged++; raggedL == 0 {
				raggedL = line
			}
		}

		for i := range min(len(rec), len(kinds)) {
			kinds[i].see(rec[i])
		}
	}

	if tsvFile(f.Path) { // a TSV has no quoting: a row is a line, a quote is a character
		for n, l := range f.Lines {
			if strings.TrimSpace(l) != "" {
				row(strings.Split(strings.TrimSuffix(l, "\r"), "\t"), n+1)
			}
		}
	} else {
		r := csv.NewReader(bytes.NewReader(f.Src))
		r.Comma, r.LazyQuotes, r.FieldsPerRecord, r.ReuseRecord = sep, true, -1, true

		for {
			rec, err := r.Read()
			if errors.Is(err, io.EOF) {
				break
			}

			if err != nil {
				bad = err.Error()
				break
			}

			line, _ := r.FieldPos(0)
			row(rec, line)
		}
	}

	name := map[rune]string{',': `","`, ';': `";"`, '\t': "tab", '|': `"|"`}[sep]
	fmt.Fprintf(b, "%s · summary:\n", header)

	start := b.Len()
	room := func() int { return left - (b.Len() - start) - 400 }

	fmt.Fprintf(b, "columns (%d, separated by %s, by the first line):\n", len(cols), name)

	for i, c := range cols {
		row := fmt.Sprintf("  %d %s\t%s\n", i+1, clip(strings.TrimSpace(c), 80), kinds[i].String(rows))
		if len(row) > room()/2 {
			fmt.Fprintf(b, "  … %d more columns\n", len(cols)-i)
			break
		}

		b.WriteString(row)
	}

	fmt.Fprintf(b, "rows: %s after the header line", thousands(rows))

	if ragged > 0 {
		fmt.Fprintf(b, "; %s of another width (first at line %d)", thousands(ragged), raggedL)
	}

	if bad != "" {
		fmt.Fprintf(b, "; parsing stopped: %s", bad)
	}

	b.WriteString("\n")
	writeTableRows(b, f, path, room())
}

// writeJSONLSummary writes a JSON Lines file's keys (of its objects, in the order first seen) with each one's types
// and how many rows have it, the number of rows and lines that are no object, then its first rows and its last.
func writeJSONLSummary(b *strings.Builder, f *File, path, header string, left int) {
	var (
		keys   []string
		kinds  = map[string]*colKind{}
		rows   int
		other  int
		otherL int
	)

	for n, l := range f.Lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}

		rows++

		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(l), &obj) != nil {
			if other++; otherL == 0 {
				otherL = n + 1
			}

			continue
		}

		for k, v := range obj {
			c := kinds[k]
			if c == nil {
				c = &colKind{}
				kinds[k] = c
				keys = append(keys, k)
			}

			c.seeJSON(v)
		}
	}

	// keys in the order of the first object's text, then the rest as found
	if len(f.Lines) > 0 {
		pos := func(k string) int {
			q, _ := json.Marshal(k)
			if i := strings.Index(f.Lines[0], string(q)+":"); i >= 0 {
				return i
			}

			return 1 << 30
		}
		slices.SortStableFunc(keys, func(a, b string) int { return pos(a) - pos(b) })
	}

	fmt.Fprintf(b, "%s · summary:\n", header)

	start := b.Len()
	room := func() int { return left - (b.Len() - start) - 400 }

	fmt.Fprintf(b, "keys (%d, of the objects on each line):\n", len(keys))

	for i, k := range keys {
		row := fmt.Sprintf("  %s\t%s\n", clip(k, 80), kinds[k].String(rows-other))
		if len(row) > room()/2 {
			fmt.Fprintf(b, "  … %d more keys\n", len(keys)-i)
			break
		}

		b.WriteString(row)
	}

	fmt.Fprintf(b, "rows: %s", thousands(rows))

	if other > 0 {
		fmt.Fprintf(b, "; %s not a JSON object (first at line %d)", thousands(other), otherL)
	}

	b.WriteString("\n")
	writeTableRows(b, f, path, room())
}

// writeTableRows writes the first rows and the last, each cut at tableCell bytes, then how to read more.
func writeTableRows(b *strings.Builder, f *File, path string, left int) {
	row := func(n int) string {
		l := f.Lines[n-1]
		if len(l) > tableCell {
			l = clipBytes(l, tableCell) + "…"
		}

		return strconv.Itoa(n) + "\t" + l + "\n"
	}

	first := min(tableFirst+1, len(f.Lines)) // the header line and five rows
	b.WriteString("first rows:\n")

	used := 0

	for n := 1; n <= first; n++ {
		r := row(n)
		if used+len(r) > left {
			break
		}

		b.WriteString(r)
		used += len(r)
	}

	if last := len(f.Lines); last > first {
		if r := row(last); used+len(r) <= left {
			b.WriteString("last row:\n")
			b.WriteString(r)
		}
	}

	fmt.Fprintf(b, "read rows: zmina read %s:1-50 | zmina grep <regex> %s · all of it: --full\n", path, path)
}

// csvSeparator is the file's separator: tab for .tsv and .tab, else the one of , ; tab | that the first line holds
// most of outside quotes (a CSV from a European spreadsheet uses ;).
func csvSeparator(f *File) rune {
	if tsvFile(f.Path) {
		return '\t'
	}

	if len(f.Lines) == 0 {
		return ','
	}

	count := map[rune]int{}
	quoted := false

	for _, c := range f.Lines[0] {
		switch {
		case c == '"':
			quoted = !quoted
		case !quoted:
			count[c]++
		}
	}

	best := ','
	for _, c := range []rune{';', '\t', '|'} {
		if count[c] > count[best] {
			best = c
		}
	}

	return best
}

func tsvFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".tsv" || ext == ".tab"
}

// colKind is what a column holds: how many values of each kind, how many empty.
type colKind struct {
	empty, ints, nums, bools, dates, texts, objs, arrs int
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}([ T]\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:?\d{2})?)?$|^\d{1,2}[./]\d{1,2}[./]\d{2,4}( \d{1,2}:\d{2}(:\d{2})?)?$`)

func (c *colKind) see(v string) {
	v = strings.TrimSpace(v)

	switch {
	case v == "" || strings.EqualFold(v, "null") || v == `\N`:
		c.empty++
	case isInt(v):
		c.ints++
	case isNum(v):
		c.nums++
	case strings.EqualFold(v, "true") || strings.EqualFold(v, "false"):
		c.bools++
	case dateRe.MatchString(v):
		c.dates++
	default:
		c.texts++
	}
}

func (c *colKind) seeJSON(v json.RawMessage) {
	t := bytes.TrimSpace(v)
	if len(t) == 0 {
		c.empty++
		return
	}

	switch t[0] {
	case 'n':
		c.empty++
	case 't', 'f':
		c.bools++
	case '{':
		c.objs++
	case '[':
		c.arrs++
	case '"':
		var s string
		_ = json.Unmarshal(t, &s)

		if s == "" {
			c.empty++
		} else if dateRe.MatchString(s) {
			c.dates++
		} else {
			c.texts++
		}
	default:
		if isInt(string(t)) {
			c.ints++
		} else {
			c.nums++
		}
	}
}

// String names the kinds a column holds, most first ("int", "text 80%, int 20%"), and its empty and missing values.
func (c *colKind) String(rows int) string {
	all := []struct {
		name string
		n    int
	}{{"int", c.ints}, {"number", c.nums}, {"bool", c.bools}, {"date", c.dates}, {"text", c.texts},
		{"object", c.objs}, {"array", c.arrs}}

	full := 0
	for _, k := range all {
		full += k.n
	}

	var parts []string

	if full > 0 {
		// an int column with some decimals is a number column
		if c.nums > 0 && c.ints > 0 {
			all[1].n += all[0].n
			all[0].n = 0
		}

		slices.SortStableFunc(all, func(a, b struct {
			name string
			n    int
		}) int {
			return b.n - a.n
		})

		for _, k := range all {
			if k.n == 0 {
				continue
			}

			if k.n == full {
				parts = append(parts, k.name)
				break
			}

			parts = append(parts, fmt.Sprintf("%s %d%%", k.name, max(1, k.n*100/full)))
		}
	}

	if c.empty > 0 {
		parts = append(parts, thousands(c.empty)+" empty")
	}

	if miss := rows - full - c.empty; miss > 0 {
		parts = append(parts, thousands(miss)+" missing")
	}

	if len(parts) == 0 {
		return "empty"
	}

	return strings.Join(parts, ", ")
}

func isInt(s string) bool {
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}

func isNum(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil && !strings.ContainsAny(s, "xXnN") // not hex, inf or NaN
}
