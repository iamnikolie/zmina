package shift

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/handoff"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
)

const (
	// restartColdFloor is the least context restart_idle restarts: below it a cold wake rewrites little.
	restartColdFloor = 100_000
	// restartNoteAhead is how long before restart_idle the note is asked for, while the conversation's cache is
	// still warm: the note's turn then reads the conversation from cache instead of writing it again.
	restartNoteAhead = 10 * time.Minute
	// restartNoteWait is how long a role asked for its note has to write it; past it the restart is given up for
	// that conversation, and context_max trims it as before.
	restartNoteWait = 10 * time.Minute
	// restartSettle is how long a role sits idle before restart_at restarts it, so a reply being read is not cut.
	// A conversation its Stop hook flagged at a turn's end (RestartDue) waits restartDueSettle instead: that turn is
	// known to be over.
	restartSettle = trimIdle
	// restartDueSettle is how long a restart for restart_at waits after the role's last turn ended with no new turn
	// begun: one full beat and a half. A role that ended its turn waiting for a peer's direct reply (a developer and
	// a reviewer over SendMessage) has that reply in flight; put down at the very next beat, the reply would go to a
	// dead socket. Far under restartSettle, which a flagged conversation no longer waits.
	restartDueSettle = 90 * time.Second
	// restartEarly is the share of restart_at (4/5) at which a role is asked for its note ahead of the restart.
	restartEarlyNum, restartEarlyDen = 4, 5
	// restartReset is the share of restart_at (1/2) a conversation has to fall below, after a compaction or a trim,
	// for the early ask to be made again on its next climb: well under restartEarly, so a size that hovers around the
	// mark asks once.
	restartResetNum, restartResetDen = 1, 2
	// restartOwnerQuiet is how long after the owner's last line to a role it is left alone: he may be mid-talk.
	restartOwnerQuiet = 15 * time.Minute
	// restartDoneGrace is how long a role whose task is done sits before restart_done restarts it, so a quick
	// follow-up on the task still lands in the conversation that did it.
	restartDoneGrace = 10 * time.Minute
)

// Restart is a restart the tick began: the role's down is under way (Claim is its Downing), and FinishRestart starts
// it again fresh with Opts, its Talk line and each task's packet. Done is the task whose end it follows
// (restart_done), when the role holds none.
type Restart struct {
	Name  string
	Claim time.Time
	Opts  UpOptions
	Tasks []string
	Why   string
	Notes string
	Done  string
}

// activityReader is a registry that reads a conversation's background work and the owner's last line (Claude's;
// a Codex bridge's thread has neither to read).
type activityReader interface {
	Activity(sessionID string) (registry.Activity, error)
}

// restarts reports whether the ticker restarts the role for its context: restart_at, restart_idle or restart_done is
// on for it and it is not the dispatcher (its recovery pack and context_max do that job).
func (s *Shift) restarts(r *crew.Role) bool {
	return (r.RestartContext() > 0 || r.RestartAfter() > 0 || r.RestartDoneAt() > 0) && r.Name != s.Crew.Dispatcher
}

