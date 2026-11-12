package shift

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/task"
)

// A rotation is a restart of a role into a fresh conversation (the ticker's restart_at, restart_idle and restart_done),
// told as one record from its ask to the answer to "did the new conversation get what it needs": asked, prepared,
// switching, started, closed. Its steps are the lines of the role's rotations/<role>.jsonl in the crew's directory,
// append-only:
//
//	prepared   the note is durable and the old conversation named (from); written before the role is put down
//	switching  the role is being put down (claim: the Downing the restart's job carries)
//	started    the fresh session runs and its lines (restart line, a packet per task) are in its queue
//	evidence   something the new conversation did: hook-start, boot, shown, accepted, note-fetched
//	closed     one outcome: resumed (the same conversation goes on), rebuilt (a fresh one that confirmed) or lost
//
// The ask is not a step: a role asked for its note and called off was never rotated, and keeps no record (its time is in
// prepared as asked_at). Like the delivery cards the file is a report and nothing decides from it: the restart never reads
// it, a file that is missing or unreadable changes no restart and no delivery (the rotation is then simply not told), and a
// write that fails is told once as an event (rotation-failed) and never fails or delays the restart. The one place it is
// read to hold a note back is the acceptance alert, which is not delivery: a task handed by a rotation that is open, or was
// lost and told, is not told a second time (acceptanceDue).
//
// Appends are one write of one line, so no writer tears another. The shift appends under the state lock; what the role
// itself did (its SessionStart hook, the commands that read a task's note) is appended by its own commands without it
// (RecordRotationEvidence), as a fetch is. A reader folds the lines by id over the newest rotationReadMax bytes: the first
// line of a rotation is its prepared line, the first closing line wins (a rotation closes once), a line that does not parse
// is left out, and a line cut short by a write that died is ended by the next append, never joined to it.

const (
	// Phases of a rotation, as its record shows them.
	PhasePrepared  = "prepared"
	PhaseSwitching = "switching"
	PhaseStarted   = "started"
	PhaseClosed    = "closed"

	// phaseEvidence is the phase of a line that records something the new conversation did.
	phaseEvidence = "evidence"

	// How a rotation ended.
	OutcomeResumed = "resumed"
	OutcomeRebuilt = "rebuilt"
	OutcomeLost    = "lost"

	// Kinds of a rotation: the key of the restart that began it.
	KindRestartAt   = "restart_at"
	KindRestartIdle = "restart_idle"
	KindRestartDone = "restart_done"

	// What the new conversation did, as evidence names it.
	EvidenceHookStart   = "hook-start"
	EvidenceBoot        = "boot"
	EvidenceShown       = "shown"
	EvidenceAccepted    = "accepted"
	EvidenceNoteFetched = "note-fetched"

	// Bookkeeping lines of the beat: the conversation the fresh session runs, and the one note a rotation that stays
	// open too long makes.
	evidenceSession = "session"
	evidenceSlow    = "slow"

	// rotationReadMax is how much of a role's record a look reads (the newest lines); rotationBeatRead how much the beat
	// reads to follow an open rotation, which is a few lines at its end.
	rotationReadMax  = 1 << 20
	rotationBeatRead = 64 << 10

	// rotationAcceptWait is how long a booted role that was shown its context sits idle without accepting its tasks
	// before the rotation is closed lost. rotationSlow is how long a rotation may stay open before the dispatcher is told
	// once that it has not confirmed.
	rotationAcceptWait = 10 * time.Minute
	rotationSlow       = 15 * time.Minute
)

// restartLineHead is how the line a restart queues for the fresh session begins; the card of that line is how a
// rotation finds it shown.
const restartLineHead = "[zmina] restarted fresh: "

