package shift

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/inbox"
	"github.com/iamnikolie/zmina/internal/registry"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/tmux"
)

const (
	// reopenPause is the least time between two starts of one role by the ticker, so a role that dies at once
	// does not start claude every minute.
	reopenPause = 5 * time.Minute
	// notesMax caps the ticker's own health notes waiting for the dispatcher (signals are capped at intake).
	notesMax = 20
	// quickEnd is how soon after its start an end counts as dying on boot.
	quickEnd = 10 * time.Minute
	// quickEndsMax ends in a row soon after a start mark a keep: always role broken.
	quickEndsMax = 2
	// screenLines is how much of an ended role's screen is kept; screensKept how many screens per role.
	screenLines = 60
	screensKept = 20
	// trimIdle is how long a role sits idle before its conversation is compacted, so a reply being read or written
	// is not cut into (a clear, which loses the conversation, waits idle_minutes); trimPause the least time between
	// two trims of one role, so its next turn shows the new size.
	trimIdle = 5 * time.Minute
	// trimIdleOver is the shorter wait of a role far past its context_max (trimFar): a dispatcher that gets a line
	// every few minutes is seldom idle for trimIdle, and its conversation only grows meanwhile.
	trimIdleOver = 2 * time.Minute
	trimPause    = 30 * time.Minute
	// trimFar: a role over its context_max by more than a quarter of it compacts after trimIdleOver.
	trimFar = 4
	// unstartedFor is how long an open task may sit on a down role, never accepted, before the dispatcher hears of
	// it: task new records a task for a down role without starting it.
	unstartedFor = 5 * time.Minute
)

// Tick is one beat of the clock: it rings the alarms that are due (Rang: the caller runs their notify commands and
// tells), collects roles that ended (keeping their last screen), starts the always-roles
// that are down, runs schedules, notes roles busy or idle too long and seats past their expiry, releases roles idle
// past release_idle_minutes (Released: the caller finishes those downs), types waiting
// lines into idle roles and the queue into the dispatcher, and trims conversations past their context_max. A role tmux cannot be asked about is left alone for the
// beat. It returns what it did, for the log.
func (s *Shift) Tick() []string {
	defer s.FlushCards()

	now := s.Now()
	s.mark("reconcile", "")
	if s.ReconcileMessages != nil {
		if err := s.ReconcileMessages(s); err != nil && s.Warn != nil {
			s.Warn(err)
		}
	}
	s.mark("notices", "")
	for _, name := range s.Names() {
		s.pruneTaskInbox(name)
	}
	s.NotifyRecords()
	s.NotifyClosed()
	s.acceptanceAlerts()
	held := s.held()
	stopped := !s.State.Stopped.IsZero() || s.Crew.IsRetired()
	s.lastBeat = s.State.LastTick
	s.State.LastTick = now
	s.pruneAbsent(now)

	// Alarms first: a role an alarm starts is up for the rest of the beat.
	s.mark("alarms", "")
	did, notes := s.ring(now, stopped)
	did = append(did, s.reportUnreadable(now)...)

	// The roles this beat probed up, for the age sweep: a role skipped below (tmux did not answer) is not among them.
	probedUp := map[string]bool{}

	for _, name := range s.Names() {
		s.mark("roles", name)
		r, err := s.Role(name)
		if err != nil {
			continue
		}

		rs := s.State.Role(name)

		if !rs.Downing.IsZero() && now.Sub(rs.Downing) < downingFor {
			continue
		}

		// Past downingFor a begun down should be over: the process that began it died during its wait.
		if !rs.Downing.IsZero() {
			if line := s.finishStale(name, rs, now); line != "" {
				did = append(did, line)

				continue
			}
		}

		p, err := s.probe(name)
		if err != nil {
			did = append(did, name+": left alone this beat, tmux did not answer: "+err.Error())

			continue
		}

		up := p.State == tmux.Up
		if up {
			probedUp[name] = true

			s.remember(name, p)
			s.closeTakenTyped(name)
			s.observeRecovery(name)
			// A role started by an older zmina would vanish without a screen.
			_ = s.Tmux.KeepScreens(name)
		}

		if !up && rs.Up {
			notes = append(notes, s.ended(name, r, now, held[name], p)...)

			if isInstance(name) {
				s.forget(name)

				continue
			}
		} else if p.State == tmux.Dead {
			// Ended while zmina was not looking (state lost or a down that did not finish): nothing to report.
			_ = s.Tmux.Kill(name)
		}

		// A role with no session leaves nothing posted or typed behind: settled once, here, whether it ended, was put
		// down or released, or its state was kept from before. Only when it is really gone: a probe that fails to see
		// a live session (tmux answered "no server") must not settle what is posted to it. So the beat before saw it
		// not up either (rs.Up is still what that beat left; a down by command leaves it false), and no session of its
		// name lives in the registry, the evidence start asks for.
		if !up && !rs.Up && s.registryFor(name).LiveByName(r.Session) == nil {
			s.settleEnded(name, now)
		}

		if !up && !stopped && r.Keep == crew.KeepAlways && !rs.Held && !rs.Broken && s.flushErr == nil &&
			s.registryFor(name).LiveByName(r.Session) == nil && now.Sub(rs.Started) >= reopenPause {
			if err := s.start(name, UpOptions{}); err != nil {
				// Wait the usual pause before the next try, so a role that cannot start does not fill the log.
				rs.Started = now
				did = append(did, "start "+name+": "+err.Error())
			} else {
				up = true

				did = append(did, "started "+name)
				if name != s.Crew.Dispatcher {
					// What zmina did, told once: an info line.
					note := name + ": started again (keep: always)"
					s.markInfo(note, now)
					notes = append(notes, note)
				}
			}
		}

		rs.Up = up

		line, note, started := s.schedule(name, r, now, held[name], up, stopped)
		if line != "" {
			did = append(did, line)
		}

		if note != "" {
			notes = append(notes, note)
		}

		if started {
			up, rs.Up = true, true
		}

		if note := s.stuck(name, now, held[name]); note != "" {
			notes = append(notes, note)
		}

		if note := s.idle(name, r, now, held[name]); note != "" {
			notes = append(notes, note)
		}

		if line := s.restart(name, r, now, held[name], true); line != "" {
			did = append(did, line)
			up = up && rs.Downing.IsZero()
		}

		if line, note := s.release(name, r, now); line != "" {
			did = append(did, line)

			if note != "" {
				notes = append(notes, note)
			}

			up = up && rs.Up
		}

		if note := s.expired(name, r, now, up); note != "" {
			notes = append(notes, note)
		}

		notes = append(notes, s.unstarted(name, now, up || stopped || !rs.Downing.IsZero(), held[name])...)

		if up {
			notes = append(notes, s.watchScreen(name, now)...)

			if note := s.hooksNote(name, r); note != "" {
				notes = append(notes, note)
			}
		}
	}

	s.mark("notes", "")
	// Delivery records past their age limit go, told once (one more note).
	// The beat after a gap judges no record by age (beatGapMax): nobody could have looked at it or retried it meanwhile.
	if !s.lastBeat.IsZero() && now.Sub(s.lastBeat) <= beatGapMax {
		notes = append(notes, s.sweepDelivery(now, probedUp)...)
	}

	// The doorbells rung and not read: what the roles fetched is put on the cards, and a bell shown and left alone for long
	// is told to the dispatcher once.
	notes = append(notes, s.bellWatch(now)...)

	s.addNotes(notes)
	did = append(did, notes...)

	s.renotify(now)

	// Each line typed or posted is saved at once (flush): a beat killed after it, or a save that fails at its end,
	// does not type it again. After a failed flush nothing more is typed this beat.
	typed := map[string]bool{}
	s.mark("deliver", "")
	if line := s.deliver(now); line != "" {
		typed[s.Crew.Dispatcher] = true
		did = append(did, "told the dispatcher: "+line)
	}

	for _, name := range s.Names() {
		if typed[name] || s.flushErr != nil {
			continue
		}

		s.mark("deliver", name)
		if line := s.deliverInbox(name, now); line != "" {
			typed[name] = true
			did = append(did, line)
		}
	}

	for _, name := range s.Names() {
		if r, err := s.Role(name); err == nil && !typed[name] {
			s.mark("trim", name)
			if line := s.trim(name, r, now, held[name]); line != "" {
				did = append(did, line)
			}
		}
	}

	// Again after delivery, which it sees: a report note refused for a full queue is queued once this beat drained it.
	s.mark("reconcile-end", "")
	if s.ReconcileMessages != nil {
		if err := s.ReconcileMessages(s); err != nil && s.Warn != nil {
			s.Warn(err)
		}
	}

	return did
}

