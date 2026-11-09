package lean

import (
	"regexp"
	"strconv"
	"strings"
)

// sqlOutline lists a SQL file by statement: its kind and object ("CREATE TABLE users", "INSERT INTO users"), a run
// of statements of one label as one entry ("INSERT INTO users ×120"), each with the comment lines right above it.
// A migration's sections (goose and sql-migrate "-- +goose Up", dbmate "-- migrate:up") are the top level with their
// statements under them. Statements end at ";" (or a DELIMITER's) outside strings, quoted names, comments and $$
// bodies; StatementBegin … StatementEnd is one statement whatever it holds; pg_dump's COPY … FROM stdin takes its
// data lines up to "\.". A statement's Name is its object without its schema, so file.sql:users reads every
// statement on users.
func sqlOutline(src []byte) []Symbol {
	stmts, sections, _ := sqlScan(src)
	if stmts == nil {
		return nil
	}

	if len(stmts) == 0 {
		return nil
	}

	lines := splitLines(src)

	var out []Symbol

	sec := -1 // index in sections of the section open
	floor := 1

	for i := 0; i < len(stmts); i++ {
		s := stmts[i]

		for sec+1 < len(sections) && sections[sec+1].Start <= s.start {
			sec++
			out = append(out, sections[sec])
			floor = sections[sec].Start + 1
		}

		label, name := s.label()

		// a run: the next statements of the same label, with nothing but blanks and comments between
		end, n := s.end, 1
		for !s.long() && i+1 < len(stmts) && (sec+1 >= len(sections) || sections[sec+1].Start > stmts[i+1].start) {
			if l, _ := stmts[i+1].label(); l != label || stmts[i+1].long() {
				break
			}

			i++
			end, n = stmts[i].end, n+1
		}

		if n > 1 {
			label += " ×" + strconv.Itoa(n)
		}

		depth := 0
		if sec >= 0 {
			depth = 1
		}

		start := sqlHead(lines, s.start, floor)
		out = append(out, Symbol{Start: start, End: end, Depth: depth, Name: name, Label: clip(label, 110)})
		floor = end + 1

		// a long query shows its CTEs and the query after them
		if s.long() {
			low := s.start + 1
			for _, p := range s.parts {
				p.Start, p.Depth = sqlHead(lines, p.Start, low), depth+1
				out = append(out, p)
				low = p.End + 1
			}
		}
	}

	for sec+1 < len(sections) { // sections after the last statement (an empty Down)
		sec++
		out = append(out, sections[sec])
	}

	for k := range out { // a string or body open to the end of the file ends there
		out[k].End = max(min(out[k].End, len(lines)), out[k].Start)
	}

	return out
}

// sqlLongStatement is the length from which a statement's outline shows its CTEs.
const sqlLongStatement = 30

// sqlScan reads the file's statements. A backslash escapes a quote in MySQL ('it\'s'), not in a Postgres standard
// string ('C:\dir\'), so the file is scanned both ways. sure: both agree on every statement — the backslash makes no
// difference, and no string's text can have been read as code — or the file is MySQL's (its signs) and the scan with
// escapes, its own rule, closes. Else the scan that closes its strings is taken, the one without escapes when both
// do; open both ways, nil: cut short or no SQL.
func sqlScan(src []byte) (stmts []sqlStmt, sections []Symbol, sure bool) {
	esc, escSec, escOpen := sqlStatements(src, true)
	raw, rawSec, rawOpen := sqlStatements(src, false)

	switch {
	case escOpen == rawOpen && sameStatements(esc, raw):
		if escOpen {
			return nil, nil, false
		}

		return esc, escSec, true
	case escOpen && rawOpen:
		return nil, nil, false
	case mysqlSigns.Match(src) && !escOpen: // MySQL escapes its quotes with a backslash: that scan is right
		return esc, escSec, true
	case rawOpen:
		return esc, escSec, false
	case escOpen:
		return raw, rawSec, false
	default:
		return raw, rawSec, false
	}
}

