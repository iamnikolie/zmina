package shift

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/registry"
)

// What a role's hooks record is one small file a role and kind in the crew's hooks directory (crew.Layout.Hooks).
// A record is a hint, never the truth: it names the session it is about and when it was written, and a view
// reads it only while the session file of Claude Code agrees it is still current (askedLook, TurnFailure.current). Nothing
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

// permissionInputMax is the most characters of a tool call's input a permission record keeps to show: the whole input, however long
// (a Write's file), is in the record's sum.
const permissionInputMax = 1000

// AskedPermission is what a role's PermissionRequest hook wrote when a tool call put a permission prompt on its screen: which tool,
// what it was asked to do (its command, path or input, cut), and a sum of the whole input, so that two calls that differ anywhere
// are never the same request. It is the only thing that says what a permission prompt is about: the prompt on the screen asks
// "Do you want to proceed?" of a command it draws above.
type AskedPermission struct {
	SessionID string    `json:"session_id"`
	At        time.Time `json:"at"`
	Tool      string    `json:"tool"`
	// Input is what the call was asked to do, on one line: a newline of it is shown as ⏎.
	Input    string `json:"input"`
	InputSum string `json:"input_sum"`
	// Desc is a shell call's own words for what it does, which the prompt draws under the command.
	Desc string `json:"description,omitempty"`
	// Cut: Input is not all of what the call asks. It was cut, folding changed more of it than the space at its ends (several
	// lines, control characters: what is shown is not what runs), or the call has parts Input does not show and that change what is
	// approved (a shell call's sandbox switch, a file call's content): a phone shown it cannot judge the call, and is offered nothing
	// to press.
	Cut bool `json:"subject_cut,omitempty"`
}

// permissionRecords is the file of a role's permission records: the last few calls, not the last one, because two requests of a
// session may be made within a moment (parallel tool calls, sub-agents) and the one on the screen is then not the one that wrote
// last. Lost is the newest record the file had to let go of (the cap, a day's age): a prompt whose wait began before it is not
// known to be anyone's.
type permissionRecords struct {
	Records []AskedPermission `json:"records"`
	Lost    time.Time         `json:"lost,omitzero"`
}

const (
	// permissionKeep is how many calls a role's permission file keeps; permissionAge how long a call is kept at most, for the sake of
	// what it holds, not of what it says: a call that a wait may still be of is never dropped for its age (a prompt can stand for
	// hours), only for the cap, and then the file says so (Lost) and the prompt is not told.
	permissionKeep = 8
	permissionAge  = 24 * time.Hour
	// permissionDescMax is how much of a shell call's description is kept.
	permissionDescMax = 300
	// newlineMark shows a line break of a command in its subject.
	newlineMark = " ⏎ "
)

