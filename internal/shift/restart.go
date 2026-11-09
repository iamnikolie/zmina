package shift

import (
	"fmt"
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
	restartSettle = trimIdle
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
// the restart off. It returns a line for the log.
func (s *Shift) restart(name string, r *crew.Role, now time.Time, tasks []string) string {
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

	since := sess.StatusSince()
	if turn.At.After(since) {
		since = turn.At
	}

	idle := now.Sub(since)

	var why string

	switch {
	case r.RestartContext() > 0 && turn.Context >= r.RestartContext() && idle >= restartSettle:
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
		return s.beginRestart(name, mine, why, "")
	}

	request, err := s.restartRequest(name, mine)
	if err != nil {
		return name + ": restart: note request: " + err.Error()
	}

	rs.Inbox = append(rs.Inbox, request)
	rs.RestartAsked, rs.RestartSession, rs.RestartTasks, rs.RestartWhy = now, sess.SessionID, tasks, why
	s.log(name, "restart-asked", strings.Join(tasks, ","), why)

	return fmt.Sprintf("%s: %s; asked for its note to restart fresh", name, why)
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
		return s.restartOff(name, "it holds no task")
	}

	if draft, _, ok, err := s.Tmux.Input(name); err == nil && ok && draft != "" {
		return s.restartOff(name, "a draft in its input line")
	}

	if act := s.activity(name, sess); act.Owner.After(rs.RestartAsked) || s.lastOwnerChat(name).After(rs.RestartAsked) {
		return s.restartOff(name, "the owner wrote to it")
	}

	mine := s.heldTasks(name)
	if s.notesLanded(mine, rs.RestartAsked) {
		act, ok := s.plainIdle(name, now, sess)
		if ok {
			return s.beginRestart(name, mine, rs.RestartWhy, "")
		}

		if line := s.restartHeld(name, act.Background, tasks); line != "" {
			return line
		}
	}

	if now.Sub(rs.RestartAsked) < restartNoteWait {
		return ""
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

// restartRequest is the line that asks the role for its note: a short locator to one file of role-shaped
// checkpoint requests, never multiline guidance through the input line. It starts with "[zmina]", a line of
// zmina's own, never one the owner typed.
func (s *Shift) restartRequest(name string, tasks []task.Task) (string, error) {
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

	return fmt.Sprintf("[zmina] restart: your conversation has grown; zmina restarts you fresh from your own note. "+
		"Read %q, write each checkpoint it asks for, then stop.", path), nil
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
	rs.RestartHeld = ""

	logTask := strings.Join(ids, ",")
	if done != "" {
		logTask = done
	}

	if _, err := s.BeginDown(name, DownOptions{}); err != nil {
		rs.RestartOff = s.Now()
		s.log(name, "restart-failed", logTask, err.Error())

		return name + ": restart: " + err.Error()
	}

	s.Restarts = append(s.Restarts, Restart{Name: name, Claim: rs.Downing, Opts: UpOptions{Fresh: true, Dir: dir},
		Tasks: ids, Why: why, Notes: strings.Join(notes, ", "), Done: done})

	if done != "" {
		return fmt.Sprintf("%s: %s; restarting fresh", name, why)
	}

	return fmt.Sprintf("%s: %s; restarting fresh with its note (%s)", name, why, strings.Join(notes, ", "))
}

// FinishRestart ends the down beginRestart began and starts the role fresh: first a line its Talk shows (why its
// conversation was reset), then each task's packet, its note in it. Nothing when another command took the role
// over meanwhile.
func (s *Shift) FinishRestart(job Restart) string {
	rs := s.State.Role(job.Name)
	if !rs.Downing.Equal(job.Claim) {
		s.log(job.Name, "restart-failed", job.logTask(), "another command took it over meanwhile")

		return job.Name + ": restart: another command took it over meanwhile"
	}

	if err := s.FinishDown(job.Name); err != nil {
		return s.restartFailed(job, err)
	}

	if err := s.UpWith(job.Name, job.Opts); err != nil {
		return s.restartFailed(job, err)
	}

	line := fmt.Sprintf("[zmina] restarted fresh: %s. Your note: %s, in each task's packet that follows.", job.Why,
		job.Notes)
	if job.Done != "" {
		line = fmt.Sprintf("[zmina] restarted fresh: %s. You hold no task; what it ended with: zmina task show %s.",
			job.Why, job.Done)
	}

	if err := s.Queue(job.Name, line); err != nil {
		return s.restartFailed(job, err)
	}

	for _, id := range job.Tasks {
		t, err := s.Tasks.Get(id)
		if err != nil {
			return s.restartFailed(job, err)
		}

		if _, err := s.HandTask(job.Name, t); err != nil {
			return s.restartFailed(job, err)
		}
	}

	if job.Done != "" {
		s.log(job.Name, "restarted", job.Done, job.Why)

		return fmt.Sprintf("%s: restarted fresh (%s)", job.Name, job.Why)
	}

	s.log(job.Name, "restarted", strings.Join(job.Tasks, ","), job.Why+"; note "+job.Notes)

	return fmt.Sprintf("%s: restarted fresh (%s); note %s", job.Name, job.Why, job.Notes)
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