// trim types /compact into a role whose conversation fills more than its context_max — or, with context_action:
// clear, /clear and then its boot line — once it has sat idle with nothing waiting for it and an empty input line
// on its screen (a half-typed line would swallow the command). A clear also waits for no terminal to be attached. A role holding an active task is compacted, never cleared: the task's work lives in its
// conversation. A conversation that is over the limit from its first turn after a trim (a boot that alone fills
// it) is left alone, so a trim never feeds itself. It returns a line for the log.
func (s *Shift) trim(name string, r *crew.Role, now time.Time, tasks []string) string {
	limit := r.ContextLimit()
	rs := s.State.Role(name)

	// A role asked for its note before a restart, or being restarted, is left to that.
	if !rs.RestartAsked.IsZero() || !rs.Downing.IsZero() {
		return ""
	}

	do := r.ContextDo()
	if do == crew.ContextClear && len(tasks) > 0 {
		do = crew.ContextCompact
	}

	// A compaction also goes to a role whose turn is over while a background shell or agent it started keeps its
	// status off idle: such a role (a dispatcher with a watcher running) would otherwise never be compacted. A clear
	// waits for a plain idle: it must not end the conversation a background process reports back to.
	between := s.ready(name, now) || do == crew.ContextCompact && s.atPrompt(name, now)
	if limit == 0 || !between {
		return ""
	}

	sess := s.session(name)
	if sess == nil {
		return ""
	}

	if rs.TrimPending {
		turn, err := s.registryFor(name).Last(sess.SessionID)
		if err != nil {
			return ""
		}

		// The size the conversation shows counts only once the trim took: a compaction written after it, or the
		// conversation a /clear started. Until then the last turn is the one before the trim, over the limit.
		if (rs.TrimFrom != "" && sess.SessionID != rs.TrimFrom) || (!turn.Compacted.IsZero() && !turn.Compacted.Before(rs.Trimmed)) {
			rs.TrimTook = true
		}

		if !rs.TrimTook {
			if now.Sub(rs.Trimmed) >= trimPause {
				rs.TrimPending = false
				s.log(name, "trim-unseen", "", "no compaction or new conversation seen "+trimPause.String()+" after the trim")
			}

			return ""
		}

		used := turn.Context
		if used == 0 {
			return ""
		}

		rs.TrimPending, rs.TrimTook, rs.TrimSession, rs.TrimBase = false, false, sess.SessionID, used
		// The trim queued its own recovery line: the drop it caused is the new baseline, not a native compaction
		// for observeRecovery to post a second line for.
		if rs.RecoverySession == sess.SessionID {
			rs.RecoveryContext, rs.RecoveryDrops = used, 0
		}
		if used > limit {
			note := fmt.Sprintf("%s: fills %s right after a trim, over its context_max %s; not trimmed again in "+
				"this conversation — raise context_max", name, Tokens(used), Tokens(limit))
			s.log(name, "trim-stuck", "", note)
			s.addNotes([]string{note})

			return note
		}
	}

	if rs.TrimSession == sess.SessionID && rs.TrimBase > limit {
		return ""
	}

	if len(rs.Inbox) > 0 || now.Sub(rs.Trimmed) < trimPause {
		return ""
	}

	if name == s.Crew.Dispatcher && len(s.State.Signals)+len(s.State.Notes) > 0 {
		return ""
	}

	turn, err := s.registryFor(name).Last(sess.SessionID)
	used := turn.Context

	if err != nil || used <= limit {
		return ""
	}

	wait := trimIdle
	if do == crew.ContextClear {
		wait = max(wait, time.Duration(s.Crew.IdleMinutes)*time.Minute)
	} else if used > limit+limit/trimFar {
		wait = trimIdleOver
	}

	// Idle since the later of the status's change and the turn's end: a background shell keeps a status from before
	// the turn, so its time says nothing of when the reply was given. Off idle with no known end, it waits.
	since := sess.StatusSince()
	if turn.At.After(since) {
		since = turn.At
	} else if sess.Status != "idle" && turn.At.IsZero() {
		return ""
	}

	if now.Sub(since) < wait {
		return ""
	}

	if do == crew.ContextClear {
		if attached, err := s.Tmux.Attached(name); err != nil || attached {
			return ""
		}
	}

	if draft, ok, err := s.Tmux.Prompt(name); err != nil || !ok || draft != "" {
		return ""
	}

	recoveryPath := ""
	if name == s.Crew.Dispatcher {
		var err error
		recoveryPath, _, err = s.RecoveryPack()
		if err != nil {
			return name + ": trim skipped: recovery pack: " + err.Error()
		}
	}
	// A failed try waits the pause too, so a role tmux refuses does not fill the log every beat.
	rs.Trimmed, rs.TrimPending, rs.TrimTook, rs.TrimFrom = now, true, false, sess.SessionID
	detail := fmt.Sprintf("context %s over %s", Tokens(used), Tokens(limit))

	if err := s.Tmux.Type(name, "/"+do); err != nil {
		return fmt.Sprintf("%s: /%s (%s): %s", name, do, detail, err)
	}

	// A /compact or a /clear leaves no record to wait for: what is queued behind it waits slashSettle.
	rs.SlashAt = now

	// The boot again, without the tag: queued, it is a line the log, the queue and the recovery pack show; the
	// clear's hook gives the tag.
	if boot := s.bootText(r, false); do == crew.ContextClear && !r.IsCodex() && boot != "" {
		s.enqueue(name, boot, Facts{From: "zmina"})
		rs.QueuedBoot = boot
	}

	if recoveryPath != "" && (do != crew.ContextClear || r.IsCodex()) {
		s.queueRecovery(recoveryPath)
	}
	event := "compacted"
	if do == crew.ContextClear {
		event = "cleared"
	}

	s.log(name, event, strings.Join(tasks, ","), detail)

	return fmt.Sprintf("%s: /%s, %s", name, do, detail)
}

// Tokens writes a token count the way context_max takes it: 362k, 1.2m.
func Tokens(n int) string {
	switch {
	case n >= 1_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e6), ".0") + "m"
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprint(n)
	}
}

// AddNotes queues health notes for the dispatcher (see addNotes).
func (s *Shift) AddNotes(notes []string) { s.addNotes(notes) }