// mysqlSigns are what a MySQL file has and a Postgres one has not: backticks, /*! … */, a table's ENGINE=, DELIMITER.
var mysqlSigns = regexp.MustCompile("(?im)`|/\\*!|\\bengine\\s*=|^\\s*delimiter\\s")

func sameStatements(a, b []sqlStmt) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i].start != b[i].start || a[i].end != b[i].end || a[i].code != b[i].code {
			return false
		}
	}

	return true
}

// sqlSure reports a SQL file whose outline cannot hold a string's text as code: its backslashes (if any) make no
// difference to its statements. The read guard answers only with such an outline.
func sqlSure(src []byte) bool {
	_, _, sure := sqlScan(src)
	return sure
}

type sqlStmt struct {
	start, end int
	code       string   // the start of its code: comments out, strings as '', whitespace one space
	parts      []Symbol // its CTEs and the query after them (Start is the name's line)
}

// long: the statement is long enough for its outline to show its parts, and has some.
func (s sqlStmt) long() bool { return s.end-s.start+1 >= sqlLongStatement && len(s.parts) > 0 }

// label is the statement's label and name: a WITH says how many CTEs it holds, a name is without its schema
// (public.users is found as users).
func (s sqlStmt) label() (label, name string) {
	label, name = sqlLabel(s.code)
	if label == "WITH" && len(s.parts) > 0 {
		label = sqlWithLabel(s.parts)
	}

	if k := strings.LastIndexByte(name, '.'); k >= 0 {
		name = name[k+1:]
	}

	return label, name
}

// sqlDirective is a migration tool's line: goose and sql-migrate (-- +goose Up, -- +migrate StatementBegin), dbmate
// (-- migrate:up).
var sqlDirective = regexp.MustCompile(`(?i)^--\s*(?:\+(?:goose|migrate)\s+(up|down|statementbegin|statementend)\b|migrate:(up|down)\b)`)

var (
	sqlDelimiter = regexp.MustCompile(`(?i)^delimiter\s+(\S+)`)
	// sqlGo is T-SQL's batch line: what comes before it is run, so it ends a statement written without ";".
	sqlGo = regexp.MustCompile(`(?i)^go(?:\s+\d+)?;?$`)
)