// restart restarts fresh a role that holds a task and whose conversation grew past restart_at, or that has sat idle
// close to restart_idle with over 100k in it, so it goes on from its own note instead of carrying the whole
// conversation (or waking on a cold cache). Only between turns, by the plain idle test (see plainIdle). The note is
// each task's checkpoint: one saved since the last turn began is the note already; else the role is asked for it
// and restarted once every one landed. A new task, a draft, the owner's line or no note within restartNoteWait calls
// the restart off. A turn's end (begin false) asks for the note and leaves the down to the beat, which finishes it
// outside the lock. It returns a line for the log.
func (s *Shift) restart(name string, r *crew.Role, now time.Time, tasks []string, begin bool) string {
	rs := s.State.Role(name)
	asked := !rs.RestartAsked.IsZero()

	if !s.restarts(r) || !rs.Up || !rs.Downing.IsZero() || !rs.Releasing.IsZero() {
		if asked {
			return s.restartOff(name, "it is no longer restarted or is being put down")
		}

		return ""
	}

	sess := s.session(name)
	if sess == nil {
		return ""
	}

	if !begin && (asked || len(tasks) == 0) {
		return ""
	}

	if asked {
		return s.restartAsked(name, now, tasks, sess)
	}

	if len(tasks) == 0 {
		return s.restartDone(name, r, now, sess)
	}

	if rs.RestartGaveUp == sess.SessionID || now.Sub(rs.RestartOff) < trimPause {
		return ""
	}

	turn, err := s.registryFor(name).Last(sess.SessionID)
	if err != nil || turn.Context == 0 {
		return ""
	}

	idle := now.Sub(quietSince(sess, turn))
	// Flagged at a turn's end and still between turns by its transcript: the settle has nothing left to guard.
	due := rs.RestartDue == sess.SessionID && turn.Ended

	var why string

	switch {
	case r.RestartContext() > 0 && turn.Context >= r.RestartContext() && (due || idle >= restartSettle):
		why = fmt.Sprintf("context %s over restart_at %s", Tokens(turn.Context), Tokens(r.RestartContext()))
	case r.RestartAfter() > 0 && turn.Context > restartColdFloor && idle >= r.RestartAfter()-restartNoteAhead:
		why = fmt.Sprintf("idle %s at %s, near restart_idle %s", idleFor(idle), Tokens(turn.Context),
			idleFor(r.RestartAfter()))
	default:
		return ""
	}

	act, ok := s.plainIdle(name, now, sess)
	if !ok {
		return s.restartHeld(name, act.Background, tasks)
	}

	mine := s.heldTasks(name)
	if s.notesLanded(mine, act.Prompt) {
		// Not put down at the very beat after a turn's end: a reply in flight to the role would be lost.
		if !begin || idle < restartDueSettle {
			return ""
		}

		return s.beginRestart(name, mine, why, "")
	}

	request, err := s.restartRequest(name, mine, restartLine)
	if err != nil {
		return name + ": restart: note request: " + err.Error()
	}

	s.enqueue(name, request, Facts{From: "zmina"})
	rs.RestartAsked, rs.RestartSession, rs.RestartTasks, rs.RestartWhy = now, sess.SessionID, tasks, why
	s.log(name, "restart-asked", strings.Join(tasks, ","), why)

	return fmt.Sprintf("%s: %s; asked for its note to restart fresh", name, why)
}

// restartAtTurnEnd is the restart gate at a turn's end (the Stop hook of a role on its own channel), where a busy
// role is caught between turns: the beat alone sees it only once it has sat idle restartSettle. A role holding a task
// whose turn ended at or over restart_at is flagged (RestartDue) and, when no current note is there, asked for it
// at once; the first beat after it has settled restartDueSettle puts it down. One at restartEarly of restart_at is
// asked for its note once a conversation, so the note is there when it crosses. Every guard of the beat's gate holds
// here too (restart, plainIdle).
func (s *Shift) restartAtTurnEnd(name string, sess *registry.Session, now time.Time) string {
	r, err := s.Role(name)
	if err != nil {
		return ""
	}

	if !s.restarts(r) {
		return ""
	}

	// The line that began this turn is settled now, as a beat or the delivery after this would settle it: one the
	// conversation showed must not read as still unseen (plainIdle) until the next beat — the note's request too,
	// once the note's turn is over.
	s.followPosted(name, now)

	rs := s.State.Role(name)
	s.restartRearm(name, rs, sess, r.RestartContext())

	if r.RestartContext() == 0 || !rs.RestartAsked.IsZero() || !rs.Downing.IsZero() || !rs.Releasing.IsZero() ||
		rs.RestartGaveUp == sess.SessionID {
		return ""
	}

	tasks := s.held()[name]
	if len(tasks) == 0 {
		return ""
	}

	turn, err := s.registryFor(name).Last(sess.SessionID)
	if err != nil || !turn.Ended {
		return ""
	}

	at := r.RestartContext()
	if turn.Context >= at {
		if rs.RestartDue != sess.SessionID {
			rs.RestartDue = sess.SessionID
			s.log(name, "restart-due", strings.Join(tasks, ","),
				fmt.Sprintf("context %s over restart_at %s at a turn's end", Tokens(turn.Context), Tokens(at)))
		}

		return s.restart(name, r, now, tasks, false)
	}

	if turn.Context < at/restartEarlyDen*restartEarlyNum || rs.RestartNoted == sess.SessionID ||
		now.Sub(rs.RestartOff) < trimPause {
		return ""
	}

	act, ok := s.plainIdle(name, now, sess)
	if !ok {
		return ""
	}

	mine := s.heldTasks(name)
	if s.notesLanded(mine, act.Prompt) {
		// It saved its note in this very turn: nothing to ask for.
		rs.RestartNoted = sess.SessionID

		return ""
	}

	request, err := s.restartRequest(name, mine, fmt.Sprintf(earlyLine, Tokens(turn.Context), Tokens(at)))
	if err != nil {
		return name + ": restart: note request: " + err.Error()
	}

	why := fmt.Sprintf("context %s near restart_at %s", Tokens(turn.Context), Tokens(at))
	s.enqueue(name, request, Facts{From: "zmina"})
	rs.RestartNoted = sess.SessionID
	s.log(name, "restart-note", strings.Join(tasks, ","), why)

	return fmt.Sprintf("%s: %s; asked for its note ahead of the restart", name, why)
}