// addNotes queues health notes for the dispatcher: a note already waiting is not added twice, and past notesMax the
// oldest health notes go. Signals (zmina note, watches) are a separate queue and never go this way.
func (s *Shift) addNotes(notes []string) {
	if s.State.NoticeMeta == nil {
		s.State.NoticeMeta = map[string]NoticeMeta{}
	}
	for _, n := range notes {
		meta := s.State.NoticeMeta[n]
		if meta.Count == 0 {
			meta = s.noticeMeta(n)
		} else {
			meta.Count++
		}
		s.State.NoticeMeta[n] = meta
		if !slices.Contains(s.State.Notes, n) {
			s.State.Notes = append(s.State.Notes, n)
		}
	}

	if len(s.State.Notes) > notesMax {
		s.State.Notes = s.State.Notes[len(s.State.Notes)-notesMax:]
	}
	for key := range s.State.NoticeMeta {
		if !slices.Contains(s.State.Notes, key) {
			delete(s.State.NoticeMeta, key)
		}
	}
}

// ended handles a role that was up and is gone: keeps its last screen, counts ends soon after a start, and marks a
// keep: always role that keeps dying on boot as broken.
func (s *Shift) ended(name string, r *crew.Role, now time.Time, tasks []string, p tmux.Pane) []string {
	rs := s.State.Role(name)

	note := name + ": ended"
	if len(tasks) > 0 {
		note += " (held " + strings.Join(tasks, ", ") + ")"
	}

	if p.State == tmux.Dead {
		if file := s.saveScreen(name, now); file != "" {
			note += ", last screen " + file
		}

		_ = s.Tmux.Kill(name)
	}

	s.log(name, "ended", strings.Join(tasks, ","), note)

	if now.Sub(rs.Started) < quickEnd {
		rs.QuickEnds++
	} else {
		rs.QuickEnds = 0
	}

	notes := []string{note}

	if r.Keep == crew.KeepAlways && name != s.Crew.Dispatcher && rs.QuickEnds >= quickEndsMax {
		rs.Broken = true
		notes = append(notes, fmt.Sprintf("%s: dies on boot (%d ends within %d min of a start), not started again "+
			"until zmina up %s", name, rs.QuickEnds, int(quickEnd.Minutes()), name))
		s.log(name, "broken", "", notes[1])
	}

	return notes
}

// saveScreen keeps the last screen of an ended role in screens/<role>-<time>.txt and returns that path, relative to
// the crew directory. Only the newest screensKept screens of a role are kept.
func (s *Shift) saveScreen(name string, now time.Time) string {
	text, err := s.Tmux.Capture(name, screenLines)
	if err != nil || strings.TrimSpace(text) == "" {
		return ""
	}

	dir := s.ScreensDir()
	if err := fsx.MkdirAll(dir, 0o700); err != nil {
		return ""
	}

	base := strings.ReplaceAll(name, "#", "-")
	file := filepath.Join(dir, base+"-"+now.Format("20060102-150405")+".txt")

	if err := fsx.WriteFile(file, []byte(text+"\n"), 0o600); err != nil {
		return ""
	}

	if old, _ := filepath.Glob(filepath.Join(dir, base+"-2*.txt")); len(old) > screensKept {
		sort.Strings(old)

		for _, f := range old[:len(old)-screensKept] {
			_ = os.Remove(f)
		}
	}

	rel, _ := filepath.Rel(s.Crew.StateDir(), file)

	return rel
}

// CrewClock is t on the crew file's timezone when it sets one, and as it is, the host's, when it does not. What is read
// as a time of day (a role's schedule, the budget's day) is read on it.
func (s *Shift) CrewClock(t time.Time) time.Time {
	if s.Crew.Timezone == "" {
		return t
	}

	return t.In(s.Crew.Location())
}

// schedule runs a role's schedule: at each time it records a task, queues the prompt and starts the role if it is
// down. A run is skipped while the role holds an active task (the dispatcher hears of it once per set of tasks), and
// while the shift is stopped. It returns a line for the log, a health note, and whether it started the role.
func (s *Shift) schedule(name string, r *crew.Role, now time.Time, tasks []string, up, stopped bool) (
	line, note string, started bool) {
	rs := s.State.Role(name)

	sched := r.Scheduled()
	if sched == nil {
		rs.NextRun, rs.NextSpec = time.Time{}, ""

		return "", "", false
	}

	// The schedule's hours are the crew's clock, as a reminder's are (CrewClock).
	at := s.CrewClock(now)

	spec := r.Schedule
	if s.Crew.Timezone != "" {
		spec += " @" + s.Crew.Timezone
	}

	if rs.NextRun.IsZero() || rs.NextSpec != spec {
		// First sight, or the schedule or its timezone changed: count from now.
		rs.NextRun, rs.NextSpec = sched.Next(at), spec

		return "", "", false
	}

	if now.Before(rs.NextRun) {
		return "", "", false
	}

	rs.NextRun = sched.Next(at)

	if stopped {
		s.log(name, "schedule-skipped", "", "the shift is stopped")

		return name + ": scheduled run skipped, the shift is stopped", "", false
	}

	if len(tasks) > 0 {
		heldBy := strings.Join(tasks, ", ")
		s.log(name, "schedule-skipped", strings.Join(tasks, ","), "holds an active task")

		if rs.SkipNoted != heldBy {
			rs.SkipNoted = heldBy
			note = name + ": scheduled run skipped, still holds " + heldBy
		}

		return name + ": scheduled run skipped, holds " + heldBy, note, false
	}

	rs.SkipNoted = ""

	t := task.Task{Role: name, Asked: r.Prompt, By: "schedule"}
	if s.Tasks != nil {
		var err error
		if t, err = s.Tasks.Add(t, now); err != nil {
			return name + ": scheduled run: " + err.Error(), "", false
		}
	}

	queued := r.Prompt
	if t.ID != "" {
		queued = "[zmina " + t.ID + ", scheduled] " + r.Prompt
	}

	s.enqueue(name, queued, Facts{From: "zmina", Task: t.ID})
	s.log(name, "scheduled", t.ID, r.Prompt)

	if up || rs.Broken || s.registryFor(name).LiveByName(r.Session) != nil {
		return name + ": scheduled run " + t.ID, "", false
	}

	if err := s.start(name, UpOptions{}); err != nil {
		return name + ": scheduled run " + t.ID + ", start: " + err.Error(), "", false
	}

	return name + ": scheduled run " + t.ID + ", started", "", true
}

// stuck notes a role busy longer than stuck_minutes, once per busy stretch.
func (s *Shift) stuck(name string, now time.Time, tasks []string) string {
	rs := s.State.Role(name)

	sess := s.session(name)
	if sess == nil || sess.Status != "busy" || rs.StuckAt == sess.StatusUpdatedAt {
		return ""
	}

	busy := now.Sub(sess.StatusSince())
	if busy < time.Duration(s.Crew.StuckMinutes)*time.Minute {
		return ""
	}

	rs.StuckAt = sess.StatusUpdatedAt

	note := fmt.Sprintf("%s: busy for %d min", name, int(busy.Minutes()))
	if len(tasks) > 0 {
		note += " on " + strings.Join(tasks, ", ")
	}

	s.log(name, "stuck", strings.Join(tasks, ","), note)

	return note
}