// sqlStatements scans the file into its statements and its migration sections (Name Up or Down, End set).
func sqlStatements(src []byte, escapes bool) ([]sqlStmt, []Symbol, bool) {
	var (
		stmts    []sqlStmt
		sections []Symbol
		cur      sqlStmt
		code     strings.Builder
		have     bool // the statement has code
		lastCode int  // the line of its last code
		line     = 1
		delim    = ";"
		block    bool // between StatementBegin and StatementEnd
		copyData bool // in the data lines of COPY … FROM stdin
		exec     bool // inside a MySQL /*! … */ that runs
		open     bool // a string or $$ body ran to the end of the file
		lineHead = true
		parts    = sqlParts{open: -1}
	)

	add := func(s string) {
		if code.Len() < 400 {
			code.WriteString(s)
		}
	}

	emit := func(end int) {
		if have {
			cur.end, cur.code, cur.parts = end, strings.Join(strings.Fields(code.String()), " "), parts.done(end)
			stmts = append(stmts, cur)

			if sqlCopyStdin(cur.code) {
				copyData = true
			}
		}

		cur, have, parts = sqlStmt{}, false, sqlParts{open: -1}
		code.Reset()
	}

	closeSection := func(n int) {
		if k := len(sections) - 1; k >= 0 && sections[k].End == 0 {
			sections[k].End = max(n, sections[k].Start)
		}
	}

	for i := 0; i < len(src); {
		if lineHead {
			lineHead = false
			eol := i
			for eol < len(src) && src[eol] != '\n' {
				eol++
			}

			l := strings.TrimSpace(strings.TrimSuffix(string(src[i:eol]), "\r"))

			if copyData {
				if l == `\.` {
					stmts[len(stmts)-1].end = line
					copyData = false
				}

				i = eol

				continue
			}

			if m := sqlDirective.FindStringSubmatch(l); m != nil && !exec {
				switch w := strings.ToLower(m[1] + m[2]); w {
				case "up", "down":
					emit(lastCode)
					closeSection(line - 1)
					// the directive alone: the rest of a comment line is the file's text
					sections = append(sections, Symbol{Start: line, Name: strings.ToUpper(w[:1]) + w[1:], Label: strings.TrimSpace(m[0])})
				case "statementbegin":
					emit(lastCode)
					block = true
				case "statementend":
					emit(lastCode)
					block = false
				}

				i = eol

				continue
			}

			if m := sqlDelimiter.FindStringSubmatch(l); m != nil && !have {
				delim = m[1]
				i = eol

				continue
			}

			if sqlGo.MatchString(l) && !block && !exec {
				emit(lastCode)
				i = eol

				continue
			}
		}

		c := src[i]

		switch {
		case c == '\n':
			line++
			lineHead = true
			add(" ")
			i++
		case exec && c == '*' && i+1 < len(src) && src[i+1] == '/':
			exec = false
			i += 2
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			if i+2 < len(src) && src[i+2] == '!' { // MySQL's /*!40101 SET … */ runs: its inside is code
				i += 3
				for i < len(src) && src[i] >= '0' && src[i] <= '9' {
					i++
				}

				exec = true

				continue
			}

			i += 2
			for i < len(src) && (src[i] != '*' || i+1 >= len(src) || src[i+1] != '/') {
				if src[i] == '\n' {
					line++
				}

				i++
			}

			i += 2
		case c == '\'' || c == '"' || c == '`':
			if !have {
				cur.start, have = line, true
			}

			at := line

			j, closed := sqlQuoted(src, i, &line, escapes)
			open = open || !closed
			if c == '\'' {
				add("''")
			} else {
				add(string(src[i:j]))
				parts.word(string(src[i+1:max(i+1, j-1)]), at)
			}

			lastCode = line
			i = j
		// before a $$ body: a DELIMITER $$ ends a statement with "END $$" or "$$" on its own line
		case !block && strings.HasPrefix(string(src[i:min(len(src), i+len(delim))]), delim):
			emit(line)
			i += len(delim)
		case c == '$' && (i == 0 || !identByte(src[i-1])):
			tag := sqlDollarTag(src[i:])
			if tag == "" {
				add("$")
				i++

				continue
			}

			if !have {
				cur.start, have = line, true
			}

			i += len(tag)
			for i < len(src) && !strings.HasPrefix(string(src[i:min(len(src), i+len(tag))]), tag) {
				if src[i] == '\n' {
					line++
				}

				i++
			}

			open = open || i >= len(src)

			i += len(tag)
			add("$$")
			lastCode = line
		case c == ' ' || c == '\t' || c == '\r':
			add(" ")
			i++
		default:
			if !have {
				cur.start, have = line, true
			}

			lastCode = line

			if identByte(c) {
				j := i + 1
				for j < len(src) && identByte(src[j]) {
					j++
				}

				add(string(src[i:j]))
				parts.word(string(src[i:j]), line)
				i = j

				continue
			}

			switch c {
			case '(':
				parts.openParen(line)
			case ')':
				parts.closeParen(line)
			case ',':
				parts.comma(line)
			}

			add(string(c))
			i++
		}
	}

	emit(lastCode)
	closeSection(line - boolInt(len(src) > 0 && src[len(src)-1] == '\n'))

	return stmts, sections, open
}

func boolInt(b bool) int {
	if b {
		return 1
	}

	return 0
}