// RecordPermission reads a PermissionRequest hook's input and, for any tool but AskUserQuestion, adds the call to the role's
// permission file (the last few calls of the session, under a lock: hooks of parallel requests run at once). Input that is empty
// records nothing and is no error; input that is not empty and is not a call it can read — cut at the bound, not JSON, with no tool or
// session — is noted in the file as a call it lost (Lost), so that the wait it may have been of is no one's.
func RecordPermission(l crew.Layout, role string, input []byte, now time.Time) error {
	if !hookRole(role) || len(input) == 0 {
		return nil
	}

	var in struct {
		SessionID string          `json:"session_id"`
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}

	if json.Unmarshal(input, &in) == nil && in.ToolName == askTool {
		return nil
	}

	// A call whose input does not read (a hook input over its bound is cut, and does not), or is not a call, may be the very call a
	// prompt is on the screen for, and leaves no trace of it: what that wait is of is not known, and is no one's.
	if json.Unmarshal(input, &in) != nil || in.ToolName == "" || in.SessionID == "" {
		return updatePermissions(l, role, func(have *permissionRecords) {
			have.Lost = laterOf(have.Lost, now)
		})
	}

	// The sum is of the input as data, however it was spaced or ordered: a map marshals with its keys in order.
	canon := []byte(in.ToolInput)

	var v any
	if json.Unmarshal(in.ToolInput, &v) == nil {
		if raw, err := json.Marshal(v); err == nil {
			canon = raw
		}
	}

	sum := sha256.Sum256(append([]byte(in.ToolName+"\x00"), canon...))
	words, whole := toolInputWords(in.ToolInput, canon)
	norm := strings.TrimSpace(words)

	// Folded, what is shown is the call only if folding changed nothing but the space at its ends; several lines are several things
	// to run, and are shown with their breaks, and are not pressed. A mark that is in the words already is not told from a break.
	shown := oneLine(strings.NewReplacer("\r\n", newlineMark, "\n", newlineMark, "\r", newlineMark).Replace(norm))

	rec := AskedPermission{SessionID: in.SessionID, At: now.UTC(), Tool: oneLine(in.ToolName), Input: clipRunes(shown, permissionInputMax),
		InputSum: hex.EncodeToString(sum[:8]), Cut: !whole || oneLine(norm) != norm || len([]rune(shown)) > permissionInputMax ||
			strings.Contains(norm, strings.TrimSpace(newlineMark))}

	var m map[string]any
	if json.Unmarshal(in.ToolInput, &m) == nil {
		if d, ok := m["description"].(string); ok {
			rec.Desc = clipRunes(oneLine(d), permissionDescMax)
		}
	}

	return updatePermissions(l, role, func(have *permissionRecords) {
		kept := have.Records[:0:0]

		for _, r := range have.Records {
			if now.Sub(r.At) > permissionAge {
				have.Lost = laterOf(have.Lost, r.At)

				continue
			}

			kept = append(kept, r)
		}

		kept = append(kept, rec)

		for len(kept) > permissionKeep {
			have.Lost = laterOf(have.Lost, kept[0].At)
			kept = kept[1:]
		}

		have.Records = kept
	})
}

// updatePermissions changes a role's permission file under its lock (hooks of parallel requests run at once), whole or not at all.
func updatePermissions(l crew.Layout, role string, change func(have *permissionRecords)) error {
	if st, err := os.Stat(l.Dir); err != nil || !st.IsDir() {
		return errors.New("no crew directory to record in")
	}

	if err := fsx.MkdirAll(l.Hooks(), 0o700); err != nil {
		return err
	}

	lock, err := os.OpenFile(l.HookPermission(role)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}

	defer func() { _ = lock.Close() }()

	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}

	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	var have permissionRecords

	_ = readRecord(l.HookPermission(role), &have)

	change(&have)

	if have.Records == nil {
		have.Records = []AskedPermission{}
	}

	return writeRecord(l, l.HookPermission(role), have)
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}

	return a
}

// benignInputKeys are the keys of a tool's input that change nothing of what is approved: a shell call's own words for what it does,
// and its time limit.
var benignInputKeys = []string{"description", "timeout"}

// toolInputWords says what a tool call was asked to do: the command of a shell call, the path or address of a file or web call, else
// the input as JSON (compact, canon). whole is false when the call has other parts that change what is approved (a key that is not
// the one shown, and not a benign one): the words are then not all of it.
func toolInputWords(raw json.RawMessage, canon []byte) (words string, whole bool) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return string(canon), true
	}

	for _, key := range []string{"command", "file_path", "path", "notebook_path", "url", "pattern", "query"} {
		v, ok := m[key].(string)
		if !ok || v == "" {
			continue
		}

		for other := range m {
			if other != key && !slices.Contains(benignInputKeys, other) {
				return v, false
			}
		}

		return v, true
	}

	return string(canon), true
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

