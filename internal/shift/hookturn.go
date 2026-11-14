package shift

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/iamnikolie/zmina/internal/crew"
)

// A role may end its turn on a question to the owner written in prose, with nothing on his todo and no dialog on its screen:
// it is idle, and nobody is told that he is awaited. The role's Stop hook finds such a question in the last reply and records it
// (hooks/<role>.turn.json, one file a role, replaced at the end of each turn: a turn that asks nothing leaves a record with no text,
// which says when it ended). Like every hook record it is a hint: it names its session and the time it was written, and a view
// stands by it only while the role is idle since that very turn (TurnRecord.Stands).

const (
	// turnTextMax is the most characters of a question a record keeps: the question's paragraph and what follows it, cut at its end.
	turnTextMax = 4000
	// turnReplyMax is how much of the end of a reply is read for a question: a longer one is not read from its start.
	turnReplyMax = 256 << 10
	// turnParagraphs is how many of the last paragraphs of prose of a reply a question is looked for in: a `?` further up is of a
	// question the reply has since moved on from, or of the report it ends with.
	turnParagraphs = 3
	// turnSkew is how far apart in time the record and the moment the role's session went idle may be. The hook and the session file
	// are written within moments of each other, in either order, and a slow Stop hook of the role's own may hold either back. A
	// later turn is further than that from this one, and has its own hook write a record or remove this one.
	turnSkew = 10 * time.Second
)

// trailMarks are what may stand between a `?` and the end of its sentence: the closing of an emphasis, a quote or a bracket,
// another mark of the same sentence, and the full stop, colon or ellipsis that ends it after them.
const trailMarks = "*_~\"'»”’›」』）】)]}!?？؟.:…"

// isQuestionMark reports a character that asks: the question mark, its fullwidth form and the Arabic one.
func isQuestionMark(r rune) bool { return r == '?' || r == '？' || r == '؟' }

// zeroWidth reports an invisible character that ends a sentence as white space does (an emphasis boundary leaves one).
func zeroWidth(r rune) bool {
	switch r {
	case 0x200b, 0x200c, 0x200d, 0x2060, 0xfeff: // zero width space, non-joiner, joiner, word joiner, byte order mark
		return true
	}

	return false
}

// htmlComment is an HTML comment inside a paragraph: not words to the owner.
var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// TurnRecord is what a role's Stop hook wrote at the end of a turn: the session the turn was of, when the hook ran, the question
// the last reply held — its paragraph and what follows it; empty when the reply asked none, which is a record of a turn and no question
// — and a fingerprint of the three.
type TurnRecord struct {
	SessionID string    `json:"session"`
	At        time.Time `json:"at"`
	// Prev is when the turn before this one ended, from the record this one replaced; zero when it is not known. What the role put on
	// the owner's todo after it was put there in this turn, the one that ended on the question.
	Prev time.Time `json:"prev,omitzero"`
	Text string    `json:"text"`
	ID   string    `json:"id"`
}

// TurnQuestion is the question a role's reply holds, with what the owner needs to read it: the reply from the start of the first
// paragraph that holds one, of the last turnParagraphs paragraphs of prose, through its end, cut at turnTextMax characters. False
// when the reply holds none.
//
// A question is a `?` (or `？`, `؟`) that ends a sentence: it is followed by the end of the text, by white space or a zero-width
// space, or by closing marks, a full stop, a colon or an ellipsis and then one of those, so that a `?` inside a URL or a query
// string is none. Not looked in, and no prose: fenced code blocks, indented code (a paragraph that opens with four spaces or a tab),
// inline code spans, quoted lines (`>`), headings (`#`), table rows (`|`) and HTML comments.
func TurnQuestion(reply string) (string, bool) {
	reply = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(reply)

	if len(reply) > turnReplyMax {
		reply = reply[len(reply)-turnReplyMax:]

		// Not from the middle of a line, nor of a character.
		if i := strings.IndexByte(reply, '\n'); i >= 0 {
			reply = reply[i+1:]
		}

		// Not from the middle of a fenced block: the rest of one is code, whatever it looks like.
		reply = withoutFenceRemainder(reply)
	}

	lines := strings.Split(reply, "\n")
	prose := proseLines(lines)

	// A paragraph runs to the next blank line that is not inside code; the lines of a fence, a quote, a heading or a table in it are
	// its own, and are not looked in. Only the last of those that hold prose are; the question is kept from the first line of prose of
	// the first of them that asks. The items of a list are one paragraph, whether or not blank lines stand between them: a loose list
	// of five items is not five paragraphs that push the question above it out of the last three.
	type paragraph struct{ from, to, first int }

	var paras []paragraph

	blank := func(i int) bool { return prose[i] && strings.TrimSpace(lines[i]) == "" }
	afterList := false

	for from, i := 0, 0; i <= len(lines); i++ {
		if i < len(lines) && !blank(i) {
			continue
		}

		first := -1

		for j := from; j < i && first < 0; j++ {
			if prose[j] {
				first = j
			}
		}

		switch {
		case first < 0:
			// Blank lines, or a paragraph of code, a quote, a heading or a table: no prose, and a list does not go on through it.
			if i > from {
				afterList = false
			}
		case afterList && listItem(lines[first]):
			paras[len(paras)-1].to = i
		default:
			paras = append(paras, paragraph{from, i, first})
			afterList = listItem(lines[first])
		}

		from = i + 1
	}

	for _, p := range paras[max(len(paras)-turnParagraphs, 0):] {
		var text []string

		for i := p.from; i < p.to; i++ {
			if prose[i] {
				text = append(text, lines[i])
			}
		}

		if endsSentence(dropCodeSpans(htmlComment.ReplaceAllString(strings.Join(text, "\n"), " "))) {
			return clipRunes(strings.TrimSpace(strings.Join(lines[p.first:], "\n")), turnTextMax), true
		}
	}

	return "", false
}

