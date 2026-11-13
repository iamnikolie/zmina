package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/fsx"
)

// What is remembered of a plugin, so that each event goes once and an answered goes only to a plugin that was told: one small
// file a plugin (crew.Layout.PluginTold), apart from state.json, written by the run that holds the plugin's lock. Nothing
// decides by it but what to send next; a file that is missing or does not read is a plugin never told anything, which is
// at worst an event sent twice.

// The retry schedule of an event whose run failed (non-zero, a deadline, a program that is not there): the first try, and
// then one after each wait of Backoff; the last one that fails drops the event.
const (
	MaxTries = 4
	// Keep is how long a closed or dropped entry stays in the file.
	Keep = 7 * 24 * time.Hour
	// Jitter is what a beat's start may be late by: a question that has stood a beat less this is taken to have stood the
	// beat (the beat is a minute, and one that began a few seconds late must not push a dialog a whole beat on).
	Jitter = 5 * time.Second
	// readMax bounds what is read of the file.
	readMax = 4 << 20
	// recent is how far back a beat counts an ask as new on a plugin's first look: the one beat before it.
	recent = time.Minute
)

var backoffs = [MaxTries - 1]time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}

// Backoff is how long to wait after the n-th failed try (1 to MaxTries-1) before the next.
func Backoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}

	if n > len(backoffs) {
		n = len(backoffs)
	}

	return backoffs[n-1]
}

// Entry is one thing a plugin was told of, or given up on telling.
type Entry struct {
	ID string `json:"id"`
	// Kind is the event it went as: ask, blocking, dialog or reminder.
	Kind string `json:"kind"`
	// Role is a dialog's role.
	Role   string    `json:"role,omitempty"`
	Opened time.Time `json:"opened,omitzero"`
	Told   time.Time `json:"told,omitzero"`
	// Dropped: every try failed; the plugin was never told, so it is never told of the end. Revive arms one more go for a
	// dropped thing that is still open (Revived: it has had it), taken once the plugin has taken another event, so a plugin
	// that was down is told what it missed, and a thing it cannot take is not tried for ever.
	Dropped bool `json:"dropped,omitempty"`
	Revive  bool `json:"revive,omitempty"`
	Revived bool `json:"revived,omitempty"`
	// Escalated: it was told as ask and then held a role up, and the plugin has been told once more as blocking, or the try
	// to was given up.
	Escalated bool `json:"escalated,omitempty"`
	// Closed is when its answered went out (or was given up, or the plugin takes none); zero while it is open.
	Closed time.Time `json:"closed,omitzero"`
}

// Queued is an event whose run failed: the event as it was built, how many tries it has had, and when the next is.
type Queued struct {
	// Key is the thing's key (ItemKey, DialogKey), or ClosingKey of it for the answered that closes it.
	Key   string `json:"key"`
	Entry Entry  `json:"entry"`
	// Event is the kind it is sent as (Entry.Kind, or Answered).
	Event string    `json:"event"`
	Line  string    `json:"line"`
	Tries int       `json:"tries"`
	First time.Time `json:"first"`
	Next  time.Time `json:"next"`
	Err   string    `json:"err,omitempty"`
}

// Last is how a run ended: when, for which event and id, and how.
type Last struct {
	At    time.Time `json:"at"`
	Event string    `json:"event"`
	ID    string    `json:"id"`
	OK    bool      `json:"ok"`
	Err   string    `json:"err,omitempty"`
}

// State is what is remembered of one plugin.
type State struct {
	V int `json:"v"`
	// Since is when the plugin was first looked at: an ask opened before it is not new to it (only one that holds a role up
	// is told, as notify_on: blocking does).
	Since time.Time `json:"since,omitzero"`
	// Told is each thing told of, keyed by ItemKey or DialogKey.
	Told map[string]*Entry `json:"told,omitempty"`
	// Queue is the events waiting for another try.
	Queue []Queued `json:"queue,omitempty"`
	// Seen is each question on a role's screen when it was first seen (DialogKey): it is told once it has stood dialog_after.
	Seen map[string]Sight `json:"seen,omitempty"`
	// Partial is what the last look could not read (Gaps.Words), empty when it read everything; Problem is why the plugin's
	// program could not be run at the last delivery, empty when it could.
	Partial string `json:"partial,omitempty"`
	Problem string `json:"problem,omitempty"`
	// Arming is why the last dialog could not be armed for a one-shot press, empty when it could: said once, as Problem is.
	Arming string `json:"arming,omitempty"`
	// Last is the newest run, Failed the newest run that failed.
	Last   *Last `json:"last,omitempty"`
	Failed *Last `json:"failed,omitempty"`
}

