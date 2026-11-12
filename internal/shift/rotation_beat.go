package shift

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
)

// The shift's side of a rotation's record (rotation.go): the steps a restart writes as it goes (prepare, switching,
// started, the closing line of a start that failed), and the beat's reconcile, which closes a rotation when what it waits
// for has come or its time has run out. Every step is "append what the record does not show yet", so a beat run twice, or
// a process that dies between two steps, leaves the record as true as one run once; the closing line is written before the
// event and the note, so that a crash loses a note and never repeats one.

// rotationPrepare opens the record of a restart that is about to put the role down, once its note is durable: the
// conversation the role is on, and what the restart checked. It returns the rotation's id, empty when no record is kept
// (then the restart goes on as ever). A rotation still open for the role is closed lost, superseded by this one.
func (s *Shift) rotationPrepare(name string, tasks []task.Task, why, done string, asked time.Time) string {
	if !s.rotationsOn(name) {
		return ""
	}

	id := newRotationID()
	from := s.rotationFrom(name)

	if !s.rotationAppend(name, rotationLine{ID: id, At: s.Now().UTC(), Phase: PhasePrepared, Role: name,
		Kind: rotationKind(why, done), Why: why, From: &from, Artifacts: s.rotationArtifacts(tasks, done), AskedAt: asked.UTC()}) {
		return ""
	}

	for _, r := range s.rotationsOf(name) {
		if r.ID != id && r.Open() {
			s.rotationEnd(name, r.ID, OutcomeLost, "superseded by rotation "+id, nil, true)
		}
	}

	return id
}

// rotationFrom is the conversation the role is on, to come back to.
func (s *Shift) rotationFrom(name string) RotationFrom {
	rs := s.State.Role(name)
	from := RotationFrom{Dir: rs.Dir}

	if r, err := s.Role(name); err == nil {
		from.Runtime, from.Home = s.runtimeScope(r)
	}

	if sess := s.session(name); sess != nil && sess.SessionID != "" {
		from.Session = sess.SessionID

		if turn, err := s.registryFor(name).Last(sess.SessionID); err == nil {
			from.Context = turn.Context
		}
	}

	if from.Session == "" {
		from.Session = rs.LastSession
	}

	return from
}

// rotationArtifacts are what the restart checked durable: each held task's checkpoint, or for a restart after a task was
// done that task's result.
func (s *Shift) rotationArtifacts(tasks []task.Task, done string) []RotationArtifact {
	out := make([]RotationArtifact, 0, len(tasks)+1)

	for _, t := range tasks {
		a := RotationArtifact{Task: t.ID}
		if t.Checkpoint != nil {
			a.Checkpoint = shortRev(t.Checkpoint.Revision)
		}

		out = append(out, a)
	}

	if done != "" {
		a := RotationArtifact{Task: done}

		if s.Tasks != nil {
			if t, err := s.Tasks.Get(done); err == nil && t.Result != nil {
				a.Result = shortRev(t.Result.Revision)
			}
		}

		out = append(out, a)
	}

	return out
}

// rotationSwitching notes that the role is being put down: the claim is the Downing its restart's job carries, which is
// how the restart finds its rotation again, from the job or from the role's saved pending down.
func (s *Shift) rotationSwitching(name, id string, claim time.Time) {
	if id != "" {
		s.rotationAppend(name, rotationLine{ID: id, At: s.Now().UTC(), Phase: PhaseSwitching, Claim: claim.UTC()})
	}
}

// rotationNotDown closes the record of a restart whose down could not be begun: the role is on its conversation as it was,
// unless the down went far enough to show (a role that is no longer up, with a down begun): that one is left to the down
// that never finishes (finishStale), as every half-made down is.
func (s *Shift) rotationNotDown(name, id string, rs *RoleState, err error) {
	if rs.Up {
		s.rotationEnd(name, id, OutcomeResumed, "not put down: "+err.Error(), nil, false)

		return
	}

	if !rs.Downing.IsZero() {
		s.rotationSwitching(name, id, rs.Downing)
	}
}