// quietSince is when the role last went quiet: the later of its status's change and its last turn's end.
func quietSince(sess *registry.Session, turn registry.Turn) time.Time {
	since := sess.StatusSince()
	if turn.At.After(since) {
		since = turn.At
	}

	return since
}

// quietFor is how long the role has sat between turns (quietSince); a turn that cannot be read counts by status alone.
func (s *Shift) quietFor(name string, sess *registry.Session, now time.Time) time.Duration {
	turn, _ := s.registryFor(name).Last(sess.SessionID)

	return now.Sub(quietSince(sess, turn))
}

// restartRearm forgets what a conversation was asked or flagged for (RestartNoted, RestartDue) once a turn ends with
// its context well under restart_at (restartReset): a compaction or a trim dropped it, and its next climb is a new
// one, asked for its note again at restartEarly, once. A size not known (0: right after a compaction, before a turn
// shows it) forgets nothing, and one that only dips toward restartReset asks no second time.
func (s *Shift) restartRearm(name string, rs *RoleState, sess *registry.Session, at int) {
	if at == 0 || (rs.RestartNoted == "" && rs.RestartDue == "") {
		return
	}

	turn, err := s.registryFor(name).Last(sess.SessionID)
	if err != nil || !turn.Ended || turn.Context <= 0 || turn.Context >= at/restartResetDen*restartResetNum {
		return
	}

	rs.RestartNoted, rs.RestartDue = "", ""
}

// restartAsked follows a role asked for its note: restarted once every task's note landed and it is plainly idle,
// called off when anything else came first.
func (s *Shift) restartAsked(name string, now time.Time, tasks []string, sess *registry.Session) string {
	rs := s.State.Role(name)

	if sess.SessionID != rs.RestartSession {
		return s.restartOff(name, "its conversation changed")
	}

	for _, id := range tasks {
		if !slices.Contains(rs.RestartTasks, id) {
			return s.restartOff(name, "it got "+id)
		}
	}

	if len(tasks) == 0 {
		// Its last task was closed or parked after the ask. No pause follows: nothing is asked of a role holding no
		// task, and restart_done is to apply to the close without waiting trimPause out.
		off := rs.RestartOff
		line := s.restartOff(name, "it holds no task")
		rs.RestartOff = off

		return line
	}

	if draft, _, ok, err := s.Tmux.Input(name); err == nil && ok && draft != "" {
		return s.restartOff(name, "a draft in its input line")
	}

	if act := s.activity(name, sess); act.Owner.After(rs.RestartAsked) || s.lastOwnerChat(name).After(rs.RestartAsked) {
		return s.restartOff(name, "the owner wrote to it")
	}

	mine := s.heldTasks(name)

	landed := s.notesLanded(mine, rs.RestartAsked)
	if landed {
		act, ok := s.plainIdle(name, now, sess)
		if ok {
			// The note's turn just ended: the role settles restartDueSettle before it goes down, as a flagged one does.
			// It is quiet or busy again within a beat; the wait below bounds it either way.
			if s.quietFor(name, sess, now) < restartDueSettle {
				return ""
			}

			return s.beginRestart(name, mine, rs.RestartWhy, "")
		}

		if line := s.restartHeld(name, act.Background, tasks); line != "" {
			return line
		}
	}

	if now.Sub(rs.RestartAsked) < restartNoteWait {
		return ""
	}

	// The note came and the role went on working (a line it got as the note's turn ended): called off, not given
	// up. It wrote what was asked; its next turn's end over restart_at asks again once the pause is over.
	if landed {
		return s.restartOff(name, "its note is there, but it was not between turns within "+restartNoteWait.String())
	}

	rs.RestartGaveUp = sess.SessionID
	line := s.restartOff(name, "no note within "+restartNoteWait.String())
	s.addNotes([]string{name + ": wrote no note within " + restartNoteWait.String() + " of the restart ask; not " +
		"restarted in this conversation (context_max trims it)"})

	return line
}