// idle notes a role idle longer than idle_minutes without an active task or a waiting line, once per idle stretch:
// the dispatcher may put it down. A role the ticker releases on its own (release_idle_minutes) is not noted.
func (s *Shift) idle(name string, r *crew.Role, now time.Time, tasks []string) string {
	rs := s.State.Role(name)
	if name == s.Crew.Dispatcher || len(tasks) > 0 || len(rs.Inbox) > 0 || s.releases(r) {
		return ""
	}

	sess := s.session(name)
	if sess == nil || sess.Status != "idle" || rs.IdleAt == sess.StatusUpdatedAt {
		return ""
	}

	idle := now.Sub(sess.StatusSince())
	if idle < time.Duration(s.Crew.IdleMinutes)*time.Minute {
		return ""
	}

	rs.IdleAt = sess.StatusUpdatedAt
	note := fmt.Sprintf("%s: idle for %d min, no active task", name, int(idle.Minutes()))
	s.log(name, "idle", "", note)

	return note
}

// expired notes a temporary seat past its expiry once it is down, once per expires value.
func (s *Shift) expired(name string, r *crew.Role, now time.Time, up bool) string {
	rs := s.State.Role(name)

	at := r.ExpiresAt()
	if at.IsZero() || up || now.Before(at) || rs.Expired == r.Expires {
		return ""
	}

	rs.Expired = r.Expires
	note := fmt.Sprintf("%s: seat expired %s; zmina seat rm %s removes it", name, r.Expires, name)
	s.log(name, "expired", "", note)

	return note
}

// unstarted tells the dispatcher, once per task, of each open task that sits on a down role it never accepted, with
// the command that starts the role on the task; quiet while the role is up or the shift stopped. A role asleep is
// left to its alarm; a task accepted before the role went down is the red «down, holds» line instead. Each note is
// its own notice, checked again before it is delivered (noticeCurrent).
func (s *Shift) unstarted(name string, now time.Time, quiet bool, held []string) []string {
	rs := s.State.Role(name)
	rs.Unstarted = slices.DeleteFunc(rs.Unstarted, func(id string) bool { return !slices.Contains(held, id) })

	if quiet || s.Tasks == nil || name == s.Crew.Dispatcher || !s.Asleep(name).IsZero() {
		return nil
	}

	var notes []string

	for _, id := range held {
		t, err := s.Tasks.Get(id)
		if err != nil || !unaccepted(t) || now.Sub(t.Created) < unstartedFor || slices.Contains(rs.Unstarted, id) {
			continue
		}

		rs.Unstarted = append(rs.Unstarted, id)
		note := fmt.Sprintf("%s: %s%s; zmina --crew %s up %s --task %s starts it on it", name, unstartedWord, id,
			shellWord(s.Crew.Path), shellWord(name), id)
		s.log(name, "unstarted", id, note)
		notes = append(notes, note)
	}

	return notes
}

// unstartedWord opens the dispatcher's note about a task left on a down role, so the notice can be read back.
const unstartedWord = "down, has not taken "

// unaccepted is an open task its role never accepted.
func unaccepted(t task.Task) bool {
	return t.State == task.Open && (t.Delivery == nil || t.Delivery.AcceptedAt.IsZero())
}

// session is the registry entry of a role up under zmina, nil when it is not up or has not written one.
func (s *Shift) session(name string) *registry.Session {
	if !s.State.Role(name).Up {
		return nil
	}

	p, err := s.probe(name)
	if err != nil || p.State != tmux.Up {
		return nil
	}

	sess, _ := s.registryFor(name).ByPID(p.PID)

	return sess
}

// booted reports whether a role's start is over: the conversation of this start's session showed a turn that ended
// at or after the start (its boot turn), or the role has been up boot_grace, the longest a boot is waited for. So a
// queued line goes as soon as the boot is read, not a fixed time after the start. The boot's end is kept (Booted):
// the turn a first line starts is not over, and must not read as a boot again.
func (s *Shift) booted(name string, now time.Time) bool {
	rs := s.State.Role(name)
	if !rs.Booted.IsZero() && !rs.Booted.Before(rs.Started) || now.Sub(rs.Started) >= s.Crew.BootGraceD() {
		return true
	}

	sess := s.session(name)
	if sess == nil {
		return false
	}

	turn, err := s.registryFor(name).Last(sess.SessionID)
	if err != nil || !turn.Ended || turn.At.IsZero() || turn.At.Before(rs.Started) {
		return false
	}

	rs.Booted = turn.At

	return true
}

// ready reports whether a role is up, booted and idle, so a typed line lands at its prompt.
func (s *Shift) ready(name string, now time.Time) bool {
	if !s.booted(name, now) {
		return false
	}

	sess := s.session(name)

	return sess != nil && sess.Status == "idle"
}

// atPrompt reports whether a typed line lands at a role's prompt: it is ready (idle), or its last turn is over and
// its input line is on screen and empty while its status says otherwise — a background shell or agent it started
// keeps Claude Code's status off idle for as long as it runs, and a line queued for it would wait as long. A dialog
// (status waiting) is never typed into.
func (s *Shift) atPrompt(name string, now time.Time) bool {
	if s.ready(name, now) {
		return true
	}

	if !s.booted(name, now) {
		return false
	}

	sess := s.session(name)
	if sess == nil || sess.Status == "idle" || sess.Status == "waiting" {
		return false
	}

	turn, err := s.registryFor(name).Last(sess.SessionID)
	if err != nil || !turn.Ended {
		return false
	}

	// The line typed last has taken: the role is busy with it, though the transcript, which does not show it yet,
	// still ends in the turn before it. That turn is not this role's last (typedSettle).
	if rs := s.State.Role(name); within(now, rs.TypedAt, typedSettle) && !turn.At.After(rs.TypedAt) {
		return false
	}

	draft, ok, err := s.Tmux.Prompt(name)

	return err == nil && ok && draft == ""
}

// deliver gives the dispatcher what waits for it: the signals (work, with their ids), the lines of other crews and
// the health notes. Posted to its own channel, one post takes everything (postDispatcher); typed, it is one line when
// it is up, booted and idle (typeDispatcher).
func (s *Shift) deliverOnce(now time.Time) string {
	d := s.Crew.Dispatcher

	// A slash command was just typed into it (a trim's): its queue waits as any role's does (slashSettle). So does
	// a post or a typed line that failed a moment ago (retryGap): a caller that comes every few seconds must not try
	// again, and log again, each time.
	if rs := s.State.Role(d); rs.settling(now) || rs.dispatchTriedLately(now) {
		return ""
	}

	// Its last turn ended on an API error a moment ago: nothing goes until the hold is over (failureHeld).
	if s.failureHeld(d, now) {
		return ""
	}

	s.pruneTaskInbox(d)
	s.pruneInfo()

	var local, remote []string

	for _, sig := range s.State.Signals {
		if IsRemote(signalBody(sig)) {
			remote = append(remote, sig)
		} else {
			local = append(local, sig)
		}
	}

	for sig := range s.State.SignalSent {
		if !slices.Contains(remote, sig) {
			delete(s.State.SignalSent, sig)
		}
	}

	notes := s.currentNotes()

	if sess := s.native(d, now); sess != nil {
		return s.postDispatcher(d, sess, local, remote, notes, now)
	}

	return s.typeDispatcher(d, local, remote, notes, now)
}

// postMax is the most one post to the dispatcher carries, in bytes; what does not fit goes in the next.
const postMax = 12 << 10

