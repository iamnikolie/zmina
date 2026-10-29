// Package watch runs the crew's detectors: each run a source reports what is true now, as events, and Apply decides
// what of it is news for the dispatcher. A watch finds work; it never does it.
package watch

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Event is one thing a source reports, level-triggered: the same key with the same fingerprint is not news.
type Event struct {
	// Key is what it is about, stable across runs (group/repo!12:review).
	Key string `json:"key"`
	// Fingerprint is its state; a changed one is news.
	Fingerprint string `json:"fingerprint"`
	// Text is one line, without the source prefix.
	Text string `json:"text"`
	// At is when it last moved; the quiet window is measured from it.
	At *time.Time `json:"at,omitempty"`
	// URL is where the dispatcher acts; it is appended to the note.
	URL string `json:"url,omitempty"`
	// Info marks activity a kind reports only on request (gitlab-mr info: true).
	Info bool `json:"-"`
	// Fold joins the events noted in one run that share it into one note: Fold, " — ", their texts after their
	// own "<Fold> — ", by ", ".
	Fold string `json:"-"`
}

// Line is the note's text: the event's text and its URL.
func (e Event) Line() string {
	if e.URL == "" {
		return e.Text
	}

	return e.Text + " " + e.URL
}

// Seen is the stored fingerprint of each key, as a source reads it.
type Seen map[string]string

// Result is what one run of a source brought back.
type Result struct {
	Events []Event
	// Cache is the kind's own state to keep for its next run, written only when the run succeeded.
	Cache json.RawMessage
	// Err fails the run: the stored keys stay as they are.
	Err error
	// Stderr is the source's error output, for the tick log.
	Stderr []string
}