// restartDone restarts fresh a role that holds no task, whose own task was closed done during its current run and
// whose conversation is at restart_done or past it, once it has sat restartDoneGrace since that close and since its
// last turn, plainly idle. No note is asked for: the task's result is in the ledger. Only the ledger tells: a role
// that closed no task of its own in this run (one the owner talks to directly) is never restarted by it, whatever
// its conversation says. A task reopened or handed meanwhile makes it hold one again, and this no longer applies;
// after the restart its start is later than the close, so it restarts once a task. It returns a line for the log.
func (s *Shift) restartDone(name string, r *crew.Role, now time.Time, sess *registry.Session) string {
	rs := s.State.Role(name)
	if r.RestartDoneAt() == 0 || rs.Started.IsZero() || now.Sub(rs.RestartOff) < trimPause {
		return ""
	}

	// The context first: the ledger is read only for a role that has grown past restart_done.
	turn, err := s.registryFor(name).Last(sess.SessionID)
	if err != nil || turn.Context < r.RestartDoneAt() {
		return ""
	}

	done, closed, ok := s.lastDone(name, rs.Started, now)
	if !ok {
		return ""
	}

	since := closed
	for _, at := range []time.Time{sess.StatusSince(), turn.At} {
		if at.After(since) {
			since = at
		}
	}

	if now.Sub(since) < restartDoneGrace {
		return ""
	}

	act, ok := s.plainIdle(name, now, sess)
	if !ok {
		return s.restartHeld(name, act.Background, []string{done})
	}

	// Any line the role got after the close (a peer's message, the owner's line or Chat, zmina's line, a background
	// report: each begins a turn; one still queued or unseen fails plainIdle) means it went on with other work, a
	// review loop paused mid-way perhaps: its conversation is that work's now, and context_max bounds it.
	if act.Prompt.After(closed) || act.Owner.After(closed) || s.lastOwnerChat(name).After(closed) {
		return ""
	}

	why := fmt.Sprintf("task done: %s, context %s over restart_done %s", done, Tokens(turn.Context),
		Tokens(r.RestartDoneAt()))

	return s.beginRestart(name, nil, why, done)
}

// lastDone is the role's task closed done most recently, and when, if it was closed after a time (the role's start).
// The ledger is read once a beat (now), for every role at once.
func (s *Shift) lastDone(name string, after, now time.Time) (string, time.Time, bool) {
	if s.Tasks == nil {
		return "", time.Time{}, false
	}

	if s.dones == nil || !s.doneAt.Equal(now) {
		all, err := s.Tasks.All()
		if err != nil {
			return "", time.Time{}, false
		}

		s.dones, s.doneAt = map[string]task.Task{}, now

		for _, t := range all {
			if t.State != task.Done {
				continue
			}

			if prev, ok := s.dones[t.Role]; !ok || closedAt(t).After(closedAt(prev)) {
				s.dones[t.Role] = t
			}
		}
	}

	t, ok := s.dones[name]
	if !ok {
		return "", time.Time{}, false
	}

	at := closedAt(t)

	return t.ID, at, at.After(after)
}

// closedAt is when a task was closed: its closure's time, else its last update (a zmina that kept no closure).
func closedAt(t task.Task) time.Time {
	if t.Closed != nil && !t.Closed.At.IsZero() {
		return t.Closed.At
	}

	return t.Updated
}

// restartHeld says once, in the log, which background work holds a role's restart off: a launch whose end was never
// reported holds it for good, and the role would sit at its context with nothing said.
func (s *Shift) restartHeld(name string, background, tasks []string) string {
	rs := s.State.Role(name)
	ids := strings.Join(background, ",")

	if ids == "" || ids == rs.RestartHeld {
		return ""
	}

	rs.RestartHeld = ids
	s.log(name, "restart-held", strings.Join(tasks, ","), "background "+ids+" running")

	return name + ": restart held: background " + ids + " running"
}

// restartOff ends an ask for the note; another may come after the usual pause.
func (s *Shift) restartOff(name, why string) string {
	rs := s.State.Role(name)
	tasks := strings.Join(rs.RestartTasks, ",")
	rs.RestartAsked, rs.RestartSession, rs.RestartTasks, rs.RestartWhy = time.Time{}, "", nil, ""
	rs.RestartOff = s.Now()
	s.log(name, "restart-off", tasks, why)

	return name + ": restart called off, " + why
}