// listItem reports a line that opens an item of a list: a bullet (`-`, `*`, `+`) or a number and a full stop or bracket, then a space.
// Bold text (`**word**`) and a number that begins a sentence (`2026 was`) are no items.
func listItem(line string) bool {
	t := strings.TrimLeft(line, " \t")

	if len(t) >= 2 && strings.ContainsRune("-*+", rune(t[0])) && (t[1] == ' ' || t[1] == '\t') {
		return true
	}

	n := len(t) - len(strings.TrimLeft(t, "0123456789"))

	return n >= 1 && n <= 9 && len(t) > n+1 && (t[n] == '.' || t[n] == ')') && (t[n+1] == ' ' || t[n+1] == '\t')
}

// withoutFenceRemainder takes the start of a text that is cut inside a fenced block off: an odd number of fence lines in it says the
// first of them closes a block that was open before the cut, and everything up to and including it is that block's.
func withoutFenceRemainder(text string) string {
	count, end := 0, -1

	for pos := 0; pos < len(text); {
		next := strings.IndexByte(text[pos:], '\n')
		line, stop := text[pos:], len(text)

		if next >= 0 {
			line, stop = text[pos:pos+next], pos+next+1
		}

		if fenceOf(strings.TrimLeft(line, " \t")) != "" {
			count++

			if end < 0 {
				end = stop
			}
		}

		pos = stop
	}

	if count%2 == 1 {
		return text[end:]
	}

	return text
}

// proseLines marks the lines that are prose: not inside a fenced block, not a fence, not indented code, a quote, a heading, a table
// row or an HTML comment. A blank line outside any of them is a paragraph's end, and is marked too.
func proseLines(lines []string) []bool {
	var (
		prose    = make([]bool, len(lines))
		fence    string
		comment  bool
		indented bool
		start    = true
	)

	for i, line := range lines {
		t := strings.TrimLeft(line, " \t")

		switch {
		case fence != "":
			if closesFence(t, fence) {
				fence = ""
			}
		case comment:
			comment = !strings.Contains(line, "-->")
		case t == "":
			prose[i], indented, start = true, false, true

			continue
		case indented || start && (strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")):
			indented = true
		case fenceOf(t) != "":
			fence = fenceOf(t)
		case strings.HasPrefix(t, ">"), isHeading(t), strings.HasPrefix(t, "|"):
		case strings.HasPrefix(t, "<!--"):
			comment = !strings.Contains(t[len("<!--"):], "-->")
		default:
			prose[i] = true
		}

		start = false
	}

	return prose
}

// isHeading reports a line of a heading: one to six `#` and a space, or nothing more (`#123 is broken` is words).
func isHeading(t string) bool {
	n := len(t) - len(strings.TrimLeft(t, "#"))

	return n >= 1 && n <= 6 && (n == len(t) || t[n] == ' ' || t[n] == '\t')
}

// fenceOf is the fence a line opens: its run of three or more backticks or tildes; "" when it opens none. A backtick fence's
// info string holds no backtick (else the line is an inline span).
func fenceOf(t string) string {
	if t == "" || t[0] != '`' && t[0] != '~' {
		return ""
	}

	n := len(t) - len(strings.TrimLeft(t, t[:1]))
	if n < 3 {
		return ""
	}

	if t[0] == '`' && strings.Contains(t[n:], "`") {
		return ""
	}

	return t[:n]
}

// closesFence reports a line that closes the fence: a run of the same mark at least as long, and nothing else.
func closesFence(t, fence string) bool {
	rest := strings.TrimLeft(t, fence[:1])

	return len(t)-len(rest) >= len(fence) && strings.TrimSpace(rest) == ""
}

// dropCodeSpans takes the inline code spans out of a paragraph: a run of backticks to the next run of as many. A run with no
// partner stands for itself. A span is a space, so that `?` after one still ends a sentence.
func dropCodeSpans(s string) string {
	var b strings.Builder

	for i := 0; i < len(s); {
		if s[i] != '`' {
			b.WriteByte(s[i])

			i++

			continue
		}

		n := backticks(s[i:])
		end := -1

		for j := i + n; j < len(s); {
			k := strings.IndexByte(s[j:], '`')
			if k < 0 {
				break
			}

			j += k

			m := backticks(s[j:])
			if m == n {
				end = j + m

				break
			}

			j += m
		}

		if end < 0 {
			b.WriteString(s[i : i+n])

			i += n

			continue
		}

		b.WriteByte(' ')

		i = end
	}

	return b.String()
}

// backticks is the length of the run of backticks a string starts with.
func backticks(s string) int { return len(s) - len(strings.TrimLeft(s, "`")) }

// endsSentence reports a question mark that ends a sentence. It goes once over the text: the marks after a question mark that does
// not end one are skipped whole, since every question mark among them has the same end.
func endsSentence(s string) bool {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n

		if !isQuestionMark(r) {
			continue
		}

		for i < len(s) {
			c, w := utf8.DecodeRuneInString(s[i:])
			if !strings.ContainsRune(trailMarks, c) {
				break
			}

			i += w
		}

		if i == len(s) {
			return true
		}

		if c, _ := utf8.DecodeRuneInString(s[i:]); unicode.IsSpace(c) || zeroWidth(c) {
			return true
		}
	}

	return false
}

