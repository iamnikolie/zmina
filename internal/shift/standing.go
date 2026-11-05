package shift

import (
	"fmt"
	"strings"
	"time"
)

// StaleAfter is how long a crew may have stood still before what its ledger shows is said to be possibly behind.
const StaleAfter = time.Hour

// Standing is what anyone reading this crew must know before they report its ledger as the state of the work: that
// the owner retired it and where the live crew is, or that it has stood still and since when. Empty for a crew at
// work. Other crews of its name are named as that and no more: one name does not make them one crew.
func (s *Shift) Standing() string {
	if s.Crew.IsRetired() {
		return s.Crew.RetiredLine() + ". Its ledger, log and queues are not current: never report them as the state of the work."
	}

	now := s.Now()

	var line string

	stop := s.State.Stopped
	if stop.IsZero() {
		stop = s.lastStop
	}

	last := s.lastBeat
	if last.IsZero() {
		last = s.State.LastTick
	}

	switch {
	case !stop.IsZero() && now.Sub(stop) > StaleAfter:
		line = fmt.Sprintf("This crew's shift was stopped on %s, %s ago.", s.clock(stop), spanWords(now.Sub(stop)))
	case !last.IsZero() && now.Sub(last) > StaleAfter:
		line = fmt.Sprintf("This crew's ticker last beat on %s, %s ago.", s.clock(last), spanWords(now.Sub(last)))
	default:
		return ""
	}

	line += " What its ledger and log show may stop there and the work may have gone on elsewhere: look before you " +
		"report it as standing still (zmina node ls)."

	if s.OtherCrews != nil {
		if others := s.OtherCrews(); len(others) > 0 {
			line += " Other crews of this name: " + strings.Join(others, ", ") + "; one name does not prove one crew."
		}
	}

	return line
}

func spanWords(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}

	return fmt.Sprintf("%dm", int(d.Minutes()))
}
