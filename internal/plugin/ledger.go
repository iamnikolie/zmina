package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	// Dropped: every try failed; the plugin was never told, so it is never told of the end.
	Dropped bool `json:"dropped,omitempty"`
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
	// Seen is when each question on a role's screen was first seen (DialogKey): it is told once it has stood dialog_after.
	Seen map[string]time.Time `json:"seen,omitempty"`
	// Last is the newest run, Failed the newest run that failed.
	Last   *Last `json:"last,omitempty"`
	Failed *Last `json:"failed,omitempty"`
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

// Save writes the file whole or not at all.
func (s *State) Save(path string) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}

	if err := fsx.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	return fsx.WriteFile(path, append(raw, '\n'), 0o600)
}

// Observe notes what a look found open: when the plugin was first looked at, and when each question on a role's screen was
// first seen. A question seen before and gone, and never told, is forgotten, so one that comes back stands anew. whole is
// false when the look may not have found everything (a file did not read), and then nothing is forgotten.
func (s *State) Observe(things []Thing, now time.Time, whole bool) {
	if s.Since.IsZero() {
		s.Since = now.Add(-recent)
	}

	if s.Seen == nil {
		s.Seen = map[string]time.Time{}
	}

	standing := map[string]bool{}

	for _, t := range things {
		if t.Class != DialogThing {
			continue
		}

		standing[t.Key] = true

		if _, ok := s.Seen[t.Key]; !ok {
			s.Seen[t.Key] = now
		}
	}

	if !whole {
		return
	}

	for key := range s.Seen {
		if !standing[key] {
			delete(s.Seen, key)
		}
	}

	// A question the plugin was never told of (every try failed) that has left the screen is not held against the next one that
	// has the same words.
	for key, e := range s.Told {
		if e.Dropped && e.Kind == Dialog && !standing[key] {
			delete(s.Told, key)
		}
	}
}

// Prune forgets what has been closed or dropped for Keep, and a seen question older than Keep.
func (s *State) Prune(now time.Time) {
	for key, e := range s.Told {
		switch {
		case !e.Closed.IsZero() && now.Sub(e.Closed) > Keep:
			delete(s.Told, key)
		case e.Dropped && now.Sub(e.Told) > Keep:
			delete(s.Told, key)
		}
	}

	for key, at := range s.Seen {
		if now.Sub(at) > Keep {
			delete(s.Seen, key)
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
