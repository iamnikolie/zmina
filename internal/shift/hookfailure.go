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
	// time and the least of any limit's; an account error waits AccountHold, a limit with a reset time until then (holdFor).
	FailHold = 5 * time.Minute
	// failSame is how far apart the transcript's error line and a hook's record may be and still be one failed turn.
	failSame = time.Minute
)

// TurnFailure is what a role's StopFailure hook recorded: its turn ended on an API error. Claude Code runs no Stop
// hook for such a turn, and its status goes idle as for any other turn that ended. Error is the short code Claude Code
// gives the error (model_not_found) and Details the words it showed for it, passed on as they came. ErrorDetails is the
// hook's own error_details, which only the sorting reads. What kind of error it is (transient, a limit, an account problem)
// is worked out once, when the record is written (Kind), and for a limit the moment it lifts is read then too (Reset), in
// the zone of the host the notice was shown on: every reader after that takes both from the record and parses nothing. A
// record written before they existed has neither, and is sorted by its words when it is read, with no reset time.
type TurnFailure struct {
	SessionID    string    `json:"session_id"`
	At           time.Time `json:"at"`
	Error        string    `json:"error,omitempty"`
	Details      string    `json:"details,omitempty"`
	ErrorDetails string    `json:"error_details,omitempty"`
	Kind         string    `json:"kind,omitempty"`
	Reset        time.Time `json:"reset,omitzero"`
}

// Class is what kind of error the record is, with the reset time of a limit when the record has one: the record's own, never
// worked out again from words and the reader's zone. A record with no Kind is sorted by its words, its reset left unknown.
func (f *TurnFailure) Class() FailureClass {
	switch kind := FailureKind(f.Kind); kind {
	case KindLimit:
		return FailureClass{Kind: KindLimit, Reset: f.Reset}
	case KindTransient, KindAccount:
		return FailureClass{Kind: kind}
	}

	c := ClassifyFailure(f.Error, f.ErrorDetails+" "+f.Details, f.At)
	c.Reset = time.Time{}

	return c
}

// holdFor is how long after the record the lines queued for the role wait, by what kind of error it is: FailHold for a
// transient one; AccountHold for an account error; for a limit until its reset time, never less than FailHold nor more than
// LimitHoldMax, FailHold when the record has no reset time.
func (f *TurnFailure) holdFor() time.Duration {
	c := f.Class()

	switch {
	case c.Kind == KindAccount:
		return AccountHold
	case c.Kind == KindLimit && c.Reset.After(f.At):
		return min(max(c.Reset.Sub(f.At), FailHold), LimitHoldMax)
	default:
		return FailHold
	}
}

// holdEnd is the moment the hold of the record ends.
func (f *TurnFailure) holdEnd() time.Time { return f.At.Add(f.holdFor()) }