// rotationOf is the id of the open rotation whose down carries the claim, empty when there is none.
func (s *Shift) rotationOf(name string, claim time.Time) string {
	rots := s.rotationsOf(name)

	for i := len(rots) - 1; i >= 0; i-- {
		if rots[i].Open() && !rots[i].Claim.IsZero() && rots[i].Claim.Equal(claim) {
			return rots[i].ID
		}
	}

	return ""
}

// rotationStarted notes that the fresh session runs and its lines are in the queue.
func (s *Shift) rotationStarted(name, id string, lines []string) {
	if id != "" {
		s.rotationAppend(name, rotationLine{ID: id, At: s.Now().UTC(), Phase: PhaseStarted, Lines: clipLines(lines)})
	}
}

// rotationEnd closes a rotation once: the closing line first, then the event, then (for a rotation lost, where note says
// the dispatcher is to be told) the note. It reports whether it closed it: not when it is closed already, or the record
// cannot be written (told once, and the next beat tries again).
func (s *Shift) rotationEnd(name, id, outcome, detail string, confirmed []string, note bool) bool {
	if id == "" {
		return false
	}

	r, ok := s.rotationByID(name, id)
	if !ok || !r.Open() {
		return false
	}

	line := rotationLine{ID: id, At: s.Now().UTC(), Phase: PhaseClosed, Outcome: outcome, Detail: detail, Confirmed: confirmed}
	if outcome == OutcomeLost {
		line.Back = s.rotationBack(name, r.From)
	}

	if !s.rotationAppend(name, line) {
		return false
	}

	how := detail
	if len(confirmed) > 0 {
		how = strings.Join(confirmed, ", ")
	}

	s.log(name, "rotation", r.logTask(), fmt.Sprintf("%s %s: %s; %s", outcome, id, r.Why, how))

	if note && outcome == OutcomeLost {
		text := fmt.Sprintf("%s: rotation %s (%s) lost: %s; zmina --crew %s rotation show %s", name, id, r.Kind, detail,
			shellWord(s.Crew.Path), shellWord(name))
		if line.Back != "" {
			text += "; back on its old conversation: " + line.Back
		}

		s.addNotes([]string{text})
	}

	return true
}

// rotationBack is the command that puts a role back on the conversation a rotation took it from, empty when none is known:
// with the fresh session still up it is put down first.
func (s *Shift) rotationBack(name string, from RotationFrom) string {
	if from.Session == "" {
		return ""
	}

	crewArg := shellWord(s.Crew.Path)
	up := fmt.Sprintf("zmina --crew %s up %s --resume=%s", crewArg, shellWord(name), shellWord(from.Session))

	if s.State.Role(name).Up {
		return fmt.Sprintf("zmina --crew %s down %s --clear && %s", crewArg, shellWord(name), up)
	}

	return up
}

// rotationTail is what a note of a restart that failed says of its rotation: where to look.
func rotationTail(id string) string {
	if id == "" {
		return ""
	}

	return "; rotation " + id
}

// reconcileRotations follows every role's open rotation, once a beat, after the roles were looked at: it closes a rotation
// whose evidence has come or whose time has run out, and notes what the new conversation did that the beat can see. It
// returns lines for the log.
func (s *Shift) reconcileRotations(now time.Time) []string {
	var did []string

	if s.Crew == nil {
		return nil
	}

	for _, name := range s.Names() {
		rots := s.rotationsOf(name)
		if len(rots) == 0 {
			continue
		}

		r := rots[len(rots)-1]
		if !r.Open() {
			continue
		}

		var line string

		switch r.Phase {
		case PhaseStarted:
			line = s.reconcileStarted(name, r, now)
		default:
			line = s.reconcileUnstarted(name, r, now)
		}

		if line != "" {
			did = append(did, line)
		}
	}

	return did
}

