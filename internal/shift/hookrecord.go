package shift

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/registry"
)

// What a role's hooks record is one small file a role and kind in the crew's hooks directory (crew.Layout.Hooks).
// A record is a hint, never the truth: it names the session it is about and when it was written, and a view
// reads it only while the session file of Claude Code agrees it is still current (askedFor, failureFor). Nothing
// deletes a record: a stale one is ignored by those two tests, and the next one replaces it. A role started before
// its hook existed has no record, and everything reads from the screen and the transcript as it always did.

const (
	// hookSkew is how far apart in time a hook's record and the status change of its session may be: Claude Code writes its
	// session file and runs the hook within milliseconds of each other, in either order. A record further off than this,
	// either way, is of another moment.
	hookSkew = time.Second
	// hookRecordMax caps how much of a record is read; a record is a few kilobytes.
	hookRecordMax = 256 << 10
	// askTool is the tool whose PermissionRequest carries the question a role puts to the owner.
	askTool = "AskUserQuestion"
)

// AskedQuestion is what a role's PermissionRequest hook wrote when an AskUserQuestion put a question on its screen:
// the questions and options as the tool was called with them, the session it was asked in and when the hook ran.
type AskedQuestion struct {
	SessionID string      `json:"session_id"`
	At        time.Time   `json:"at"`
	Questions []AskedItem `json:"questions"`
}

// AskedItem is one question of an AskUserQuestion call (the tool's own field names).
type AskedItem struct {
	Question    string        `json:"question"`
	Header      string        `json:"header,omitempty"`
	MultiSelect bool          `json:"multiSelect,omitempty"`
	Options     []AskedOption `json:"options"`
}

// AskedOption is one choice of a question.
type AskedOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// hookRole reports a role name that can be a file name of its own in the hooks directory: a role of the crew or one of
// its copies (dev#2), never a path.
func hookRole(role string) bool {
	return role != "" && len(role) <= 128 && role != "." && role != ".." && filepath.Base(role) == role &&
		!strings.ContainsAny(role, "/\\\x00")
}

// RecordQuestion reads a PermissionRequest hook's input and, when the tool is AskUserQuestion, writes the question
// as the role's record (replacing the one before). Any other tool, input that is not that, or a call with no question
// records nothing and is no error. A crew directory that is not there is an error: no crew to write for, and none is
// made.
func RecordQuestion(l crew.Layout, role string, input []byte, now time.Time) error {
	var in struct {
		SessionID string `json:"session_id"`
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			Questions []AskedItem `json:"questions"`
		} `json:"tool_input"`
	}

	if !hookRole(role) || json.Unmarshal(input, &in) != nil || in.ToolName != askTool || in.SessionID == "" ||
		len(in.ToolInput.Questions) == 0 {
		return nil
	}

	return writeRecord(l, l.HookQuestion(role), AskedQuestion{SessionID: in.SessionID, At: now.UTC(),
		Questions: in.ToolInput.Questions})
}

// writeRecord writes a record whole or not at all, in a directory it makes under an existing crew directory.
func writeRecord(l crew.Layout, path string, record any) error {
	if st, err := os.Stat(l.Dir); err != nil || !st.IsDir() {
		return errors.New("no crew directory to record in")
	}

	data, err := json.Marshal(record)
	if err != nil {
		return err
	}

	if err := fsx.MkdirAll(l.Hooks(), 0o700); err != nil {
		return err
	}

	return fsx.WriteFile(path, append(data, '\n'), 0o600)
}

// dropHookRecords removes the two records a role's hooks left (the question and the failure): a role that starts a new
// session has none of that session's, and what the old one recorded must not read as the new one's. Nothing else in the
// hooks directory is touched.
func (s *Shift) dropHookRecords(name string) {
	if !hookRole(name) {
		return
	}

	l := s.Crew.Layout()
	_ = os.Remove(l.HookQuestion(name))
	_ = os.Remove(l.HookFailure(name))
}

// readRecord reads a record into v; false when there is none or it does not read.
func readRecord(path string, v any) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	raw, err := io.ReadAll(io.LimitReader(f, hookRecordMax))

	return err == nil && json.Unmarshal(raw, v) == nil
}

// near reports whether two moments are within hookSkew of each other.
func near(a, b time.Time) bool {
	d := a.Sub(b)

	return d >= -hookSkew && d <= hookSkew
}

