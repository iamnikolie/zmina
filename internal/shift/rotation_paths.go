package shift

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// The other ways a role's conversation is changed under zmina's hand, each with a record of its own (rotation.go): zmina
// restart, a role's restart of itself, the ticker's reopen of a keep: always role whose session ended, zmina up of a role that
// holds tasks, and the /compact or /clear that context_max types. They change the role only through the primitives the
// ticker's restart does (BeginDown, FinishDown, UpWith, the hand-out of restart.go); what they add is the record, and one
// rule: a fresh start of a role that holds tasks hands them. The record decides nothing: a record that cannot be written
// leaves the start and the hand-out as they are.

const (
	// Why of a rotation by a command or the reopen: it reads after "[zmina] restarted fresh: " in the line the fresh session is shown.
	whyManual = "by hand (zmina restart)"
	whyOwn    = "its own restart"
	whyCrash  = "keep: always, after its session ended"
	whyFirst  = "keep: always started it"
	whyUp     = "started by hand (zmina up)"
)

// ErrDownTaken says the down a command began was taken over while it waited: the pane it meant to finish is another
// command's, and it is left alone.
var ErrDownTaken = errors.New("another command took it over meanwhile")

// noteOf is how a task's checkpoint is named in the line that hands it over: its short revision, or that there is none.
func noteOf(a RotationArtifact) string {
	if a.Checkpoint == "" {
		return "(none saved)"
	}

	return a.Checkpoint
}

// handFresh hands the tasks a role holds to the session just started fresh for it: the line that says why and a packet
// each, at the head of its queue, as a restart's hand-out does. A role that holds none is handed nothing. A hand-out that
// fails queues nothing and puts the deliveries it renewed back.
func (s *Shift) handFresh(name, why string, tasks []task.Task) (handout, error) {
	if len(tasks) == 0 || s.Tasks == nil {
		return handout{}, nil
	}

	ids := make([]string, 0, len(tasks))
	notes := make([]string, 0, len(tasks))

	for _, a := range s.rotationArtifacts(tasks, "") {
		ids = append(ids, a.Task)
		notes = append(notes, a.Task+" "+noteOf(a))
	}

	out, err := s.handOutFit(Restart{Name: name, Tasks: ids, Why: why, Notes: strings.Join(notes, ", ")}, true)
	if err != nil {
		s.restoreDeliveries(name, out)

		return out, err
	}

	for _, p := range out.packets {
		s.replacePackets(p.task, p.head.line)
	}

	return out, nil
}

// rotationStartedBy follows a start that a command (or the beat's reopen) made for the rotation id: on its old conversation
// the rotation is over (resumed); on a fresh one the tasks the role holds are handed, whether or not a record is kept, and the
// record follows it as it does a restart's (boot, and each task accepted or the line shown). A hand-out that fails closes the
// rotation lost at once and returns the error; told says the dispatcher is to hear it (the beat has no one else to tell).
func (s *Shift) rotationStartedBy(name, id, why string, told bool) error {
	if conv := s.Resumed(name); conv != "" {
		s.rotationEnd(name, id, OutcomeResumed, "continues its conversation "+conv, nil, false)

		return nil
	}

	tasks := s.heldTasks(name)

	out, err := s.handFresh(name, why, tasks)
	if err != nil {
		err = fmt.Errorf("%s: it started fresh, but handing its tasks to it failed: %w; zmina rotation retry %s hands them again",
			name, err, name)
		s.log(name, "restart-failed", taskIDs(tasks), err.Error())
		s.rotationStartedWith(name, id, out, tasks)
		s.rotationEnd(name, id, OutcomeLost, "its tasks were not handed to it: "+err.Error(), nil, false)

		if told {
			s.addNotes([]string{err.Error()})
		}

		return err
	}

	s.rotationStartedWith(name, id, out, tasks)

	return nil
}

// taskIDs are the ids of tasks, as an event names them.
func taskIDs(tasks []task.Task) string {
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}

	return strings.Join(ids, ",")
}

