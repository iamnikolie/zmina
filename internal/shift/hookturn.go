package shift

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/iamnikolie/zmina/internal/crew"
)

// A role may end its turn on a question to the owner written in prose, with nothing on his todo and no dialog on its screen:
// it is idle, and nobody is told that he is awaited. The role's Stop hook finds such a question in the last reply and records it
// (hooks/<role>.turn.json, one file a role, replaced by the next turn's record or removed by a turn that asks nothing). Like every
// hook record it is a hint: it names its session and the time it was written, and a view stands by it only while the role is idle
// since that very turn (TurnRecord.Stands).

const (
	// turnTextMax is the most characters of a question a record keeps: the question's paragraph and what follows it, cut at its end.
	turnTextMax = 4000
	// turnReplyMax is how much of the end of a reply is read for a question: a longer one is not read from its start.
	turnReplyMax = 256 << 10
	// turnSkew is how far apart in time the record and the moment the role's session went idle may be. The hook and the session file
	// are written within moments of each other, in either order, and a slow Stop hook of the role's own may hold either back. A
	// later turn is further than that from this one, and has its own hook write a record or remove this one.
	turnSkew = 10 * time.Second
)

// closingMarks are what may stand between a `?` and the end of its sentence: the closing of an emphasis, a quote or a bracket, or
// another mark of the same sentence.
const closingMarks = "*_~\"'»”’›」』）】)]}!?？"

// TurnRecord is what a role's Stop hook wrote when its last reply held a question to the owner: the session the turn was of, when
// the hook ran, the question's paragraph and what follows it, and a fingerprint of the three.
type TurnRecord struct {
	SessionID string    `json:"session"`
	At        time.Time `json:"at"`
	Text      string    `json:"text"`
	ID        string    `json:"id"`
}

// TurnQuestion is the question a role's reply holds, with what the owner needs to read it: the reply from the start of the first
// paragraph that holds a question through its end, cut at turnTextMax characters. False when the reply holds none.
//
// A question is a `?` (or `？`) that ends a sentence: it is followed by the end of the text, by white space, or by closing marks
// and then one of those, so that a `?` inside a URL or a query string is none. Fenced code blocks, inline code spans and quoted
// lines (`>`) are not looked in: what is quoted or run is not asked.
func TurnQuestion(reply string) (string, bool) {
	reply = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(reply)

	if len(reply) > turnReplyMax {
		reply = reply[len(reply)-turnReplyMax:]

		// Not from the middle of a line, nor of a character.
		if i := strings.IndexByte(reply, '\n'); i >= 0 {
			reply = reply[i+1:]
		}
	}

	lines := strings.Split(reply, "\n")
	prose := proseLines(lines)

	// A paragraph runs to the next blank line that is not inside code; the lines of a fence or a quote in it are its own, and are not
	// looked in. The question is kept from its paragraph's first line of prose.
	blank := func(i int) bool { return prose[i] && strings.TrimSpace(lines[i]) == "" }

	for from, i := 0, 0; i <= len(lines); i++ {
		if i < len(lines) && !blank(i) {
			continue
		}

		if at := questionAt(lines[from:i], prose[from:i]); at >= 0 {
			return clipRunes(strings.TrimSpace(strings.Join(lines[from+at:], "\n")), turnTextMax), true
		}

		from = i + 1
	}

	return "", false
}

// questionAt is the index of the first line of prose of a paragraph when its prose holds a question; -1 when it holds none, or
// no prose.
func questionAt(lines []string, prose []bool) int {
	first := -1

	var text []string

	for i, line := range lines {
		if prose[i] {
			text = append(text, line)

			if first < 0 {
				first = i
			}
		}
	}

	if first < 0 || !endsSentence(dropCodeSpans(strings.Join(text, "\n"))) {
		return -1
	}

	return first
}

// proseLines marks the lines that are prose: not inside a fenced code block, not a fence, not a quote.
func proseLines(lines []string) []bool {
	prose := make([]bool, len(lines))

	var fence string

	for i, line := range lines {
		t := strings.TrimLeft(line, " \t")

		switch {
		case fence != "":
			if closesFence(t, fence) {
				fence = ""
			}
		case fenceOf(t) != "":
			fence = fenceOf(t)
		case strings.HasPrefix(t, ">"):
		default:
			prose[i] = true
		}
	}

	return prose
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

// endsSentence reports a `?` that ends a sentence.
func endsSentence(s string) bool {
	for i, r := range s {
		if r != '?' && r != '？' {
			continue
		}

		rest := strings.TrimLeftFunc(s[i+utf8.RuneLen(r):], func(c rune) bool { return strings.ContainsRune(closingMarks, c) })
		if rest == "" {
			return true
		}

		if c, _ := utf8.DecodeRuneInString(rest); unicode.IsSpace(c) {
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

// RecordTurn reads what a Stop hook gave (the session and the turn's last reply) and writes the question the reply holds as the
// role's record, replacing the one before; a reply that holds none removes it. A role that is no file name, or no session, records
// nothing. An error when the record could not be written or removed.
func RecordTurn(l crew.Layout, role, session, reply string, now time.Time) error {
	if !hookRole(role) || session == "" {
		return nil
	}

	text, ok := TurnQuestion(reply)
	if !ok {
		if err := os.Remove(l.HookTurn(role)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}

		return nil
	}

	now = now.UTC()

	return writeRecord(l, l.HookTurn(role), TurnRecord{SessionID: session, At: now, Text: text, ID: turnID(session, now, text)})
}

// ReadTurn is the question a role's hook recorded: nil, with no error, when it recorded none; an error when the file is there
// and does not read.
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

	if rec.SessionID == "" || rec.ID == "" || rec.Text == "" || rec.At.UnixMilli() <= 0 {
		return nil, errors.New("a turn record with no session, id, text or time")
	}

	return &rec, nil
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
