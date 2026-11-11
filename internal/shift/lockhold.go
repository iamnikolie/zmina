package shift

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/tmux"
)

// Who holds the state lock, and for how long. Whoever takes the lock writes a small record beside it (lock.holder:
// pid, the command's verb, when it took the lock, the phase it is in) and clears it before it lets go; a command that
// waits long, and a hold that lasts long, each leave one event in the log. The record is advisory: nothing decides
// anything by it, a write that fails is ignored, and a reader trusts it only while its pid is alive and is the pid
// the lock file names.

var (
	// LockWaitNote is how long a command waits for the lock before it logs a lock-wait event.
	LockWaitNote = 5 * time.Second
	// LockHeldNote is how long a hold lasts before it logs a lock-held event.
	LockHeldNote = 20 * time.Second
)

// The clock and probes the lock code reads; tests replace them.
var (
	lockNow     = time.Now
	lockSleep   = time.Sleep
	slowestTmux = tmux.Slowest
	pidAlive    = func(pid int) bool {
		err := syscall.Kill(pid, 0)

		return err == nil || errors.Is(err, syscall.EPERM)
	}
)

const (
	// holderSize is the record's fixed size: it is padded with blanks and rewritten in place with one write, so no
	// reader sees it half cut and no truncate is needed. holderField caps a verb, a phase and a detail.
	holderSize  = 512
	holderField = 48
)