// Sight is a question on a role's screen as it was first seen: when, and which stand of the role's session it is of (the moment
// the session began waiting): a role that answers and is asked again within a beat has a new stand, whatever the words.
type Sight struct {
	First time.Time `json:"first"`
	Since time.Time `json:"since,omitzero"`
	Role  string    `json:"role,omitempty"`
}

// Gaps is what a look could not read: the ids of items whose files did not read, and the roles whose screens could not be
// seen. A thing in a gap is not judged ended, nor forgotten: only its own entries wait, and the rest of the look is as good as
// it was.
type Gaps struct {
	Items map[string]bool
	Roles map[string]bool
}

// LaterAll is the key of Gaps.Items that stands for every parked item: their files do not say which did not read.
const LaterAll = "L-*"

// Hides reports whether the look could not see what an entry is about.
func (g Gaps) Hides(e Entry) bool {
	if e.Kind == Dialog {
		return g.Roles[e.Role]
	}

	return g.Items[e.ID] || strings.HasPrefix(e.ID, "L-") && g.Items[LaterAll]
}

// Empty reports a look that read everything.
func (g Gaps) Empty() bool { return len(g.Items) == 0 && len(g.Roles) == 0 }

// Words says what was not read, for doctor and plugin ls: `A-3, role dev`.
func (g Gaps) Words() string {
	var out []string

	for id := range g.Items {
		out = append(out, id)
	}

	for role := range g.Roles {
		out = append(out, "role "+role)
	}

	sort.Strings(out)

	return strings.Join(out, ", ")
}

// ItemKey names an item for the ledger: its id. A task that waits on the owner a second time has the same id, and is a new
// thing only because the first was closed in between (an entry that is closed does not stand in the way of the next); a task
// whose record changes while it waits is the same thing, not a new one.
func ItemKey(id string) string { return id }

// DialogKey names a dialog for the ledger.
func DialogKey(id string) string { return "dialog:" + id }

// ClosingKey names the answered that closes a thing.
func ClosingKey(key string) string { return "closing:" + key }

// Load reads a plugin's file; a state that has never been told anything when there is none, or it does not read (the
// error says so, and the state is still usable).
func Load(path string) (*State, error) {
	st := &State{V: Version}

	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}

	if err != nil {
		return st, err
	}

	defer func() { _ = f.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(f, readMax))

	if err := json.Unmarshal(raw, st); err != nil {
		return &State{V: Version}, fmt.Errorf("%s: %w (taken as a plugin told of nothing)", path, err)
	}

	return st, nil
}

// Save writes the file whole or not at all, and not at all when it already says the same: a beat that changed nothing writes
// nothing.
func (s *State) Save(path string) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}

	raw = append(raw, '\n')

	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, raw) {
		return nil
	}

	if err := fsx.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	return fsx.WriteFile(path, raw, 0o600)
}

// Observe notes what a look found open: when the plugin was first looked at, and when each question on a role's screen was first
// seen, and of which stand of its session. A question seen before and gone, and never told, is forgotten, so one that comes
// back stands anew; so is one whose session left waiting and began again. What a gap hides is not forgotten.
func (s *State) Observe(things []Thing, now time.Time, gaps Gaps) {
	if s.Since.IsZero() {
		s.Since = now.Add(-recent)
	}

	if s.Seen == nil {
		s.Seen = map[string]Sight{}
	}

	standing := map[string]bool{}

	for _, t := range things {
		if t.Class != DialogThing {
			continue
		}

		standing[t.Key] = true

		if sight, ok := s.Seen[t.Key]; !ok || !t.Opened.IsZero() && !sight.Since.IsZero() && !t.Opened.Equal(sight.Since) {
			s.Seen[t.Key] = Sight{First: now, Since: t.Opened, Role: t.Role}
		}
	}

	for key, sight := range s.Seen {
		if !standing[key] && !gaps.Roles[sight.Role] {
			delete(s.Seen, key)
		}
	}

	// A question the plugin was never told of (every try failed) that has left the screen is not held against the next one that
	// has the same words.
	for key, e := range s.Told {
		if e.Dropped && e.Kind == Dialog && !standing[key] && !gaps.Hides(*e) {
			delete(s.Told, key)
		}
	}
}

