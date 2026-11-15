package shift

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/codex"
	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/tmux"
)

const (
	// reconcileBeatMax is the most seats, the dispatcher apart, the beat starts when the crew file says reconcile: restart: after a
	// reboot every seat that holds a task is down at once, and each start is a boot, a model call. The seats left wait for the next beat.
	reconcileBeatMax = 2
	// reconcileWaitStep is how many times the pause between two starts of a seat doubles at most: reopenPause << 3 is 40 minutes.
	reconcileWaitStep = 3
	// reconcileRestartCap is how many times in reconcileRestartWindow the ticker starts again a seat that is wanted up only for the task
	// it holds before it gives it up as broken. A seat that dies after more than ten minutes each time is no quick end, and the pause
	// of a failed start does not bound it: it would be started again for ever without ever finishing what it holds.
	reconcileRestartCap    = 6
	reconcileRestartWindow = 24 * time.Hour
	// restartPrefix opens the RecordNotified key of each such start: restartPrefix + seat + "/" + unix seconds.
	restartPrefix = "reconcile-restart/"
	// keepAlwaysWhy is what wantedUp says of a seat that is wanted up because the crew file says keep: always.
	keepAlwaysWhy = "keep: always"
)

// sessionLister is what a Tmux may offer beyond the interface: the sessions of the server, which no role names are found among.
// It is optional so that the fakes of Tmux need not have it; a Tmux without it has the ticker see no orphans.
type sessionLister interface {
	Sessions() ([]tmux.SessionInfo, error)
}

// wantedUp says why the ticker should have the seat name up, "" when it should not: a keep: always seat always is; with the crew
// file's reconcile: restart a seat that holds a task of its own to work on is too. That is an open task (not one waiting for the owner or
// another, parked or closed) of a seat that is no copy (a copy is gone once down: its task is not reconciled), is not asleep (its
// alarm wakes it) and is not a temporary seat past its expiry. It is the first of the ticker's reasons to start a seat; the rest is
// what stands in the way (the guards at the start in Tick, and StartBlock for the commands).
func (s *Shift) wantedUp(name string, r *crew.Role, now time.Time) string {
	if r.Keep == crew.KeepAlways {
		return keepAlwaysWhy
	}

	if !s.Crew.Reconcile.Restart || s.Tasks == nil || isInstance(name) {
		return ""
	}

	if !s.Asleep(name).IsZero() {
		return ""
	}

	if at := r.ExpiresAt(); !at.IsZero() && !now.Before(at) {
		return ""
	}

	var ids []string

	for _, t := range s.Tasks.Active() {
		if t.Role == name && t.State == task.Open {
			ids = append(ids, t.ID)
		}
	}

	if len(ids) == 0 {
		return ""
	}

	return "holds " + strings.Join(ids, ", ")
}

// restartWait is how long after its last start, or its last try, the ticker waits before it starts the seat again: reopenPause, and
// with reconcile: restart doubled for each start that failed or died on boot in a row, up to eight times (40 minutes).
func (s *Shift) restartWait(rs *RoleState) time.Duration {
	if !s.Crew.Reconcile.Restart {
		return reopenPause
	}

	return reopenPause << min(rs.QuickEnds, reconcileWaitStep)
}

// failedStart counts a start that failed with reconcile: restart on: it is a try like a quick end, and tries of them in a row mark a
// seat broken: not started again until zmina up, one note. The dispatcher is never marked broken, since a crew with no dispatcher is
// worse than one that keeps trying: it is told once, when it is up to hear, and the pause between tries grows meanwhile.
func (s *Shift) failedStart(name string, rs *RoleState, err error) []string {
	if !s.Crew.Reconcile.Restart {
		return nil
	}

	rs.QuickEnds++
	tries := s.Crew.Reconcile.MaxTries()
	why := strings.TrimPrefix(clipRunes(oneLine(err.Error()), startReasonMax), name+": ")

	switch {
	case name != s.Crew.Dispatcher && rs.QuickEnds >= tries && !rs.Broken:
		rs.Broken = true
		note := fmt.Sprintf("%s: could not be started %d times (last: %s); not tried again until zmina up %s", name, rs.QuickEnds, why, name)
		s.log(name, "broken", "", note)

		return []string{note}
	case name == s.Crew.Dispatcher && rs.QuickEnds == tries:
		note := fmt.Sprintf("%s: could not be started %d times (last: %s); tried again, the pause between tries grows", name, rs.QuickEnds, why)
		s.log(name, "start-failing", "", note)

		return []string{note}
	}

	return nil
}