// State is what is remembered of one watch, in state.json under watch.<name>.
type State struct {
	Keys    map[string]*Key     `json:"keys,omitempty"`
	Pending map[string]*Pending `json:"pending,omitempty"`
	// Baselined is set by the first successful run.
	Baselined bool      `json:"baselined,omitempty"`
	LastRun   time.Time `json:"last_run,omitzero"`
	LastOK    bool      `json:"last_ok,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	// Failures counts failed runs in a row; FailNoted is set once the dispatcher heard of them.
	Failures  int  `json:"failures,omitempty"`
	FailNoted bool `json:"fail_noted,omitempty"`
	// Recent are the last notes and drops, for watch ls.
	Recent  []string        `json:"recent,omitempty"`
	Cache   json.RawMessage `json:"cache,omitempty"`
	Running *Running        `json:"running,omitempty"`
}

// Key is a key the dispatcher was told of (or that a baseline recorded).
type Key struct {
	Fingerprint string    `json:"fingerprint"`
	NotedAt     time.Time `json:"noted_at,omitzero"`
	FirstSeen   time.Time `json:"first_seen"`
	// GoneAt is when a successful run first missed it; it is forgotten forget_after later.
	GoneAt time.Time `json:"gone_at,omitzero"`
}

// Pending is an event held by the quiet window.
type Pending struct {
	Fingerprint string    `json:"fingerprint"`
	At          time.Time `json:"at"`
	Text        string    `json:"text"`
}

// Running marks a run in progress, so two runs of one watch never overlap. Token names this one reservation: a
// result is applied only while it is still the current one.
type Running struct {
	PID   int       `json:"pid"`
	Since time.Time `json:"since"`
	Token string    `json:"token,omitempty"`
}

// Live reports whether the run is still going: its process lives and it is not past its timeout by a minute.
func (r *Running) Live(now time.Time, timeout time.Duration) bool {
	if r == nil || r.PID <= 0 || now.Sub(r.Since) > timeout+time.Minute {
		return false
	}

	err := syscall.Kill(r.PID, 0)

	return err == nil || err == syscall.EPERM
}

// NewState is the state of a watch that has never run.
func NewState() *State { return &State{} }

func (s *State) init() {
	if s.Keys == nil {
		s.Keys = map[string]*Key{}
	}

	if s.Pending == nil {
		s.Pending = map[string]*Pending{}
	}
}

// Seen is the stored fingerprints.
func (s *State) Seen() Seen {
	out := Seen{}

	if s == nil {
		return out
	}

	for k, v := range s.Keys {
		out[k] = v.Fingerprint
	}

	return out
}

// Clone is a deep copy, for a dry run.
func (s *State) Clone() *State {
	cp := &State{}
	if s == nil {
		return cp
	}

	raw, _ := json.Marshal(s)
	_ = json.Unmarshal(raw, cp)

	return cp
}

// Remember keeps a line in Recent, the last five.
func (s *State) Remember(line string) {
	s.Recent = append(s.Recent, line)
	if len(s.Recent) > recentMax {
		s.Recent = s.Recent[len(s.Recent)-recentMax:]
	}
}

const recentMax = 5

// Fate is what Apply did with an event.
type Fate string

// Fates.
const (
	Noted     Fate = "note"
	Dup       Fate = "dup"
	Quiet     Fate = "quiet"
	Baseline  Fate = "baseline"
	InfoOff   Fate = "info-off"
	QueueFull Fate = "queue-full"
	Dropped   Fate = "dropped"
)

// Outcome is one event's fate.
type Outcome struct {
	Event Event
	Fate  Fate
	// Wait is how much longer a quiet event is held.
	Wait time.Duration
	// Reason says why a dropped event was dropped.
	Reason string
}

// String is the fate as watch run --dry-run prints it.
func (o Outcome) String() string {
	switch o.Fate {
	case Quiet:
		return fmt.Sprintf("quiet %dm", int((o.Wait+time.Minute-1)/time.Minute))
	case Dropped:
		return string(o.Fate) + " (" + o.Reason + ")"
	default:
		return string(o.Fate)
	}
}

// Config is what Apply needs of the watch.
type Config struct {
	Quiet       time.Duration
	ForgetAfter time.Duration
	// Baseline records the first run without noting.
	Baseline bool
	// Info notes events a kind marks as info.
	Info bool
}

// GoneBeforeQuiet is the reason a held event is dropped when its news went away inside the quiet window.
const GoneBeforeQuiet = "gone-before-quiet"

// Apply diffs a successful run's events against the state: news is queued (queue returns nil once the note waits
// for the dispatcher) and its fingerprint stored only then; an event that moved within the quiet window is held;
// keys a run no longer reports are gone and forgotten after ForgetAfter.
func Apply(st *State, events []Event, cfg Config, now time.Time, queue func(Event) error) []Outcome {
	st.init()

	var out []Outcome

	if !st.Baselined && cfg.Baseline {
		for _, e := range events {
			st.Keys[e.Key] = &Key{Fingerprint: e.Fingerprint, FirstSeen: now}
			out = append(out, Outcome{Event: e, Fate: Baseline})
		}

		st.Baselined = true
		clear(st.Pending)

		return out
	}

	st.Baselined = true
	present := map[string]Event{}
	// due are the outcomes to note, in order.
	var due []int

	for _, e := range events {
		present[e.Key] = e
		k := st.Keys[e.Key]

		if k != nil {
			k.GoneAt = time.Time{}
		}

		switch {
		case k != nil && k.Fingerprint == e.Fingerprint:
			out = append(out, Outcome{Event: e, Fate: Dup})

			continue
		case e.Info && !cfg.Info:
			out = append(out, Outcome{Event: e, Fate: InfoOff})

			continue
		}

		if e.At != nil && cfg.Quiet > 0 {
			if moved := now.Sub(*e.At); moved < cfg.Quiet {
				st.Pending[e.Key] = &Pending{Fingerprint: e.Fingerprint, At: *e.At, Text: e.Line()}
				out = append(out, Outcome{Event: e, Fate: Quiet, Wait: cfg.Quiet - moved})

				continue
			}
		}

		due = append(due, len(out))
		out = append(out, Outcome{Event: e, Fate: Noted})
	}

	// News is queued once per fold: the events of one fold go out as one note, or none of them does.
	for _, group := range folds(out, due) {
		e := out[group[0]].Event
		if len(group) > 1 {
			e = folded(out, group)
		}

		err := queue(e)

		for _, i := range group {
			o := &out[i]
			if err != nil {
				o.Fate, o.Reason = QueueFull, err.Error()

				continue
			}

			first := now
			if k := st.Keys[o.Event.Key]; k != nil {
				first = k.FirstSeen
			}

			st.Keys[o.Event.Key] = &Key{Fingerprint: o.Event.Fingerprint, NotedAt: now, FirstSeen: first}
			delete(st.Pending, o.Event.Key)
		}
	}

	// A held event whose news went away (its key gone, back to the stored fingerprint, or now only info) is dropped.
	for _, key := range slices.Sorted(maps.Keys(st.Pending)) {
		p := st.Pending[key]
		e, ok := present[key]

		if ok && (st.Keys[key] == nil || st.Keys[key].Fingerprint != e.Fingerprint) && (!e.Info || cfg.Info) {
			continue
		}

		delete(st.Pending, key)
		out = append(out, Outcome{Event: Event{Key: key, Fingerprint: p.Fingerprint, Text: p.Text}, Fate: Dropped,
			Reason: GoneBeforeQuiet})
	}

	for key, k := range st.Keys {
		if _, ok := present[key]; ok {
			continue
		}

		if k.GoneAt.IsZero() {
			k.GoneAt = now
		} else if now.Sub(k.GoneAt) >= cfg.ForgetAfter {
			delete(st.Keys, key)
		}
	}

	return out
}

// folds groups the due outcomes by their events' Fold, in the order of each group's first event; an event without
// one is a group of its own.
func folds(out []Outcome, due []int) [][]int {
	var groups [][]int

	at := map[string]int{}

	for _, i := range due {
		f := out[i].Event.Fold
		if g, ok := at[f]; ok && f != "" {
			groups[g] = append(groups[g], i)

			continue
		}

		at[f] = len(groups)
		groups = append(groups, []int{i})
	}

	return groups
}

// folded is one event for the group's note: the fold, then each event's own part.
func folded(out []Outcome, group []int) Event {
	first := out[group[0]].Event
	parts := make([]string, 0, len(group))

	for _, i := range group {
		e := out[i].Event
		parts = append(parts, strings.TrimPrefix(e.Text, e.Fold+" — "))
	}

	return Event{Key: first.Key, Fingerprint: first.Fingerprint, Text: first.Fold + " — " + strings.Join(parts, ", "),
		At: first.At, URL: first.URL}
}

// Forget drops the stored fingerprints (one key, or all when key is empty), so the next run notes them again.
func (s *State) Forget(key string) int {
	s.init()

	if key == "" {
		n := len(s.Keys)
		clear(s.Keys)
		clear(s.Pending)
		s.Cache = nil

		return n
	}

	_, ok := s.Keys[key]
	delete(s.Keys, key)
	delete(s.Pending, key)
	s.Cache = nil

	if ok {
		return 1
	}

	return 0
}

// OneLine folds whitespace, newlines included, to single spaces.
func OneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
