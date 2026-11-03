package shift

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Ack is a line zmina delivered that waits for its reader's ack: a signal for the dispatcher, a queued line or an
// alarm for a role. Until it is acked it is said again (renotify), listed in the recovery pack and given back after a
// compaction (zmina pending); a task packet has its own acceptance and is not one.
type Ack struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	// Text is the signal (for the dispatcher) or the line, without its id.
	Text string `json:"text"`
	// First and Last are when it was delivered first and last; zero while it waits in a queue.
	First   time.Time `json:"first,omitzero"`
	Last    time.Time `json:"last,omitzero"`
	Renotes int       `json:"renotes,omitempty"`
}

const (
	// acksMax is how many unacked lines are kept; past it the oldest is dropped, logged.
	acksMax = 256
	// renotesMax is how many times a line is said again; then it stays listed (pending, the pack) and is not said.
	renotesMax = 3
)

var (
	// signalID is the id at the head of a dispatcher's signal part: "S-12 mrs: …".
	signalID = regexp.MustCompile(`^(S-[0-9]+) `)
	// lineID is the id in a queued line's tail: "… (zmina S-14)", "… (zmina S-14, still pending since …)".
	lineID = regexp.MustCompile(`\(zmina (S-[0-9]+)[,)]`)
)

// signalBody is a signal without the id and the "still pending" mark it carries once it was delivered.
func signalBody(sig string) string {
	if m := signalID.FindStringSubmatch(sig); m != nil {
		sig = strings.TrimPrefix(sig, m[0])
		if rest, ok := strings.CutPrefix(sig, "(still pending since "); ok {
			if _, after, ok := strings.Cut(rest, ") "); ok {
				sig = after
			}
		}
	}

	return sig
}

// newAck records a line that waits for an ack and returns its id.
func (s *Shift) newAck(role, text string) string {
	if s.State.Acks == nil {
		s.State.Acks = map[string]*Ack{}
	}

	if len(s.State.Acks) >= acksMax {
		oldest := slices.MinFunc(slices.Collect(maps.Values(s.State.Acks)), func(a, b *Ack) int {
			return ackNum(a.ID) - ackNum(b.ID)
		})
		delete(s.State.Acks, oldest.ID)
		s.log(oldest.Role, "ack-dropped", "", oldest.ID+" (over "+strconv.Itoa(acksMax)+" unacked): "+oldest.Text)
	}

	s.State.AckSeq++
	id := "S-" + strconv.Itoa(s.State.AckSeq)
	s.State.Acks[id] = &Ack{ID: id, Role: role, Text: text}

	return id
}

func ackNum(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "S-"))

	return n
}

// lineSuffix is what a tracked line carries: its id, and since when it waits once it is said again. The line names
// no command: the role's own boot line says how to ack (a command inside a message from another session reads as
// that session asking for it).
func (s *Shift) lineSuffix(a *Ack) string {
	since := ""
	if !a.First.IsZero() {
		since = ", still pending since " + s.clock(a.First)
	}

	return " (zmina " + a.ID + since + ")"
}

// clock is a time as a line shows it, on the crew's clock.
func (s *Shift) clock(t time.Time) string { return t.In(s.Crew.Location()).Format("Mon 2 Jan 15:04") }

// acking reports whether a role's lines wait for its ack: it runs, or will run, with zmina's settings and the boot line
// that tells it of acks (socket delivery). A role started by an earlier zmina is not told, and is left as it was.
func (s *Shift) acking(name string) bool {
	if s.has(name) {
		return s.State.Role(name).Inbound
	}

	r, err := s.Role(name)

	return err == nil && s.inbound(r)
}

// trackLine gives a queued line its id when its role acks; it returns the line to queue.
func (s *Shift) trackLine(role, line string) string {
	if !s.acking(role) {
		return line
	}

	a := s.State.Acks[s.newAck(role, line)]

	return line + s.lineSuffix(a)
}

// delivered marks the tracked line or signal parts in a delivered line as delivered now.
func (s *Shift) delivered(line string, ids []string, now time.Time) {
	if m := lineID.FindStringSubmatch(line); m != nil {
		ids = append(ids, m[1])
	}

	for _, id := range ids {
		if a := s.State.Acks[id]; a != nil {
			if a.First.IsZero() {
				a.First = now
			}

			a.Last = now
		}
	}
}

// signalPart is a dispatcher's signal as its line carries it: with its id, a new one for a signal never delivered
// (watch notes have their own ack, and are left as they are). It returns the part and its id, "" for none.
func (s *Shift) signalPart(sig string) (string, string) {
	if m := signalID.FindStringSubmatch(sig); m != nil {
		return sig, m[1]
	}

	if src, _, _ := strings.Cut(sig, ": "); s.Crew.Watches[src] != nil || !s.acking(s.Crew.Dispatcher) {
		return sig, ""
	}

	id := s.newAck(s.Crew.Dispatcher, sig)

	return id + " " + sig, id
}

// ackTail is the end of the dispatcher's line: which of its signals wait for the ack.
func ackTail(ids []string) string {
	if len(ids) == 0 {
		return ""
	}

	return " (zmina " + strings.Join(ids, " ") + ")"
}