// Prune forgets what has been closed or dropped for Keep, and a seen question older than Keep. A thing that is still open is
// never forgotten, however long ago it was dropped: forgotten, it would be told as new.
func (s *State) Prune(now time.Time, open func(key string) bool) {
	for key, e := range s.Told {
		switch {
		case !e.Closed.IsZero() && now.Sub(e.Closed) > Keep:
			delete(s.Told, key)
		case e.Dropped && now.Sub(e.Told) > Keep && (open == nil || !open(key)):
			delete(s.Told, key)
		}
	}

	for key, sight := range s.Seen {
		if now.Sub(sight.First) > Keep {
			delete(s.Seen, key)
		}
	}
}

// Revive arms one more go for what was dropped and is still open (open says which are): called once the plugin has taken an
// event, which says it can again. A thing that has had its go and was dropped again stays dropped.
func (s *State) Revive(open func(key string) bool) {
	for key, e := range s.Told {
		if e.Dropped && !e.Revived && !e.Revive && open(key) {
			e.Revive = true
		}
	}
}

// queued is the waiting event of a key.
func (s *State) queued(key string) (int, bool) {
	for i, q := range s.Queue {
		if q.Key == key {
			return i, true
		}
	}

	return -1, false
}

// Succeed records an event that went: a new one is told (the thing is in Told), an answered closes its entry.
func (s *State) Succeed(d Due, now time.Time) {
	if s.Told == nil {
		s.Told = map[string]*Entry{}
	}

	s.dequeue(d.QueueKey())

	if d.Kind == Answered {
		if e, ok := s.Told[d.Key]; ok {
			e.Closed = now
		}
	} else {
		e := d.Entry
		e.Told = now
		s.Told[d.Key] = &e
	}

	s.Last = &Last{At: now, Event: d.Kind, ID: d.Entry.ID, OK: true}
}

// Fail records an event whose run failed: it waits for its next try, or, when that was its last, it is dropped (a new
// event is marked so it is never sent again, an answered is given up). dropped reports the last.
func (s *State) Fail(d Due, line string, now time.Time, reason string) (dropped bool) {
	if s.Told == nil {
		s.Told = map[string]*Entry{}
	}

	key := d.QueueKey()
	i, ok := s.queued(key)

	if !ok {
		s.Queue = append(s.Queue, Queued{Key: key, Entry: d.Entry, Event: d.Kind, Line: line, First: now})
		i = len(s.Queue) - 1
	}

	q := &s.Queue[i]
	q.Tries++
	q.Err = reason

	l := &Last{At: now, Event: d.Kind, ID: d.Entry.ID, Err: reason}
	s.Last, s.Failed = l, l

	if q.Tries >= MaxTries {
		s.dequeue(key)

		if d.Kind == Answered {
			if e, ok := s.Told[d.Key]; ok {
				e.Closed = now
			}
		} else if prev, ok := s.Told[d.Key]; ok && !prev.Dropped && prev.Closed.IsZero() {
			// A try to tell it once more (as blocking) that failed: what it was told as before stands.
			prev.Escalated = true
		} else {
			e := d.Entry
			e.Told, e.Dropped = now, true
			s.Told[d.Key] = &e
		}

		return true
	}

	q.Next = now.Add(Backoff(q.Tries))

	return false
}

func (s *State) dequeue(key string) {
	if i, ok := s.queued(key); ok {
		s.Queue = append(s.Queue[:i], s.Queue[i+1:]...)
	}
}

// QueuedEntry is what a waiting event is about.
func (s *State) QueuedEntry(key string) (Entry, bool) {
	if i, ok := s.queued(key); ok {
		return s.Queue[i].Entry, true
	}

	return Entry{}, false
}

// Cancel takes a waiting event off its queue without sending it: what it was about ended before the plugin was told.
func (s *State) Cancel(key string) { s.dequeue(key) }

// Open is how many things the plugin was told of that are still open.
func (s *State) Open() int {
	n := 0

	for _, e := range s.Told {
		if !e.Dropped && e.Closed.IsZero() {
			n++
		}
	}

	return n
}