// sqlQuoted is the index after the string or quoted name that starts at i, and whether it closed: a quote doubled
// stays inside, and with escapes (MySQL) one after a backslash in a '…' or "…" string.
func sqlQuoted(src []byte, i int, line *int, escapes bool) (int, bool) {
	q := src[i]

	j := i + 1
	for j < len(src) {
		switch src[j] {
		case '\n':
			*line++
		case '\\':
			if escapes && q != '`' {
				j++
				if j < len(src) && src[j] == '\n' {
					*line++
				}
			}
		case q:
			if j+1 < len(src) && src[j+1] == q {
				j++
			} else {
				return j + 1, true
			}
		}

		j++
	}

	return j, false
}

var dollarTagRe = regexp.MustCompile(`^\$(?:[A-Za-z_][A-Za-z_0-9]*)?\$`)

func sqlDollarTag(b []byte) string {
	return string(dollarTagRe.Find(b[:min(len(b), 64)]))
}

func identByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= 0x80
}

var copyStdinRe = regexp.MustCompile(`(?i)^copy\s.*\bfrom\s+stdin\b`)

func sqlCopyStdin(code string) bool { return copyStdinRe.MatchString(code) }

// sqlHead is where a statement at line n begins: the "--" comment lines right above it, not above floor.
func sqlHead(lines []string, n, floor int) int {
	for n > floor && n-2 < len(lines) {
		l := strings.TrimSpace(lines[n-2])
		if !strings.HasPrefix(l, "--") || sqlDirective.MatchString(l) {
			break
		}

		n--
	}

	return n
}

const sqlIdent = "((?:[\\w$]+|\"[^\"]*\"|`[^`]*`|\\[[^\\]]*\\])(?:\\s*\\.\\s*(?:[\\w$]+|\"[^\"]*\"|`[^`]*`|\\[[^\\]]*\\]))*)"

var (
	sqlObjectRe = regexp.MustCompile(`(?i)^(create|alter|drop)\s+` +
		`(?:or\s+replace\s+|definer\s*=\s*\S+\s+|sql\s+security\s+\w+\s+|algorithm\s*=\s*\w+\s+|` +
		`(?:unique|temp|temporary|materialized|global|local|unlogged|recursive|constraint|online|offline)\s+)*` +
		`(table|view|index|function|procedure|trigger|sequence|type|schema|database|extension|domain|event|role|user|` +
		`policy|aggregate|rule|publication|subscription|collation|operator|server)\s+` +
		`(?:concurrently\s+)?(?:if\s+(?:not\s+)?exists\s+)?(?:only\s+)?` + sqlIdent + `(?:.*?\s+on\s+(?:only\s+)?` + sqlIdent + `)?`)
	sqlInsertRe = regexp.MustCompile(`(?i)^(insert|replace|merge)\s+(?:(?:low_priority|delayed|high_priority|ignore)\s+)*` +
		`(?:into\s+)?` + sqlIdent)
	sqlUpdateRe = regexp.MustCompile(`(?i)^update\s+(?:(?:low_priority|ignore|only)\s+)*` + sqlIdent)
	sqlDeleteRe = regexp.MustCompile(`(?i)^delete\s+(?:(?:low_priority|quick|ignore)\s+)*from\s+(?:only\s+)?` + sqlIdent)
	sqlTableRe  = regexp.MustCompile(`(?i)^(copy|truncate(?:\s+table)?|lock\s+tables?|use|comment\s+on\s+\w+|` +
		`analyze|vacuum(?:\s+full)?|optimize\s+table|reindex\s+\w+|call|exec(?:ute)?)\s+(?:only\s+)?` + sqlIdent)
	sqlFromRe  = regexp.MustCompile(`(?i)\bfrom\s+(?:only\s+)?` + sqlIdent)
	sqlGrantRe = regexp.MustCompile(`(?i)^(grant|revoke)\s.*?\son\s+(?:(?:table|schema|sequence|function|database|all\s+\w+\s+in\s+schema)\s+)?` + sqlIdent)
	sqlQuoteRe = regexp.MustCompile("[\"`\\[\\]\\s]")
)