// dropHookRecords removes the records a role's hooks left (the question, the permission calls, the failure and the turn's
// question): a role that starts a new session has none of that session's, and what the old one recorded must not read as the
// new one's. Nothing else in the hooks directory is touched.
func (s *Shift) dropHookRecords(name string) {
	if !hookRole(name) {
		return
	}

	l := s.Crew.Layout()
	_ = os.Remove(l.HookQuestion(name))
	_ = os.Remove(l.HookPermission(name))
	_ = os.Remove(l.HookPermission(name) + ".lock")
	_ = os.Remove(l.HookFailure(name))
	_ = os.Remove(l.HookTurn(name))
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

// askedLook is the question a role's hook recorded, and whether it is the question on the role's screen: the session file
// says the session waits on a question (input needed), the record is of that very session, and it was written within
// hookSkew of the moment that wait began. Anything else — a record of an earlier question, of a session before a /clear,
// one left from before the role was answered, a permission prompt, a file that does not read — is not current, and the
// screen is read as before; why is then said in words. A role that does not wait on a question is not asked about one (no
// reason). The record is returned whenever there is one of this session.
func (s *Shift) askedLook(name string, sess *registry.Session) (q *AskedQuestion, current bool, why string) {
	if sess == nil || !hookRole(name) || waitKind(sess) != WaitQuestion || sess.StatusUpdatedAt <= 0 {
		return nil, false, ""
	}

	var rec AskedQuestion

	switch {
	case !readRecord(s.Crew.Layout().HookQuestion(name), &rec):
		return nil, false, "no record"
	case rec.SessionID == "":
		return nil, false, "a record with no session"
	case rec.SessionID != sess.SessionID:
		return nil, false, "a record of another session (before a restart or a /clear)"
	case len(rec.Questions) == 0:
		return nil, false, "a record with no question"
	case !near(rec.At, sess.StatusSince()):
		return &rec, false, "a record of an earlier question: " + apart(rec.At, sess.StatusSince()) + " the moment the session began waiting"
	}

	return &rec, true, ""
}

// permissionBefore is how long before a wait began a different call of the role still makes the prompt nobody's: a hook that wrote
// its record late (a loaded machine) and a parallel request that wrote soon after it are two calls within a few seconds of the wait.
const permissionBefore = 5 * time.Second

// permissionLook is the tool call a role's hook recorded, when it is certainly the one on the role's screen: the session file says the
// session waits on a permission prompt, and one call, and only one, was recorded for the role at or after the moment the wait began
// less a few seconds (the same call twice is one), and that call is of this session and was recorded within a second of the wait's
// start. Claude Code does not write the session file again while a session stays waiting on a permission prompt, so the prompt
// that follows an answered one in a queue has the first prompt's wait start: a different call recorded since is the next prompt's, or a
// parallel request's, and nothing says which of them is on the screen. Then there is nil, and no card, and a card told before the
// second call was recorded stops matching the moment it is (its fingerprint covers the call). So do no record, one of an earlier
// prompt, and a file that had to let go of a record that may have been of this wait, or never had it (an input it could not read).
func (s *Shift) permissionLook(name string, sess *registry.Session) *AskedPermission {
	if sess == nil || !hookRole(name) || waitKind(sess) != WaitPermission || sess.StatusUpdatedAt <= 0 {
		return nil
	}

	var have permissionRecords
	if !readRecord(s.Crew.Layout().HookPermission(name), &have) {
		return nil
	}

	since := sess.StatusSince()
	from := since.Add(-permissionBefore)

	if !have.Lost.IsZero() && !have.Lost.Before(from) {
		return nil
	}

	var (
		found   *AskedPermission
		nearOne bool
	)

	for i := range have.Records {
		r := have.Records[i]
		if r.SessionID == "" || r.Tool == "" || r.At.Before(from) {
			continue
		}

		// A call of another session of the role, at or after the wait began, may as well be what the screen shows.
		if found != nil && (found.Tool != r.Tool || found.InputSum != r.InputSum) {
			return nil
		}

		if r.SessionID == sess.SessionID {
			found = &r
			nearOne = nearOne || near(r.At, since)
		} else if found == nil {
			found = &r
		}
	}

	// The call is this session's, and recorded within the moment the wait began.
	if found == nil || found.SessionID != sess.SessionID || !nearOne {
		return nil
	}

	return found
}

// permissionTitles are the titles a permission prompt draws over the call it asks about; the goldens show the shell call's.
var permissionTitles = []string{"Bash command"}

// PermissionHinted reports whether a wait that is said to be of a question shows signs of being a permission prompt: a call recorded
// for the role since the wait began (or one the file lost or could not read), or a permission prompt's title among the last lines of
// the screen. A question wait is told and pressed only when none of these says otherwise: the session file is not written again while a
// session stays waiting, so a permission prompt can follow an answered question under the same words.
func (s *Shift) PermissionHinted(name string, since time.Time, screen string) bool {
	if hookRole(name) {
		var have permissionRecords

		if readRecord(s.Crew.Layout().HookPermission(name), &have) {
			from := since.Add(-permissionBefore)

			if !have.Lost.IsZero() && !have.Lost.Before(from) {
				return true
			}

			for _, r := range have.Records {
				if !r.At.Before(from) {
					return true
				}
			}
		}
	}

	lines := strings.Split(screen, "\n")
	if len(lines) > nowLines {
		lines = lines[len(lines)-nowLines:]
	}

	for _, l := range lines {
		folded := foldFramed(l)

		for _, title := range permissionTitles {
			if strings.HasPrefix(folded, title) {
				return true
			}
		}
	}

	return false
}

// How the prompt on a screen stands to the call a record is of.
type screenMatch int

const (
	// matchNone: the prompt shows another call, or none this could be.
	matchNone screenMatch = iota
	// matchWhole: the prompt's block shows the whole call, word for word.
	matchWhole
	// matchPartial: it shows the call, but not so that the whole can be compared (wrapped through a word, cut, more than the record
	// says).
	matchPartial
	// matchUnchecked: a tool no screen of the goldens shows: nothing of the screen is compared.
	matchUnchecked
)

// frameChars are what a box's frame draws: they are no part of what the prompt says, on either side of a comparison.
var frameChars = strings.NewReplacer("│", " ", "┃", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ", "─", " ")

func foldFramed(s string) string { return oneLine(frameChars.Replace(s)) }

// checkPermissionScreen says how the prompt on a screen stands to a recorded call. For a shell call it is strict: the prompt draws a
// block under its "Bash command" title and above its question, and that block must be the command, and the call's description under it
// as lines of their own, and nothing else: the whole of it, not the head of it somewhere among the last lines, where a tool line of an
// earlier call stands too. The title is the first one of the screen's last lines: text of a command that looks like a title can only make
// the block longer, and so make it fail to match. The goldens show the command of a shell call so; they show no other tool, so no
// other tool is held to a screen check and none is pressed (matchUnchecked).
func checkPermissionScreen(p *AskedPermission, screen string, d *Dialog) screenMatch {
	if p.Tool != "Bash" {
		return matchUnchecked
	}

	lines := strings.Split(screen, "\n")
	if len(lines) > nowLines {
		lines = lines[len(lines)-nowLines:]
	}

	title := -1

	for i, l := range lines {
		if strings.HasPrefix(foldFramed(l), "Bash command") {
			title = i

			break
		}
	}

	question := foldFramed(d.Question)
	if title < 0 || question == "" {
		return matchNone
	}

	end := -1

	for m := title + 1; m < len(lines); m++ {
		if strings.HasPrefix(foldFramed(strings.Join(lines[m:], " ")), question) {
			end = m

			break
		}
	}

	if end < 0 {
		return matchNone
	}

	var block []string

	for _, l := range lines[title+1 : end] {
		if f := foldFramed(l); f != "" {
			block = append(block, f)
		}
	}

	command := foldFramed(strings.ReplaceAll(p.Input, strings.TrimSpace(newlineMark), " "))
	desc := foldFramed(p.Desc)
	cut := strings.HasSuffix(p.Input, "…")
	best := matchNone

	// The command is the first lines of the block and the description the rest, the boundary between them a line's end: a command and a
	// description that run together on a line are not the command and the description the record has.
	for k := 1; k <= len(block); k++ {
		head, tail := strings.Join(block[:k], " "), strings.Join(block[k:], " ")

		switch {
		case head == command && tail == desc && !cut:
			return matchWhole
		case unspaced(head) == unspaced(command) && unspaced(tail) == unspaced(desc):
			// The same words, broken through a word: whole, but not word for word.
			best = matchPartial
		case cut && tail == desc && strings.HasPrefix(unspaced(head), unspaced(strings.TrimSuffix(command, "…"))):
			// A command that was cut in the record: the prompt shows more of it than the record has.
			best = matchPartial
		}
	}

	return best
}

// unspaced is a text without its spaces: wrapped through a word, it is what it was.
func unspaced(s string) string { return strings.ReplaceAll(s, " ", "") }

// apart says how far a moment is from another, in words: "1m0s before".
func apart(at, from time.Time) string {
	d := at.Sub(from).Round(time.Second)
	if d < 0 {
		return (-d).String() + " before"
	}

	return d.String() + " after"
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

	d := &Dialog{Question: q.words(), Cursor: 1, Multi: q.Questions[0].MultiSelect}

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
	shown.Multi = shown.Multi || q.Questions[0].MultiSelect
	shown.Asked = true

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