// plainIdle reports whether a role may be put down between turns without losing anything: up, booted and idle (no
// dialog), nothing queued or posted and unseen, its input line on screen and empty, no terminal attached, no
// background work it started still running (a background shell, a Monitor, a background agent: a restart would
// end them), and no line of the owner's in the last restartOwnerQuiet (he may be mid-talk with it).
func (s *Shift) plainIdle(name string, now time.Time, sess *registry.Session) (registry.Activity, bool) {
	rs := s.State.Role(name)

	if len(rs.Inbox) > 0 || len(rs.Posted) > 0 || !s.ready(name, now) || !s.landed(name, now) {
		return registry.Activity{}, false
	}

	if draft, _, ok, err := s.Tmux.Input(name); err != nil || !ok || draft != "" {
		return registry.Activity{}, false
	}

	if attached, err := s.Tmux.Attached(name); err != nil || attached {
		return registry.Activity{}, false
	}

	act := s.activity(name, sess)
	if len(act.Background) > 0 {
		return act, false
	}

	owner := act.Owner
	if chat := s.lastOwnerChat(name); chat.After(owner) {
		owner = chat
	}

	return act, now.Sub(owner) >= restartOwnerQuiet
}

// activity reads what the role's conversation says of background work and the owner's last line; nothing for a
// registry that cannot tell.
func (s *Shift) activity(name string, sess *registry.Session) registry.Activity {
	if a, ok := s.registryFor(name).(activityReader); ok {
		act, err := a.Activity(sess.SessionID)
		if err == nil {
			return act
		}
	}

	return registry.Activity{}
}

// lastOwnerChat is when the owner's newest Chat message to the role was accepted.
func (s *Shift) lastOwnerChat(name string) time.Time {
	var at time.Time

	for _, r := range s.State.Role(name).ChatReceipts {
		if r.At.After(at) {
			at = r.At
		}
	}

	return at
}

// heldTasks are the active tasks the role holds.
func (s *Shift) heldTasks(name string) []task.Task {
	var mine []task.Task

	if s.Tasks == nil {
		return nil
	}

	for _, t := range s.Tasks.Active() {
		if t.Role == name {
			mine = append(mine, t)
		}
	}

	return mine
}

// notesLanded reports whether every task has a current checkpoint saved at or after a time.
func (s *Shift) notesLanded(tasks []task.Task, after time.Time) bool {
	if len(tasks) == 0 || after.IsZero() {
		return false
	}

	for _, t := range tasks {
		if !s.checkpointCurrent(t) || t.Checkpoint.At.Before(after) {
			return false
		}
	}

	return true
}

const (
	// restartLine asks for the note the restart waits for; earlyLine asks for it ahead, at restartEarly of restart_at
	// (the context, then restart_at). Each ends with the request file's path.
	restartLine = "[zmina] restart: your conversation has grown; zmina restarts you fresh from your own note. " +
		"Read %q, write each checkpoint it asks for, then stop."
	earlyLine = "[zmina] restart ahead: your conversation is at %s; past restart_at %s zmina restarts you fresh " +
		"from your own note, between turns. Read %%q and write each checkpoint it asks for now, then stop; no " +
		"restart follows yet, your next line is work as usual. Save the checkpoint again at the end of any later " +
		"turn that moved the work: a note saved in your last turn restarts you with no extra turn."
)

// restartRequest is the line (restartLine, earlyLine) that asks the role for its note: a short locator to one file
// of role-shaped checkpoint requests, never multiline guidance through the input line. It starts with "[zmina]", a
// line of zmina's own, never one the owner typed.
func (s *Shift) restartRequest(name string, tasks []task.Task, line string) (string, error) {
	packets := make([]handoff.Packet, 0, len(tasks))
	for _, t := range tasks {
		p, err := s.TaskPacket(name, t)
		if err != nil {
			return "", err
		}

		p.Instructions = fmt.Sprintf("Before the restart, write zmina --crew %s task checkpoint %s --file <json-file>: "+
			"the note your fresh self starts from. Do not park the task.\n", shellWord(s.Crew.Path), t.ID) +
			p.Shape.Request("checkpoint")
		packets = append(packets, p)
	}

	path, err := handoff.Restart(s.Crew.StateDir(), name, packets)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(line, path), nil
}