// sqlLabel is how the outline shows a statement and the name a target finds it by: "CREATE TABLE users" and users,
// "CREATE INDEX users_email ON users" and users_email, "INSERT INTO users" and users; a statement without an object
// ("SET", "BEGIN") is its first word, its name that word.
func sqlLabel(code string) (label, name string) {
	ident := func(s string) string { return sqlQuoteRe.ReplaceAllString(s, "") }
	upper := func(s string) string { return strings.ToUpper(strings.Join(strings.Fields(s), " ")) }

	if m := sqlObjectRe.FindStringSubmatch(code); m != nil {
		name = ident(m[3])
		label = upper(m[1]+" "+m[2]) + " " + name

		if m[4] != "" && sqlOnKind(m[2]) {
			label += " ON " + ident(m[4])
		}

		return label, name
	}

	for _, re := range []*regexp.Regexp{sqlInsertRe, sqlUpdateRe, sqlDeleteRe} {
		if m := re.FindStringSubmatch(code); m != nil {
			name = ident(m[len(m)-1])
			kind := strings.ToUpper(firstWord(code))

			switch kind {
			case "INSERT", "REPLACE", "MERGE":
				kind += " INTO"
			case "DELETE":
				kind += " FROM"
			}

			return kind + " " + name, name
		}
	}

	if m := sqlTableRe.FindStringSubmatch(code); m != nil {
		name = ident(m[2])
		return upper(m[1]) + " " + name, name
	}

	if m := sqlGrantRe.FindStringSubmatch(code); m != nil {
		name = ident(m[2])
		return upper(m[1]) + " ON " + name, name
	}

	kind := strings.ToUpper(strings.TrimRight(firstWord(code), "(;,"))
	if kind == "SELECT" {
		if m := sqlFromRe.FindStringSubmatch(code); m != nil {
			name = ident(m[1])
			return kind + " … FROM " + name, name
		}
	}

	if !sqlKeywords[kind] { // never a word of the file's data, even when a bad scan takes a string for code
		kind = "statement"
	}

	return kind, kind
}

// sqlKeywords start the statements without an object that the outline names by their first word.
var sqlKeywords = map[string]bool{"SET": true, "BEGIN": true, "COMMIT": true, "ROLLBACK": true, "START": true,
	"END": true, "RETURN": true, "DECLARE": true, "IF": true, "ELSE": true, "WHILE": true, "LOOP": true, "SAVEPOINT": true,
	"RELEASE": true, "UNLOCK": true, "SHOW": true, "DESCRIBE": true, "DESC": true, "EXPLAIN": true, "PRAGMA": true,
	"FLUSH": true, "PREPARE": true, "DEALLOCATE": true, "LISTEN": true, "NOTIFY": true, "DO": true, "VALUES": true,
	"CHECKPOINT": true, "DISCARD": true, "RESET": true, "LOAD": true, "SOURCE": true, "PRINT": true, "OPEN": true,
	"CLOSE": true, "FETCH": true, "SELECT": true, "WITH": true, "CREATE": true, "ALTER": true, "DROP": true,
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "GRANT": true, "REVOKE": true, "COPY": true,
	"TRUNCATE": true, "LOCK": true, "USE": true, "COMMENT": true, "ANALYZE": true, "VACUUM": true, "CALL": true,
	"EXEC": true, "EXECUTE": true, "RAISERROR": true, "THROW": true, "TRY": true, "CATCH": true, "GO": true}

// sqlWithLabel says what a WITH statement holds: "WITH: 52 CTEs, then INSERT".
func sqlWithLabel(parts []Symbol) string {
	ctes, then := 0, ""

	for _, p := range parts {
		if p.Name == "main" {
			then = ", then " + firstWord(p.Label)
		} else {
			ctes++
		}
	}

	plural := "s"
	if ctes == 1 {
		plural = ""
	}

	return "WITH: " + strconv.Itoa(ctes) + " CTE" + plural + then
}