// postDispatcher posts what waits for the dispatcher as one message, a line each: its own queued lines (the recovery
// line), this crew's signals, each other crew's line on a line of its own (a remote body holds no line break, so it
// cannot start a line that reads as zmina's), the notices last. It goes once the post before it showed and the
// dispatcher is between turns. Into a turn it is in (delivery_mid_turn) only the lines that wait for its ack go —
// Claude Code puts them into the turn at its next tool call — never into a dialog; notices and its own queue wait
// for the turn's end. A boot line queued after a clear goes alone first (deliverInbox): the lines belong to a
// dispatcher that knows its job.
func (s *Shift) postDispatcher(d string, sess *registry.Session, local, remote, notes []string, now time.Time) string {
	rs := s.State.Role(d)
	if rs.QueuedBoot != "" && !slices.Contains(rs.Inbox, rs.QueuedBoot) {
		rs.QueuedBoot = ""
	}

	if s.followPosted(d, now) || slices.ContainsFunc(rs.Inbox, func(l string) bool { return s.isBoot(d, l) }) {
		return ""
	}

	quiet := s.quiet(d, sess)
	if !quiet && !s.midTurn(d, sess) {
		return ""
	}

	var own []string

	if quiet {
		for _, l := range rs.Inbox {
			if !s.postableFor(d, l) {
				break
			}

			own = append(own, l)
		}
	} else {
		// Into a turn: only what waits for an ack.
		acked := func(sig string) bool { return !signalID.MatchString(sig) }
		local = slices.DeleteFunc(slices.Clone(local), acked)
		remote = slices.DeleteFunc(slices.Clone(remote), acked)
		notes = nil
	}

	// Info lines alone wait a while for a line that goes anyway (infoRide); they go on the notices' line.
	if len(own) == 0 && len(remote) == 0 && s.infoWaits(local, now) {
		local, notes = nil, nil
	}

	local, info := s.splitInfo(local)

	var (
		lines, ids, fresh []string
		parts             []Part
		size              int
		took              = map[string]bool{}
		full              bool
	)

	add := func(line string) bool {
		if full || len(lines) > 0 && size+len(line)+1 > postMax {
			full = true

			return false
		}

		lines = append(lines, line)
		size += len(line) + 1

		return true
	}

	// Each signal that goes is kept as a part, with its id at its head, for it to go back as it waited.
	var sigParts []Part
	part := func(sig string) (string, string) {
		p, id := s.signalPart(sig)
		if id != "" && !signalID.MatchString(sig) {
			fresh = append(fresh, id)
		}

		kept := Part{Signal: p, Count: s.State.SignalCounts[sig], Sent: s.State.SignalSent[sig]}
		if id != "" {
			kept.IDs = []string{id}
		}
		sigParts = append(sigParts, kept)

		return repeated(p, s.State.SignalCounts[sig]), id
	}

	nOwn := 0
	for _, l := range own {
		// A queued line that no post could carry goes as a bounded one: its whole text stored, the line its head
		// and where the rest is, still the same line (its ids).
		bounded, err := s.boundLine(d, l, postMax)
		if err != nil {
			s.log(d, "post-failed", "", "a queued line over "+strconv.Itoa(postMax)+" bytes could not be stored: "+err.Error())

			break
		}

		if !add(bounded) {
			break
		}
		nOwn++
	}

	// This crew's signals share a line, as many as fit: the line is measured whole, its separators, ids and
	// wrapper included, against what the post has left.
	var localParts, partIDs []string
	localLine := func(parts, ids []string) string { return "[zmina] " + strings.Join(parts, " · ") + ackTail(ids) }
	for _, sig := range local {
		p, id := part(sig)
		withIDs := partIDs
		if id != "" {
			withIDs = append(slices.Clone(partIDs), id)
		}

		if size+len(localLine(append(slices.Clone(localParts), p), withIDs))+1 > postMax && (len(lines) > 0 || len(localParts) > 0) {
			sigParts = sigParts[:len(sigParts)-1]
			full = true

			break
		}

		localParts, partIDs = append(localParts, p), withIDs
		took[sig] = true
	}

	if len(localParts) > 0 {
		line := localLine(localParts, partIDs)
		lines = append(lines, line)
		size += len(line) + 1
		ids = append(ids, partIDs...)
	}

	for _, sig := range remote {
		p, id := part(sig)
		var tail []string
		if id != "" {
			tail = []string{id}
		}

		if !add("[zmina] " + p + s.lateness(sig, now) + ackTail(tail)) {
			sigParts = sigParts[:len(sigParts)-1]

			break
		}

		took[sig] = true
		ids = append(ids, tail...)
	}

	// The notices' line: the info lines (signals with no id, each kept as a part to go back as it waited), then the
	// notes. It goes whole or waits whole.
	var last []string
	for _, sig := range info {
		p, _ := part(sig)
		last = append(last, s.fyi(sig, p))
	}

	last = append(last, notes...)

	if len(last) > 0 && add("[zmina] "+strings.Join(last, " · ")) {
		for _, sig := range info {
			took[sig] = true
		}

		if len(notes) > 0 {
			took["\x00notes"] = true
		}
	} else {
		sigParts = sigParts[:len(sigParts)-len(info)]
	}

	if len(lines) == 0 {
		for _, id := range fresh {
			delete(s.State.Acks, id)
		}

		return ""
	}

	text := strings.Join(lines, "\n")

	if err := s.post(d, sess, text, now); err != nil {
		rs.dispatchTried(now)
		s.log(d, "post-failed", "", err.Error())

		for _, id := range fresh {
			delete(s.State.Acks, id)
		}

		return ""
	}

	// An id given here to a signal queued without one that did not fit goes with it: it gets one when it goes.
	for _, id := range fresh {
		if !slices.Contains(ids, id) {
			delete(s.State.Acks, id)
		}
	}

	for _, l := range own[:nOwn] {
		lineIDs := s.takeIDs(d, l)
		parts = append(parts, Part{Line: l, IDs: lineIDs})
		ids = append(ids, lineIDs...)
		s.submittedTask(d, l)
	}

	parts = append(parts, sigParts...)
	if took["\x00notes"] {
		parts = append(parts, Part{Notes: slices.Clone(notes)})
	}

	rs.Inbox = rs.Inbox[nOwn:]
	rs.clearTries()
	rs.Posted[len(rs.Posted)-1].IDs = slices.Clone(ids)
	rs.Posted[len(rs.Posted)-1].Parts = parts
	s.delivered(ids, now)
	s.cardsAccepted(d, text, parts, s.acceptedBy(d), sess.SessionID, now)

	for sig := range took {
		if sig == "\x00notes" {
			continue
		}

		s.State.Signals = slices.DeleteFunc(s.State.Signals, func(v string) bool { return v == sig })
		delete(s.State.SignalCounts, sig)
		delete(s.State.SignalSent, sig)
	}

	if took["\x00notes"] {
		s.State.Notes, s.State.NoticeMeta = nil, nil
	}

	s.State.RemoteBatches = 0
	s.Told = true
	s.log(d, "told", "", text)

	return text
}

