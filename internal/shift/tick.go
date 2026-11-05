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

// Tick is one beat of the clock: it rings the alarms that are due (Rang: the caller runs their notify commands and
// tells), collects roles that ended (keeping their last screen), starts the always-roles
// that are down, runs schedules, notes roles busy or idle too long and seats past their expiry, releases roles idle
// past release_idle_minutes (Released: the caller finishes those downs), types waiting
// lines into idle roles and the queue into the dispatcher, and trims conversations past their context_max. A role tmux cannot be asked about is left alone for the
// beat. It returns what it did, for the log.
func (s *Shift) Tick() []string {
	now := s.Now()
	if s.ReconcileMessages != nil {
		if err := s.ReconcileMessages(s); err != nil && s.Warn != nil {
			s.Warn(err)
		}
	}
	for _, name := range s.Names() {
		s.pruneTaskInbox(name)
	}
	s.NotifyRecords()
	s.acceptanceAlerts()
	held := s.held()
	stopped := !s.State.Stopped.IsZero()
	s.State.LastTick = now

	// Alarms first: a role an alarm starts is up for the rest of the beat.
	did, notes := s.ring(now, stopped)

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

		if !up && !stopped && r.Keep == crew.KeepAlways && !rs.Held && !rs.Broken &&
			s.registryFor(name).LiveByName(r.Session) == nil && now.Sub(rs.Started) >= reopenPause {
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

		if note := s.idle(name, r, now, held[name]); note != "" {
			notes = append(notes, note)
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

		if up {
			notes = append(notes, s.watchScreen(name, now)...)

			if note := s.hooksNote(name, r); note != "" {
				notes = append(notes, note)
			}
		}
	}

	s.addNotes(notes)
	did = append(did, notes...)

	s.renotify(now)

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

	if limit == 0 || !s.ready(name, now) {
		return ""
	}

	sess := s.session(name)
	if sess == nil {
		return ""
	}

	if rs.TrimPending {
		turn, err := s.registryFor(name).Last(sess.SessionID)
		used := turn.Context

		if err != nil || used == 0 {
			return ""
		}

		rs.TrimPending, rs.TrimSession, rs.TrimBase = false, sess.SessionID, used
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

	turn, err := s.registryFor(name).Last(sess.SessionID)
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

	recoveryPath := ""
	if name == s.Crew.Dispatcher {
		var err error
		recoveryPath, _, err = s.RecoveryPack()
		if err != nil {
			return name + ": trim skipped: recovery pack: " + err.Error()
		}
	}
	// A failed try waits the pause too, so a role tmux refuses does not fill the log every beat.
	rs.Trimmed, rs.TrimPending = now, true
	detail := fmt.Sprintf("context %s over %s", Tokens(used), Tokens(limit))

	if err := s.Tmux.Type(name, "/"+do); err != nil {
		return fmt.Sprintf("%s: /%s (%s): %s", name, do, detail, err)
	}

	// The boot again, without the tag: queued, it is a line the log, the queue and the recovery pack show; the
	// clear's hook gives the tag.
	if boot := s.bootText(r, false); do == crew.ContextClear && !r.IsCodex() && boot != "" {
		rs.Inbox = append(rs.Inbox, boot)
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

	if turn, err := s.registryFor(name).Last(sess.SessionID); err != nil || !turn.Ended {
		return false
	}

	draft, ok, err := s.Tmux.Prompt(name)

	return err == nil && ok && draft == ""
}

// deliver gives the dispatcher what waits for it: the signals (work, with their ids), the lines of other crews and
// the health notes. Posted to its own channel, one post takes everything (postDispatcher); typed, it is one line when
// it is up, booted and idle (typeDispatcher).
func (s *Shift) deliver(now time.Time) string {
	d := s.Crew.Dispatcher
	s.pruneTaskInbox(d)

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
			if !postable(l) {
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

	var (
		lines, ids, fresh []string
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

	part := func(sig string) (string, string) {
		p, id := s.signalPart(sig)
		if id != "" && !signalID.MatchString(sig) {
			fresh = append(fresh, id)
		}

		return repeated(p, s.State.SignalCounts[sig]), id
	}

	nOwn := 0
	for _, l := range own {
		if !add(l) {
			break
		}
		nOwn++
	}

	// This crew's signals share a line, as many as fit.
	var parts, partIDs []string
	for _, sig := range local {
		p, id := part(sig)
		if len(lines) > 0 && size+len(p)+64 > postMax {
			full = true

			break
		}

		parts = append(parts, p)
		took[sig] = true
		size += len(p) + 3

		if id != "" {
			partIDs = append(partIDs, id)
		}
	}

	if len(parts) > 0 {
		lines = append(lines, "[zmina] "+strings.Join(parts, " · ")+ackTail(partIDs))
		ids = append(ids, partIDs...)
	}

	for _, sig := range remote {
		p, id := part(sig)
		var tail []string
		if id != "" {
			tail = []string{id}
		}

		if !add("[zmina] " + p + s.lateness(sig, now) + ackTail(tail)) {
			break
		}

		took[sig] = true
		ids = append(ids, tail...)
	}

	if len(notes) > 0 && add("[zmina] "+strings.Join(notes, " · ")) {
		took["\x00notes"] = true
	}

	if len(lines) == 0 {
		for _, id := range fresh {
			delete(s.State.Acks, id)
		}

		return ""
	}

	text := strings.Join(lines, "\n")

	if err := s.post(d, sess, text, now); err != nil {
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
		s.delivered(l, nil, now)
		s.submittedTask(d, l)
	}

	rs.Inbox = rs.Inbox[nOwn:]
	rs.Attempts = 0
	s.delivered(text, ids, now)

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

	// Never into a draft: the owner's half-typed words would go out joined with the line.
	if draft, ok, err := s.Tmux.Prompt(d); err == nil && ok && draft != "" {
		return ""
	}

	// Each signal goes with its id, and the line ends with how to ack them (watch notes have their own ack).
	var ids, fresh []string

	part := func(sig string) string {
		p, id := s.signalPart(sig)
		if id != "" {
			ids = append(ids, id)
			if !signalID.MatchString(sig) {
				fresh = append(fresh, id)
			}
		}

		return repeated(p, s.State.SignalCounts[sig])
	}

	var parts []string
	if alone {
		parts = []string{part(remote[0]) + s.lateness(remote[0], now)}
	} else {
		for _, sig := range local {
			parts = append(parts, part(sig))
		}
		parts = append(parts, notes...)
	}

	line := "[zmina] " + strings.Join(parts, " · ") + ackTail(ids)

	if err := s.typeLine(d, line); err != nil {
		for _, id := range fresh {
			delete(s.State.Acks, id)
		}

		return ""
	}

	s.State.Role(d).Sent = &Sent{Line: line, At: now}
	s.delivered(line, ids, now)
	s.markSubmitted(line)

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

// markSubmitted records the lines of other crews a delivered line carries as submitted (their receipts).
func (s *Shift) markSubmitted(line string) {
	for marker, note := range s.State.MessageLines {
		if note != "" && strings.Contains(line, note) {
			s.markerStage(marker, "submitted")
		}
	}
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

// deliverInbox types a role's first waiting line when it is up, booted and idle; one line per beat, so the role
// reads each before the next. A line tmux refuses is tried typeTries beats, then moves to the failed list (zmina
// queue), so it never blocks the lines after it. It returns a line for the log.
func (s *Shift) deliverInbox(name string, now time.Time) string {
	s.pruneTaskInbox(name)
	rs := s.State.Role(name)

	// A line posted to the inbox socket needs no empty input line and no Enter: it goes once the one before it
	// showed in the conversation and the role is between turns. A slash command is typed.
	if sess := s.native(name, now); sess != nil && rs.Sent == nil {
		pending := s.followPosted(name, now)

		switch {
		case len(rs.Inbox) == 0:
			return ""
		case postable(rs.Inbox[0]):
			if pending || !s.quiet(name, sess) {
				return ""
			}

			return s.postInbox(name, sess, now)
		case pending:
			// A slash command is typed, below, once the lines posted before it showed.
			return ""
		}
	}

	if len(rs.Inbox) == 0 || !s.landed(name, now) || !s.atPrompt(name, now) {
		return ""
	}

	// Retiring an obsolete Sent record must not append a replacement packet to a preserved owner draft.
	// An unreadable input is ambiguous too; leave it alone until it is visibly empty.
	if draft, ok, err := s.Tmux.Prompt(name); err != nil || !ok || draft != "" {
		return ""
	}

	head := rs.Inbox[0]

	if err := s.typeLine(name, head); err != nil {
		rs.Attempts++
		s.deliveryFailure(name, head, err.Error())
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
	s.delivered(head, nil, now)
	s.submittedTask(name, head)

	// A slash command may not change the status; only a line for the conversation is watched until it lands.
	if !strings.HasPrefix(head, "/") {
		rs.Sent = &Sent{Line: head, At: now}
	}

	return "typed into " + name + ": " + head
}

// postInbox posts a role's first waiting line to its inbox socket. A post that never started is tried again next beat
// (typeTries beats, then kept as failed); one whose write broke is followed as posted (post), since the role may have
// it; any other error keeps it as failed at once.
func (s *Shift) postInbox(name string, sess *registry.Session, now time.Time) string {
	rs := s.State.Role(name)
	head := rs.Inbox[0]

	if err := s.post(name, sess, head, now); err != nil {
		rs.Attempts++
		s.log(name, "post-failed", "", fmt.Sprintf("try %d: %s: %s", rs.Attempts, head, err))

		if unavailable(err) && rs.Attempts < typeTries {
			return ""
		}

		s.deliveryFailure(name, head, err.Error())
		rs.Failed = append(rs.Failed, Failed{Line: head, Error: err.Error(), At: now})
		rs.Inbox, rs.Attempts = rs.Inbox[1:], 0

		return name + ": a queued line could not be posted, kept in zmina queue: " + head
	}

	rs.Inbox, rs.Attempts = rs.Inbox[1:], 0
	s.submittedTask(name, head)
	s.delivered(head, nil, now)

	return "posted to " + name + ": " + head
}

// FlushInbox attempts the ordinary safe idle delivery path for one role, without starting any roles.
// Call under the crew state lock. Busy roles, dialogs, drafts and unlanded messages retain their queue.
func (s *Shift) FlushInbox(name string) string { return s.deliverInbox(name, s.Now()) }