// beginRestart puts the role down to start it again fresh (FinishRestart, once it ended): in the worktree of its
// first task that has one, else where it runs now. A role restarted after its task is done (done names it) starts
// in its own dir: the task's worktree may be removed once it is merged.
func (s *Shift) beginRestart(name string, tasks []task.Task, why, done string) string {
	rs := s.State.Role(name)

	dir := rs.Dir
	if done != "" {
		dir = ""
	}

	ids := make([]string, 0, len(tasks))
	notes := make([]string, 0, len(tasks))

	for i := len(tasks) - 1; i >= 0; i-- {
		if tasks[i].Worktree != "" {
			dir = tasks[i].Worktree
		}
	}

	for _, t := range tasks {
		ids = append(ids, t.ID)
		notes = append(notes, t.ID+" "+shortRev(t.Checkpoint.Revision))
	}

	rs.RestartAsked, rs.RestartSession, rs.RestartTasks, rs.RestartWhy = time.Time{}, "", nil, ""
	rs.RestartHeld, rs.RestartDue = "", ""

	logTask := strings.Join(ids, ",")
	if done != "" {
		logTask = done
	}

	// The restart hands out a line and a packet per task at the head of the queue, and a line that waits is never
	// dropped to make room: with no room it is not begun, before the old session is put down. (plainIdle lets only an
	// empty queue through, so this holds a role whose tasks alone fill the queue; lines that come while it is down are
	// put behind the hand-out whatever their number, FinishRestart.)
	if need := 1 + len(ids); len(rs.Inbox)+need > inboxMax {
		why := fmt.Sprintf("%d lines already wait for it and its restart needs room for %d more (a line, a packet "+
			"per task); let it read them first", len(rs.Inbox), need)
		rs.RestartOff = s.Now()
		s.log(name, "restart-failed", logTask, why)

		return name + ": restart: " + why
	}

	if _, err := s.BeginDown(name, DownOptions{}); err != nil {
		rs.RestartOff = s.Now()
		s.log(name, "restart-failed", logTask, err.Error())

		return name + ": restart: " + err.Error()
	}

	job := Restart{Name: name, Claim: rs.Downing, Opts: UpOptions{Fresh: true, Dir: dir}, Tasks: ids, Why: why,
		Notes: strings.Join(notes, ", "), Done: done}
	s.Restarts = append(s.Restarts, job)
	rs.Pending = pendingOf(job) // saved with Downing: a beat that dies during the wait is finished by the next one

	if done != "" {
		return fmt.Sprintf("%s: %s; restarting fresh", name, why)
	}

	return fmt.Sprintf("%s: %s; restarting fresh with its note (%s)", name, why, strings.Join(notes, ", "))
}

// FinishRestart ends the down beginRestart began and starts the role fresh, with first a line its Talk shows (why its
// conversation was reset), then each task's packet, its note in it, at the head of its queue, ahead of whatever waited
// while it was down. They are queued before the start, so that the start's own save of the state (it has the session
// running, Started and no Pending) holds them too: a process that dies after the start finds the role up with its
// context in its queue, never up with only its boot. A start that fails takes them out again and goes back to the
// conversation the role was on (restartRollback). Nothing when another command took the role over meanwhile.
func (s *Shift) FinishRestart(job Restart) string {
	rs := s.State.Role(job.Name)
	if !rs.Downing.Equal(job.Claim) {
		// Started since: a later beat finished this restart from its saved job (finishStale), or a command did.
		if rs.Pending == nil && rs.Downing.IsZero() && rs.Started.After(job.Claim) {
			s.log(job.Name, "restart-superseded", job.logTask(), "a later beat or a command started it meanwhile")

			return job.Name + ": restart: a later beat or a command started it meanwhile"
		}

		s.log(job.Name, "restart-failed", job.logTask(), "another command took it over meanwhile")

		return job.Name + ": restart: another command took it over meanwhile"
	}

	if err := s.FinishDown(job.Name); err != nil {
		return s.restartFailed(job, err)
	}

	// The conversation it was on, to go back to if the fresh start fails: what a start changes comes after it.
	back := oldRun{session: rs.LastSession, dir: rs.Dir}

	// What the session that is gone left posted or typed is settled before the hand-out goes in: a line it never
	// showed goes back to the head of the queue (start would do it, after the hand-out, and put it ahead of it).
	s.settleEnded(job.Name, s.Now())

	out, err := s.handOut(job)
	if err == nil {
		err = s.UpWith(job.Name, job.Opts)
	}

	if err != nil {
		return s.restartRollback(job, back, out, err)
	}

	// The older packets of these tasks that waited in a queue are out now that the role runs on the new ones: before the
	// start they stay, for a start that fails goes back to the conversation they were for.
	for _, p := range out.packets {
		s.replacePackets(p.task, p.head.line)
	}

	if job.Done != "" {
		s.log(job.Name, "restarted", job.Done, job.Why)

		return fmt.Sprintf("%s: restarted fresh (%s)", job.Name, job.Why)
	}

	s.log(job.Name, "restarted", strings.Join(job.Tasks, ","), job.Why+"; note "+job.Notes)

	return fmt.Sprintf("%s: restarted fresh (%s); note %s", job.Name, job.Why, job.Notes)
}

