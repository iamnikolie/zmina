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

// beatGapMax is how long since the beat before is a gap: the machine slept or the ticker stood still. The beat after
// one judges no in-flight record by age (a line posted to a busy role may show seconds later, and a record's age was
// measured by a clock nobody watched); the next ordinary beat does.
const beatGapMax = 5 * time.Minute

// sweepIDsShown is how many ids the event and the dispatcher's line of a sweep name; the rest are counted.
const sweepIDsShown = 8

// sweepDelivery drops the delivery records past their age limit: posted and typed lines of a role the beat found up (up)
// past inFlightMaxAge, failed lines past failedMaxAge. One event names how many, of which kinds and the ids (clipped), and one FYI line tells the
// dispatcher (an info line: no id, nothing says it again). It returns that line, none when nothing aged. Lines that
// wait in a role's inbox are not records of this kind and stay, whatever the role is doing. The tracked line of a
// failed record that was never delivered loses its ack with it (as dropSignal does): nothing queued would carry it;
// the ack of a delivered one stays, listed by zmina pending and said again.
func (s *Shift) sweepDelivery(now time.Time, up map[string]bool) []string {
	var (
		roles   = map[string]bool{}
		ids     []string
		perKind = map[string]int{}
		total   int
	)

	note := func(name, kind string, at time.Time, carried []string, limit time.Duration) bool {
		if !aged(now, at, limit) {
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
		ch, dir := s.peerFor(name)

		// The in-flight records of a role are judged while this beat probed it up (up: the names the beat's loop saw up,
		// never the stored flag, which is what an earlier beat left when the loop skipped the role because tmux did not
		// answer). One that is not up is settleEnded's: it gives its lines back or drops them by their kind, a beat later
		// than the one that saw it end, and a sweep that ran in between would report as dropped what that beat gives
		// back.
		if up[name] && rs.Up && rs.Downing.IsZero() {
			// Look before judging: a record this old may be one nothing has looked at since (the beats stopped, the
			// machine slept). A line the conversation did show closes as it always does (followPosted), a typed one that
			// landed as it always does (closeTyped), and neither is reported as dropped.
			if slices.ContainsFunc(rs.Posted, func(p Posted) bool { return aged(now, p.At, inFlightMaxAge) }) {
				s.followPosted(name, now)
			}

			// A typed line still in the input line is not judged: dropping its record would leave the text there, which
			// the next delivery takes for a draft and waits behind for ever. landed finishes it when the next line comes.
			inInput := false

			if sent := rs.Sent; sent != nil && aged(now, sent.At, inFlightMaxAge) {
				landed, unsent := s.typedLanded(name, sent)
				if landed {
					s.closeTyped(name)
				}

				inInput = unsent
			}

			rs.Posted = slices.DeleteFunc(rs.Posted, func(p Posted) bool {
				// A line waiting in a bridge's inbox waits for the next bridge, as an inbox line waits for its role.
				return !stillQueued(ch, dir, p) && note(name, "posted", p.At, p.IDs, inFlightMaxAge)
			})

			if rs.Sent != nil && !inInput && note(name, "sent", rs.Sent.At, rs.Sent.IDs, inFlightMaxAge) {
				rs.Sent = nil
			}
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

// aged reports a record stamped at, past limit by now; one with no stamp is not aged: its age is not known.
func aged(now, at time.Time, limit time.Duration) bool { return !at.IsZero() && now.Sub(at) >= limit }

// dropUndelivered drops the acks of lines that were never delivered, which no queue carries any more.
func (s *Shift) dropUndelivered(ids []string) {
	for _, id := range ids {
		if a := s.State.Acks[id]; a != nil && a.First.IsZero() {
			delete(s.State.Acks, id)
		}
	}
}