// boundLine is a queued line as a post can carry it, within limit bytes: whole when it fits, else its text stored
// whole in the crew's inbox and the line its head and where the rest is (Pointer), with the tail zmina tracks it by
// kept at its end.
func (s *Shift) boundLine(name, line string, limit int) (string, error) {
	if len(line) <= limit {
		return line, nil
	}

	// The tail a tracked line ends with stays at its end, for its reader: what it is known by is its ids.
	body, tail := line, ""
	if loc := trackedTail.FindStringIndex(line); loc != nil && len(s.State.Role(name).QueuedIDs[line]) > 0 {
		body, tail = strings.TrimRight(line[:loc[0]], " "), " "+line[loc[0]:]
	}

	store := s.inboxStore()

	// The entry is known by the role and the text, so a post that fails and is tried again stores it over itself: one
	// entry for the line, not one for each try.
	id := inbox.IDOf("queue " + name + "\x00" + body)
	if err := store.Put(inbox.Entry{ID: id, From: "queue " + name, Body: body}); err != nil {
		return "", err
	}

	room := []rune(body)
	n := len(room)
	for n > 1 {
		out := Pointer(body, id, n) + tail
		if len(out) <= limit {
			return out, nil
		}

		n = n * 3 / 4
	}

	return Pointer(body, id, 1) + tail, nil
}

// midTurn reports whether the dispatcher's acked lines may go into the turn it is in: the crew allows it, it is a
// Claude session (Claude Code queues a message that comes mid-turn and puts it into the turn at its next tool call),
// and no dialog waits on the owner.
func (s *Shift) midTurn(d string, sess *registry.Session) bool {
	r, err := s.Role(d)

	return err == nil && s.Crew.MidTurn() && !r.IsCodex() && sess.Status != "waiting"
}

// isBoot reports whether a queued line is the role's boot line, queued after a clear (a state written before
// QueuedBoot was kept: a line that starts with the role's boot).
func (s *Shift) isBoot(name, line string) bool {
	if q := s.State.Role(name).QueuedBoot; q != "" {
		return line == q
	}

	r, err := s.Role(name)

	return err == nil && strings.TrimSpace(r.Boot) != "" && strings.HasPrefix(line, strings.TrimSpace(r.Boot))
}

// typeDispatcher types the waiting signals and health notes into the dispatcher as one line when it is up, booted
// and idle. Signals go first: they are work. A line from another crew goes as a line of its own, never joined with
// this crew's signals and notes: one per beat, after this crew's signals, and before the notices — a notice never
// goes alone while another crew's line waits.
func (s *Shift) typeDispatcher(d string, local, remote, notes []string, now time.Time) string {
	alone := len(remote) > 0 && (len(local) == 0 || s.State.RemoteBatches >= 3)

	// A line queued for the dispatcher itself goes first: after a clear it is the boot line, and the signals
	// belong to a dispatcher that knows its job.
	if len(local)+len(notes) == 0 && !alone || len(s.State.Role(d).Inbox) > 0 {
		return ""
	}

	// The line typed last has to land first: landed presses Enter on it while it still sits in the input line,
	// whatever the dispatcher's status says.
	if !s.landed(d, now) || !s.atPrompt(d, now) {
		return ""
	}

	// Info lines alone wait a while for a line that goes anyway (infoRide); in the line they follow the signals.
	if len(remote) == 0 && s.infoWaits(local, now) {
		return ""
	}

	act, info := s.splitInfo(local)
	local = append(act, info...)

	// Never into a draft: the owner's half-typed words would go out joined with the line. What zmina itself left of
	// a line that broke is not one. An input line that cannot be read is no empty one either, as for any role's queue
	// (deliverInboxOnce).
	s.clearLeftover(d)

	if draft, ok, err := s.Tmux.Prompt(d); err != nil || !ok || draft != "" {
		return ""
	}

	// Each signal goes with its id, and the line ends with how to ack them (watch notes have their own ack). What
	// the line is made of is kept (kept), for it to go back as it waited if it never lands.
	var (
		ids, fresh []string
		kept       []Part
	)

	part := func(sig string) string {
		p, id := s.signalPart(sig)
		k := Part{Signal: p, Count: s.State.SignalCounts[sig], Sent: s.State.SignalSent[sig]}
		if id != "" {
			ids = append(ids, id)
			k.IDs = []string{id}
			if !signalID.MatchString(sig) {
				fresh = append(fresh, id)
			}
		}
		kept = append(kept, k)

		return s.fyi(sig, repeated(p, s.State.SignalCounts[sig]))
	}

	var parts []string
	if alone {
		parts = []string{part(remote[0]) + s.lateness(remote[0], now)}
	} else {
		for _, sig := range local {
			parts = append(parts, part(sig))
		}
		parts = append(parts, notes...)

		if len(notes) > 0 {
			kept = append(kept, Part{Notes: slices.Clone(notes)})
		}
	}

	line := "[zmina] " + strings.Join(parts, " · ") + ackTail(ids)

	// Unlike a role's queued line, a line the dispatcher's typing broke on is never kept as failed, whatever the pane
	// shows of it: its signals and notes stay queued and the next try waits retryGap, however often it fails. A crew
	// that runs unattended is worse off with a dispatcher that hears nothing than with a signal told twice, and what the
	// line carries has ids (and the dispatcher's ack), which make a repeat recognisable. What it left in the input line
	// is cleared by typeLine all the same, and looked for again by the next try (clearLeftover).
	typedAt := s.Now()

	if err := s.typeLine(d, line); err != nil {
		s.State.Role(d).dispatchTried(now)

		for _, id := range fresh {
			delete(s.State.Acks, id)
		}

		return ""
	}

	s.State.Role(d).Sent, s.State.Role(d).TypedAt = &Sent{Line: line, At: typedAt, IDs: ids, Parts: kept}, now
	s.cardsAccepted(d, line, kept, AcceptedTyped, s.cardSession(d), typedAt)
	s.delivered(ids, now)
	s.stageIDs(ids, "submitted")

	if alone {
		delete(s.State.SignalSent, remote[0])
		delete(s.State.SignalCounts, remote[0])
		s.State.Signals = slices.DeleteFunc(slices.Clone(s.State.Signals), func(v string) bool { return v == remote[0] })
		s.State.RemoteBatches = 0
	} else {
		if len(remote) > 0 {
			s.State.RemoteBatches++
		} else {
			s.State.RemoteBatches = 0
		}
		for _, sig := range local {
			delete(s.State.SignalCounts, sig)
		}
		s.State.Signals, s.State.Notes = remote, nil
		s.State.NoticeMeta = nil
	}

	s.Told = true
	s.log(d, "told", "", line)

	return line
}

const (
	// remoteLateAfter is how long after it was sent a line from another crew is typed before it says when it is
	// delivered and how late.
	remoteLateAfter = 5 * time.Minute
	// staleAfter is how old such a line is before it is marked stale: what it reports may no longer hold.
	staleAfter = 6 * time.Hour
)

// Lateness is what a line from another crew carries after its closing quote, where its body cannot reach: when it
// was sent, and past remoteLateAfter when it is delivered and how late — " (sent 11:55, delivered 21:11, 9h late, stale)".
func Lateness(sent, now time.Time, loc *time.Location) string {
	if sent.IsZero() {
		return ""
	}

	sent, now = sent.In(loc), now.In(loc)

	clock := func(t time.Time) string {
		if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
			return t.Format("15:04")
		}

		return t.Format("Mon 2 Jan 15:04")
	}

	late := now.Sub(sent)
	if late < remoteLateAfter {
		return " (sent " + clock(sent) + ")"
	}

	out := " (sent " + clock(sent) + ", delivered " + now.Format("15:04") + ", " + span(late) + " late"
	if late >= staleAfter {
		out += ", stale"
	}

	return out + ")"
}