// BeginRotationDown is BeginDown for a command that restarts a role (zmina restart, a role's restart of itself): the record
// of the rotation is opened with what the role is on, and the down is noted in it once it is begun, for FinishRotationDown to
// find by the claim. A down that cannot begin (a draft in the input line, a dialog) leaves no record: nothing changed. kind says
// which rotation it is, why what it reads after "restarted fresh: " (whyManual, whyOwn).
func (s *Shift) BeginRotationDown(name, kind string, o DownOptions) (wait bool, claim time.Time, err error) {
	// A down that is followed by a start is refused for a role the crew file places on another node, before anything is put down:
	// the start would be refused too, and the role would be left down. zmina down of it is not this, and works.
	if err := s.errPlacedAway(name); err != nil {
		return false, time.Time{}, err
	}

	why := rotationWhy(kind)
	from := s.rotationFrom(name)
	tasks := s.heldTasks(name)

	wait, err = s.BeginDown(name, o)
	if err != nil {
		return false, time.Time{}, err
	}

	claim = s.State.Role(name).Downing
	s.rotationSwitching(name, s.rotationPrepareFrom(name, kind, why, from, tasks, "", time.Time{}), claim)

	return wait, claim, nil
}

// rotationWhy is what a rotation by a command reads after "restarted fresh: ".
func rotationWhy(kind string) string {
	switch kind {
	case KindOwn:
		return whyOwn
	case KindCrash:
		return whyCrash
	case KindUp:
		return whyUp
	}

	return whyManual
}

// FinishRotationDown ends the down BeginRotationDown began, unless another command took the role over while it was waited for
// (its down is not this claim): the pane of that command is left alone. It returns the id of the rotation this down belongs to.
func (s *Shift) FinishRotationDown(name string, claim time.Time) (string, error) {
	if rs := s.State.Role(name); !rs.Downing.Equal(claim) {
		return "", fmt.Errorf("%s: %w", name, ErrDownTaken)
	}

	rid := s.rotationOf(name, claim)

	if err := s.FinishDown(name); err != nil {
		s.rotationEnd(name, rid, OutcomeLost, "no session: the session it was on could not be removed: "+err.Error(), nil, false)

		return rid, err
	}

	return rid, nil
}

// StartRotation starts the role the rotation id put down, as opts say. A start that fails leaves the role down and held, as
// zmina restart always did, and closes the rotation lost with the way back; it is the command's error to tell. A start that
// goes through is followed (rotationStartedBy): the error it returns is of the hand-out, with the role up, and is a Partial
// one: what was done is saved.
func (s *Shift) StartRotation(name, id string, opts UpOptions, kind string) error {
	if err := s.UpWith(name, opts); err != nil {
		s.rotationEnd(name, id, OutcomeLost, "no session: its start failed: "+err.Error(), nil, false)

		return err
	}

	return Partial(s.rotationStartedBy(name, id, rotationWhy(kind), false))
}

// UpHanding is UpWith for a start by hand: a fresh start of a role that holds tasks hands them, with a record of the start that
// the beat follows to the end (accepted, or lost with the way back). A start that resumes a conversation, or of a role that
// holds nothing, is UpWith and nothing more. A hand-out that fails is a Partial error, the role being up.
func (s *Shift) UpHanding(name string, o UpOptions) error { return s.upHanding(name, o, s.UpWith) }

// upHanding is UpHanding with the start it makes given: UpWith, or a start that keeps the seat's count of failed starts (upFor).
func (s *Shift) upHanding(name string, o UpOptions, up func(string, UpOptions) error) error {
	from := s.rotationFrom(name)
	tasks := s.heldTasks(name)

	if err := up(name, o); err != nil {
		return err
	}

	if s.Resumed(name) != "" || len(tasks) == 0 {
		return nil
	}

	id := s.rotationPrepareFrom(name, KindUp, whyUp, from, tasks, "", time.Time{})

	return Partial(s.rotationStartedBy(name, id, whyUp, false))
}

