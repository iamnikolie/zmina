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
	// too, its own record holds the role again. It is the wait for a transient error, and for a limit that gave no reset
	// time; an account error waits for the role's next turn instead (holds), a limit with a reset time until then.
	FailHold = 5 * time.Minute
	// failSame is how far apart the transcript's error line and a hook's record may be and still be one failed turn.
	failSame = time.Minute
)

// TurnFailure is what a role's StopFailure hook recorded: its turn ended on an API error. Claude Code runs no Stop
// hook for such a turn, and its status goes idle as for any other turn that ended. Error is the short code Claude Code
// gives the error (model_not_found) and Details the words it showed for it, passed on as they came. What kind of error
// it is (Class: transient, a limit, an account problem) is worked out from them when it is looked at, and never kept:
// the record is what the hook said. ErrorDetails is the hook's own error_details, which only the sorting reads.
type TurnFailure struct {
	SessionID    string    `json:"session_id"`
	At           time.Time `json:"at"`
	Error        string    `json:"error,omitempty"`
	Details      string    `json:"details,omitempty"`
	ErrorDetails string    `json:"error_details,omitempty"`
}

// Class is what kind of error the record is (ClassifyFailure over what the hook said), with its reset time for a limit,
// read against the moment the hook ran: a time the notice gives with no zone is in the zone of the host, which is where
// Claude Code showed it.
func (f *TurnFailure) Class() FailureClass { return f.classWith("") }

// classWith is Class over what the transcript showed of the same error besides.
func (f *TurnFailure) classWith(more string) FailureClass {
	return ClassifyFailure(f.Error, f.ErrorDetails+" "+f.Details+" "+more, f.At.In(time.Local))
}

// holds reports whether the lines queued for a role wait at now because of this failure, by what kind of error it is: a
// transient one FailHold; an account problem until the role's next turn ends, which is not a time (the caller asks whether
// the record is still how the session stands); a limit until its reset time, at most LimitHoldMax, FailHold when the message
// gave none.
func (f *TurnFailure) holds(now time.Time) bool {
	c := f.Class()

	switch {
	case c.Kind == KindAccount:
		return !f.At.IsZero() && !now.Before(f.At)
	case c.Kind == KindLimit && !c.Reset.IsZero():
		return within(now, f.At, min(c.Reset.Sub(f.At), LimitHoldMax))
	default:
		return within(now, f.At, FailHold)
	}
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
		Details   json.RawMessage `json:"error_details"`
	}

	if json.Unmarshal(input, &in) != nil || in.SessionID == "" {
		return TurnFailure{}, false
	}

	return TurnFailure{SessionID: in.SessionID, Error: hookText(in.Error), Details: hookText(in.Last),
		ErrorDetails: hookText(in.Details)}, true
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

// HoldWords is how long the lines queued for a role wait because of the failure, in the words of the failure-hold event:
// "the lines queued for it wait 5m0s" for a transient error, as it always said; for a limit until its reset time when the
// message gave one, and for an account problem until the role's next turn ends or it is restarted, the owner to mend it.
func HoldWords(f *TurnFailure) string {
	c := f.Class()

	switch {
	case c.Kind == KindAccount:
		return "an account problem that needs the owner: the lines queued for it wait until its next turn ends or it is " +
			"restarted"
	case c.Kind == KindLimit && !c.Reset.IsZero():
		return "a usage limit: the lines queued for it wait until " + c.Reset.Format("Mon 15:04 MST") + ", at most " +
			span(LimitHoldMax)
	case c.Kind == KindLimit:
		return "a usage limit with no reset time given: the lines queued for it wait " + FailHold.String()
	default:
		return "the lines queued for it wait " + FailHold.String()
	}
}

// failureHeld reports whether the lines queued for a role wait because its last turn ended on an API error that still holds
// them (TurnFailure.holds: FailHold for a transient one, until the reset time for a limit that gave one, until the role's
// next turn for an account problem), a record of the hook that is of its current session and still how it stands
// (failureFor). A role started anew has no record, and an owner's message goes as ever (ownerNext). Delivery
// of any kind — the beat's, the pass's, a turn end's — leaves the role alone while it holds. It asks no tmux: the
// role's session is found by its name in Claude Code's session files, as Waiting needs, and only for a role that has such
// a record. A record of another session, an older one, or a role the hook never ran for holds nothing.
func (s *Shift) failureHeld(name string, now time.Time) bool {
	held, _ := s.failureHold(name, now)

	return held
}

// HeldUntilNextTurn reports whether the role's queue is held by a failure that no time lifts: an account problem, which
// ends with the role's next turn, a restart or the owner's own message, each of which changes the state or runs a hook. A
// courier has no use for looking again every few seconds for such a role, as it has for a hold that runs out.
func (s *Shift) HeldUntilNextTurn(name string, now time.Time) bool {
	held, untimed := s.failureHold(name, now)

	return held && untimed
}

// failureHold is failureHeld and, for a role that is held, whether it is for an account problem.
func (s *Shift) failureHold(name string, now time.Time) (held, untimed bool) {
	if s.Reg == nil || !hookRole(name) {
		return false, false
	}

	var f TurnFailure
	if !readRecord(s.Crew.Layout().HookFailure(name), &f) || f.SessionID == "" || !f.holds(now) {
		return false, false
	}

	r, err := s.Role(name)
	if err != nil || r.IsCodex() {
		return false, false
	}

	sess := s.Reg.LiveByName(r.Session)
	if sess == nil || f.SessionID != sess.SessionID || !f.current(sess) {
		return false, false
	}

	return true, f.Class().Kind == KindAccount
}

// ownerNext reports whether the next line queued for a role is the owner's own message (the cockpit's chat). The hold
// after a failed turn is for what zmina would feed the role by itself; what the owner sends goes, as their typing into
// the pane would.
func (s *Shift) ownerNext(name string) bool {
	rs, ok := s.State.Roles[name]

	return ok && len(rs.Inbox) > 0 && s.ownerChat(name, rs.Inbox[0])
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

	note := apiErrorLine(name, text, s.classOf(rec, turn, text))
	s.log(name, "api-error", "", note)

	return note
}

// classOf is the kind of the error a note is told of: the hook's record of that turn when there is one (its code and
// details, and the words the transcript showed besides), else the words alone, read against now. A transcript's error that
// is of another turn than the record's is not the record's.
func (s *Shift) classOf(rec *TurnFailure, turn registry.Turn, text string) FailureClass {
	if rec == nil || turn.Error != "" && !rec.sameTurn(turn) {
		return ClassifyFailure("", text, s.Now())
	}

	return rec.classWith(text)
}

// apiErrorLine is the note for the dispatcher about a role whose last turn ended in an API error. A transient error is the
// plain line it always was. A limit says so, and when it lifts if the message did. An account problem says it needs the
// owner (zmina cannot mend a billing, login or permission error, nor the role by retrying) and that the role's queue waits
// until its next turn ends or it is restarted, not for a few minutes.
func apiErrorLine(name, text string, c FailureClass) string {
	switch c.Kind {
	case KindLimit:
		return name + ": its last turn ended in an API error (" + c.Words() + "): " + clip(text)
	case KindAccount:
		return name + ": its last turn ended in an API error that needs the owner (an account, billing, login or permission " +
			"problem, which retrying does not mend): " + clip(text) + "; its queued lines wait until its next turn ends or it is " +
			"restarted"
	default:
		return name + ": its last turn ended in an API error: " + clip(text)
	}
}