// oldRun is the conversation a role was on, and the directory it ran in, when its restart began.
type oldRun struct{ session, dir string }

// handout is what a restart queued for the fresh session: its line (empty until it is queued) and, for each task, the
// packet's line and the task's delivery as it was before.
type handout struct {
	line    string
	packets []handedPacket
}

// handedPacket is one task's packet in a handout. queued: its line went into the queue with the restart's, as opposed to
// one that waited there already.
type handedPacket struct {
	task   string
	head   headLine
	queued bool
	before *task.Delivery
}

// lines are the lines the restart queued, in queue order.
func (h handout) lines() []string {
	if h.line == "" {
		return nil
	}

	lines := []string{h.line}
	for _, p := range h.packets {
		if p.queued {
			lines = append(lines, p.head.line)
		}
	}

	return lines
}

// handOut queues a restart's lines at the head of the role's queue, in order: the line its Talk shows, then each task's
// packet (HandTask's, in the order the restart holds the tasks). A packet whose line waits in the queue already is left
// where it is, not queued twice. It returns what it did also when it fails: the deliveries of the tasks before the one
// that failed are renewed, and nothing is queued (the lines go in last).
func (s *Shift) handOut(job Restart) (handout, error) {
	var out handout

	line := fmt.Sprintf("[zmina] restarted fresh: %s. Your note: %s, in each task's packet that follows.", job.Why,
		job.Notes)
	if job.Done != "" {
		line = fmt.Sprintf("[zmina] restarted fresh: %s. You hold no task; what it ended with: zmina task show %s.",
			job.Why, job.Done)
	}

	heads := []headLine{{line: line, f: Facts{From: "zmina"}}}

	for _, id := range job.Tasks {
		t, err := s.Tasks.Get(id)
		if err != nil {
			return out, err
		}

		h, err := s.prepareHand(job.Name, t, true)
		if err != nil {
			return out, err
		}

		p := handedPacket{task: id, head: headLine{line: h.line, f: Facts{From: senderOf(s.By), Task: id}},
			queued: h.queue, before: h.before}
		out.packets = append(out.packets, p)

		if h.queue {
			heads = append(heads, p.head)
		}
	}

	s.enqueueHeadLines(job.Name, heads...)
	out.line = line

	return out, nil
}