// Holder is the record of who holds the lock.
type Holder struct {
	PID int `json:"pid"`
	// Verb is the command that holds it: tick, task set, ... Never its arguments.
	Verb  string    `json:"verb,omitempty"`
	Since time.Time `json:"since"`
	// Phase is the step the holder is in now (a tick's roles, deliver, ...), Detail what it is working on there (a
	// role), PhaseSince when it began.
	Phase      string    `json:"phase,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	PhaseSince time.Time `json:"phase_since,omitzero"`
}

// Describe is the record as a line: "pid 4242 (tick), held 4m10s, in roles (dev) for 3m55s".
func (h Holder) Describe(now time.Time) string {
	out := "pid " + strconv.Itoa(h.PID)
	if h.Verb != "" {
		out += " (" + h.Verb + ")"
	}

	if !h.Since.IsZero() {
		out += ", held " + shortDur(now.Sub(h.Since))
	}

	if h.Phase != "" {
		out += ", in " + h.Phase
		if h.Detail != "" {
			out += " (" + h.Detail + ")"
		}

		if !h.PhaseSince.IsZero() {
			out += " for " + shortDur(now.Sub(h.PhaseSince))
		}
	}

	return out
}

// LockOpts says how a command takes the lock.
type LockOpts struct {
	// Verb names the command in the holder record and the events: tick, task set. Never its arguments.
	Verb string
	// Log gets the lock-wait and lock-held events; nil: none are logged.
	Log func(events.Event)
}

// LockStatus is what a look at the lock finds. A look never waits and never writes.
type LockStatus struct {
	// Held is true while some process holds the lock.
	Held bool
	// PID is the pid the lock file names: the last process that took the lock.
	PID int
	// Holder is the record of the process that holds the lock now; nil when none matches.
	Holder *Holder
	// Stale is a record left behind: its process is gone, or the lock is not held by it.
	Stale *Holder
}

// LockLook reads who holds the lock in dir. It tries the lock without waiting (and lets go at once), so it works
// while a hang holds it.
func LockLook(dir string) LockStatus {
	var st LockStatus

	path := crew.Layout{Dir: dir}.Lock()

	f, err := os.Open(path)
	if err != nil {
		return staleOnly(dir, st)
	}
	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

		return staleOnly(dir, st)
	}

	st.Held = true
	st.PID = lockFilePID(dir)

	if h, ok := readHolder(dir); ok {
		if h.PID == st.PID && pidAlive(h.PID) {
			st.Holder = &h
		} else {
			st.Stale = &h
		}
	}

	return st
}

// staleOnly is a free lock: a record still beside it was left by a process that died holding it.
func staleOnly(dir string, st LockStatus) LockStatus {
	if h, ok := readHolder(dir); ok {
		st.Stale = &h
	}

	return st
}

// readHolder reads the record beside the lock in dir.
func readHolder(dir string) (Holder, bool) {
	raw, err := os.ReadFile(holderPath(dir))
	if err != nil {
		return Holder{}, false
	}

	var h Holder
	if json.Unmarshal(bytes.TrimSpace(raw), &h) != nil || h.PID <= 0 {
		return Holder{}, false
	}

	return h, true
}

// liveHolder is the record of dir when it matches the lock: its pid is alive and is the one the lock file names.
func liveHolder(dir string) (Holder, bool) {
	h, ok := readHolder(dir)
	if !ok || h.PID != lockFilePID(dir) || !pidAlive(h.PID) {
		return Holder{}, false
	}

	return h, true
}

func lockFilePID(dir string) int {
	raw, err := os.ReadFile(crew.Layout{Dir: dir}.Lock())
	if err != nil {
		return 0
	}

	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))

	return pid
}

func holderPath(dir string) string { return crew.Layout{Dir: dir}.LockHolder() }

// waiter is a command that has not got the lock yet: how long it has waited, and the holder it saw last.
type waiter struct {
	dir     string
	opts    LockOpts
	start   time.Time
	sampled time.Time
	seen    *Holder
}

func newWaiter(dir string, opts LockOpts) *waiter {
	return &waiter{dir: dir, opts: opts, start: lockNow()}
}

// poll is called after each try that found the lock held: about once a second it reads the holder's record, so the
// event names the one that held the lock last, not a holder that has gone by the time the wait ends.
func (w *waiter) poll() {
	if now := lockNow(); w.sampled.IsZero() || now.Sub(w.sampled) >= time.Second {
		w.sample(now)
	}
}

func (w *waiter) sample(now time.Time) {
	w.sampled = now

	if h, ok := liveHolder(w.dir); ok {
		w.seen = &h
	}
}

// took is the lock got: it logs the wait when it was long, and starts the hold.
func (w *waiter) took(f *os.File) *hold {
	now := lockNow()
	h := &hold{dir: w.dir, f: f, opts: w.opts, at: now}

	if waited := now.Sub(w.start); waited >= LockWaitNote {
		w.note(waited, now, false)
	}

	tmux.ResetSlowest()

	h.rec = Holder{PID: os.Getpid(), Verb: fieldClip(w.opts.Verb), Since: now}
	h.side, _ = os.OpenFile(holderPath(w.dir), os.O_CREATE|os.O_WRONLY, 0o600)
	h.write()

	return h
}

// gaveUp is the wait that ran out: it logs it and says who held the lock.
func (w *waiter) gaveUp(path string) error {
	now := lockNow()
	w.sample(now)
	w.note(now.Sub(w.start), now, true)

	holder := "another zmina"
	if w.seen != nil {
		holder = "zmina " + w.seen.Describe(now)
	} else if pid := lockFilePID(w.dir); pid > 0 {
		holder = "zmina pid " + strconv.Itoa(pid)
	}

	return fmt.Errorf("state: locked by %s; waited %s for %s: try again, or see what it holds (zmina lock)",
		holder, shortDur(now.Sub(w.start)), path)
}

// note logs a lock-wait event.
func (w *waiter) note(waited time.Duration, now time.Time, gaveUp bool) {
	if w.opts.Log == nil {
		return
	}

	kv := []string{"waited=" + shortDur(waited)}

	if h := w.seen; h != nil {
		kv = append(kv, "holder="+safe(h.Verb), "holder_pid="+strconv.Itoa(h.PID),
			"holder_for="+shortDur(now.Sub(h.Since)))

		if h.Phase != "" {
			kv = append(kv, "holder_phase="+stepName(h.Phase, h.Detail))
		}
	}

	if gaveUp {
		kv = append(kv, "gave_up=yes")
	}

	w.opts.Log(events.Event{Event: "lock-wait", Detail: strings.Join(kv, " ")})
}

// markAt is one step a hold reached: the time it began.
type markAt struct {
	phase, detail string
	at            time.Time
}

// hold is the lock while it is ours: the lock file, the holder record and the steps passed.
type hold struct {
	dir   string
	f     *os.File
	side  *os.File
	opts  LockOpts
	rec   Holder
	at    time.Time
	marks []markAt
	done  bool
}

// mark notes that the hold has reached a step (phase, and what it works on in it): the time goes in memory and the
// record beside the lock says it. Nothing else is written.
func (h *hold) mark(phase, detail string) {
	if h.done {
		return
	}

	now := lockNow()
	h.marks = append(h.marks, markAt{phase, detail, now})
	h.rec.Phase, h.rec.Detail, h.rec.PhaseSince = fieldClip(phase), fieldClip(detail), now
	h.write()
}

// write puts the record in its file: one write over its whole fixed size. A failure is ignored.
func (h *hold) write() {
	if h.side == nil {
		return
	}

	raw, err := json.Marshal(h.rec)
	if err != nil || len(raw) >= holderSize {
		return
	}

	buf := bytes.Repeat([]byte{' '}, holderSize)
	copy(buf, raw)
	buf[holderSize-1] = '\n'

	_, _ = h.side.WriteAt(buf, 0)
}

// release clears the record while the lock is still ours (a later holder's record must not be the one removed), lets
// the lock go, and logs a lock-held event when the hold was long.
func (h *hold) release() {
	if h == nil || h.done {
		return
	}

	h.done = true
	end := lockNow()

	if h.side != nil {
		_ = os.Remove(holderPath(h.dir))
		_ = h.side.Close()
	}

	_ = h.f.Close()

	if held := end.Sub(h.at); held >= LockHeldNote && h.opts.Log != nil {
		h.opts.Log(events.Event{Event: "lock-held", Detail: h.detail(held, end)})
	}
}

// step is how long a hold spent in one phase (and on one thing in it).
type step struct {
	phase, detail string
	took          time.Duration
}

// steps are the stretches between the marks, from the take to end; what came before the first mark is "run".
func (h *hold) steps(end time.Time) []step {
	var out []step

	from, name, detail := h.at, "run", ""

	for _, m := range h.marks {
		out = append(out, step{name, detail, m.at.Sub(from)})
		from, name, detail = m.at, m.phase, m.detail
	}

	return append(out, step{name, detail, end.Sub(from)})
}

// detail is the lock-held event's text: key=value words with no blanks in a value, the answer first.
func (h *hold) detail(held time.Duration, end time.Time) string {
	kv := []string{"held=" + shortDur(held), "verb=" + safe(h.rec.Verb)}

	var (
		order  []string
		totals = map[string]time.Duration{}
		best   step
	)

	for _, s := range h.steps(end) {
		if _, seen := totals[s.phase]; !seen {
			order = append(order, s.phase)
		}

		totals[s.phase] += s.took

		if s.took > best.took {
			best = s
		}
	}

	sort.SliceStable(order, func(i, j int) bool { return totals[order[i]] > totals[order[j]] })

	if len(order) > 0 {
		kv = append(kv, "phase="+safe(order[0]), "phase_for="+shortDur(totals[order[0]]))
	}

	if best.detail != "" {
		kv = append(kv, "step="+stepName(best.phase, best.detail), "step_for="+shortDur(best.took))
	}

	if name, took := slowestTmux(); name != "" {
		kv = append(kv, "tmux="+safe(name), "tmux_for="+shortDur(took))
	}

	var all []string

	for _, p := range order {
		all = append(all, safe(p)+":"+shortDur(totals[p]))
	}

	return strings.Join(append(kv, "phases="+strings.Join(all, ",")), " ")
}

// HoldEvent is what a lock-held event says.
type HoldEvent struct {
	Verb     string
	Held     time.Duration
	Phase    string
	PhaseFor time.Duration
	// Step is the longest single stretch when it named a thing (roles/dev), StepFor how long it took.
	Step    string
	StepFor time.Duration
	// Tmux is the slowest tmux subcommand during the hold, TmuxFor how long it took.
	Tmux    string
	TmuxFor time.Duration
}

// ParseHoldEvent reads the detail of a lock-held event.
func ParseHoldEvent(detail string) HoldEvent {
	kv := words(detail)

	return HoldEvent{
		Verb: unsafe(kv["verb"]), Held: parseDur(kv["held"]), Phase: kv["phase"], PhaseFor: parseDur(kv["phase_for"]),
		Step: kv["step"], StepFor: parseDur(kv["step_for"]), Tmux: kv["tmux"], TmuxFor: parseDur(kv["tmux_for"]),
	}
}

// WaitEvent is what a lock-wait event says.
type WaitEvent struct {
	Waited time.Duration
	// GaveUp: the wait ran out and the command failed.
	GaveUp bool
	// HolderPID, HolderVerb, HolderFor and HolderPhase are the holder the waiter saw last; the pid is 0 when it saw none.
	HolderPID   int
	HolderVerb  string
	HolderFor   time.Duration
	HolderPhase string
}

// ParseWaitEvent reads the detail of a lock-wait event.
func ParseWaitEvent(detail string) WaitEvent {
	kv := words(detail)
	pid, _ := strconv.Atoi(kv["holder_pid"])

	return WaitEvent{
		Waited: parseDur(kv["waited"]), GaveUp: kv["gave_up"] == "yes", HolderPID: pid, HolderVerb: unsafe(kv["holder"]),
		HolderFor: parseDur(kv["holder_for"]), HolderPhase: kv["holder_phase"],
	}
}

func words(detail string) map[string]string {
	out := map[string]string{}

	for _, w := range strings.Fields(detail) {
		if k, v, ok := strings.Cut(w, "="); ok {
			out[k] = v
		}
	}

	return out
}

func parseDur(s string) time.Duration {
	d, _ := time.ParseDuration(s)

	return d
}

// safe makes a value one word: a verb has blanks ("task set"), which a event's words cannot.
func safe(s string) string { return strings.ReplaceAll(strings.TrimSpace(s), " ", "+") }

func unsafe(s string) string { return strings.ReplaceAll(s, "+", " ") }

func stepName(phase, detail string) string {
	if detail == "" {
		return safe(phase)
	}

	return safe(phase) + "/" + safe(detail)
}

// fieldClip keeps a verb, a phase or a detail short enough for the record's fixed size.
func fieldClip(s string) string {
	if r := []rune(s); len(r) > holderField {
		return string(r[:holderField])
	}

	return s
}

// shortDur is a duration to the tenth of a second (to the millisecond below one second): 4m10.3s, 12ms.
func shortDur(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}

	return d.Round(100 * time.Millisecond).String()
}

// mark is a step of the beat a hold is in. It is bookkeeping only: nothing runs when no hold records it (tests, a
// shift not under the lock).
func (s *Shift) mark(phase, detail string) {
	if s.Mark != nil {
		s.Mark(phase, detail)
	}
}