// rotationReopened is the beat's record of a keep: always role it started again after its session ended: on the
// conversation it had (resume: always) the rotation is resumed, on a fresh one the tasks it holds are handed again, the
// dispatcher hears if that fails, and the record follows it. from is read before the start, which is another session's.
// A fresh start hands the tasks whether or not there is a record of it.
func (s *Shift) rotationReopened(name string, from RotationFrom, tasks []task.Task) {
	// A role that never ran (the crew's first start of a keep: always seat) has no conversation that was changed, and no session
	// that ended.
	if from.Session == "" {
		_ = s.rotationStartedBy(name, "", whyFirst, true)

		return
	}

	id := s.rotationPrepareFrom(name, KindCrash, whyCrash, from, tasks, "", time.Time{})

	_ = s.rotationStartedBy(name, id, whyCrash, true)
}

// openTrim is the newest compact or clear of the role that is not closed yet.
func (s *Shift) openTrim(name string) (Rotation, bool) {
	rots := s.rotationsOf(name)

	for i := len(rots) - 1; i >= 0; i-- {
		if r := rots[i]; r.Trim() && r.Open() {
			return r, true
		}
	}

	return Rotation{}, false
}

// rotationTrimOpen opens the record of a /compact or /clear about to be typed into the role: the conversation it is on now.
func (s *Shift) rotationTrimOpen(name, kind, why string) string {
	if !s.rotationsOn(name) {
		return ""
	}

	return s.rotationPrepareFrom(name, kind, why, s.rotationFrom(name), nil, "", time.Time{})
}

// rotationTrimTook closes the open compact or clear of the role once the trim showed: a compaction written after it, or a
// conversation other than the one it was typed into. A compact is the same conversation, resumed; a clear that started
// another is rebuilt on it, named by its id (Claude's new session, Codex's new thread), and not waited on its boot line.
func (s *Shift) rotationTrimTook(name, session string, compacted bool) {
	r, ok := s.openTrim(name)
	if !ok {
		return
	}

	changed := session != "" && session != r.From.Session

	switch {
	case r.Kind == KindClear && changed:
		s.rotationAppend(name, rotationLine{ID: r.ID, At: s.Now().UTC(), Phase: phaseEvidence, What: evidenceSession, Session: session})
		s.rotationClose(name, r, OutcomeRebuilt, "", []string{"new conversation " + session}, false, false)
	case r.Kind == KindClear:
		s.rotationClose(name, r, OutcomeResumed, "its conversation did not change", nil, false, false)
	case compacted:
		s.rotationClose(name, r, OutcomeResumed, "compacted: its conversation went on, summarised", nil, false, false)
	default:
		s.rotationClose(name, r, OutcomeResumed, "its conversation changed meanwhile: it is on "+session, nil, false, false)
	}
}

// rotationTrimUnseen closes the open compact or clear the trim gave up waiting for: the conversation is as it was.
func (s *Shift) rotationTrimUnseen(name string) {
	if r, ok := s.openTrim(name); ok {
		s.rotationClose(name, r, OutcomeResumed, "nothing changed: no compaction or new conversation seen "+trimPause.String()+
			" after it", nil, false, false)
	}
}

// reconcileTrim is the beat's last word on a compact or clear: the trim closes it from what it sees, and does so only for a
// role that is between turns, so a record that the trim never settled (the role went down, was never idle again) is closed
// once it is twice as old as the trim waits.
func (s *Shift) reconcileTrim(name string, r Rotation, now time.Time) string {
	if now.Sub(r.Prepared) < 2*trimPause {
		return ""
	}

	if s.rotationClose(name, r, OutcomeResumed, "nothing was seen of it: the role was not between turns for "+(2*trimPause).String(),
		nil, false, false) {
		return name + ": rotation " + r.ID + " closed, resumed (never seen)"
	}

	return ""
}

// RotationAbort is what an abort knows between its two holds of the lock: the rotation it goes back from, the conversation it
// goes back to, the one it leaves, and the down it began (none when the role was down already).
type RotationAbort struct {
	ID      string
	Session string
	Dir     string
	Claim   time.Time
	Begun   bool
	// Closed is that the rotation had closed (lost) already: the abort is then a line of evidence, not its closing.
	Closed bool
	// Left is the fresh conversation the abort leaves (empty when none is known), Return the command that comes back to it.
	Left, Return string

	rot Rotation
}