// span is a lateness in its biggest unit: 9h, 2d, 40m.
func span(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}

	return fmt.Sprintf("%dm", int(d/time.Minute))
}

func (s *Shift) lateness(signal string, now time.Time) string {
	return Lateness(s.State.SignalSent[signal], now, s.Crew.Location())
}

// IsRemote reports a signal from another crew (zmina tell): its source starts with "remote ".
func IsRemote(signal string) bool { return strings.HasPrefix(signal, "remote ") }

// Deliver types the waiting queue into the dispatcher when it is up, booted and idle, and returns the line.
func (s *Shift) Deliver() string { return s.deliver(s.Now()) }

// deliverInbox types a role's first waiting line when it is up, booted and idle; one line per pass, so the role
// reads each before the next. A line tmux refuses is tried typeTries times, at least countedGap apart, then moves to
// the failed list (zmina queue), so it never blocks the lines after it. Nothing goes to a role a slash command was
// just typed into (slashSettle). It returns a line for the log.
func (s *Shift) deliverInboxOnce(name string, now time.Time) string {
	rs := s.State.Role(name)

	// What the role fetched since the last pass is seen on its lines' cards (bell.go).
	s.syncBells(name)

	if rs.settling(now) {
		return ""
	}

	// Its last turn ended on an API error a moment ago: no line is posted or typed until the hold is over (failureHeld),
	// whoever asks, the beat, a pass or a turn's end.
	if s.failureHeld(name, now) && !s.ownerNext(name) {
		return ""
	}

	s.pruneTaskInbox(name)

	// A line posted to the inbox socket needs no empty input line and no Enter: it goes once the one before it
	// showed in the conversation and the role is between turns. A slash command is typed.
	if sess := s.native(name, now); sess != nil && rs.Sent == nil {
		pending := s.followPosted(name, now)

		switch {
		case len(rs.Inbox) == 0:
			return ""
		case s.postableFor(name, rs.Inbox[0]):
			if pending || !s.quiet(name, sess) {
				return ""
			}

			return s.postInbox(name, sess, now)
		case pending:
			// A slash command or the owner's own message is typed, below, once the lines posted before it showed.
			return ""
		}
	}

	if len(rs.Inbox) == 0 || !s.landed(name, now) || !s.atPrompt(name, now) {
		return ""
	}

	// Retiring an obsolete Sent record must not append a replacement packet to a preserved owner draft.
	// An unreadable input is ambiguous too; leave it alone until it is visibly empty. What zmina itself left of a
	// line that broke is not the owner's draft.
	s.clearLeftover(name)

	if draft, ok, err := s.Tmux.Prompt(name); err != nil || !ok || draft != "" {
		return ""
	}

	// A line that failed a moment ago is neither tried nor counted again yet.
	if rs.triedLately(now) {
		return ""
	}

	head := rs.Inbox[0]

	// More than one line that may be read together, or a long one, is typed as a doorbell, one line, and what it is for is
	// read with its command (bell.go).
	run := s.joinRun(name)
	marks := s.crossMarks(name, run)

	if b := s.bellFor(name, run, marks, true, now); b != nil {
		return s.typeBell(name, run, marks, b, now)
	}

	// The line's time is the moment its typing starts, not the beat's: a turn that ended earlier in this beat is not a
	// sign that the role took it (typedTaken).
	typedAt := s.Now()

	// It is typed behind its mark when it crossed a newer line of its reader's (crossing.go), and is the queue's line in
	// everything that follows: kept, failed, given back, retried and closed as it was written. What the input line holds
	// while it is not taken is that line behind the mark (holds).
	mark := s.crossingFor(name, head)
	typed := mark.withMark(head)

	if err := s.typeLine(name, typed); err != nil {
		// It may have reached the pane: it is not typed again by itself, on this try or any other.
		if typedMayHaveReached(err) {
			ids := s.takeIDs(name, head)
			rs.Inbox = rs.Inbox[1:]
			rs.clearTries()

			// A tracked line counts as delivered and has one way of being said again, its ack (renotify), as a typed
			// line whose session ended has (settleEnded): no failed record stands beside it, which a retry or a drop
			// would act on apart from the ack.
			if s.tracked(ids) {
				s.delivered(ids, now)
				s.log(name, "type-unclear", "", "may have reached the pane; its ack says it again: "+head)
				s.cardUnclearTracked(name, head, err.Error(), ids, now)

				return ""
			}

			s.failUnclear(name, Failed{Line: head, Error: err.Error(), At: now, IDs: ids})

			return name + ": a queued line may have reached its pane, kept in zmina queue as needing attention: " + head
		}

		rs.Attempts++
		rs.TriedAt = now
		s.deliveryFailure(name, head, err.Error())
		s.log(name, "type-failed", "", fmt.Sprintf("try %d: %s: %s", rs.Attempts, head, err))

		if rs.Attempts < typeTries {
			return ""
		}

		s.addFailed(name, Failed{Line: head, Error: err.Error(), At: now, IDs: s.takeIDs(name, head)})
		rs.Inbox = rs.Inbox[1:]
		rs.clearTries()

		return name + ": a queued line failed " + fmt.Sprint(typeTries) + " times, kept in zmina queue: " + head
	}

	rs.Inbox = rs.Inbox[1:]
	rs.clearTries()
	s.log(name, "typed", "", typed)
	s.cardCrossed(name, head, mark, now)
	s.cardAccept(name, head, AcceptedTyped, s.cardSession(name), typedAt)

	// The rename this start queued is typed: from now on the conversation runs under that name.
	if head == rs.QueuedRename && rs.RenameOf != "" {
		s.State.noteTitle(rs.RenameOf, strings.TrimPrefix(head, "/rename "))
		rs.QueuedRename, rs.RenameOf = "", ""
	}
	ids := s.takeIDs(name, head)
	s.delivered(ids, now)
	s.submittedTask(name, head)

	// A slash command may not change the status; only a line for the conversation is watched until it lands. A
	// command is waited for by its time instead (slashSettle).
	if strings.HasPrefix(head, "/") {
		rs.SlashAt = now
		s.cardClose(name, head, ClosedLanded, "a slash command: waited for by its time, not by a status", typedAt)
	} else {
		rs.Sent, rs.TypedAt = &Sent{Line: head, At: typedAt, IDs: ids}, now
	}

	return "typed into " + name + ": " + head
}

