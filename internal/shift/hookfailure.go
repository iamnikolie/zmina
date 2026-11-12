package shift

import (
	"encoding/json"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/registry"
)

const (
	// failureTextMax caps what is kept of the text an error came with, in bytes.
	failureTextMax = 600
	// FailHold is how long the lines queued for a role wait after its turn ended on an API error (failureHeld). A
	// session that keeps failing — a model that is not there, a login that lapsed, a limit — would otherwise take its
	// whole queue at one line a second, each line a turn that fails again. Past it one line goes; if that turn fails
	// too, its own record holds the role again.
	FailHold = 5 * time.Minute
	// failSame is how far apart the transcript's error line and a hook's record may be and still be one failed turn.
	failSame = time.Minute
)

// TurnFailure is what a role's StopFailure hook recorded: its turn ended on an API error. Claude Code runs no Stop
// hook for such a turn, and its status goes idle as for any other turn that ended. Error is the short code Claude Code
// gives the error (model_not_found) and Details the words it showed for it, passed on as they came and never sorted
// into kinds: which errors are a usage limit is not established.
type TurnFailure struct {
	SessionID string    `json:"session_id"`
	At        time.Time `json:"at"`
	Error     string    `json:"error,omitempty"`
	Details   string    `json:"details,omitempty"`
}

// Text is the error in a few words: what Claude Code named, then what it said of it.
func (f *TurnFailure) Text() string {
	switch {
	case f.Error != "" && f.Details != "" && f.Details != f.Error:
		return clip(f.Error + ": " + f.Details)
	case f.Error != "":
		return clip(f.Error)
	case f.Details != "":
		return clip(f.Details)
	}

	return "no text given"
}

// id tells this failure from the next one in the record of what was told of it.
func (f *TurnFailure) id() string {
	return "hook:" + f.SessionID + ":" + strconv.FormatInt(f.At.UnixMilli(), 10)
}

// hookText is the words of a field of a hook's input that may be a string or a small object: the string, an object's
// message or text, otherwise the field as JSON. Cut to failureTextMax.
func hookText(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}

	if json.Unmarshal(raw, &s) != nil {
		var obj struct {
			Message string `json:"message"`
			Text    string `json:"text"`
		}

		if json.Unmarshal(raw, &obj) == nil && (obj.Message != "" || obj.Text != "") {
			s = obj.Message + obj.Text
		} else {
			s = string(raw)
		}
	}

	s = oneLine(s)
	if len(s) > failureTextMax {
		s = s[:failureTextMax]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}

	return s
}

// failureInput reads a StopFailure hook's input: the session it ran for, the error's code (error) and the words
// Claude Code showed for it (last_assistant_message). False when the input is not one.
func failureInput(input []byte) (TurnFailure, bool) {
	var in struct {
		SessionID string          `json:"session_id"`
		Error     json.RawMessage `json:"error"`
		Last      json.RawMessage `json:"last_assistant_message"`
	}

	if json.Unmarshal(input, &in) != nil || in.SessionID == "" {
		return TurnFailure{}, false
	}

	return TurnFailure{SessionID: in.SessionID, Error: hookText(in.Error), Details: hookText(in.Last)}, true
}

// RecordFailure reads a StopFailure hook's input and writes it as the role's record (replacing the one before), and
// returns what it recorded. Nil, with no error, for input that is not a StopFailure's or a role that is no file name;
// an error when the record could not be written.
func RecordFailure(l crew.Layout, role string, input []byte, now time.Time) (*TurnFailure, error) {
	f, ok := failureInput(input)
	if !ok || !hookRole(role) {
		return nil, nil
	}

	f.At = now.UTC()

	if err := writeRecord(l, l.HookFailure(role), f); err != nil {
		return nil, err
	}

	return &f, nil
}

// failureOf is the failure a role's hook recorded in this session, whatever has happened since; nil when there is none,
// or it is of another session (before a restart or a /clear).
func (s *Shift) failureOf(name string, sess *registry.Session) *TurnFailure {
	if sess == nil || !hookRole(name) {
		return nil
	}

	var f TurnFailure
	if !readRecord(s.Crew.Layout().HookFailure(name), &f) || f.SessionID == "" || f.SessionID != sess.SessionID {
		return nil
	}

	return &f
}