// abortFreshAge is how long a fresh conversation may have run before an abort of it is a loss of work that needs --force.
const abortFreshAge = 30 * time.Minute

// newestRotation is the role's newest rotation that is not a compact or a clear, from its whole record.
func (s *Shift) newestRotation(name string) (Rotation, bool, error) {
	rots, err := ReadRotations(s.Crew.Layout(), name)
	if err != nil {
		return Rotation{}, false, err
	}

	rots = slices.DeleteFunc(rots, Rotation.Trim)
	if len(rots) == 0 {
		return Rotation{}, false, nil
	}

	return rots[len(rots)-1], true, nil
}

// BeginAbort checks that a role can be put back on the conversation its newest rotation took it from, and puts it down. It
// refuses, naming why and what to do instead, when:
//   - the role has no rotation, or its newest one closed rebuilt or resumed (only an open or a lost one is aborted);
//   - the old conversation is not known, is of another runtime or home, is not under the namespace any more (zmina up --fresh
//     starts a new one), or the directory it ran in is gone;
//   - a down of the role is under way: the command or the restart that began it finishes it, never a second down of one role;
//   - the role runs the old conversation already;
//   - the fresh conversation shows work (it accepted a task of the rotation, the rotation was lost and then accepted late, or it
//     has run over abortFreshAge) and force is not set: the refusal names the conversation it would leave and the command that
//     comes back to it. The fresh conversation stays on disk; what the abort discards is its place in the role.
//
// The caller waits for the role to end (WaitGone) outside the lock and calls FinishAbort. The down it begins is saved as a
// pending abort, so that a command that dies during the wait leaves a role the ticker's stale-down finisher puts right (not
// held: a keep: always role is opened again).
func (s *Shift) BeginAbort(name string, o DownOptions, force bool) (RotationAbort, bool, error) {
	r, err := s.Role(name)
	if err != nil {
		return RotationAbort{}, false, err
	}

	rot, ok, err := s.newestRotation(name)
	if err != nil {
		return RotationAbort{}, false, err
	}

	if !ok {
		return RotationAbort{}, false, fmt.Errorf("%s: no rotation of it to abort (zmina rotation ls %s)", name, name)
	}

	if !rot.Open() && rot.Closed.Outcome != OutcomeLost {
		return RotationAbort{}, false, fmt.Errorf("%s: its newest rotation %s closed %s; only an open or a lost one is aborted "+
			"(zmina rotation show %s)", name, rot.ID, rot.Closed.Outcome, rot.ID)
	}

	if err := s.abortable(name, r.IsCodex(), rot); err != nil {
		return RotationAbort{}, false, err
	}

	if !s.State.Role(name).Downing.IsZero() {
		return RotationAbort{}, false, fmt.Errorf("%s: a down of it is under way (since %s): the command or the restart that began "+
			"it finishes it, a stale one the ticker does within a few minutes; abort again then", name,
			s.State.Role(name).Downing.Local().Format("15:04:05"))
	}

	live := s.session(name)
	if live != nil && live.SessionID == rot.From.Session {
		return RotationAbort{}, false, fmt.Errorf("%s: it runs its old conversation %s already", name, rot.From.Session)
	}

	plan := RotationAbort{ID: rot.ID, Session: rot.From.Session, Dir: rot.From.Dir, Closed: !rot.Open(), rot: rot}

	// The conversation left: the one the role runs now, else the one the rotation saw it start.
	switch {
	case live != nil && live.SessionID != "":
		plan.Left = live.SessionID
	default:
		plan.Left = rot.To.Session
	}

	if plan.Left != "" {
		plan.Return = s.resumeCommand(name, plan.Left, true)
	}

	if why := s.freshWork(rot, live != nil, s.Now()); why != "" && !force {
		msg := fmt.Sprintf("%s: its fresh conversation%s did work (%s): aborting leaves it, and the role is on its old conversation %s "+
			"instead; zmina rotation abort %s --force does that", name, namedSession(plan.Left), why, rot.From.Session, name)
		if plan.Return != "" {
			msg += "; this comes back to the fresh one afterwards: " + plan.Return
		}

		return RotationAbort{}, false, errors.New(msg)
	}

	wait, err := s.BeginDown(name, o)

	switch {
	case errors.Is(err, ErrNotUp):
		return plan, false, nil
	case err != nil:
		return RotationAbort{}, false, err
	}

	rs := s.State.Role(name)
	plan.Begun, plan.Claim = true, rs.Downing

	if !rs.Downing.IsZero() {
		rs.Pending = &PendingDown{Kind: pendingAbort, Claim: rs.Downing, Why: "rotation " + rot.ID + " abort", Notes: rot.From.Session,
			Dir: rot.From.Dir}
	}

	return plan, wait, nil
}

