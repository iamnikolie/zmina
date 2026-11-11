package shift

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Nothing else ends a delivery record that no session settles (a posted line whose session never came back, a failed
// line nobody retried): past its age limit the beat drops it, told once (sweepDelivery). Lines that wait in a role's
// inbox are not such records: a role that is down simply has them waiting.

// Age limits of the records a beat drops (sweepDelivery). Variables so that tests can shorten them.
var (
	// inFlightMaxAge is how long a posted or typed line may stay unsettled: past postedWait it fails on its own, so a
	// record this old is one nothing follows (a session whose registry entry never came back, a role that never idles).
	inFlightMaxAge = 24 * time.Hour
	// failedMaxAge is how long a failed line is kept for zmina queue retry or drop.
	failedMaxAge = 7 * 24 * time.Hour
)

// sweepIDsShown is how many ids the event and the dispatcher's line of a sweep name; the rest are counted.
const sweepIDsShown = 8

// sweepDelivery drops the delivery records past their age limit: posted and typed lines past inFlightMaxAge, failed
// lines past failedMaxAge. One event names how many, of which kinds and the ids (clipped), and one FYI line tells the
// dispatcher (an info line: no id, nothing says it again). It returns that line, none when nothing aged. Lines that
// wait in a role's inbox are not records of this kind and stay, whatever the role is doing. The tracked line of a
// failed record that was never delivered loses its ack with it (as dropSignal does): nothing queued would carry it;
// the ack of a delivered one stays, listed by zmina pending and said again.
func (s *Shift) sweepDelivery(now time.Time) []string {
	var (
		roles   = map[string]bool{}
		ids     []string
		perKind = map[string]int{}
		total   int
	)

	note := func(name, kind string, at time.Time, carried []string, limit time.Duration) bool {
		if at.IsZero() || now.Sub(at) < limit {
			return false
		}

		roles[name] = true
		ids = append(ids, carried...)
		perKind[kind]++
		total++

		if kind == "failed" {
			s.dropUndelivered(carried)
		}

		return true
	}

	for _, name := range slices.Sorted(maps.Keys(s.State.Roles)) {
		rs := s.State.Roles[name]

		rs.Posted = slices.DeleteFunc(rs.Posted, func(p Posted) bool {
			return note(name, "posted", p.At, p.IDs, inFlightMaxAge)
		})

		if rs.Sent != nil && note(name, "sent", rs.Sent.At, rs.Sent.IDs, inFlightMaxAge) {
			rs.Sent = nil
		}

		rs.Failed = slices.DeleteFunc(rs.Failed, func(f Failed) bool {
			return note(name, "failed", f.At, f.IDs, failedMaxAge)
		})
	}

	if total == 0 {
		return nil
	}

	var kinds []string

	for _, kind := range []string{"posted", "sent", "failed"} {
		if n := perKind[kind]; n > 0 {
			kinds = append(kinds, strconv.Itoa(n)+" "+kind)
		}
	}

	noun := "records"
	if total == 1 {
		noun = "record"
	}

	text := fmt.Sprintf("delivery: dropped %d %s past their age limit (%s) for %s", total, noun, strings.Join(kinds, ", "),
		strings.Join(slices.Sorted(maps.Keys(roles)), ", "))

	if len(ids) > 0 {
		shown, more := ids, ""

		if len(shown) > sweepIDsShown {
			shown, more = shown[:sweepIDsShown], " and "+strconv.Itoa(len(ids)-sweepIDsShown)+" more"
		}

		text += ": " + strings.Join(shown, " ") + more
	}

	s.log("", "delivery-aged", "", text)
	s.markInfo(text, now)

	return []string{text}
}

// dropUndelivered drops the acks of lines that were never delivered, which no queue carries any more.
func (s *Shift) dropUndelivered(ids []string) {
	for _, id := range ids {
		if a := s.State.Acks[id]; a != nil && a.First.IsZero() {
			delete(s.State.Acks, id)
		}
	}
}