// turnID is the fingerprint of a turn question: the session, the moment and the words, 16 hex characters as a dialog's.
func turnID(session string, at time.Time, text string) string {
	sum := sha256.Sum256([]byte(session + "\x00" + at.UTC().Format(time.RFC3339Nano) + "\x00" + text))

	return hex.EncodeToString(sum[:])[:16]
}

// RecordTurn reads what a Stop hook gave (the session and the turn's last reply) and writes the role's record of the turn, replacing the
// one before: the question the reply holds, or none (an empty text: the turn asked nothing, and is no thing for anyone to be told of).
// The record keeps when the turn before it ended (Prev), which is the record it replaces, when that is of the same session: so the
// next look knows which turn an ask of the role was made in. A role that is no file name, or no session, records nothing. An error
// when the record could not be written.
func RecordTurn(l crew.Layout, role, session, reply string, now time.Time) error {
	if !hookRole(role) || session == "" {
		return nil
	}

	text, _ := TurnQuestion(reply)
	now = now.UTC()
	rec := TurnRecord{SessionID: session, At: now, Text: text, ID: turnID(session, now, text)}

	// What cannot be read is no turn before this one: the first, or the one a start, a compaction, a clear or an error dropped.
	if old, err := ReadTurn(l, role); err == nil && old != nil && old.SessionID == session && old.At.Before(now) {
		rec.Prev = old.At
	}

	if err := writeRecord(l, l.HookTurn(role), rec); err != nil {
		// The record of the turn before would stand for this one: none is better than a stale one.
		_ = DropTurn(l, role)

		return err
	}

	return nil
}

// ClearTurn ends the question a record holds without losing when its turn ended: the record is written again with no text. A role with
// no record, or none that reads, has nothing to clear (the caller drops what does not read, DropTurn).
func ClearTurn(l crew.Layout, role string) error {
	rec, err := ReadTurn(l, role)
	if err != nil || rec == nil {
		return err
	}

	rec.Text, rec.ID = "", turnID(rec.SessionID, rec.At, "")

	return writeRecord(l, l.HookTurn(role), *rec)
}

// DropTurn removes a role's turn record: a turn that ended on an error, the start of a new conversation (a start, a compaction, a
// clear) leave none. No record is no error.
func DropTurn(l crew.Layout, role string) error {
	if !hookRole(role) {
		return nil
	}

	if err := os.Remove(l.HookTurn(role)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
}

// ReadTurn is the record a role's hook kept of its last turn (whose Text is empty when the turn asked nothing): nil, with no error,
// when there is none; an error when the file is there and does not read, or its id is not the fingerprint of its session, moment and
// words (a record that was written by no hook).
func ReadTurn(l crew.Layout, role string) (*TurnRecord, error) {
	if !hookRole(role) {
		return nil, nil
	}

	f, err := os.Open(l.HookTurn(role))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	raw, err := io.ReadAll(io.LimitReader(f, hookRecordMax))
	if err != nil {
		return nil, err
	}

	var rec TurnRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}

	if rec.SessionID == "" || rec.At.UnixMilli() <= 0 {
		return nil, errors.New("a turn record with no session or time")
	}

	if !validTurnID(rec.ID) || rec.ID != turnID(rec.SessionID, rec.At, rec.Text) {
		return nil, errors.New("a turn record whose id is not the fingerprint of its session, time and words")
	}

	return &rec, nil
}

// validTurnID reports 16 lower-case hex characters.
func validTurnID(id string) bool {
	if len(id) != 16 {
		return false
	}

	for _, c := range id {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}

	return true
}

// Stands reports whether a role's view is of the turn the record is of, still: the role is up and idle at its input line, in the
// session the record is of, and it went idle within turnSkew of the moment the hook ran. A role that took another turn since is
// busy, or idle since a later moment, or in another session (a restart, a /clear), and the record is no longer its own.
func (r *TurnRecord) Stands(v View) bool {
	if r == nil || v.State != PresenceUp || v.Status != "idle" || v.Prompt || v.Dialog != nil || r.SessionID == "" ||
		v.SessionID != r.SessionID || v.Since.UnixMilli() <= 0 {
		return false
	}

	d := r.At.Sub(v.Since)

	return d >= -turnSkew && d <= turnSkew
}