// restartRollback answers a fresh start that failed (or a hand-out that could not be made): what was queued for the
// fresh session is taken out again (each line's card closed as dropped), and the role is started once more on the
// conversation it was on.
//
// That works: the restart is given up for that conversation as a called-off ask is (RestartGaveUp, RestartOff: it is not
// rotated again in it, and context_max handles it as before), and the dispatcher hears it once.
//
// That fails too (also when the directory it ran in is gone: no pane is started in another place): the role stays down
// and is not held, so that a role with keep: always is opened again by the ticker as after a crash, the restart is given
// up for the conversation as above, and the dispatcher hears it once. The packets stay queued (see below), the restart's own line does
// not: "restarted fresh" is true only of a fresh session, and the next start may be a resume. A packet is a pointer to
// a task that the role still holds, and is retired by the beat (pruneTaskInbox) once the ledger says it is not its
// task any more; a session that starts without it has no context at all, which is the failure the hand-out is for.
func (s *Shift) restartRollback(job Restart, back oldRun, out handout, cause error) string {
	name := job.Name
	rs := s.State.Role(name)
	s.log(name, "restart-failed", job.logTask(), cause.Error())

	// The conversation it goes back to has what the hand-out would give: it is not queued behind it.
	s.takeOutLines(name, out.lines(), ClosedDropped, "the fresh start of the restart failed")

	var err error

	switch {
	case back.session == "":
		err = errors.New("no conversation of its own is known to go back to")
	case !dirExists(back.dir):
		// A task's worktree may be gone by now: the conversation is found by the directory it ran in, and a pane started
		// in another place is not it.
		err = fmt.Errorf("the directory it ran in is gone: %s", back.dir)
	default:
		err = s.UpWith(name, UpOptions{Resume: back.session, Dir: back.dir})
	}

	what := "restart for its context failed"
	if job.Done != "" {
		what = "restart after " + job.Done + " was done failed"
	}

	if err == nil {
		s.restoreDeliveries(name, out)
		rs.RestartGaveUp = back.session
		s.restartOff(name, "its fresh start failed; it is back on its conversation")
		s.addNotes([]string{fmt.Sprintf("%s: %s: %v; it is back on its conversation and is not restarted again in it "+
			"(context_max trims it)", name, what, cause)})

		return name + ": restart: " + cause.Error() + "; back on its conversation"
	}

	rs.Held = false

	// Given up for the conversation it was on as a successful way back is: a role opened again on it (resume: always,
	// or by hand) is not asked for its note and put down again, a note for the dispatcher at each round, until someone
	// acts. A fresh conversation is another one.
	rs.RestartGaveUp, rs.RestartOff = back.session, s.Now()

	// The note below already says which tasks it holds and how to start it: the ticker's hint for a down role holding
	// a task it has not taken (unstarted) would be the same news again, the hand-out having left the delivery unaccepted.
	for _, id := range job.Tasks {
		if !slices.Contains(rs.Unstarted, id) {
			rs.Unstarted = append(rs.Unstarted, id)
		}
	}

	// A copy of a role is not a seat: nothing keeps it down or its queue (forget, below).
	if out.line != "" && !isInstance(name) {
		var kept []headLine

		for _, p := range out.packets {
			if p.queued {
				kept = append(kept, p.head)
			}
		}

		s.enqueueHeadLines(name, kept...)
	}

	s.restoreDeliveries(name, out)
	s.log(name, "restart-failed", job.logTask(), "back on its conversation failed too: "+err.Error())

	next := fmt.Sprintf("zmina --crew %s up %s --fresh", shellWord(s.Crew.Path), shellWord(name))
	if len(job.Tasks) > 0 {
		next = fmt.Sprintf("zmina --crew %s up %s --task %s --fresh", shellWord(s.Crew.Path), shellWord(name), job.Tasks[0])
	}

	s.addNotes([]string{fmt.Sprintf("%s: %s: %v; starting it on its conversation failed too: %v; it is down and not "+
		"held (the ticker opens a keep: always role again); by hand: %s", name, what, cause, err, next)})

	s.forget(name)

	return name + ": restart: " + cause.Error() + "; back on its conversation: " + err.Error()
}

// dirExists reports whether a directory a role ran in is still there; no directory (the role's own, which a start
// checks itself) is.
func dirExists(dir string) bool {
	if dir == "" {
		return true
	}

	info, err := os.Stat(dir)

	return err == nil && info.IsDir()
}

// restoreDeliveries puts the deliveries a restart renewed back as the hand-out found them, for each packet whose line is
// not in the role's queue: a restart that ended with the role on its old conversation did not hand the task over, and
// that conversation's own delivery (accepted, perhaps) is the one that is still true. A restart that a later beat
// finished, its first run having died after the hand-out, finds the renewed deliveries and keeps those. A packet whose
// line waits is left with the delivery it has: the line is the one that delivery is about.
func (s *Shift) restoreDeliveries(name string, out handout) {
	rs := s.State.Role(name)

	for _, p := range out.packets {
		if slices.Contains(rs.Inbox, p.head.line) {
			continue
		}

		t, err := s.Tasks.Get(p.task)
		if err != nil || t.Delivery == nil || t.Delivery.Line != p.head.line {
			continue
		}

		t.Delivery = p.before
		if err := s.Tasks.Put(t); err != nil && s.Warn != nil {
			s.Warn(err)
		}
	}
}

// logTask is the task the log names for the restart: the tasks it holds, else the one whose end it follows.
func (job Restart) logTask() string {
	if job.Done != "" {
		return job.Done
	}

	return strings.Join(job.Tasks, ",")
}

func (s *Shift) restartFailed(job Restart, err error) string {
	s.log(job.Name, "restart-failed", job.logTask(), err.Error())

	if job.Done != "" {
		s.addNotes([]string{fmt.Sprintf("%s: restart after %s was done failed: %v — zmina --crew %s up %s --fresh",
			job.Name, job.Done, err, shellWord(s.Crew.Path), shellWord(job.Name))})

		return job.Name + ": restart: " + err.Error()
	}

	tasks := strings.Join(job.Tasks, ", ")
	first, _, _ := strings.Cut(tasks, ",")
	s.addNotes([]string{fmt.Sprintf("%s: restart for its context failed: %v; it holds %s — zmina --crew %s up %s "+
		"--task %s --fresh", job.Name, err, tasks, shellWord(s.Crew.Path), shellWord(job.Name), first)})

	return job.Name + ": restart: " + err.Error()
}

// shortRev is a checkpoint revision as people read it.
func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}

	return rev
}