// reopen starts the seat name, which wantedUp said (why) should be up and which is down, unless something stands in the way: a session
// of its name outside zmina (the ticker never takes one it did not start; with reconcile: restart the dispatcher is told once), its
// last start being too recent (restartWait), its plan being held, and, with reconcile: restart, a down begun or a release under way, and
// reconcileBeatMax seats started this beat (restarts counts them, the dispatcher apart). The seat starts in the directory it ran in
// when that is still there (a task's worktree), and the tasks it holds are handed again as after a crash (rotationReopened). A start that
// fails is a try (failedStart). It returns whether the seat is up now, lines for the log, and notes for the dispatcher.
func (s *Shift) reopen(name string, r *crew.Role, rs *RoleState, why string, now time.Time, restarts *int) (up bool, did, notes []string) {
	rc := s.Crew.Reconcile.Restart

	if other := s.registryFor(name).LiveByName(r.Session); other != nil {
		if rc {
			s.tellOutside(name, r, why, other)
		}

		return false, nil, nil
	}

	if now.Sub(rs.Started) < s.restartWait(rs) || s.planHeld(name, now) {
		return false, nil, nil
	}

	if rc && (!rs.Downing.IsZero() || !rs.Releasing.IsZero() || name != s.Crew.Dispatcher && *restarts >= reconcileBeatMax) {
		return false, nil, nil
	}

	// A seat wanted up for its task alone is started again only so many times in a day: past that it is broken, with one note.
	if rc && why != keepAlwaysWhy {
		if n := s.restartsSince(name, now); n >= reconcileRestartCap {
			rs.Broken = true
			note := fmt.Sprintf("%s: started again %d times in %s and still %s, so it is not making progress; not started again until zmina up %s",
				name, n, "24h", why, name)
			s.log(name, "broken", "", note)

			return false, nil, []string{note}
		}

		s.State.MarkRestart(name, now)
	}

	// The conversation it was on, read before the start: the record of the reopen names it, and a fresh start hands the
	// tasks the role holds.
	from, tasks := s.rotationFrom(name), s.heldTasks(name)

	opts := UpOptions{}
	if rc && rs.Dir != "" && dirExists(rs.Dir) {
		opts.Dir = rs.Dir
	}

	if rc && name != s.Crew.Dispatcher {
		*restarts++
	}

	if err := s.start(name, opts); err != nil {
		// Wait the usual pause before the next try, so a role that cannot start does not fill the log.
		rs.Started = now

		return false, []string{"start " + name + ": " + err.Error()}, s.failedStart(name, rs, err)
	}

	s.rotationReopened(name, from, tasks)

	did = []string{"started " + name}

	if name != s.Crew.Dispatcher {
		// What zmina did, told once: an info line.
		note := name + ": started again (" + why + ")"
		s.markQuiet(note, now)
		notes = append(notes, note)
	}

	return true, did, notes
}

// restartKey is the RecordNotified key of a start of the seat at a time.
func restartKey(name string, at time.Time) string {
	return restartPrefix + name + "/" + strconv.FormatInt(at.Unix(), 10)
}

// MarkRestart records a start of the seat by the beat, as a key of RecordNotified (no field of its own): restartsSince counts them.
func (st *State) MarkRestart(name string, at time.Time) {
	if st.RecordNotified == nil {
		st.RecordNotified = map[string]bool{}
	}

	st.RecordNotified[restartKey(name, at)] = true
}

// restartsSince is how many starts of the seat the beat made in the last reconcileRestartWindow.
func (s *Shift) restartsSince(name string, now time.Time) int {
	n := 0

	for key := range s.State.RecordNotified {
		rest, ok := strings.CutPrefix(key, restartPrefix+name+"/")
		if !ok {
			continue
		}

		if at, err := strconv.ParseInt(rest, 10, 64); err == nil && now.Sub(time.Unix(at, 0)) < reconcileRestartWindow {
			n++
		}
	}

	return n
}

// sweepRestarts drops the records of starts that are past the window, of every seat.
func (s *Shift) sweepRestarts(now time.Time) {
	for key := range s.State.RecordNotified {
		rest, ok := strings.CutPrefix(key, restartPrefix)
		if !ok {
			continue
		}

		stamp := rest[strings.LastIndex(rest, "/")+1:]
		if at, err := strconv.ParseInt(stamp, 10, 64); err != nil || now.Sub(time.Unix(at, 0)) >= reconcileRestartWindow {
			delete(s.State.RecordNotified, key)
		}
	}
}