// RotationFrom is the conversation a role was on when its rotation began: what a way back to it needs.
type RotationFrom struct {
	Runtime string `json:"runtime,omitempty"`
	Home    string `json:"home,omitempty"`
	Session string `json:"session,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Context int    `json:"context,omitempty"`
}

// RotationTo is the fresh conversation, once the beat has seen the session.
type RotationTo struct {
	Session string `json:"session,omitempty"`
}

// RotationArtifact is what a rotation checked durable before it put the role down: a held task's checkpoint, or for
// restart_done the result of the task whose end it follows.
type RotationArtifact struct {
	Task       string `json:"task"`
	Checkpoint string `json:"checkpoint,omitempty"`
	Result     string `json:"result,omitempty"`
}

// rotationLine is one line of a role's record. Every field but the first three belongs to one phase.
type rotationLine struct {
	ID    string    `json:"id"`
	At    time.Time `json:"at"`
	Phase string    `json:"phase"`

	// prepared
	Role      string             `json:"role,omitempty"`
	Kind      string             `json:"kind,omitempty"`
	Why       string             `json:"why,omitempty"`
	From      *RotationFrom      `json:"from,omitempty"`
	Artifacts []RotationArtifact `json:"artifacts,omitempty"`
	AskedAt   time.Time          `json:"asked_at,omitzero"`

	// switching
	Claim time.Time `json:"claim,omitzero"`

	// started
	Lines []string `json:"lines,omitempty"`

	// evidence
	What    string `json:"what,omitempty"`
	Task    string `json:"task,omitempty"`
	By      string `json:"by,omitempty"`
	Session string `json:"session,omitempty"`

	// closed
	Outcome   string   `json:"outcome,omitempty"`
	Detail    string   `json:"detail,omitempty"`
	Confirmed []string `json:"confirmed,omitempty"`
	Back      string   `json:"back,omitempty"`
}

// RotationEvidence is one thing the new conversation did.
type RotationEvidence struct {
	What    string    `json:"what"`
	Task    string    `json:"task,omitempty"`
	By      string    `json:"by,omitempty"`
	Session string    `json:"session,omitempty"`
	At      time.Time `json:"at"`
}

// RotationClosed is how a rotation ended: the outcome, the reason or what confirmed it, and for a lost one the command
// that puts the role back on its old conversation.
type RotationClosed struct {
	At        time.Time `json:"at"`
	Outcome   string    `json:"outcome"`
	Detail    string    `json:"detail,omitempty"`
	Confirmed []string  `json:"confirmed,omitempty"`
	Back      string    `json:"back,omitempty"`
}

// Rotation is a rotation as its record folds: what a look shows.
type Rotation struct {
	ID    string       `json:"id"`
	Role  string       `json:"role"`
	Kind  string       `json:"kind"`
	Why   string       `json:"why"`
	Phase string       `json:"phase"`
	From  RotationFrom `json:"from"`
	To    RotationTo   `json:"to"`
	// Artifacts are the tasks the rotation hands the fresh conversation.
	Artifacts []RotationArtifact `json:"artifacts,omitempty"`
	AskedAt   time.Time          `json:"asked_at,omitzero"`
	Prepared  time.Time          `json:"prepared"`
	Switching time.Time          `json:"switching,omitzero"`
	// Claim is the down this rotation began, the one the restart's job and the role's pending down carry.
	Claim   time.Time `json:"claim,omitzero"`
	Started time.Time `json:"started,omitzero"`
	// Lines are the lines the restart queued for the fresh session (clipped), in queue order.
	Lines []string `json:"lines,omitempty"`
	// Evidence is what the new conversation did before the rotation closed, Late what it did after: a rotation lost
	// that the conversation then accepted its tasks of.
	Evidence []RotationEvidence `json:"evidence,omitempty"`
	Late     []RotationEvidence `json:"late,omitempty"`
	// SlowTold is when the dispatcher was told that the rotation stayed open.
	SlowTold time.Time       `json:"slow_told,omitzero"`
	Closed   *RotationClosed `json:"closed,omitempty"`
}

// Open reports a rotation not closed yet.
func (r Rotation) Open() bool { return r.Closed == nil }

// has reports whether the evidence of what (for a task, where it is about one) was recorded before the rotation closed.
func (r Rotation) has(what, task string) bool {
	return slices.ContainsFunc(r.Evidence, func(e RotationEvidence) bool { return e.What == what && e.Task == task })
}

// evidenceOf is the first evidence of what (for a task), nil when there is none.
func (r Rotation) evidenceOf(what, task string) *RotationEvidence {
	if i := slices.IndexFunc(r.Evidence, func(e RotationEvidence) bool { return e.What == what && e.Task == task }); i >= 0 {
		return &r.Evidence[i]
	}

	return nil
}

// acceptTasks are the tasks whose packets the fresh conversation is to accept: those a restart handed over. A restart
// after a task was done holds none (its artifact is the finished task's result).
func (r Rotation) acceptTasks() []string {
	if r.Kind == KindRestartDone {
		return nil
	}

	tasks := make([]string, 0, len(r.Artifacts))
	for _, a := range r.Artifacts {
		tasks = append(tasks, a.Task)
	}

	return tasks
}

// logTask is the task the rotation's event names: the tasks it hands over, else the one whose end it follows.
func (r Rotation) logTask() string {
	tasks := make([]string, 0, len(r.Artifacts))
	for _, a := range r.Artifacts {
		tasks = append(tasks, a.Task)
	}

	return strings.Join(tasks, ",")
}

// LateRebuilt reports a rotation closed lost whose conversation went on to accept every task it was handed: it did get
// its context, after the rotation had been told as lost.
func (r Rotation) LateRebuilt() bool {
	tasks := r.acceptTasks()
	if r.Closed == nil || r.Closed.Outcome != OutcomeLost || len(tasks) == 0 {
		return false
	}

	for _, id := range tasks {
		if !slices.ContainsFunc(r.Late, func(e RotationEvidence) bool { return e.What == EvidenceAccepted && e.Task == id }) {
			return false
		}
	}

	return true
}

// newRotationID is the eight random characters that name a rotation.
func newRotationID() string { return strings.ToLower(rand.Text()[:8]) }

// rotationKind is the key a restart's why says it was made for.
func rotationKind(why, done string) string {
	switch {
	case done != "":
		return KindRestartDone
	case strings.Contains(why, "restart_idle"):
		return KindRestartIdle
	}

	return KindRestartAt
}

func knownRotationPhase(phase string) bool {
	switch phase {
	case PhasePrepared, PhaseSwitching, PhaseStarted, phaseEvidence, PhaseClosed:
		return true
	}

	return false
}

// foldRotations folds a record's lines into its rotations, oldest first. A rotation begins at its prepared line (a line
// of one whose start is out of what was read is left out), the first line of each other phase wins, and evidence after the
// closing line is late.
func foldRotations(lines []rotationLine) []Rotation {
	var out []Rotation

	at := map[string]int{}

	for _, l := range lines {
		i, ok := at[l.ID]

		if l.Phase == PhasePrepared {
			if ok {
				continue
			}

			r := Rotation{ID: l.ID, Role: l.Role, Kind: l.Kind, Why: l.Why, Artifacts: l.Artifacts, AskedAt: l.AskedAt, Prepared: l.At,
				Phase: PhasePrepared}
			if l.From != nil {
				r.From = *l.From
			}

			at[l.ID] = len(out)
			out = append(out, r)

			continue
		}

		if !ok {
			continue
		}

		r := &out[i]

		switch l.Phase {
		case PhaseSwitching:
			if r.Switching.IsZero() {
				r.Switching, r.Claim = l.At, l.Claim
			}
		case PhaseStarted:
			if r.Started.IsZero() {
				r.Started, r.Lines = l.At, l.Lines
			}
		case phaseEvidence:
			r.foldEvidence(l)
		case PhaseClosed:
			if r.Closed == nil {
				r.Closed = &RotationClosed{At: l.At, Outcome: l.Outcome, Detail: l.Detail, Confirmed: l.Confirmed, Back: l.Back}
			}
		}
	}

	for i := range out {
		out[i].Phase = out[i].phase()
	}

	return out
}

// foldEvidence puts an evidence line where it belongs: the bookkeeping ones on the rotation, the others on its list.
func (r *Rotation) foldEvidence(l rotationLine) {
	switch l.What {
	case evidenceSession:
		if r.To.Session == "" {
			r.To.Session = l.Session
		}

		return
	case evidenceSlow:
		if r.SlowTold.IsZero() {
			r.SlowTold = l.At
		}

		return
	}

	e := RotationEvidence{What: l.What, Task: l.Task, By: l.By, Session: l.Session, At: l.At}

	if r.Closed != nil {
		r.Late = append(r.Late, e)

		return
	}

	r.Evidence = append(r.Evidence, e)
}

// phase is the last step the rotation's record shows.
func (r Rotation) phase() string {
	switch {
	case r.Closed != nil:
		return PhaseClosed
	case !r.Started.IsZero():
		return PhaseStarted
	case !r.Switching.IsZero():
		return PhaseSwitching
	}

	return PhasePrepared
}

// readRotationLines reads the newest max bytes of a record: every line that reads, none for a file that is not there. A
// line cut at the start of what was read is left out; a line that does not parse is too.
func readRotationLines(path string, max int64) []rotationLine {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}

	defer func() { _ = f.Close() }()

	cut := false

	if st, err := f.Stat(); err == nil && st.Size() > max {
		if _, err := f.Seek(st.Size()-max, io.SeekStart); err != nil {
			return nil
		}

		cut = true
	}

	var out []rotationLine

	rd := bufio.NewReader(f)

	for first := true; ; first = false {
		raw, err := rd.ReadBytes('\n')

		// The first line of a tail starts somewhere in the middle of a line.
		if len(raw) > 0 && (!cut || !first) {
			var l rotationLine
			if json.Unmarshal(bytes.TrimSpace(raw), &l) == nil && l.ID != "" && !l.At.IsZero() && knownRotationPhase(l.Phase) {
				out = append(out, l)
			}
		}

		if err != nil {
			break
		}
	}

	return out
}

// appendRotationLine adds one line to a record, in one write: no lock is taken, so a writer that holds none (a role's own
// command) and one that holds the state lock cannot tear each other. A last line that a write left unfinished is ended
// first, so the new line starts a line of its own.
func appendRotationLine(path string, l rotationLine) error {
	raw, err := json.Marshal(l)
	if err != nil {
		return err
	}

	if err := fsx.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}

	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], st.Size()-1); err == nil && last[0] != '\n' {
			raw = append([]byte{'\n'}, raw...)
		}
	}

	_, werr := f.Write(append(raw, '\n'))

	return errors.Join(werr, f.Close())
}

// ReadRotations is a role's rotations as its record has them, oldest first, for a look: it writes nothing and moves
// nothing, and a record that is missing or does not read has none. An empty role is every role's.
func ReadRotations(l crew.Layout, role string) ([]Rotation, error) {
	if role != "" {
		if !hookRole(role) {
			return nil, nil
		}

		rots := foldRotations(readRotationLines(l.Rotations(role), rotationReadMax))
		rots = slices.DeleteFunc(rots, func(r Rotation) bool { return r.Role != "" && r.Role != role })

		return rots, nil
	}

	entries, err := os.ReadDir(l.RotationsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var out []Rotation

	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}

		out = append(out, foldRotations(readRotationLines(filepath.Join(l.RotationsDir(), e.Name()), rotationReadMax))...)
	}

	slices.SortStableFunc(out, func(a, b Rotation) int { return a.Prepared.Compare(b.Prepared) })

	return out, nil
}

// rotationWarned remembers the record failures already told, by file and kind, so that a beat that goes on failing the
// same way tells it once.
var rotationWarned sync.Map

// rotationWarn tells a record failure once as an event. The restart goes on: the record is a report.
func (s *Shift) rotationWarn(role, path, what string, err error) {
	if _, told := rotationWarned.LoadOrStore(path+"\x00"+what, true); told {
		return
	}

	s.log(role, "rotation-failed", "", what+": "+path+": "+err.Error())
}

// rotationsOn reports whether the shift keeps a record for the role: it has a crew and a clock, and the role is a file
// name.
func (s *Shift) rotationsOn(name string) bool { return s.Crew != nil && s.Now != nil && hookRole(name) }

// rotationAppend adds a line to the role's record. It reports whether it is there; a failure is told once and goes no
// further.
func (s *Shift) rotationAppend(name string, l rotationLine) bool {
	if !s.rotationsOn(name) {
		return false
	}

	path := s.Crew.Layout().Rotations(name)

	if err := appendRotationLine(path, l); err != nil {
		s.rotationWarn(name, path, "not written", err)

		return false
	}

	return true
}

// rotationsOf are the rotations the role's record shows in what the beat reads, oldest first.
func (s *Shift) rotationsOf(name string) []Rotation {
	if !s.rotationsOn(name) {
		return nil
	}

	return foldRotations(readRotationLines(s.Crew.Layout().Rotations(name), rotationBeatRead))
}

// rotationByID is one of the role's rotations.
func (s *Shift) rotationByID(name, id string) (Rotation, bool) {
	rots := s.rotationsOf(name)

	if i := slices.IndexFunc(rots, func(r Rotation) bool { return r.ID == id }); i >= 0 {
		return rots[i], true
	}

	return Rotation{}, false
}

// RecordRotationEvidence notes, in the role's record, something the role's own session did: its SessionStart hook ran, it
// accepted a task, it read a task's note. It takes no lock (see the top of this file) and is for the caller to ignore the
// error of: the record is a report, and what it reports on is never to fail for it. It records nothing unless a rotation
// is waiting for it: one that put the role down and is open (the evidence is of the session that replaced the old one, never
// of the old one still ending: a caller whose session is the old conversation's is not evidence), or one closed lost that
// this accept proves late. The same evidence of the same session is recorded once.
func RecordRotationEvidence(l crew.Layout, role, what, task, by, session string, at time.Time) error {
	if !hookRole(role) {
		return nil
	}

	path := l.Rotations(role)

	rots := foldRotations(readRotationLines(path, rotationBeatRead))
	if len(rots) == 0 {
		return nil
	}

	r := rots[len(rots)-1]

	if !r.wants(what, task, session) {
		return nil
	}

	seen := r.Evidence
	if r.Closed != nil {
		seen = r.Late
	}

	if slices.ContainsFunc(seen, func(e RotationEvidence) bool { return e.What == what && e.Task == task && e.Session == session }) {
		return nil
	}

	return appendRotationLine(path, rotationLine{ID: r.ID, At: at.UTC(), Phase: phaseEvidence, What: what, Task: task, By: by,
		Session: session})
}

// wants reports whether a rotation takes the evidence of what (about a task) from a session. An open one does once its
// role was put down (switching) and the session is not the old conversation's; a caller that does not say its session is
// taken only once the fresh session runs (started), when the old one is gone. A lost one takes late accepts of the
// tasks it handed, from the conversation it started.
func (r Rotation) wants(what, task, session string) bool {
	if what == EvidenceAccepted || what == EvidenceNoteFetched {
		if !slices.Contains(r.acceptTasks(), task) {
			return false
		}
	}

	if r.Closed != nil {
		return r.Closed.Outcome == OutcomeLost && what == EvidenceAccepted && !r.Started.IsZero() &&
			(session == "" || r.To.Session == "" || session == r.To.Session)
	}

	switch {
	case r.Switching.IsZero():
		return false
	case session != "" && session == r.From.Session:
		return false
	case session == "" && r.Started.IsZero():
		return false
	}

	return true
}

// rotationEvidence notes, from the shift, that a role's session did something a rotation waits for.
func (s *Shift) rotationEvidence(name, what, task, session string) {
	if !s.rotationsOn(name) {
		return
	}

	if err := RecordRotationEvidence(s.Crew.Layout(), name, what, task, name, session, s.Now()); err != nil {
		s.rotationWarn(name, s.Crew.Layout().Rotations(name), "evidence not written", err)
	}
}

// rotationOwns reports whether the delivery of a task is a rotation's own, which is told as lost by the rotation if the
// role does not accept it: the packet was queued by a restart that is still open or closed lost. The acceptance alert then
// says nothing, so that the dispatcher hears one note and not two.
func (s *Shift) rotationOwns(t task.Task) bool {
	if !s.rotationsOn(t.Role) || t.Delivery == nil || t.Delivery.QueuedAt.IsZero() {
		return false
	}

	rots := s.rotationsOf(t.Role)

	for i := len(rots) - 1; i >= 0; i-- {
		r := rots[i]

		if r.Role != "" && r.Role != t.Role || r.Started.IsZero() || !slices.Contains(r.acceptTasks(), t.ID) {
			continue
		}

		// The newest rotation that handed the task decides: a hand-out made after it started is not its own.
		if queued := t.Delivery.QueuedAt; queued.Before(r.Prepared) || queued.After(r.Started) {
			return false
		}

		return r.Open() || r.Closed.Outcome == OutcomeLost
	}

	return false
}

// PruneRotations removes, from the crew's rotation records, the files nothing was written to since the cutoff, and sets
// a file that grew past what a look reads aside (<role>.jsonl.old, the one before it replaced) once its newest rotation
// is closed: the record is appended to without the state lock by the role's own commands, so a file is never rewritten, only
// removed or renamed whole (a line appended at that moment lands in the file that was set aside). It returns how many
// rotations went. With dry it only counts. Call it under the state lock, which the shift holds when it closes a rotation.
func PruneRotations(l crew.Layout, cutoff time.Time, dry bool) (int, error) {
	entries, err := os.ReadDir(l.RotationsDir())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}

	if err != nil {
		return 0, err
	}

	var (
		total int
		errs  []error
	)

	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}

		path := filepath.Join(l.RotationsDir(), e.Name())

		info, err := e.Info()
		if err != nil {
			continue
		}

		if !strings.HasSuffix(e.Name(), ".jsonl") && !strings.HasSuffix(e.Name(), ".jsonl.old") {
			continue
		}

		if info.ModTime().Before(cutoff) {
			total += max(len(foldRotations(readRotationLines(path, rotationReadMax))), 1)

			if !dry {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, err)
				}
			}

			continue
		}

		if !strings.HasSuffix(e.Name(), ".jsonl") || info.Size() <= rotationReadMax {
			continue
		}

		rots := foldRotations(readRotationLines(path, rotationReadMax))
		if len(rots) == 0 || rots[len(rots)-1].Open() {
			continue
		}

		total += len(rots)

		if !dry {
			if err := os.Rename(path, path+".old"); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return total, errors.Join(errs...)
}

// clipLines keeps the head of each line a restart queued, as a card does.
func clipLines(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = cardHead(l)
	}

	return out
}
