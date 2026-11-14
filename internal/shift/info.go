package shift

import (
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
	// infoRideQuiet is how long a quiet info line waits for such a line: a role released holding nothing, a keep:
	// always role started again. Nothing follows from either, zmina ls shows both, and a turn of the dispatcher's
	// own to read one costs more than the line is worth; after this long it goes alone all the same, so none is
	// kept for ever.
	infoRideQuiet = 6 * time.Hour
	// resultReady is what a result's notice says after its task's id.
	resultReady = " result ready (worker-reported)"
	// infoRule is what the dispatcher's boot line says of info lines.
	infoRule = "A zmina line marked `FYI` carries no id and needs no ack."
)

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

// markQuiet makes a line an info line that waits long for a line to go with: infoRideQuiet, not infoRide. It is
// kept as an info line queued that much later, so its ride ends then: the state keeps no second mark, and a zmina
// that knows nothing of quiet lines reads it as it is meant.
func (s *Shift) markQuiet(text string, now time.Time) {
	s.markInfo(text, now.Add(infoRideQuiet-infoRide))
}

// ridingOnly reports whether all that waits for the dispatcher is info lines still waiting for a line to go with:
// nothing of it is due, so it holds nothing up (a trim of the dispatcher, the courier's look at a gated queue).
func (s *Shift) ridingOnly(now time.Time) bool { return s.infoWaits(s.State.Signals, now) }

// RidingOnly is ridingOnly at the shift's clock, for the courier.
func (s *Shift) RidingOnly() bool { return s.ridingOnly(s.Now()) }

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
// each of which has not waited its ride out (infoRide; infoRideQuiet for a quiet one, see markQuiet): they wait for
// a line to go with. One that has waited its ride out takes the others with it.
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