// reconcileUnstarted follows a rotation whose role was not started yet by its restart: prepared (the role is to be put
// down) or switching (it is, and the restart's job finishes it). The job, or the beat that finds the down stale, owns a
// down that is still its own; the rest is told as what the state shows.
func (s *Shift) reconcileUnstarted(name string, r Rotation, now time.Time) string {
	rs := s.State.Role(name)

	// The down is under way and its restart (this beat's job, or the one that finds it stale) finishes it.
	if !r.Claim.IsZero() && (rs.Downing.Equal(r.Claim) || rs.Pending != nil && rs.Pending.Claim.Equal(r.Claim)) {
		return ""
	}

	// Started since the down began (at its claim or after, a start being a later moment than the down it follows), or, with
	// no down recorded, since the rotation was prepared.
	startedSince := rs.Started.After(r.Prepared)
	if !r.Claim.IsZero() {
		startedSince = !rs.Started.Before(r.Claim)
	}

	switch {
	case rs.Up && !startedSince:
		// The role was never put down: the restart died before it, or its down was not kept.
		if s.rotationEnd(name, r.ID, OutcomeResumed, "not begun: the role was not put down, it is on its conversation", nil, false) {
			return name + ": rotation " + r.ID + " closed, resumed (not begun)"
		}
	case rs.Up:
		// Started after the down began, by the restart whose process died before it noted so, or by another command.
		// What the fresh session got is told by what it does (reconcileStarted), as for any start.
		s.rotationAppend(name, rotationLine{ID: r.ID, At: rs.Started.UTC(), Phase: PhaseStarted})

		return name + ": rotation " + r.ID + " started, seen by the beat"
	case !r.Claim.IsZero() && now.Sub(r.Claim) < staleDown, r.Claim.IsZero() && now.Sub(r.Prepared) < staleDown:
		// A command may still be finishing the down.
		return ""
	default:
		if s.rotationEnd(name, r.ID, OutcomeLost, "no session: the role was put down and none was started", nil, true) {
			return name + ": rotation " + r.ID + " closed, lost (no session)"
		}
	}

	return ""
}

// reconcileStarted follows a rotation whose fresh session runs: the evidence that it got its context is collected (what
// the beat can see is noted as it is seen), and the rotation closes rebuilt when it is all there, lost when the session is
// gone, changed or silent past its time, and not at all while the role is busy: a role at work is not timed out.
func (s *Shift) reconcileStarted(name string, r Rotation, now time.Time) string {
	rs := s.State.Role(name)
	sess := s.session(name)

	// The conversation the fresh session runs, once it has one that is not the old one's.
	changed := false

	if sess != nil && sess.SessionID != "" && sess.SessionID != r.From.Session {
		switch {
		case r.To.Session == "":
			if s.rotationAppend(name, rotationLine{ID: r.ID, At: now.UTC(), Phase: phaseEvidence, What: evidenceSession,
				Session: sess.SessionID}) {
				r.To.Session = sess.SessionID
			}
		case sess.SessionID != r.To.Session:
			changed = true
		}
	}

	// What it did that the beat can see, kept in the record. Not of a conversation that is not the one started.
	if !changed {
		s.rotationNote(name, &r, sess)
	}

	held := s.rotationHeld(name, r)

	if s.rotationConfirmed(r, held) {
		confirmed := s.rotationConfirms(r, held)
		if s.rotationEnd(name, r.ID, OutcomeRebuilt, "", confirmed, false) {
			return name + ": rotation " + r.ID + " closed, rebuilt: " + strings.Join(confirmed, ", ")
		}

		return ""
	}

	var why string

	switch {
	case !rs.Up:
		why = "its session ended before it confirmed" + s.rotationMissing(r, held)
	case rs.Started.After(r.Started):
		why = "it was started again before it confirmed" + s.rotationMissing(r, held)
	case changed:
		why = "its conversation changed before it confirmed" + s.rotationMissing(r, held)
	default:
		why = s.rotationTimedOut(name, r, held, sess, now)
	}

	if why != "" {
		if s.rotationEnd(name, r.ID, OutcomeLost, why, nil, true) {
			return name + ": rotation " + r.ID + " closed, lost: " + why
		}

		return ""
	}

	// Still open and not lost: the dispatcher is told once if it stays so.
	if now.Sub(r.Started) >= rotationSlow && r.SlowTold.IsZero() {
		if s.rotationAppend(name, rotationLine{ID: r.ID, At: now.UTC(), Phase: phaseEvidence, What: evidenceSlow}) {
			s.addNotes([]string{fmt.Sprintf("%s: rotation %s (%s) is open %s after its start, not confirmed yet%s; zmina --crew %s "+
				"rotation show %s", name, r.ID, r.Kind, span(now.Sub(r.Started)), s.rotationMissing(r, held), shellWord(s.Crew.Path),
				shellWord(name))})

			return name + ": rotation " + r.ID + " still open, told the dispatcher"
		}
	}

	return ""
}

