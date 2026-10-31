package shift

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
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
	trimIdle  = 5 * time.Minute
	trimPause = 30 * time.Minute
)

// Tick is one beat of the clock: it collects roles that ended (keeping their last screen), starts the always-roles
// that are down, runs schedules, notes roles busy or idle too long and seats past their expiry, types waiting
// lines into idle roles and the queue into the dispatcher, and trims conversations past their context_max. A role tmux cannot be asked about is left alone for the
// beat. It returns what it did, for the log.
func (s *Shift) Tick() []string {
	now := s.Now()
	held := s.held()
	stopped := !s.State.Stopped.IsZero()
	s.State.LastTick = now

	var did, notes []string

	for _, name := range s.Names() {
		r, err := s.Role(name)
		if err != nil {
			continue
		}

		rs := s.State.Role(name)

		if !rs.Downing.IsZero() && now.Sub(rs.Downing) < downingFor {
			continue
		}

		p, err := s.probe(name)
		if err != nil {
			did = append(did, name+": left alone this beat, tmux did not answer: "+err.Error())

			continue
		}

		up := p.State == tmux.Up
		if up {
			s.remember(name, p)
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

		if !up && !stopped && r.Keep == crew.KeepAlways && !rs.Held && !rs.Broken &&
			s.Reg.LiveByName(r.Session) == nil && now.Sub(rs.Started) >= reopenPause {
			if err := s.start(name, UpOptions{}); err != nil {
				// Wait the usual pause before the next try, so a role that cannot start does not fill the log.
				rs.Started = now
				did = append(did, "start "+name+": "+err.Error())
			} else {
				up = true

				did = append(did, "started "+name)
				if name != s.Crew.Dispatcher {
					notes = append(notes, name+": started again (keep: always)")
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

		if note := s.idle(name, now, held[name]); note != "" {
			notes = append(notes, note)
		}

		if note := s.expired(name, r, now, up); note != "" {
			notes = append(notes, note)
		}

		if up {
			notes = append(notes, s.watchScreen(name, now)...)
		}
	}

	s.addNotes(notes)
	did = append(did, notes...)

	typed := map[string]bool{}
	if line := s.deliver(now); line != "" {
		typed[s.Crew.Dispatcher] = true
		did = append(did, "told the dispatcher: "+line)
	}

	for _, name := range s.Names() {
		if typed[name] {
			continue
		}

		if line := s.deliverInbox(name, now); line != "" {
			typed[name] = true
			did = append(did, line)
		}
	}

	for _, name := range s.Names() {
		if r, err := s.Role(name); err == nil && !typed[name] {
			if line := s.trim(name, r, now, held[name]); line != "" {
				did = append(did, line)
			}
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

	if limit == 0 || !s.ready(name, now) {
		return ""
	}

	sess := s.session(name)
	if sess == nil {
		return ""
	}

	if rs.TrimPending {
		turn, err := s.Reg.Last(sess.SessionID)
		used := turn.Context

		if err != nil || used == 0 {
			return ""
		}

		rs.TrimPending, rs.TrimSession, rs.TrimBase = false, sess.SessionID, used
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

	do := r.ContextDo()
	if do == crew.ContextClear && len(tasks) > 0 {
		do = crew.ContextCompact
	}

	wait := trimIdle
	if do == crew.ContextClear {
		wait = max(wait, time.Duration(s.Crew.IdleMinutes)*time.Minute)
	}

	if now.Sub(sess.StatusSince()) < wait {
		return ""
	}

	turn, err := s.Reg.Last(sess.SessionID)
	used := turn.Context

	if err != nil || used <= limit {
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

	// A failed try waits the pause too, so a role tmux refuses does not fill the log every beat.
	rs.Trimmed, rs.TrimPending = now, true
	detail := fmt.Sprintf("context %s over %s", Tokens(used), Tokens(limit))

	if err := s.Tmux.Type(name, "/"+do); err != nil {
		return fmt.Sprintf("%s: /%s (%s): %s", name, do, detail, err)
	}

	if boot := s.BootText(r); do == crew.ContextClear && boot != "" {
		rs.Inbox = append(rs.Inbox, boot)
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

// addNotes queues health notes for the dispatcher: a note already waiting is not added twice, and past notesMax the
// oldest health notes go. Signals (zmina note, watches) are a separate queue and never go this way.
func (s *Shift) addNotes(notes []string) {
	for _, n := range notes {
		if !slices.Contains(s.State.Notes, n) {
			s.State.Notes = append(s.State.Notes, n)
		}
	}

	if len(s.State.Notes) > notesMax {
		s.State.Notes = s.State.Notes[len(s.State.Notes)-notesMax:]
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}

	base := strings.ReplaceAll(name, "#", "-")
	file := filepath.Join(dir, base+"-"+now.Format("20060102-150405")+".txt")

	if err := os.WriteFile(file, []byte(text+"\n"), 0o600); err != nil {
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

	if rs.NextRun.IsZero() || rs.NextSpec != r.Schedule {
		// First sight, or the schedule changed: count from now.
		rs.NextRun, rs.NextSpec = sched.Next(now), r.Schedule

		return "", "", false
	}

	if now.Before(rs.NextRun) {
		return "", "", false
	}

	rs.NextRun = sched.Next(now)

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

	rs.Inbox = append(rs.Inbox, queued)
	s.log(name, "scheduled", t.ID, r.Prompt)

	if up || rs.Broken || s.Reg.LiveByName(r.Session) != nil {
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
// the dispatcher may put it down.
func (s *Shift) idle(name string, now time.Time, tasks []string) string {
	rs := s.State.Role(name)
	if name == s.Crew.Dispatcher || len(tasks) > 0 || len(rs.Inbox) > 0 {
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

// session is the registry entry of a role up under zmina, nil when it is not up or has not written one.
func (s *Shift) session(name string) *registry.Session {
	if !s.State.Role(name).Up {
		return nil
	}

	p, err := s.probe(name)
	if err != nil || p.State != tmux.Up {
		return nil
	}

	sess, _ := s.Reg.ByPID(p.PID)

	return sess
}

// ready reports whether a role is up, booted and idle, so a typed line lands at its prompt.
func (s *Shift) ready(name string, now time.Time) bool {
	if now.Sub(s.State.Role(name).Started) < s.Crew.BootGraceD() {
		return false
	}

	sess := s.session(name)

	return sess != nil && sess.Status == "idle"
}

// deliver types the waiting signals and health notes into the dispatcher as one line when it is up, booted and idle.
// Signals go first: they are work.
func (s *Shift) deliver(now time.Time) string {
	d := s.Crew.Dispatcher

	// A line from another crew goes as a line of its own, never joined with this crew's signals and notes: one
	// per beat, after them.
	var local, remote []string

	for _, sig := range s.State.Signals {
		if IsRemote(sig) {
			remote = append(remote, sig)
		} else {
			local = append(local, sig)
		}
	}

	parts := append(local, s.State.Notes...)
	if len(parts) == 0 && len(remote) > 0 {
		parts = remote[:1]
	}

	// A line queued for the dispatcher itself goes first: after a clear it is the boot line, and the signals
	// belong to a dispatcher that knows its job.
	if len(parts) == 0 || len(s.State.Role(d).Inbox) > 0 || !s.ready(d, now) || !s.landed(d, now) {
		return ""
	}

	// Never into a draft: the owner's half-typed words would go out joined with the line.
	if draft, ok, err := s.Tmux.Prompt(d); err == nil && ok && draft != "" {
		return ""
	}

	line := "[zmina] " + strings.Join(parts, " · ")
	if err := s.Tmux.Type(d, line); err != nil {
		return ""
	}

	s.State.Role(d).Sent = &Sent{Line: line, At: now}

	if len(local)+len(s.State.Notes) > 0 {
		s.State.Signals, s.State.Notes = remote, nil
	} else {
		s.State.Signals = slices.Clone(remote[1:])
	}
	s.Told = true
	s.log(d, "told", "", line)

	return line
}

// IsRemote reports a signal from another crew (zmina tell): its source starts with "remote ".
func IsRemote(signal string) bool { return strings.HasPrefix(signal, "remote ") }

// Deliver types the waiting queue into the dispatcher when it is up, booted and idle, and returns the line.
func (s *Shift) Deliver() string { return s.deliver(s.Now()) }

// deliverInbox types a role's first waiting line when it is up, booted and idle; one line per beat, so the role
// reads each before the next. A line tmux refuses is tried typeTries beats, then moves to the failed list (zmina
// queue), so it never blocks the lines after it. It returns a line for the log.
func (s *Shift) deliverInbox(name string, now time.Time) string {
	rs := s.State.Role(name)
	if len(rs.Inbox) == 0 || !s.ready(name, now) || !s.landed(name, now) || len(rs.Inbox) == 0 {
		return ""
	}

	head := rs.Inbox[0]

	if err := s.Tmux.Type(name, head); err != nil {
		rs.Attempts++
		s.log(name, "type-failed", "", fmt.Sprintf("try %d: %s: %s", rs.Attempts, head, err))

		if rs.Attempts < typeTries {
			return ""
		}

		rs.Failed = append(rs.Failed, Failed{Line: head, Error: err.Error(), At: now})
		rs.Inbox, rs.Attempts = rs.Inbox[1:], 0

		return name + ": a queued line failed " + fmt.Sprint(typeTries) + " times, kept in zmina queue: " + head
	}

	rs.Inbox, rs.Attempts = rs.Inbox[1:], 0
	s.log(name, "typed", "", head)

	// A slash command may not change the status; only a line for the conversation is watched until it lands.
	if !strings.HasPrefix(head, "/") {
		rs.Sent = &Sent{Line: head, At: now}
	}

	return "typed into " + name + ": " + head
}