// holds reports whether the lines queued for a role wait at now because of this failure. A record older than the longest
// hold there is is stale, and is not even sorted.
func (f *TurnFailure) holds(now time.Time) bool {
	return within(now, f.At, LimitHoldMax) && within(now, f.At, f.holdFor())
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

	// Sorted once, here: the reset time of a limit is read against the clock and the zone of the host that showed it.
	c := ClassifyFailure(f.Error, f.ErrorDetails+" "+f.Details, now)
	f.Kind, f.Reset = string(c.Kind), c.Reset

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
// "the lines queued for it wait 5m0s" for a transient error, as it always said; for a limit until its reset time (never less
// than FailHold, never more than LimitHoldMax) when the record has one, shown in loc (the crew's location); for an account
// problem AccountHold, or less when the role's next turn ends or it is restarted, and the owner to mend it.
func HoldWords(f *TurnFailure, loc *time.Location) string {
	c := f.Class()

	switch {
	case c.Kind == KindAccount:
		return "an account problem that needs the owner: the lines queued for it wait " + span(AccountHold) + ", or less when its " +
			"next turn ends or it is restarted"
	case c.Kind == KindLimit && c.Reset.After(f.At):
		return "a usage limit: the lines queued for it wait until " + f.holdEnd().In(loc).Format("Mon 15:04 MST") +
			" (at least " + span(FailHold) + ", at most " + span(LimitHoldMax) + ")"
	case c.Kind == KindLimit:
		return "a usage limit with no reset time given: the lines queued for it wait " + FailHold.String()
	default:
		return "the lines queued for it wait " + FailHold.String()
	}
}

// failureHeld reports whether the lines queued for a role wait because its last turn ended on an API error that still holds
// them (TurnFailure.holds: FailHold for a transient one, AccountHold for an account problem, until the reset time for a
// limit that gave one), a record of the hook that is of its current session and still how it stands (failureFor): a turn the
// role goes through since ends it, and so does a restart, which takes the record away. A role started anew has no record, and
// the owner's own message goes as ever (deliverInboxOnce). Delivery of any kind — the beat's, the pass's, a turn end's —
// leaves the role alone while it holds. It asks no tmux: the role's session is found by its name in Claude Code's session
// files, as Waiting needs, and only for a role that has such a record, and a record past the longest hold is not even
// sorted. A record of another session, an older one, or a role the hook never ran for holds nothing.
func (s *Shift) failureHeld(name string, now time.Time) bool {
	_, held := s.failureRecord(name, now)

	return held
}

// failureRecord is the record that holds the role's queue at now, and whether one does (failureHeld).
func (s *Shift) failureRecord(name string, now time.Time) (*TurnFailure, bool) {
	if s.Reg == nil || !hookRole(name) {
		return nil, false
	}

	var f TurnFailure
	if !readRecord(s.Crew.Layout().HookFailure(name), &f) || f.SessionID == "" || !f.holds(now) {
		return nil, false
	}

	r, err := s.Role(name)
	if err != nil || r.IsCodex() {
		return nil, false
	}

	sess := s.Reg.LiveByName(r.Session)
	if sess == nil || f.SessionID != sess.SessionID || !f.current(sess) {
		return nil, false
	}

	return &f, true
}

// FailureHold is a role whose queue waits for a failed turn, as the owner is told of it (zmina doctor).
type FailureHold struct {
	Role string
	// Class is what kind of error it was, Text the words of it, Until when the hold ends if nothing ends it sooner.
	Class FailureClass
	Text  string
	Until time.Time
}

// FailureHolds lists the roles whose queues are held for a failed turn at now, by the very test the delivery uses
// (failureRecord): a record of the role's current session, still how the session stands (idle since that turn) and within its
// hold. A role the owner's message went through, one that has been through a turn since, one whose session changed (a /clear,
// a restart), or whose hold ran out, is not listed, so that zmina doctor and zmina ls agree. Until is in the crew's location
// (the crew file's timezone, else this machine's). It needs the registry (Reg) as the delivery does.
func (s *Shift) FailureHolds(now time.Time) []FailureHold {
	var out []FailureHold

	for _, name := range s.Names() {
		if f, held := s.failureRecord(name, now); held {
			out = append(out, FailureHold{Role: name, Class: f.Class(), Text: f.Text(), Until: f.holdEnd().In(s.Crew.Location())})
		}
	}

	return out
}

// ownerLine is the place in a role's queue of the first line that is the owner's own message (the cockpit's chat), -1 when
// there is none.
func (s *Shift) ownerLine(name string) int {
	rs, ok := s.State.Roles[name]
	if !ok {
		return -1
	}

	for i, line := range rs.Inbox {
		if s.ownerChat(name, line) {
			return i
		}
	}

	return -1
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

	class, held := s.classOf(name, rec, turn, text)
	note := apiErrorLine(name, text, class, held)
	s.log(name, "api-error", "", note)

	// A dispatcher that is held hears nothing of it: the owner must see it in the log (and zmina ls, zmina doctor).
	if held && name == s.Crew.Dispatcher && class.Kind != KindTransient {
		s.log(name, "dispatcher-held", "", "its turn ended on an API error ("+class.Words()+") and its queue, the crew's signals "+
			"and the health notes wait for it until "+rec.holdEnd().In(s.Crew.Location()).Format("15:04 MST")+" or its next turn: the owner has to "+
			"look (zmina ls, zmina doctor): "+clip(text))
	}

	return note
}

// classOf is the kind of the error a note is told of, and whether the role is held for it. When the hook recorded that turn
// (a record of the session, that of the transcript's error when it shows one) the record's kind is the kind, the very one
// the hold goes by; a role with no hook record has the kind of the words it was shown, and no hold to claim.
func (s *Shift) classOf(name string, rec *TurnFailure, turn registry.Turn, text string) (FailureClass, bool) {
	if rec == nil || turn.Error != "" && !rec.sameTurn(turn) {
		return ClassifyFailure("", text, s.Now()), false
	}

	_, held := s.failureRecord(name, s.Now())

	return rec.Class(), held
}

// apiErrorLine is the note for the dispatcher about a role whose last turn ended in an API error. A transient error is the
// plain line it always was. A limit says so, and when it lifts if the message did. An account problem says it needs the
// owner (zmina cannot mend a billing, login or permission error, nor the role by retrying), and, when the role's queue is
// held for it, for how long: the hold is a probe, so it is AccountHold and not for ever.
func apiErrorLine(name, text string, c FailureClass, held bool) string {
	switch c.Kind {
	case KindLimit:
		return name + ": its last turn ended in an API error (" + c.Words() + "): " + clip(text)
	case KindAccount:
		line := name + ": its last turn ended in an API error that needs the owner (an account, billing, login or permission " +
			"problem, which retrying does not mend): " + clip(text)
		if held {
			line += "; its queued lines wait " + span(AccountHold) + ", or less when its next turn ends or it is restarted"
		}

		return line
	default:
		return name + ": its last turn ended in an API error: " + clip(text)
	}
}