// forgetRestarts drops the records of starts of the seat: a hand's up starts it afresh.
func (st *State) forgetRestarts(name string) {
	for key := range st.RecordNotified {
		if strings.HasPrefix(key, restartPrefix+name+"/") {
			delete(st.RecordNotified, key)
		}
	}
}

// outsideKey is the RecordNotified key of the note that a seat which should be up is kept down by a session outside zmina.
func outsideKey(name string) string { return "reconcile-outside/" + name }

// tellOutside tells the dispatcher once that a session named like the seat already runs outside zmina, and so the seat that should be
// up is not started: the ticker never takes a session it did not start. The note is told again only after that session has been gone.
func (s *Shift) tellOutside(name string, r *crew.Role, why string, other *registry.Session) {
	key := outsideKey(name)
	if s.State.RecordNotified[key] {
		return
	}

	text := fmt.Sprintf("%s should be up (%s), but a session named %s already runs outside zmina (pid %d, %s): end it, then zmina up %s",
		name, why, r.Session, other.PID, other.Cwd, name)

	if told, _ := s.NoteOnce(key, "zmina", text, false); told {
		s.log(name, "reconcile-outside", "", text)
	}
}

// forgetOutside lets the note of an outside session be told again: that session is gone.
func (s *Shift) forgetOutside(name string) { delete(s.State.RecordNotified, outsideKey(name)) }

// orphanKey is the RecordNotified key of the report of a session no role names.
func orphanKey(name string) string { return orphanPrefix + name }

// orphanPrefix opens the key of every report of a session no role names.
const orphanPrefix = "orphan/"

// reconcileOrphans looks at the sessions of the crew's tmux server that no role of the crew file names and are no copy of one (the
// roles' own, copies recorded in the state among them, are what Names lists). Each is reported once to the dispatcher, told again
// only after it has been gone, with the tasks it holds named. With reconcile: orphans: down one is also taken down, but only when
// every one of these holds, since a wrong guess ends someone's work: the shift runs; zmina started it (its pane is tagged); the crew
// file has not named it lately, which a mistyped edit would explain (its state is gone, or it was missing for pruneAfterBeats
// beats); no task of this crew is active for it; nothing waits in its queue; its registry entry says idle for idle_minutes; and no
// terminal is attached. It keeps its last screen, is killed (there is no role to say how it exits) and the dispatcher gets an FYI.
// A session whose state cannot be read, or tmux cannot be asked about, is left alone: it does nothing for what it cannot see.
func (s *Shift) reconcileOrphans(now time.Time, stopped bool) (did, notes []string) {
	mode := s.Crew.Reconcile.OrphanMode()
	if mode == crew.OrphansOff {
		return nil, nil
	}

	lister, ok := s.Tmux.(sessionLister)
	if !ok {
		return nil, nil
	}

	// A step of the beat of its own, so that a hold in it is named; none when the key is off.
	s.mark("orphans", "")

	sessions, err := lister.Sessions()
	if err != nil {
		return nil, nil
	}

	present := map[string]bool{}

	for _, info := range s.Orphans(sessions) {
		present[info.Name] = true

		holds := s.heldBy(info.Name)

		if mode == crew.OrphansDown && s.orphanIdle(info, holds, stopped, now) {
			line, note := s.takeOrphanDown(info.Name, now)
			did, notes = append(did, line), append(notes, note...)

			continue
		}

		key := orphanKey(info.Name)
		if s.State.RecordNotified[key] {
			continue
		}

		text := s.orphanText(info, mode, holds)
		if told, _ := s.NoteOnce(key, "zmina", text, len(holds) == 0); told {
			s.log(info.Name, "orphan", strings.Join(holds, ","), text)
			did = append(did, "orphan session "+info.Name)
		}
	}

	// A session that is gone is told again if it returns.
	for key := range s.State.RecordNotified {
		if name, found := strings.CutPrefix(key, orphanPrefix); found && !present[name] {
			delete(s.State.RecordNotified, key)
		}
	}

	return did, notes
}