// rotationNote records what the beat can see of the fresh conversation that the record does not show yet: its boot turn
// over, the restart's line shown, a task's packet accepted. Each is recorded once, with the time it happened.
func (s *Shift) rotationNote(name string, r *Rotation, sess *registry.Session) {
	add := func(what, task string, at time.Time) {
		if r.has(what, task) {
			return
		}

		if s.rotationAppend(name, rotationLine{ID: r.ID, At: at.UTC(), Phase: phaseEvidence, What: what, Task: task, By: name,
			Session: r.To.Session}) {
			r.Evidence = append(r.Evidence, RotationEvidence{What: what, Task: task, By: name, Session: r.To.Session, At: at})
		}
	}

	if at, ok := s.bootSeen(name, sess); ok {
		add(EvidenceBoot, "", at)
	}

	if at, ok := s.restartLineShown(name, *r); ok {
		add(EvidenceShown, "", at)
	}

	for _, id := range r.acceptTasks() {
		if at, ok := s.packetAccepted(*r, id); ok {
			add(EvidenceAccepted, id, at)
		}
	}
}

// bootSeen is when the boot turn of this start was seen over: a turn of the conversation ended at or after the start. Not the
// boot_grace that lets a line go to a role that was not seen to boot (booted): that is no evidence.
func (s *Shift) bootSeen(name string, sess *registry.Session) (time.Time, bool) {
	rs := s.State.Role(name)

	if !rs.Booted.IsZero() && !rs.Booted.Before(rs.Started) {
		return rs.Booted, true
	}

	if sess == nil {
		return time.Time{}, false
	}

	turn, err := s.registryFor(name).Last(sess.SessionID)
	if err != nil || !turn.Ended || turn.At.IsZero() || turn.At.Before(rs.Started) {
		return time.Time{}, false
	}

	return turn.At, true
}

// restartLineShown is when the line the rotation's restart queued first was seen in the fresh conversation, by its
// card: seen by the conversation or the Codex bridge, or typed and landed. A line that went in a doorbell is seen only
// when its text was fetched. Cards older than the rotation are another restart's.
func (s *Shift) restartLineShown(name string, r Rotation) (time.Time, bool) {
	rc := s.cardsOf(name)
	if rc == nil {
		return time.Time{}, false
	}

	head := restartLineHead + r.Why + "."

	for _, c := range rc.cards {
		if c.From != "zmina" || c.Task != "" || c.Written.IsZero() || c.Written.Before(r.Prepared) || !strings.HasPrefix(c.Head, head) {
			continue
		}

		switch {
		case c.Seen != nil:
			return c.Seen.At, true
		case c.Bell == nil && c.Closed != nil && c.Closed.How == ClosedLanded:
			return c.Closed.At, true
		}
	}

	return time.Time{}, false
}

// packetAccepted is when the fresh session accepted a task's packet: the ledger has the delivery accepted after the
// rotation's start (the packet of a restart is renewed when it is handed, so an acceptance of an older one cannot be).
func (s *Shift) packetAccepted(r Rotation, id string) (time.Time, bool) {
	if s.Tasks == nil {
		return time.Time{}, false
	}

	t, err := s.Tasks.Get(id)
	if err != nil || t.Delivery == nil || t.Delivery.AcceptedAt.IsZero() || t.Delivery.AcceptedAt.Before(r.Started) {
		return time.Time{}, false
	}

	return t.Delivery.AcceptedAt, true
}