// renotify says again each line that waits for an ack past the crew's renotify since it was last delivered, while
// its role is up and the line is not queued or in flight: a signal goes back to the dispatcher's signals, a line to
// its role's queue. After renotesMax times it is only listed.
func (s *Shift) renotify(now time.Time) {
	every := s.Crew.RenotifyD()
	if every <= 0 {
		return
	}

	for _, id := range slices.SortedFunc(maps.Keys(s.State.Acks), func(a, b string) int { return ackNum(a) - ackNum(b) }) {
		a := s.State.Acks[id]
		if a.Last.IsZero() || now.Sub(a.Last) < every || a.Renotes >= renotesMax || !s.has(a.Role) || s.inFlight(a) {
			continue
		}

		a.Renotes++
		a.Last = now

		if a.Role == s.Crew.Dispatcher {
			s.State.Signals = append(s.State.Signals, a.ID+" (still pending since "+s.clock(a.First)+") "+a.Text)
		} else {
			rs := s.State.Role(a.Role)
			rs.Inbox = append(rs.Inbox, a.Text+s.lineSuffix(a))
		}

		s.log(a.Role, "renoted", "", a.ID+" "+a.Text)
	}
}

// inFlight reports whether a tracked line is queued, typed but not landed, or posted but not seen.
func (s *Shift) inFlight(a *Ack) bool {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(a.ID) + `\b`)

	if a.Role == s.Crew.Dispatcher && slices.ContainsFunc(s.State.Signals, re.MatchString) {
		return true
	}

	rs := s.State.Role(a.Role)
	if slices.ContainsFunc(rs.Inbox, re.MatchString) || rs.Sent != nil && re.MatchString(rs.Sent.Line) {
		return true
	}

	return slices.ContainsFunc(rs.Posted, func(p Posted) bool { return re.MatchString(p.Line) })
}

// AckLines acks lines by id — every one of them resolved first: an unknown id fails the call and acks nothing — or
// with all every line of the role. It returns the ids acked.
func (s *Shift) AckLines(ids []string, all bool, role string) ([]string, error) {
	if all {
		ids = nil

		for _, a := range s.State.Acks {
			if a.Role == role {
				ids = append(ids, a.ID)
			}
		}
	}

	var acked []string

	for _, id := range ids {
		id = strings.ToUpper(strings.TrimSpace(id))
		if !strings.HasPrefix(id, "S-") {
			id = "S-" + id
		}

		if s.State.Acks[id] == nil {
			return nil, fmt.Errorf("%s waits for no ack (zmina pending lists the ones that do)", id)
		}

		if !slices.Contains(acked, id) {
			acked = append(acked, id)
		}
	}

	slices.SortFunc(acked, func(a, b string) int { return ackNum(a) - ackNum(b) })

	for _, id := range acked {
		a := s.State.Acks[id]
		delete(s.State.Acks, id)
		s.log(a.Role, "acked", "", id+" "+a.Text)
	}

	return acked, nil
}

// Pending is the lines that wait for an ack, oldest first; one role's, or every role's when role is empty.
func (s *Shift) Pending(role string) []Ack {
	var out []Ack

	for _, a := range s.State.Acks {
		if role == "" || a.Role == role {
			out = append(out, *a)
		}
	}

	slices.SortFunc(out, func(a, b Ack) int { return ackNum(a.ID) - ackNum(b.ID) })

	return out
}

// PendingBrief is what a role is told after its context was compacted or cleared: the lines it has not acked, and
// for the dispatcher the watch keys that wait. Empty when nothing waits.
func (s *Shift) PendingBrief(role string) string {
	var parts []string

	for _, a := range s.Pending(role) {
		if a.First.IsZero() {
			continue // still queued: it comes anyway
		}

		parts = append(parts, a.ID+" (since "+s.clock(a.First)+") "+a.Text)
	}

	if role == s.Crew.Dispatcher {
		for _, k := range s.WaitingKeys() {
			parts = append(parts, "watch "+k.Watch+": "+k.Line()+" (ack: "+k.AckCommand()+")")
		}
	}

	if len(parts) == 0 {
		return ""
	}

	return "[zmina] Your context was trimmed; these lines still wait for your ack — act on each, then ack it " +
		"(zmina ack <ids>):\n- " + strings.Join(parts, "\n- ")
}

// roleSettings are the settings a socket role starts with: other sessions' messages let in, and a hook that gives back
// the lines it has not acked after its context is compacted or cleared (zmina pending).
func (s *Shift) roleSettings(name string) string {
	settings := map[string]any{"crossSessionInbound": "accept"}

	if s.Self != "" {
		cmd := shellWord(s.Self) + " --crew " + shellWord(s.Crew.Path) + " pending --role " + shellWord(name) + " --brief"
		hook := []any{map[string]any{"type": "command", "command": cmd}}
		settings["hooks"] = map[string]any{"SessionStart": []any{
			map[string]any{"matcher": "compact", "hooks": hook},
			map[string]any{"matcher": "clear", "hooks": hook},
		}}
	}

	raw, _ := json.Marshal(settings)

	return string(raw)
}