// postInbox posts a role's first waiting line to its inbox socket, with the lines after it that may be read with it as
// one numbered message when more than one waits (joinRun, postJoined). A post that never started is tried again next
// beat (typeTries beats, then kept as failed); one whose write broke is followed as posted (post), since the role may
// have it; any other error keeps it as failed at once.
func (s *Shift) postInbox(name string, sess *registry.Session, now time.Time) string {
	rs := s.State.Role(name)
	head := rs.Inbox[0]

	// A line that failed a moment ago is neither tried nor counted again yet. It holds the lines after it too: they go
	// in its post.
	if rs.triedLately(now) {
		return ""
	}

	// Several lines that may be read together go as one numbered message (join.go); one goes as it is. What is big goes
	// as a doorbell, one line or several (bell.go).
	run := s.joinRun(name)
	marks := s.crossMarks(name, run)

	if b := s.bellFor(name, run, marks, false, now); b != nil {
		return s.postJoined(name, sess, run, marks, b, now)
	}

	if len(run) > 1 {
		return s.postJoined(name, sess, run, marks, nil, now)
	}

	// One line goes as it was queued, behind its mark when it crossed a newer line of its reader's (crossing.go); what is
	// recorded of the post is the line.
	mark := s.crossingFor(name, head)
	out := mark.withMark(head)

	if err := s.postText(name, sess, out, head, out, now); err != nil {
		rs.Attempts++
		rs.TriedAt = now
		s.log(name, "post-failed", "", fmt.Sprintf("try %d: %s: %s", rs.Attempts, head, err))

		if unavailable(err) && rs.Attempts < typeTries {
			return ""
		}

		s.deliveryFailure(name, head, err.Error())
		s.addFailed(name, Failed{Line: head, Error: err.Error(), At: now, IDs: s.takeIDs(name, head)})
		rs.Inbox = rs.Inbox[1:]
		rs.clearTries()

		return name + ": a queued line could not be posted, kept in zmina queue: " + head
	}

	ids := s.takeIDs(name, head)
	rs.Posted[len(rs.Posted)-1].IDs = ids
	rs.Posted[len(rs.Posted)-1].Boot = rs.QueuedBoot != "" && head == rs.QueuedBoot

	rs.Inbox = rs.Inbox[1:]
	rs.clearTries()
	s.submittedTask(name, head)
	s.delivered(ids, now)
	s.cardCrossed(name, head, mark, now)
	s.cardAccept(name, head, s.acceptedBy(name), sess.SessionID, now)

	return "posted to " + name + ": " + head
}

// triedLately reports whether the role's first queued line failed a try less than countedGap ago.
func (rs *RoleState) triedLately(now time.Time) bool {
	return rs.Attempts > 0 && within(now, rs.TriedAt, countedGap)
}

// dispatchTried notes that a post or a typed line to the dispatcher failed now, so the next try waits retryGap
// (dispatchTriedLately). It counts nothing: the dispatcher's signals and notes stay queued however often it fails, and
// no line moves to the failed list for it. The stamp is its own, apart from the one a queued line's count keeps
// (TriedAt), so neither moves for a failure the other saw.
func (rs *RoleState) dispatchTried(now time.Time) { rs.DispatchTriedAt = now }

// dispatchTriedLately reports whether a post or a typed line to the dispatcher failed less than retryGap ago.
func (rs *RoleState) dispatchTriedLately(now time.Time) bool {
	return within(now, rs.DispatchTriedAt, retryGap)
}

// settling reports whether a slash command was typed into the role less than slashSettle ago.
func (rs *RoleState) settling(now time.Time) bool { return within(now, rs.SlashAt, slashSettle) }

// clearTries forgets the failed tries of the first queued line: it went, failed for good, or is gone.
func (rs *RoleState) clearTries() { rs.Attempts, rs.TriedAt = 0, time.Time{} }

// within reports whether at lies less than d before now. No stamp, or one after now (the clock moved back), is not
// within: a stamp must never hold a role for longer than d.
func within(now, at time.Time, d time.Duration) bool {
	if at.IsZero() {
		return false
	}

	gap := now.Sub(at)

	return gap >= 0 && gap < d
}

// FlushInbox attempts the ordinary safe idle delivery path for one role, without starting any roles.
// Call under the crew state lock. Busy roles, dialogs, drafts and unlanded messages retain their queue.
func (s *Shift) FlushInbox(name string) string { return s.deliverInbox(name, s.Now()) }

// DeliverTo gives a role what waits for it now, when it can take it, instead of at the next beat: the dispatcher its
// signals, notes and own queue (deliver, then deliverInbox, as the beat does), any other role its first queued line
// (deliverInbox). The beat's own rules decide whether it can: the role is up under zmina and booted, idle or able to
// take a post, no dialog and no draft in the way, the line before it shown or landed, the last failed try and the
// last slash command far enough back. It starts no role, trims nothing, tells nobody again, sweeps no record and
// writes neither LastTick nor Up: what it cannot give stays queued for the beat, which stays the safety net. Call it
// under the crew state lock, after the command has queued what it queued. It returns a line for the log, "" when
// nothing went.
func (s *Shift) DeliverTo(name string) string {
	if _, err := s.Role(name); err != nil {
		return ""
	}

	now := s.Now()

	if name == s.Crew.Dispatcher {
		if line := s.deliver(now); line != "" {
			return "told the dispatcher: " + line
		}
	}

	return s.deliverInbox(name, now)
}

// pruneAfterBeats is how many beats in a row a role must be missing from the crew file before its state is dropped:
// long enough for an edit, a merge or a rename and back to finish.
const pruneAfterBeats = 10

// pruneAbsent drops the state of a role the crew file no longer has, once it has been missing for pruneAfterBeats
// beats in a row, and keeps a copy in <state dir>/pruned/<role>-<time>.json. A role that comes back starts the
// count again.
func (s *Shift) pruneAbsent(now time.Time) {
	for _, name := range slices.Sorted(maps.Keys(s.State.Roles)) {
		rs := s.State.Roles[name]
		if s.Known(name) {
			rs.AbsentBeats = 0

			continue
		}

		if rs.AbsentBeats++; rs.AbsentBeats < pruneAfterBeats {
			continue
		}

		path, err := s.archiveRole(name, rs, now)
		if err != nil {
			if rs.AbsentBeats == pruneAfterBeats { // once, not every beat it keeps failing
				s.log(name, "prune-failed", "", err.Error())
			}

			continue
		}

		delete(s.State.Roles, name)
		s.log(name, "pruned", "", "missing from the crew file for "+strconv.Itoa(rs.AbsentBeats)+" beats; state kept in "+path)
	}
}

// archiveRole writes a role's state to <state dir>/pruned/<role>-<time>.json.
func (s *Shift) archiveRole(name string, rs *RoleState, now time.Time) (string, error) {
	dir := filepath.Join(s.Crew.StateDir(), "pruned")
	if err := fsx.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	raw, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		return "", err
	}

	safe := strings.NewReplacer("/", "_", "#", "_", string(filepath.Separator), "_").Replace(name)
	path := filepath.Join(dir, safe+"-"+now.UTC().Format("20060102-150405")+".json")

	return path, fsx.WriteFile(path, raw, 0o600)
}

// flush writes the state at once after an effect that cannot be taken back (a line typed or posted, a session
// started). A failed flush is told once and stops the beat's further effects: the state on disk no longer says
// what was done.
func (s *Shift) flush() {
	if s.Flush == nil || s.flushErr != nil {
		return
	}

	if err := s.Flush(); err != nil {
		s.flushErr = err
		if s.Warn != nil {
			s.Warn(fmt.Errorf("state not saved, nothing more typed this beat: %w", err))
		}

		return
	}

	// The cards of what the state now says are written with it: a command that fails after this flush has saved both,
	// and what it did after leaves neither.
	s.FlushCards()
}

// deliver types or posts the dispatcher's queue (deliverOnce) and saves at once what it did; after a failed flush
// nothing more is typed this beat.
func (s *Shift) deliver(now time.Time) string {
	defer s.FlushCards()

	if s.flushErr != nil {
		return ""
	}

	line := s.deliverOnce(now)
	if line != "" {
		s.flush()
	}

	return line
}

// deliverInbox types or posts a role's first waiting line (deliverInboxOnce) and saves at once what it did.
func (s *Shift) deliverInbox(name string, now time.Time) string {
	defer s.FlushCards()

	if s.flushErr != nil {
		return ""
	}

	line := s.deliverInboxOnce(name, now)
	if line != "" {
		s.flush()
	}

	return line
}