// failureFor is the failure a role's last turn ended on, while that is still how it stands: the session is idle, and
// went idle within hookSkew of the moment the hook recorded (a failed turn goes idle as the hook runs; a turn after it
// goes idle at a later moment, so a record left by an earlier turn is not this one's). Nil otherwise, and for a role
// with no such hook.
func (s *Shift) failureFor(name string, sess *registry.Session) *TurnFailure {
	if f := s.failureOf(name, sess); f.current(sess) {
		return f
	}

	return nil
}

// current reports whether the session is idle since the failed turn this record is of (see failureFor). A nil record is
// not.
func (f *TurnFailure) current(sess *registry.Session) bool {
	return f != nil && sess != nil && sess.Status == "idle" && sess.StatusUpdatedAt > 0 && near(f.At, sess.StatusSince())
}

// failureHeld reports whether the lines queued for a role wait because its last turn ended on an API error less than
// FailHold ago (a record of the hook that is of its current session and still how it stands: failureFor). Delivery
// of any kind — the beat's, the pass's, a turn end's — leaves the role alone while it holds. It asks no tmux: the
// role's session is found by its name in Claude Code's session files, as Waiting needs, and only for a role that has such
// a record. A record of another session, an older one, or a role the hook never ran for holds nothing.
func (s *Shift) failureHeld(name string, now time.Time) bool {
	if s.Reg == nil || !hookRole(name) {
		return false
	}

	var f TurnFailure
	if !readRecord(s.Crew.Layout().HookFailure(name), &f) || f.SessionID == "" || !within(now, f.At, FailHold) {
		return false
	}

	r, err := s.Role(name)
	if err != nil || r.IsCodex() {
		return false
	}

	sess := s.Reg.LiveByName(r.Session)

	return sess != nil && f.SessionID == sess.SessionID && f.current(sess)
}

// sameTurn reports whether the transcript's last error is the failed turn the record is of: written within failSame of
// it. An error line that carries no time is taken for it.
func (f *TurnFailure) sameTurn(turn registry.Turn) bool {
	if turn.ErrorAt.IsZero() {
		return true
	}

	d := turn.ErrorAt.Sub(f.At)

	return d >= -failSame && d <= failSame
}

// apiErrorNote is the note for the dispatcher about a role whose last turn ended in an API error, once for each failed
// turn: from the StopFailure hook as soon as the turn ends, or from the transcript's last lines for a role without the
// hook. The transcript's words are the ones told when it has them, so a role with the hook is told what a role without
// it is. rs.ErrorNoted holds what was last told: the transcript's id of the error, or the hook's while the transcript
// did not show it yet. Told by one, the other does not tell it again — also when the role has left idle by the time the
// transcript shows it (the next turn started before a beat saw both): the hook's record of the session, however old its
// status, says that error is that turn's.
func (s *Shift) apiErrorNote(name string, rs *RoleState) string {
	sess := s.session(name)
	if sess == nil {
		return ""
	}

	turn, err := s.registryFor(name).Last(sess.SessionID)
	if err != nil {
		turn = registry.Turn{}
	}

	rec := s.failureOf(name, sess)

	var fail *TurnFailure
	if rec.current(sess) {
		fail = rec
	}

	var id, text string

	switch {
	case turn.Error != "":
		switch {
		case rs.ErrorNoted == turn.ErrorID:
			return ""
		case rec != nil && rs.ErrorNoted == rec.id() && rec.sameTurn(turn):
			rs.ErrorNoted = turn.ErrorID

			return ""
		}

		id, text = turn.ErrorID, turn.Error
	case fail != nil:
		if rs.ErrorNoted == fail.id() {
			return ""
		}

		id, text = fail.id(), fail.Text()
	default:
		return ""
	}

	rs.ErrorNoted = id

	note := name + ": its last turn ended in an API error: " + clip(text)
	s.log(name, "api-error", "", note)

	return note
}