// rotationHeld are the tasks of the rotation the role still holds: a task closed, parked or given to another role since
// cannot be accepted by it, and is not waited for.
func (s *Shift) rotationHeld(name string, r Rotation) []string {
	var held []string

	for _, id := range r.acceptTasks() {
		if s.Tasks == nil {
			break
		}

		if t, err := s.Tasks.Get(id); err == nil && t.State.Active() && t.Role == name {
			held = append(held, id)
		}
	}

	return held
}

// rotationConfirmed reports whether the fresh conversation got its context: the boot turn is over, and every task it still
// holds has its packet accepted; a role with no task to accept (a restart after a task was done, or tasks gone since) was
// shown the line that tells it why it was restarted. A note fetched is reported when there is one, never waited for: the
// role may read the record file itself, and a lost rotation would then be a claim about its choice of tool.
func (s *Shift) rotationConfirmed(r Rotation, held []string) bool {
	if !r.has(EvidenceBoot, "") {
		return false
	}

	if len(held) == 0 {
		return r.has(EvidenceShown, "")
	}

	for _, id := range held {
		if !r.has(EvidenceAccepted, id) {
			return false
		}
	}

	return true
}

// rotationConfirms names what confirmed a rotation, for its closing line and its event.
func (s *Shift) rotationConfirms(r Rotation, held []string) []string {
	out := []string{"boot"}

	if r.has(EvidenceHookStart, "") {
		out = append(out, "session hook ran")
	}

	if r.has(EvidenceShown, "") {
		out = append(out, "restart line shown")
	}

	for _, id := range held {
		out = append(out, id+" packet accepted")

		if r.has(EvidenceNoteFetched, id) {
			out = append(out, id+" note fetched")
		}
	}

	return out
}

// rotationMissing says what a rotation waits for, for a note: "; no boot turn yet", "; T-1 not accepted".
func (s *Shift) rotationMissing(r Rotation, held []string) string {
	var missing []string

	if !r.has(EvidenceBoot, "") {
		missing = append(missing, "its boot turn is not over")
	}

	for _, id := range held {
		if !r.has(EvidenceAccepted, id) {
			missing = append(missing, id+" is not accepted")
		}
	}

	if len(held) == 0 && !r.has(EvidenceShown, "") {
		missing = append(missing, "its restart line was not shown")
	}

	if len(missing) == 0 {
		return ""
	}

	return " (" + strings.Join(missing, ", ") + ")"
}

// rotationTimedOut is why a rotation that is not confirmed is lost for want of time, empty while it may still be:
//   - no boot turn within boot_grace of the start, the session not busy (a role at work on its boot turn is working), the API
//     error of its last turn named when the hook recorded one;
//   - booted, its context shown (the restart line, which says why, for a role with no task to accept), the role idle
//     rotationAcceptWait without accepting what it was handed, or, with no task to accept, without having been shown why.
func (s *Shift) rotationTimedOut(name string, r Rotation, held []string, sess *registry.Session, now time.Time) string {
	rs := s.State.Role(name)
	busy := sess != nil && sess.Status == "busy"

	boot := r.evidenceOf(EvidenceBoot, "")
	if boot == nil {
		if busy || now.Sub(rs.Started) < s.Crew.BootGraceD() {
			return ""
		}

		why := "its boot turn never finished"
		if f := s.failureOf(name, sess); f != nil && f.Error != "" {
			why += "; its last turn ended on an API error: " + f.Error
		}

		return why
	}

	shown := r.evidenceOf(EvidenceShown, "")
	if sess == nil || sess.Status != "idle" || s.quietFor(name, sess, now) < rotationAcceptWait {
		return ""
	}

	since := boot.At
	if shown != nil && shown.At.After(since) {
		since = shown.At
	}

	if now.Sub(since) < rotationAcceptWait {
		return ""
	}

	switch {
	case len(held) == 0 && shown == nil:
		return "its restart line was never shown to it"
	case len(held) > 0 && shown != nil:
		missing := slices.DeleteFunc(slices.Clone(held), func(id string) bool { return r.has(EvidenceAccepted, id) })

		return fmt.Sprintf("it did not accept %s within %s idle after its context was shown", strings.Join(missing, ", "),
			rotationAcceptWait)
	}

	return ""
}
