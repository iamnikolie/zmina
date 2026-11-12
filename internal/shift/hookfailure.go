package shift

import (
	"encoding/json"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/registry"
)

// failureTextMax caps what is kept of the text an error came with, in bytes.
const failureTextMax = 600

// TurnFailure is what a role's StopFailure hook recorded: its turn ended on an API error. Claude Code runs no Stop
// hook for such a turn, and its status goes idle as for any other turn that ended. Error and Details are Claude Code's
// own words, passed on as they came and never sorted into kinds: which errors are a usage limit is not established.
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

// failureInput reads a StopFailure hook's input: the session it ran for, and what it says of the error. Empty
// session when the input is not one.
func failureInput(input []byte) (TurnFailure, bool) {
	var in struct {
		SessionID string          `json:"session_id"`
		Error     json.RawMessage `json:"error"`
		Details   json.RawMessage `json:"error_details"`
		Last      json.RawMessage `json:"last_assistant_message"`
	}

	if json.Unmarshal(input, &in) != nil || in.SessionID == "" {
		return TurnFailure{}, false
	}

	f := TurnFailure{SessionID: in.SessionID, Error: hookText(in.Error), Details: hookText(in.Details)}
	if f.Details == "" {
		f.Details = hookText(in.Last)
	}

	return f, true
}

// RecordFailure reads a StopFailure hook's input and writes it as the role's record (replacing the one before). It
// returns the session the hook ran for — "" when the input is not a StopFailure's — whether or not the record could be
// written: the turn is over either way.
func RecordFailure(l crew.Layout, role string, input []byte, now time.Time) (string, error) {
	f, ok := failureInput(input)
	if !ok {
		return "", nil
	}

	if !hookRole(role) {
		return f.SessionID, nil
	}

	f.At = now.UTC()

	return f.SessionID, writeRecord(l, l.HookFailure(role), f)
}

// failureFor is the failure a role's last turn ended on, while that is still how it stands: the session is idle (the
// next turn leaves it so), the record is of that very session, and it was written no earlier than the session went idle
// (less hookSlack) — a record left by an earlier turn is not this turn's. Nil otherwise, and for a role with no hook.
func (s *Shift) failureFor(name string, sess *registry.Session) *TurnFailure {
	if sess == nil || sess.Status != "idle" || sess.StatusUpdatedAt <= 0 {
		return nil
	}

	var f TurnFailure
	if !readRecord(s.Crew.Layout().HookFailure(name), &f) || f.SessionID == "" || f.SessionID != sess.SessionID ||
		f.At.Before(sess.StatusSince().Add(-hookSlack)) {
		return nil
	}

	return &f
}

// FailedTurnOver is TurnOver for a turn that ended on an API error: the StopFailure hook ran for the session, which is
// what says the turn is over — the transcript's last line is Claude Code's error message, which closes no turn — so
// the session being idle is all there is to wait for.
func (s *Shift) FailedTurnOver(name, sessionID string) bool {
	sess := s.session(name)

	return sess != nil && sess.SessionID == sessionID && sess.Status == "idle"
}

// apiErrorNote is the note for the dispatcher about a role whose last turn ended in an API error, once per error: from
// the StopFailure hook as soon as the turn ends, or from the transcript's last lines for a role without the hook. The
// transcript's words are the ones told when it has them, so a role with the hook is told what a role without it is.
// rs.ErrorNoted holds what was last told: the transcript's id of the error, or the hook's while the transcript does
// not show it yet — told by one, the other does not tell it again.
func (s *Shift) apiErrorNote(name string, rs *RoleState) string {
	sess := s.session(name)
	if sess == nil {
		return ""
	}

	turn, err := s.registryFor(name).Last(sess.SessionID)
	if err != nil {
		turn = registry.Turn{}
	}

	fail := s.failureFor(name, sess)

	var id, text string

	switch {
	case turn.Error != "":
		if rs.ErrorNoted == turn.ErrorID {
			return ""
		}

		if fail != nil && rs.ErrorNoted == fail.id() {
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