// namedSession is " <id>" for a conversation that is known, else nothing.
func namedSession(id string) string {
	if id == "" {
		return ""
	}

	return " " + id
}

// freshWork says what shows that the fresh conversation of a rotation did work that an abort would leave behind, empty when
// nothing does: a task of the rotation it accepted (also after the rotation was lost), or a run of more than abortFreshAge
// while the role is up.
func (s *Shift) freshWork(rot Rotation, up bool, now time.Time) string {
	for _, list := range [][]RotationEvidence{rot.Evidence, rot.Late} {
		if i := slices.IndexFunc(list, func(e RotationEvidence) bool { return e.What == EvidenceAccepted }); i >= 0 {
			return "it accepted " + list[i].Task
		}
	}

	if up && !rot.Started.IsZero() && now.Sub(rot.Started) > abortFreshAge {
		return "it has run for " + span(now.Sub(rot.Started))
	}

	return ""
}

// resumeCommand is the command that puts a role on a conversation: with the role running, down first.
func (s *Shift) resumeCommand(name, session string, running bool) string {
	crewArg := shellWord(s.Crew.Path)
	up := fmt.Sprintf("zmina --crew %s up %s --resume=%s", crewArg, shellWord(name), shellWord(session))

	if running {
		return fmt.Sprintf("zmina --crew %s down %s --clear && %s", crewArg, shellWord(name), up)
	}

	return up
}

// abortable checks what the conversation a rotation took the role from needs to be started again.
func (s *Shift) abortable(name string, codex bool, rot Rotation) error {
	from := rot.From

	if from.Session == "" {
		return fmt.Errorf("%s: no old conversation is recorded for rotation %s to go back to; zmina up %s --fresh starts a new one",
			name, rot.ID, name)
	}

	runtime, home := "", ""
	if r, err := s.Role(name); err == nil {
		runtime, home = s.runtimeScope(r)
	}

	if from.Runtime != "" && from.Runtime != runtime || from.Home != "" && from.Home != home {
		return fmt.Errorf("%s: its old conversation %s belongs to another runtime or home than its seat has now; zmina up %s --fresh "+
			"starts a new one", name, from.Session, name)
	}

	if !codex && s.HasTranscript != nil && !s.HasTranscript(s.Crew.Namespace, from.Session) {
		return fmt.Errorf("%s: its old conversation %s is gone (no transcript of it under %s: deleted, or recorded under another "+
			"CLAUDE_CONFIG_DIR); zmina up %s --fresh starts a new one", name, from.Session, s.Crew.Namespace, name)
	}

	if !dirExists(from.Dir) {
		return fmt.Errorf("%s: the directory its old conversation ran in is gone: %s; zmina up %s --fresh starts a new one", name,
			from.Dir, name)
	}

	return nil
}

