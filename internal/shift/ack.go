package shift

import (
	"encoding/json"
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
	// trackedTail is the id in a tracked line's tail, at its very end, the only place zmina puts it:
	// "… (zmina S-14)", "… (zmina S-14, still pending since …)".
	trackedTail = regexp.MustCompile(`\(zmina (S-[0-9]+)(?:, still pending since [^()]*)?\)$`)
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

// trackedID is the id of a role's tracked line, read from the suffix zmina gave it at its very end and checked
// against the line's own record; "" for a line zmina does not track. What the text before the suffix quotes never
// counts.
func (s *Shift) trackedID(role, line string) string {
	m := trackedTail.FindStringSubmatch(line)
	if m == nil {
		return ""
	}

	if a := s.State.Acks[m[1]]; a != nil && a.Role == role && a.tracks(line) {
		return a.ID
	}

	return ""
}

// delivered marks a role's tracked line, or the signals of a delivered line by their ids, as delivered now.
func (s *Shift) delivered(role, line string, ids []string, now time.Time) {
	if id := s.trackedID(role, line); id != "" {
		ids = append(ids, id)
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

// SignalBody is a dispatcher's signal without its id and its "still pending" mark: the note as it was taken.
func SignalBody(sig string) string { return signalBody(sig) }

// noteID is the id a new signal for the dispatcher waits for its ack under, "" for none: a watch's note has its
// own ack, and a dispatcher started by an earlier zmina is not told of acks.
func (s *Shift) noteID(note string) string {
	if src, _, _ := strings.Cut(note, ": "); s.Crew.Watches[src] != nil || !s.acking(s.Crew.Dispatcher) {
		return ""
	}

	return s.newAck(s.Crew.Dispatcher, note)
}

// signalPart is a dispatcher's signal as its line carries it: with its id, a new one for a signal never delivered
// (watch notes have their own ack, and are left as they are). It returns the part and its id, "" for none.
func (s *Shift) signalPart(sig string) (string, string) {
	if m := signalID.FindStringSubmatch(sig); m != nil {
		return sig, m[1]
	}

	id := s.noteID(sig)
	if id == "" {
		return sig, ""
	}

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
	if a.Role == s.Crew.Dispatcher && slices.ContainsFunc(s.State.Signals, func(sig string) bool {
		return queuedSignalID(sig) == a.ID
	}) {
		return true
	}

	rs := s.State.Role(a.Role)
	if slices.ContainsFunc(rs.Inbox, func(l string) bool { return a.tracks(l) || slices.Contains(rs.QueuedIDs[l], a.ID) }) {
		return true
	}

	// A line written before ids were kept with it (IDs empty) is searched for its id the old way.
	legacy := regexp.MustCompile(`\b` + regexp.QuoteMeta(a.ID) + `\b`)
	carries := func(line string, ids []string) bool {
		if len(ids) > 0 {
			return slices.Contains(ids, a.ID)
		}

		return a.tracks(line) || legacy.MatchString(line)
	}

	if rs.Sent != nil && carries(rs.Sent.Line, rs.Sent.IDs) {
		return true
	}

	return slices.ContainsFunc(rs.Posted, func(p Posted) bool { return carries(p.Line, p.IDs) })
}

// AckLines acks lines by id, or with all every line of the role. An id that waits for no ack (acked already, said
// again after a compaction, dropped) or that is another role's line is skipped with a warning, so the rest are acked
// all the same; caller is who acks, and only "hand" (the owner, at a shell) acks another role's lines, by id or
// with all. It returns the
// ids acked and the warnings.
func (s *Shift) AckLines(ids []string, all bool, role, caller string) ([]string, []string) {
	if all {
		ids = nil

		for _, a := range s.State.Acks {
			if a.Role == role {
				ids = append(ids, a.ID)
			}
		}
	}

	var acked, warn []string

	for _, id := range ids {
		id = strings.ToUpper(strings.TrimSpace(id))
		if !strings.HasPrefix(id, "S-") {
			id = "S-" + id
		}

		a := s.State.Acks[id]

		switch {
		case a == nil:
			warn = append(warn, id+" waits for no ack: acked already, or dropped (zmina pending lists the ones that do)")
		case caller != "hand" && caller != "" && a.Role != caller:
			warn = append(warn, id+" is "+a.Role+"'s line, not "+caller+"'s: left for "+a.Role+" to ack")
		case !slices.Contains(acked, id):
			acked = append(acked, id)
		}
	}

	slices.SortFunc(acked, func(a, b string) int { return ackNum(a) - ackNum(b) })

	for _, id := range acked {
		a := s.State.Acks[id]
		delete(s.State.Acks, id)
		s.log(a.Role, "acked", "", id+" "+a.Text)
		s.dropQueued(a)

		// A plain tell is read once its line is acked; a report only when the dispatcher says it consumed the
		// report itself (zmina message read), never by acking the notice that points to it.
		if a.Role == s.Crew.Dispatcher {
			s.stageIDs([]string{id}, "read", TellPrefix)
		}
	}

	return acked, warn
}

// dropQueued takes a line its reader acked out of where it still waits: the dispatcher's signals (one it read in
// zmina queue ls, or one said again), a role's queue. A line the reader already has is not posted again. A queued
// line is known by the id zmina gave it (a signal's leading id, a tracked line's own suffix), never by what its text
// quotes.
func (s *Shift) dropQueued(a *Ack) {
	if a.Role == s.Crew.Dispatcher {
		for _, sig := range slices.Clone(s.State.Signals) {
			if queuedSignalID(sig) == a.ID {
				s.dropSignal(sig)
				s.log(a.Role, "acked-queued", "", sig)
			}
		}
	}

	rs := s.State.Roles[a.Role]
	if rs == nil {
		return
	}

	for _, line := range slices.Clone(rs.Inbox) {
		if a.tracks(line) {
			rs.Inbox = slices.DeleteFunc(rs.Inbox, func(v string) bool { return v == line })
			delete(rs.QueuedIDs, line)
			s.log(a.Role, "acked-queued", "", line)
		}
	}
}

// QueuedSignalID is queuedSignalID for the node's reports.
func QueuedSignalID(sig string) string { return queuedSignalID(sig) }

// carryIDs keeps the S-ids a line given back to a role's queue carries, for when it goes again.
func (s *Shift) carryIDs(name, line string, ids []string) {
	if len(ids) == 0 {
		return
	}

	rs := s.State.Role(name)
	if rs.QueuedIDs == nil {
		rs.QueuedIDs = map[string][]string{}
	}

	for _, id := range ids {
		if !slices.Contains(rs.QueuedIDs[line], id) {
			rs.QueuedIDs[line] = append(rs.QueuedIDs[line], id)
		}
	}
}

// lineIDs are the S-ids a role's queued line carries: its own tracked id, and those it was given back with.
func (s *Shift) lineIDs(name, line string) []string {
	ids := slices.Clone(s.State.Role(name).QueuedIDs[line])
	if id := s.trackedID(name, line); id != "" && !slices.Contains(ids, id) {
		ids = append(ids, id)
	}

	return ids
}

// takeIDs is lineIDs for a line that leaves the queue.
func (s *Shift) takeIDs(name, line string) []string {
	ids := s.lineIDs(name, line)
	delete(s.State.Role(name).QueuedIDs, line)

	return ids
}

// queuedSignalID is the id a dispatcher's signal waits under: the one at its head, "" for none.
func queuedSignalID(sig string) string {
	if m := signalID.FindStringSubmatch(sig); m != nil {
		return m[1]
	}

	return ""
}

// tracks reports whether a queued line is this tracked line: its text with the suffix zmina gave it, queued
// (" (zmina S-14)") or said again (" (zmina S-14, still pending since …)").
func (a *Ack) tracks(line string) bool {
	if line == a.Text+" (zmina "+a.ID+")" {
		return true
	}

	rest, ok := strings.CutPrefix(line, a.Text+" (zmina "+a.ID+", still pending since ")

	return ok && strings.HasSuffix(rest, ")") && !strings.ContainsAny(rest[:len(rest)-1], "()")
}

// DropSignal is dropSignal for the node's reports.
func (s *Shift) DropSignal(sig string) { s.dropSignal(sig) }

// dropSignal takes a signal out of the dispatcher's queue with what is kept about it; the ack of one never delivered
// goes with it.
func (s *Shift) dropSignal(sig string) {
	s.State.Signals = slices.DeleteFunc(s.State.Signals, func(v string) bool { return v == sig })
	delete(s.State.SignalCounts, sig)
	delete(s.State.SignalSent, sig)

	if m := signalID.FindStringSubmatch(sig); m != nil {
		if a := s.State.Acks[m[1]]; a != nil && a.First.IsZero() {
			delete(s.State.Acks, m[1])
		}
	}
}

// stageIDs records a stage for the lines of other crews bound to these signal ids (MessageIDs), only those whose
// marker starts with prefix when one is given.
func (s *Shift) stageIDs(ids []string, stage string, prefix ...string) {
	for marker, id := range s.State.MessageIDs {
		if len(prefix) > 0 && !strings.HasPrefix(marker, prefix[0]) {
			continue
		}

		if slices.Contains(ids, id) {
			s.markerStage(marker, stage)
		}
	}
}

// BindMessage binds a line of another crew (its marker) to the signal it was queued as, by the signal's id.
func (s *Shift) BindMessage(marker, id string) {
	if id == "" {
		return
	}

	if s.State.MessageIDs == nil {
		s.State.MessageIDs = map[string]string{}
	}

	s.State.MessageIDs[marker] = id
}

// ForgetMessage drops what is kept of a line of another crew by its marker.
func (s *Shift) ForgetMessage(marker string) {
	delete(s.State.MessageStages, marker)
	delete(s.State.MessageLines, marker)
	delete(s.State.MessageIDs, marker)
}

// markerStage records a stage of a line from another crew by its marker, the first time it is reached.
func (s *Shift) markerStage(marker, stage string) {
	if s.State.MessageStages == nil {
		s.State.MessageStages = map[string]map[string]time.Time{}
	}

	if s.State.MessageStages[marker] == nil {
		s.State.MessageStages[marker] = map[string]time.Time{}
	}

	if _, ok := s.State.MessageStages[marker][stage]; !ok {
		s.State.MessageStages[marker][stage] = s.Now().UTC()
	}
}

// TellPrefix opens the marker of a line another crew sent with plain zmina tell: "tell/<envelope id>".
const TellPrefix = "tell/"

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

// briefMax is how many unacked lines a brief lists, the newest; the rest are counted.
const briefMax = 20

// PendingBrief is what a role is told when its session starts, resumes, or has its context compacted or cleared: with
// tagRule the rule of its start's tag (after a trim, which may have lost the boot line — never at a start, whose own
// boot line names the tag of that start, while the state may still hold the one before), the lines it has not acked
// — the newest briefMax — and for the dispatcher the watch keys that wait. Empty when nothing is to be told.
func (s *Shift) PendingBrief(role string, tagRule bool) string {
	var parts []string

	for _, a := range s.Pending(role) {
		if a.First.IsZero() {
			continue // still queued: it comes anyway
		}

		parts = append(parts, a.ID+" (since "+s.clock(a.First)+") "+a.Text)
	}

	more := 0
	if len(parts) > briefMax {
		more, parts = len(parts)-briefMax, parts[len(parts)-briefMax:]
	}

	if role == s.Crew.Dispatcher {
		for _, k := range s.WaitingKeys() {
			parts = append(parts, "watch "+k.Watch+": "+k.Line()+" (ack: "+k.AckCommand()+")")
		}
	}

	var out []string

	if rs := s.State.Roles[role]; tagRule && rs != nil && rs.Inbound && rs.Tag != "" && role != "" {
		out = append(out, InboxRule(rs.Tag))
	}

	if len(parts) > 0 {
		list := "[zmina] These lines still wait for your ack — act on each, then ack it (zmina ack <ids>):\n- " +
			strings.Join(parts, "\n- ")
		if more > 0 {
			list += "\n- and " + strconv.Itoa(more) + " older ones: zmina pending"
		}

		out = append(out, list)
	}

	return strings.Join(out, "\n\n")
}

// turnEndTimeout is the Stop hook's timeout in seconds; the hook itself returns in milliseconds (TurnEnd).
const turnEndTimeout = 5

// roleSettings are the settings a socket role starts with: other sessions' messages let in, a hook that tells it
// its tag and the lines it has not acked whenever its session starts, resumes, or is compacted or cleared (zmina
// pending --brief), and a Stop hook that has what waits for it posted once the turn is over instead of at the next
// beat (zmina _turn-end). That one is silent and always exits 0, so a turn's end puts nothing into the context.
func (s *Shift) roleSettings(name string) string {
	settings := map[string]any{"crossSessionInbound": "accept"}

	if s.Self != "" {
		base := shellWord(s.Self) + " --crew " + shellWord(s.Crew.Path)
		start := []any{map[string]any{"type": "command", "command": base + " pending --role " + shellWord(name) + " --brief"}}
		stop := []any{map[string]any{"type": "command", "timeout": turnEndTimeout,
			"command": base + " _turn-end --role " + shellWord(name) + " >/dev/null 2>&1 || true"}}
		// A compaction ends no turn, so no Stop hook runs after it: the start hook Claude Code runs once it is over
		// (source compact) has what waits posted too.
		settings["hooks"] = map[string]any{
			"SessionStart": []any{map[string]any{"hooks": start}, map[string]any{"matcher": "compact", "hooks": stop}},
			"Stop":         []any{map[string]any{"hooks": stop}},
		}
	}

	raw, _ := json.Marshal(settings)

	return string(raw)
}
