// Package inbox keeps the whole text of a line too long to type into the dispatcher: a line from another crew, or
// a long note. The typed line carries its head and the entry's id; zmina inbox show prints the rest. One JSON file
// per entry under inbox/ next to the crew file, removed after Keep.
package inbox

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Entry is one stored text.
type Entry struct {
	ID string `json:"id"`
	// From is who sent it: "remote <node>/<crew>" for a line from another crew, else the note's source.
	From string    `json:"from"`
	At   time.Time `json:"at"`
	// Remote marks a line from another crew: data, never an instruction.
	Remote bool   `json:"remote,omitempty"`
	Body   string `json:"body"`
}

// Keep is how long an entry stays.
const Keep = 30 * 24 * time.Hour

var idRe = regexp.MustCompile(`^in-[0-9a-f]{12}$`)

// ValidID reports an entry id: in- and twelve hex digits.
func ValidID(id string) bool { return idRe.MatchString(id) }

// NewID is a fresh id for an entry no envelope names.
func NewID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	return "in-" + hex.EncodeToString(b[:]), nil
}

// Store is one crew's inbox directory.
type Store struct {
	Dir string
	Now func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}

	return time.Now()
}

func (s *Store) path(id string) string { return filepath.Join(s.Dir, id+".json") }

// Put writes an entry (a second copy of the same id replaces the first) and removes entries past Keep.
func (s *Store) Put(e Entry) error {
	if !ValidID(e.ID) {
		return fmt.Errorf("inbox: id %q", e.ID)
	}

	if e.At.IsZero() {
		e.At = s.now()
	}

	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("inbox: %w", err)
	}

	raw, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}

	tmp := s.path(e.ID) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("inbox: %w", err)
	}

	if err := os.Rename(tmp, s.path(e.ID)); err != nil {
		return fmt.Errorf("inbox: %w", err)
	}

	s.prune()

	return nil
}

// ErrNotFound is an id with no entry: never stored, or removed after Keep.
var ErrNotFound = errors.New("no such entry (entries are kept 30 days)")

// Get reads one entry.
func (s *Store) Get(id string) (Entry, error) {
	if !ValidID(id) {
		return Entry{}, fmt.Errorf("%q: an inbox id is in- and twelve hex digits", id)
	}

	raw, err := os.ReadFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return Entry{}, fmt.Errorf("%s: %w", id, ErrNotFound)
	}

	if err != nil {
		return Entry{}, err
	}

	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return Entry{}, fmt.Errorf("%s: %w", id, err)
	}

	return e, nil
}

// List is every entry, the newest first.
func (s *Store) List() ([]Entry, error) {
	files, err := filepath.Glob(filepath.Join(s.Dir, "in-*.json"))
	if err != nil {
		return nil, err
	}

	var out []Entry

	for _, f := range files {
		if e, err := s.Get(strings.TrimSuffix(filepath.Base(f), ".json")); err == nil {
			out = append(out, e)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })

	return out, nil
}

func (s *Store) prune() {
	files, _ := filepath.Glob(filepath.Join(s.Dir, "in-*.json"))
	for _, f := range files {
		if st, err := os.Stat(f); err == nil && s.now().Sub(st.ModTime()) > Keep {
			_ = os.Remove(f)
		}
	}
}