// FinishAbort ends the down BeginAbort began, starts the role on its old conversation, and closes the rotation: resumed
// ("aborted by whoever ran it") when it was open, an evidence line "aborted" when it had closed lost. The restart is given up
// for the conversation (RestartGaveUp), as one whose fresh start failed is, so that it is not rotated again in it and
// context_max handles it. The hand-out of the rotation is taken back as restartRollback's way back takes it (undoHandOut):
// the lines that still wait in the queue go, and the deliveries it renewed are put back. A start that fails leaves the role
// down and held, and the rotation and its hand-out as they were.
func (s *Shift) FinishAbort(name string, a RotationAbort) error {
	if a.Begun {
		if rs := s.State.Role(name); !rs.Downing.Equal(a.Claim) {
			return fmt.Errorf("%s: %w", name, ErrDownTaken)
		}

		if err := s.FinishDown(name); err != nil {
			return err
		}
	}

	if err := s.UpWith(name, UpOptions{Resume: a.Session, Dir: a.Dir}); err != nil {
		return fmt.Errorf("%w; zmina up %s --fresh starts a new conversation", err, name)
	}

	rs := s.State.Role(name)
	rs.RestartGaveUp, rs.RestartOff = a.Session, s.Now()

	by := s.By
	if by == "" {
		by = "hand"
	}

	lines, tasks := s.undoHandOut(name, a.rot, a.Session)

	s.log(name, "rotation-abort", "", fmt.Sprintf("rotation %s aborted by %s: back on its old conversation %s; %d hand-out lines taken out, "+
		"%d deliveries put back", a.ID, by, a.Session, lines, tasks))

	if !a.Closed {
		s.rotationEnd(name, a.ID, OutcomeResumed, "aborted by "+by+": back on its old conversation "+a.Session, nil, false)
	}

	// Said in a line of its own for either, so that what looks at the rotation (retry, ls) knows it was aborted.
	s.rotationAppend(name, rotationLine{ID: a.ID, At: s.Now().UTC(), Phase: phaseEvidence, What: EvidenceAborted, By: by, Session: a.Session})

	return nil
}

// undoHandOut leaves the old conversation the way restartRollback's way back leaves it. The lines the rotation's hand-out left
// in the role's queue (its restart line, the packets of its tasks: whatever the fresh session was not shown, which the start
// just made gave back to the head of the queue) are taken out, each card closed as dropped. The deliveries the hand-out
// renewed are put back as the hand-out found them when the record kept that (RotationBefore); a record that did not (an older
// one) has the delivery re-bound to the old conversation instead, queued and not sent, not accepted: nothing is claimed of it,
// and it is not left bound to a session that is gone, which would be an acceptance alert about the wrong conversation. It
// returns how many lines it took out and deliveries it put right.
func (s *Shift) undoHandOut(name string, rot Rotation, old string) (lines, deliveries int) {
	rs := s.State.Role(name)
	tasks := rot.acceptTasks()
	why := "rotation " + rot.ID + " was aborted: the role is back on its old conversation"

	var out []string

	for _, l := range rs.Inbox {
		packet := slices.ContainsFunc(tasks, func(id string) bool { return strings.HasPrefix(l, "[zmina "+id+"] Read handoff packet ") })
		if packet || rot.hasLine() && strings.HasPrefix(l, rot.restartLineHead()) {
			out = append(out, l)
		}
	}

	s.takeOutLines(name, out, ClosedDropped, why)

	if s.Tasks == nil {
		return len(out), 0
	}

	for _, id := range tasks {
		t, err := s.Tasks.Get(id)
		if err != nil || t.Delivery == nil || t.Delivery.QueuedAt.Before(rot.Prepared) ||
			!strings.HasPrefix(t.Delivery.Line, "[zmina "+id+"] Read handoff packet ") {
			continue
		}

		if i := slices.IndexFunc(rot.Before, func(b RotationBefore) bool { return b.Task == id }); i >= 0 {
			t.Delivery = rot.Before[i].Delivery
		} else {
			d := *t.Delivery
			d.Session, d.SentAt, d.AcceptedAt = old, time.Time{}, time.Time{}
			t.Delivery = &d
		}

		if err := s.Tasks.Put(t); err != nil {
			if s.Warn != nil {
				s.Warn(err)
			}

			continue
		}

		deliveries++
	}

	return len(out), deliveries
}