// askedFor is the question a role's hook recorded, while it is the question on the role's screen: the session file says
// the session waits on a question (input needed), the record is of that very session, and it was written within
// hookSkew of the moment that wait began. Anything else — a record of an earlier question, of a session before a
// /clear, one left from before the role was answered, a permission prompt, a file that does not read — is nil, and the
// screen is read as before.
func (s *Shift) askedFor(name string, sess *registry.Session) *AskedQuestion {
	if sess == nil || !hookRole(name) || waitKind(sess) != WaitQuestion || sess.StatusUpdatedAt <= 0 {
		return nil
	}

	var q AskedQuestion
	if !readRecord(s.Crew.Layout().HookQuestion(name), &q) || q.SessionID == "" || q.SessionID != sess.SessionID ||
		len(q.Questions) == 0 || !near(q.At, sess.StatusSince()) {
		return nil
	}

	return &q
}

// dialogFor is the dialog a role shows: the question its hook recorded when that is the one on screen (askedFor),
// else the one read from the screen (nil when the screen does not read as one).
func (s *Shift) dialogFor(name string, sess *registry.Session, screen *Dialog) *Dialog {
	q := s.askedFor(name, sess)
	if q == nil {
		return screen
	}

	return askedDialog(q, screen)
}

// words are the first question as it is shown: its text, then what a Dialog has no place for — that several choices can
// be taken, and that more questions follow, which Claude Code asks one after another.
func (q *AskedQuestion) words() string {
	first := q.Questions[0]
	out := oneLine(first.Question)

	if first.MultiSelect {
		out += " (several can be chosen)"
	}

	if rest := q.Questions[1:]; len(rest) > 0 {
		var more []string
		for _, r := range rest {
			more = append(more, oneLine(r.Question))
		}

		out += fmt.Sprintf(" (and %d more after this one: %s)", len(rest), clip(strings.Join(more, " / ")))
	}

	return out
}

// Dialog is the first question of the call as a Dialog, choices numbered from 1 in the order the tool gave them. Nil when
// the first question has no choices.
func (q *AskedQuestion) Dialog() *Dialog {
	if len(q.Questions) == 0 || len(q.Questions[0].Options) == 0 {
		return nil
	}

	d := &Dialog{Question: q.words(), Cursor: 1}

	for i, o := range q.Questions[0].Options {
		d.Options = append(d.Options, DialogOption{N: i + 1, Label: oneLine(o.Label), Detail: oneLine(o.Description)})
	}

	// Never a screen's fingerprint: an answer that names it is not taken by the screen's dialog (askedDialog).
	d.ID = dialogID("hook\x00"+d.Question, d.Options)

	return d
}

// askedDialog is the recorded question as the role's dialog. Where the screen reads as a dialog too, the recorded
// question is used only when the screen shows it: the screen's question says the recorded first question's words and
// its choices are the recorded ones, in the same order. Claude Code asks a call's questions one after another and the
// hook speaks once, so a screen that has moved on to the next question — even one with the same choices (Yes, No) — is
// not this one, and then the screen's dialog is returned as it is. Where it is this one, the words are the recorded
// ones and the choices, the cursor and the fingerprint the screen's: what an answer names is the screen's, and the
// choices stay as the screen draws them (a multiple choice draws a box before each). Where the screen does not read as a
// dialog, the recorded question stands alone with a fingerprint no screen has, so nothing can be answered by it.
func askedDialog(q *AskedQuestion, screen *Dialog) *Dialog {
	d := q.Dialog()

	switch {
	case d == nil:
		return screen
	case screen == nil:
		return d
	case !sameQuestion(q.Questions[0].Question, screen.Question) || !sameChoices(d, screen):
		return screen
	}

	shown := *screen
	shown.Question = d.Question
	shown.Options = slices.Clone(screen.Options)

	for i := range shown.Options {
		if shown.Options[i].Detail == "" {
			shown.Options[i].Detail = d.Options[i].Detail
		}
	}

	return &shown
}

// plain is a text as two readings of one question can be compared: without the frame a screen draws around it, one
// space between words, in lower case.
func plain(text string) string {
	text = strings.Map(func(r rune) rune {
		if strings.ContainsRune("│|╭╮╰╯─", r) {
			return ' '
		}

		return r
	}, text)

	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

// sameQuestion reports whether the screen's question is the recorded one: either says the other's words (the screen puts
// the dialog's header before the question, and may start its words further down than the call did).
func sameQuestion(recorded, onScreen string) bool {
	rec, scr := plain(recorded), plain(onScreen)

	return rec != "" && scr != "" && (strings.Contains(scr, rec) || strings.Contains(rec, scr))
}

// sameChoices reports whether the screen shows the choices the hook recorded: as many, numbered alike, each label the
// recorded one — a long label wraps on the screen, a multiple choice draws a box before it.
func sameChoices(hook, screen *Dialog) bool {
	if len(hook.Options) != len(screen.Options) {
		return false
	}

	for i, o := range hook.Options {
		on := screen.Options[i]
		if on.N != o.N || !strings.Contains(on.Label, o.Label) && !strings.HasPrefix(o.Label, on.Label) {
			return false
		}
	}

	return true
}