// Orphans are the sessions of the list that no role of the crew file names and that are no copy of one: what Names lists (the roles,
// and the copies the state records) is not among them, and neither is a copy (dev#2) of a role the crew file has, whose state the
// beat forgets by itself, nor a session of another crew that shares the tmux server (tmux.SessionInfo.Foreign): that crew's roles
// are none of this one's business. A session of a role that has left the crew file is.
func (s *Shift) Orphans(list []tmux.SessionInfo) []tmux.SessionInfo {
	known := map[string]bool{}
	for _, name := range s.Names() {
		known[name] = true
	}

	var out []tmux.SessionInfo

	for _, info := range list {
		if !known[info.Name] && !info.Foreign && !s.copyOfARole(info.Name) {
			out = append(out, info)
		}
	}

	return out
}

// copyOfARole reports whether name is a copy (dev#2) of a role the crew file has: a copy that lost its state is the beat's to forget, not
// an orphan.
func (s *Shift) copyOfARole(name string) bool {
	base, _, ok := SplitInstance(name)
	if !ok {
		return false
	}

	_, known := s.Crew.Roles[base]

	return known
}

// heldBy is the active tasks whose role is name.
func (s *Shift) heldBy(name string) []string {
	if s.Tasks == nil {
		return nil
	}

	var ids []string

	for _, t := range s.Tasks.Active() {
		if t.Role == name {
			ids = append(ids, t.ID)
		}
	}

	return ids
}

// orphanText is what the dispatcher is told of a session no role names.
func (s *Shift) orphanText(info tmux.SessionInfo, mode string, holds []string) string {
	text := fmt.Sprintf("%s: a tmux session no role of the crew file names", info.Name)

	if len(holds) > 0 {
		text += ", and it holds " + strings.Join(holds, ", ") + ": not taken down; bring the role back into the crew file (zmina seat), or move the task (zmina task set <id> --role <seat>)"

		return text
	}

	switch {
	case !info.Tagged:
		text += ", and nothing proves this crew's zmina started it (no tag of this crew's): left alone; zmina attach " + info.Name + " to look"
	case mode == crew.OrphansDown:
		text += fmt.Sprintf("; taken down once it has been idle %d min with nothing queued for it and no terminal attached (reconcile: orphans)", s.Crew.IdleMinutes)
	default:
		text += "; left alone (reconcile: orphans: report); zmina attach " + info.Name + " to look"
	}

	return text
}

// orphanIdle reports whether the session may be taken down: every condition reconcileOrphans lists, the cheap ones first. Whatever
// cannot be read counts against it.
func (s *Shift) orphanIdle(info tmux.SessionInfo, holds []string, stopped bool, now time.Time) bool {
	if stopped || !info.Tagged || len(holds) > 0 || s.Tasks == nil || s.flushErr != nil {
		return false
	}

	if rs := s.State.Roles[info.Name]; rs != nil && (rs.AbsentBeats < pruneAfterBeats || len(rs.Inbox) > 0) {
		return false
	}

	pane, err := s.Tmux.Probe(info.Name)
	if err != nil || pane.State != tmux.Up || pane.PID == 0 {
		return false
	}

	var sess *registry.Session

	if s.Reg != nil {
		sess, _ = s.Reg.ByPID(pane.PID)
	}

	if sess == nil {
		codexReg := &codex.Registry{Dir: filepath.Join(s.Crew.StateDir(), "codex")}
		sess, _ = codexReg.ByPID(pane.PID)
	}

	if sess == nil || sess.Status != "idle" || now.Sub(sess.StatusSince()) < time.Duration(s.Crew.IdleMinutes)*time.Minute {
		return false
	}

	attached, err := s.Tmux.Attached(info.Name)

	return err == nil && !attached
}

// takeOrphanDown ends a session no role names, once orphanIdle said it may: its last screen is kept, it is killed, and the
// dispatcher gets an FYI. It returns a line for the log and the notes.
func (s *Shift) takeOrphanDown(name string, now time.Time) (line string, notes []string) {
	file := s.saveScreen(name, now)

	if err := s.Tmux.Kill(name); err != nil {
		return "orphan " + name + ": could not be taken down: " + err.Error(), nil
	}

	text := fmt.Sprintf("%s: a tmux session no role of the crew file names, idle %d min with nothing queued, taken down", name, s.Crew.IdleMinutes)
	if file != "" {
		text += ", last screen " + file
	}

	s.log(name, "orphan-down", "", text)
	s.markQuiet(text, now)

	return "took down orphan session " + name, []string{text}
}