// lineWhere says where a line the role was handed is on record, empty when it is nowhere: in its queue, typed to it and not
// landed, posted and not shown (alone or in a joined post or a doorbell), or among its failed lines.
func (s *Shift) lineWhere(name string, is func(line string) bool) string {
	rs := s.State.Role(name)
	has := func(line string, parts []Part) bool {
		return is(line) || slices.ContainsFunc(parts, func(p Part) bool { return is(p.Line) })
	}

	switch {
	case slices.ContainsFunc(rs.Inbox, is):
		return "its queue"
	case rs.Sent != nil && has(rs.Sent.Line, rs.Sent.Parts):
		return "what was typed to it and has not landed"
	case slices.ContainsFunc(rs.Posted, func(p Posted) bool { return has(p.Line, p.Parts) }):
		return "what was posted to it and is not shown"
	case slices.ContainsFunc(rs.Failed, func(f Failed) bool { return has(f.Line, f.Parts) }):
		return "its failed lines (zmina queue retry decides those)"
	}

	return ""
}

// RetryRotation hands a rotation's context to the session the role runs now: the packet of each task it handed that the role
// still holds, and the restart line. A line is handed only when it is nowhere on record (lineWhere) and the role has not
// accepted the task in this session; one that is, or that was shown, is left, and a failed line, which may have reached the
// role, is left to zmina queue retry. Only for an open rotation or one lost, of a role that is up with no down under way.
// It returns what it did and what it left, a line each.
func (s *Shift) RetryRotation(name string) ([]string, error) {
	if _, err := s.Role(name); err != nil {
		return nil, err
	}

	if s.Tasks == nil {
		return nil, fmt.Errorf("%s: no task ledger to read the tasks it holds from", name)
	}

	rot, ok, err := s.newestRotation(name)
	if err != nil {
		return nil, err
	}

	switch {
	case !ok:
		return nil, fmt.Errorf("%s: no rotation of it to retry (zmina rotation ls %s)", name, name)
	case rot.Aborted() != nil:
		return nil, fmt.Errorf("%s: rotation %s was aborted: the role was put back on its old conversation, which has its context "+
			"(zmina rotation show %s)", name, rot.ID, rot.ID)
	case !rot.Open() && rot.Closed.Outcome != OutcomeLost:
		return nil, fmt.Errorf("%s: its newest rotation %s closed %s; only an open or a lost one is retried (zmina rotation show %s)",
			name, rot.ID, rot.Closed.Outcome, rot.ID)
	case !rot.hasLine():
		return nil, fmt.Errorf("%s: rotation %s (%s) handed it nothing to hand again", name, rot.ID, rot.Kind)
	case !s.State.Role(name).Downing.IsZero():
		return nil, fmt.Errorf("%s: a down of it is under way; retry again once it ends", name)
	}

	// Up as the state and its pane say; the registry may not know its session yet (a role that is booting).
	if p, err := s.probe(name); err != nil || p.State != tmux.Up || !s.State.Role(name).Up {
		return nil, fmt.Errorf("%s: it is not up, there is no session to hand it to; zmina up %s --task <task> --fresh starts it on a "+
			"task, zmina rotation abort %s puts it back on its old conversation", name, name, name)
	}

	live := s.session(name)
	if live != nil && live.SessionID == rot.From.Session {
		return nil, fmt.Errorf("%s: it runs its old conversation %s, which has what the rotation took it from: there is nothing to hand "+
			"again to it", name, rot.From.Session)
	}

	var (
		report []string
		heads  []headLine
		packet []handedPacket
		held   []RotationArtifact
	)

	// A hand-out that cannot be made whole puts the deliveries it renewed back.
	fail := func(err error) ([]string, error) {
		s.restoreDeliveries(name, handout{packets: packet})

		return report, err
	}

	for _, a := range rot.acceptTasks() {
		t, err := s.Tasks.Get(a)

		if err != nil || !t.State.Active() || t.Role != name {
			report = append(report, a+": left, the role does not hold it any more")

			continue
		}

		// The restart line names what the role still holds, and no task it has lost.
		held = append(held, rotationArtifact(rot, a))

		if s.acceptedHere(t, live, rot) {
			report = append(report, a+": left, it accepted the task in this session")

			continue
		}

		_, _, line, err := s.packetFile(name, t)
		if err != nil {
			return fail(err)
		}

		if where := s.lineWhere(name, func(l string) bool { return l == line }); where != "" {
			report = append(report, a+": left, its packet is already in "+where)

			continue
		}

		h, err := s.prepareHand(name, t, true)
		if err != nil {
			return fail(err)
		}

		p := handedPacket{task: a, head: headLine{line: h.line, f: Facts{From: senderOf(s.By), Task: a}}, queued: h.queue, before: h.before}
		packet = append(packet, p)
		heads = append(heads, p.head)
		report = append(report, a+": its packet is queued again")
	}

	// Its restart line, naming only the tasks the role still holds; a rotation that held none and was not about a task done has
	// no line to hand.
	var earlier []string

	if done := doneOf(rot); done != "" || len(held) > 0 {
		line := restartLineFor(rot.Why, artifactNotes(held), done)
		if !s.restartLineHandled(name, rot, line) {
			earlier = s.restartLinesWaiting(name)
			heads = append([]headLine{{line: line, f: Facts{From: "zmina"}}}, heads...)
			report = append(report, "its restart line is queued again")
		}
	}

	rs := s.State.Role(name)

	if len(heads) == 0 {
		return report, nil
	}

	if len(rs.Inbox)-len(earlier)+len(heads) > inboxMax {
		return fail(fmt.Errorf("%s: %d lines already wait for it and the hand-out needs room for %d more; let it read them first",
			name, len(rs.Inbox), len(heads)))
	}

	// The restart line is never queued beside another: an earlier one that still waits is replaced.
	s.takeOutLines(name, earlier, ClosedDropped, "a newer restart line replaces it")
	s.enqueueHeadLines(name, heads...)

	for _, p := range packet {
		s.replacePackets(p.task, p.head.line)
	}

	by := s.By
	if by == "" {
		by = "hand"
	}

	s.rotationAppend(name, rotationLine{ID: rot.ID, At: s.Now().UTC(), Phase: phaseEvidence, What: EvidenceRetried, By: by})
	s.log(name, "rotation-retry", "", fmt.Sprintf("rotation %s: %d lines handed again by %s", rot.ID, len(heads), by))

	return report, nil
}