// sqlOnKind: an object of this kind is defined on a table, which the label names too.
func sqlOnKind(kind string) bool {
	switch strings.ToLower(kind) {
	case "index", "trigger", "policy", "rule":
		return true
	}

	return false
}

// sqlParts follows a statement's top level (outside parentheses) for its CTEs ("name AS (", "name (a, b) AS (")
// and the query that follows them.
type sqlParts struct {
	depth int
	toks  []sqlTok // the last words and signs at the top level
	seen  int      // how many were pushed
	parts []Symbol
	open  int  // index in parts of the CTE whose ")" is not seen yet, -1: none
	cte   bool // a CTE closed last: a top-level SELECT next is the main query
}

type sqlTok struct {
	s    string
	line int
}

var sqlDML = map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true}

func (p *sqlParts) push(s string, line int) {
	p.toks = append(p.toks, sqlTok{s, line})
	if len(p.toks) > 6 {
		p.toks = p.toks[1:]
	}

	p.seen++
}

func (p *sqlParts) word(s string, line int) {
	if p.depth != 0 {
		return
	}

	p.push(s, line)

	if p.cte && p.open < 0 && sqlDML[strings.ToUpper(s)] {
		p.parts = append(p.parts, Symbol{Start: line, Name: "main", Label: strings.ToUpper(s) + " … (the main query)"})
		p.cte = false
	}
}

func (p *sqlParts) comma(line int) {
	if p.depth == 0 {
		p.push(",", line)
	}
}

func (p *sqlParts) openParen(line int) {
	if p.depth == 0 {
		if name, at, ok := p.cteHead(); ok {
			p.endMain(at - 1)
			p.parts = append(p.parts, Symbol{Start: at, Name: name, Label: clip(name, 80) + " AS (…)"})
			p.open, p.cte = len(p.parts)-1, true
		}

		p.push("(", line)
	}

	p.depth++
}

func (p *sqlParts) closeParen(line int) {
	if p.depth > 0 {
		p.depth--
	}

	if p.depth == 0 {
		if p.open >= 0 {
			p.parts[p.open].End, p.open = line, -1
		}

		p.push(")", line)
	}
}

// cteHead: the top-level words before this "(" are a CTE's head; its name and the name's line.
func (p *sqlParts) cteHead() (string, int, bool) {
	t := p.toks
	k := len(t) - 1

	if k >= 0 && strings.EqualFold(t[k].s, "materialized") {
		k--
		if k >= 0 && strings.EqualFold(t[k].s, "not") {
			k--
		}
	}

	if k < 1 || !strings.EqualFold(t[k].s, "as") {
		return "", 0, false
	}

	k--
	if t[k].s == ")" { // a column list
		if k < 2 || t[k-1].s != "(" {
			return "", 0, false
		}

		k -= 2
	}

	name := t[k].s
	if name == "" || name == "(" || name == ")" || name == "," {
		return "", 0, false
	}

	first := p.seen-(len(t)-k) == 0
	if !first && (k == 0 || t[k-1].s != "," && !strings.EqualFold(t[k-1].s, "with") && !strings.EqualFold(t[k-1].s, "recursive")) {
		return "", 0, false
	}

	return name, t[k].line, true
}

func (p *sqlParts) endMain(n int) {
	if k := len(p.parts) - 1; k >= 0 && p.parts[k].Name == "main" && p.parts[k].End == 0 {
		p.parts[k].End = max(n, p.parts[k].Start)
	}
}

// done closes what is open at the statement's end and gives the parts.
func (p *sqlParts) done(end int) []Symbol {
	p.endMain(end)

	if p.open >= 0 {
		p.parts[p.open].End = end
	}

	return p.parts
}
