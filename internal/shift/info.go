package shift

import (
	"regexp"
	"slices"
	"strings"
	"time"
)

// A line made for the dispatcher has a class, given where the line is made and never read from its text:
//
//   - act, the default and everything not named below: it gets its ack id, is said again and is listed in the brief
//     and the recovery pack until the dispatcher acks it.
//   - info, a line that asks nothing of the dispatcher: a result ready (NotifyRecords), a role released (putDown), a
//     keep: always role started again (Tick). It is shown as `FYI …`, gets no ack id — so nothing says it again and
//     no brief or pack lists it — and is closed when its conversation shows the post that carried it (followPosted),
//     or, typed, when the typed line lands (landed).
//
// A line a role wrote itself and a line of another crew are never info. With info_lines: false no line is.
const (
	// infoMark opens an info line as the dispatcher reads it.
	infoMark = "FYI "
	// infoRide is how long info lines wait for a line the dispatcher is posted anyway, to go with it; then they go
	// alone.
	infoRide = 90 * time.Second
	// roleLineWindow is how long after a role's own line about a task a result of that task is not announced, and
	// how long a result's notice still queued gives way to the role's own line.
	roleLineWindow = 15 * time.Minute
	// resultReady is what a result's notice says after its task's id.
	resultReady = " result ready (worker-reported)"
	// infoRule is what the dispatcher's boot line says of info lines.
	infoRule = "A zmina line marked `FYI` carries no id and needs no ack."
)

// taskRef is a task's id in a line's text.
var taskRef = regexp.MustCompile(`\bT-[0-9]+\b`)

// markInfo makes a line for the dispatcher an info line, by the text it waits under; the first time counts.
func (s *Shift) markInfo(text string, now time.Time) {
	if !s.Crew.InfoLinesOn() {
		return
	}

	if s.State.Info == nil {
		s.State.Info = map[string]time.Time{}
	}

	if _, ok := s.State.Info[text]; !ok {
		s.State.Info[text] = now
	}
}

// isInfo reports whether a queued signal or note is an info line.
func (s *Shift) isInfo(text string) bool {
	_, ok := s.State.Info[text]

	return ok && s.Crew.InfoLinesOn()
}

// fyi is a queued signal or note as the dispatcher reads it: an info line behind its mark.
func (s *Shift) fyi(text, shown string) string {
	if s.isInfo(text) {
		return infoMark + shown
	}

	return shown
}

// splitInfo parts the signals to act on from the info lines, each in its order.
func (s *Shift) splitInfo(sigs []string) (act, info []string) {
	for _, sig := range sigs {
		if s.isInfo(sig) {
			info = append(info, sig)
		} else {
			act = append(act, sig)
		}
	}

	return act, info
}

// infoWaits reports whether all that waits for the dispatcher — these signals and the notes — is info lines the
// oldest of which was queued less than infoRide ago: they wait for a line to go with.
func (s *Shift) infoWaits(sigs []string, now time.Time) bool {
	all := append(slices.Clone(sigs), s.State.Notes...)
	if len(all) == 0 {
		return false
	}

	for _, text := range all {
		if !s.isInfo(text) || now.Sub(s.State.Info[text]) >= infoRide {
			return false
		}
	}

	return true
}

// closeInfo closes the info lines a post or a typed line carried, now that the dispatcher has it (how: seen, typed):
// nothing is kept of them but the event. A note is known by the mark it went with: its part keeps it as it was shown.
func (s *Shift) closeInfo(name string, parts []Part, how string) {
	for _, p := range parts {
		if p.Signal != "" && s.isInfo(p.Signal) {
			s.log(name, "closed", "", how+": "+p.Signal)
		}

		for _, n := range p.Notes {
			if s.Crew.InfoLinesOn() && strings.HasPrefix(n, infoMark) {
				s.log(name, "closed", "", how+": "+strings.TrimPrefix(n, infoMark))
			}
		}
	}
}

// pruneInfo forgets the class of the lines that wait nowhere any more: not queued, and in no line or post to the
// dispatcher that may still be given back.
func (s *Shift) pruneInfo() {
	if len(s.State.Info) == 0 {
		return
	}

	rs := s.State.Role(s.Crew.Dispatcher)
	parts := [][]Part{}

	if rs.Sent != nil {
		parts = append(parts, rs.Sent.Parts)
	}

	for _, p := range rs.Posted {
		parts = append(parts, p.Parts)
	}

	for _, f := range rs.Failed {
		parts = append(parts, f.Parts)
	}

	waits := func(text string) bool {
		if slices.Contains(s.State.Signals, text) || slices.Contains(s.State.Notes, text) {
			return true
		}

		for _, list := range parts {
			if slices.ContainsFunc(list, func(p Part) bool { return p.Signal == text }) {
				return true
			}
		}

		return false
	}

	for text := range s.State.Info {
		if !waits(text) {
			delete(s.State.Info, text)
		}
	}
}

// NoteRole is Note for a line a role wrote itself (zmina note, a closure's note): the line is one to act on, and the
// tasks it names are remembered (roleLine), so a result of one of them ready within roleLineWindow is not announced
// twice.
func (s *Shift) NoteRole(from, text string, ifUp bool) (bool, error) {
	sig, queued, err := s.note(from, text, ifUp, false)
	if err == nil {
		s.roleLine(from, text, sig)
	}

	return queued, err
}

// roleLine remembers the tasks a role's own line names, with the signal it waits as, and takes out of the
// dispatcher's signals the notices of that role's results of those tasks that were queued within roleLineWindow and
// have not gone yet: the role's own line tells it. Only for a role of this crew, and only with info_lines on.
func (s *Shift) roleLine(from, text, sig string) {
	if _, err := s.Role(from); err != nil || !s.Crew.InfoLinesOn() {
		return
	}

	now := s.Now()

	for key, l := range s.State.RoleLines {
		if now.Sub(l.At) >= roleLineWindow && !slices.Contains(s.State.Signals, l.Signal) {
			delete(s.State.RoleLines, key)
		}
	}

	for _, id := range taskRef.FindAllString(text, -1) {
		if s.State.RoleLines == nil {
			s.State.RoleLines = map[string]RoleLine{}
		}

		s.State.RoleLines[from+" "+id] = RoleLine{Signal: sig, At: now}

		for _, queued := range slices.Clone(s.State.Signals) {
			if s.isInfo(queued) && strings.HasPrefix(queued, from+": "+id+resultReady) &&
				now.Sub(s.State.Info[queued]) < roleLineWindow {
				s.dropSignal(queued)
				delete(s.State.Info, queued)
				s.log(from, "result-told", id, "its own line names "+id+", the result's notice is dropped: "+queued)
			}
		}
	}
}

// roleLineWent stamps a role's own line as gone to the dispatcher now: the window runs from when it went.
func (s *Shift) roleLineWent(sig string, now time.Time) {
	for key, l := range s.State.RoleLines {
		if l.Signal == sig {
			l.At = now
			s.State.RoleLines[key] = l
		}
	}
}

// roleTold reports whether a role's own line that names the task still waits in the dispatcher's signals, or was
// queued or went to it within roleLineWindow.
func (s *Shift) roleTold(role, id string, now time.Time) bool {
	l, ok := s.State.RoleLines[role+" "+id]

	return ok && s.Crew.InfoLinesOn() && (now.Sub(l.At) < roleLineWindow || slices.Contains(s.State.Signals, l.Signal))
}