// acceptedHere reports whether the role accepted a task in the session it runs now: its delivery was accepted by that
// conversation, or, where the registry does not name the session, after the rotation started.
func (s *Shift) acceptedHere(t task.Task, live *registry.Session, r Rotation) bool {
	d := t.Delivery
	if d == nil || d.AcceptedAt.IsZero() {
		return false
	}

	if live != nil {
		return d.Session == live.SessionID
	}

	return !r.Started.IsZero() && !d.AcceptedAt.Before(r.Started)
}

// doneOf is the task whose end a restart_done rotation followed, empty for another kind.
func doneOf(r Rotation) string {
	if r.Kind != KindRestartDone || len(r.Artifacts) == 0 {
		return ""
	}

	return r.Artifacts[len(r.Artifacts)-1].Task
}

// artifactNotes names tasks and their checkpoints, as the restart line does.
func artifactNotes(arts []RotationArtifact) string {
	notes := make([]string, 0, len(arts))

	for _, a := range arts {
		notes = append(notes, a.Task+" "+noteOf(a))
	}

	return strings.Join(notes, ", ")
}

// rotationArtifact is the artifact of a task the rotation handed.
func rotationArtifact(r Rotation, id string) RotationArtifact {
	if i := slices.IndexFunc(r.Artifacts, func(a RotationArtifact) bool { return a.Task == id }); i >= 0 {
		return r.Artifacts[i]
	}

	return RotationArtifact{Task: id}
}

// restartLinesWaiting are the restart lines of earlier hand-outs that wait in the role's queue.
func (s *Shift) restartLinesWaiting(name string) []string {
	var out []string

	for _, l := range s.State.Role(name).Inbox {
		if strings.HasPrefix(l, restartLineHead) {
			out = append(out, l)
		}
	}

	return out
}

// restartLineHandled reports whether a rotation's restart line needs no hand-out again: it is on record (lineWhere), or it was
// shown to the session.
func (s *Shift) restartLineHandled(name string, r Rotation, line string) bool {
	if s.lineWhere(name, func(l string) bool { return l == line }) != "" {
		return true
	}

	_, shown := s.restartLineShown(name, r)

	return shown
}
