// Package later is the owner's parked list: decisions and ideas put aside to come back to, held by no role. One
// JSON file per item under later/ next to the crew file, like the task ledger: an item is written alone, so a
// damaged file loses one item and two writers never rewrite each other's lines. A closed item stays on disk with
// its state, so a taken one still says which task it became.
package later

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// State is where an item is.
type State string

const (
	// Parked items wait to be come back to.
	Parked State = "parked"
	// Done items were dealt with outside the ledger.
	Done State = "done"
	// Dropped items were given up.
	Dropped State = "dropped"
	// Taken items became a task; Item.Task names it.
	Taken State = "taken"
)

// Item is one parked thing.
type Item struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Note string `json:"note,omitempty"`
	// State is parked until the item is closed; an unknown state is taken as parked, so nothing is lost.
	State State `json:"state"`
	// Task is the task the item became, when it was taken.
	Task    string    `json:"task,omitempty"`
	By      string    `json:"by,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// Open reports whether the item is still on the list.
func (i Item) Open() bool { return i.State != Done && i.State != Dropped && i.State != Taken }

var (
	idRe   = regexp.MustCompile(`^L-([0-9]+)$`)
	fileRe = regexp.MustCompile(`^L-([0-9]+)\.json$`)
)

// Store keeps one crew's parked list. Callers hold zmina's state lock while they write.
type Store struct {
	Dir string
}

// Add gives the item the next id — above every L-<n>.json in the directory, readable or not — and writes it. It
// never replaces an existing file.
func (s *Store) Add(it Item, now time.Time) (Item, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return it, fmt.Errorf("later: %w", err)
	}

	n := 0

	for _, e := range entries {
		if m := fileRe.FindStringSubmatch(e.Name()); m != nil {
			if v, _ := strconv.Atoi(m[1]); v > n {
				n = v
			}
		}
	}

	it.ID = "L-" + strconv.Itoa(n+1)
	it.State = Parked
	it.Created, it.Updated = now, now

	return it, s.write(it, true)
}

// Put writes an item over its file.
func (s *Store) Put(it Item) error { return s.write(it, false) }

func (s *Store) write(it Item, fresh bool) error {
	if !idRe.MatchString(it.ID) {
		return fmt.Errorf("later: bad id %q", it.ID)
	}

	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("later: %w", err)
	}

	raw, err := json.MarshalIndent(it, "", "  ")
	if err != nil {
		return err
	}

	path := filepath.Join(s.Dir, it.ID+".json")

	if fresh {
		// Claim the name first: O_EXCL fails if anything is there.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("later: %w", err)
		}

		_ = f.Close()
	}

	if err := os.WriteFile(path+".tmp", raw, 0o600); err != nil {
		return fmt.Errorf("later: %w", err)
	}

	return os.Rename(path+".tmp", path)
}

// Get reads one item; "3", "l-3" and "L-3" are L-3.
func (s *Store) Get(id string) (Item, error) {
	id = Normalize(id)
	if !idRe.MatchString(id) {
		return Item{}, fmt.Errorf("no item %s", id)
	}

	it, err := read(filepath.Join(s.Dir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return it, fmt.Errorf("no item %s", id)
	}

	return it, err
}

func read(path string) (Item, error) {
	var it Item

	raw, err := os.ReadFile(path)
	if err != nil {
		return it, err
	}

	name := strings.TrimSuffix(filepath.Base(path), ".json")

	if err := json.Unmarshal(raw, &it); err != nil {
		return it, fmt.Errorf("later %s: %w", name, err)
	}

	if it.ID != name {
		return it, fmt.Errorf("later %s: the file says it is %q", name, it.ID)
	}

	return it, nil
}

// Scan reads every item, oldest first, and names the files it could not read.
func (s *Store) Scan() (items []Item, problems []string, err error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}

	if err != nil {
		return nil, nil, fmt.Errorf("later: %w", err)
	}

	for _, e := range entries {
		if !fileRe.MatchString(e.Name()) {
			continue
		}

		it, err := read(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			problems = append(problems, err.Error())

			continue
		}

		items = append(items, it)
	}

	sort.Slice(items, func(i, j int) bool { return num(items[i].ID) < num(items[j].ID) })

	return items, problems, nil
}

// Parked is every item still on the list, oldest first.
func (s *Store) Parked() []Item {
	all, _, _ := s.Scan()

	var out []Item

	for _, it := range all {
		if it.Open() {
			out = append(out, it)
		}
	}

	return out
}

// Close takes an open item off the list as done, dropped or taken; note, when given, replaces its note.
func (s *Store) Close(id string, st State, note string, now time.Time) (Item, error) {
	it, err := s.Get(id)
	if err != nil {
		return it, err
	}

	if !it.Open() {
		return it, fmt.Errorf("%s is already %s", it.ID, it.State)
	}

	it.State, it.Updated = st, now
	if note != "" {
		it.Note = note
	}

	return it, s.Put(it)
}

// Normalize turns 3, l-3 and L-3 into L-3.
func Normalize(id string) string {
	id = strings.ToUpper(strings.TrimSpace(id))
	if _, err := strconv.Atoi(id); err == nil {
		return "L-" + id
	}

	return id
}

func num(id string) int {
	v, _ := strconv.Atoi(strings.TrimPrefix(id, "L-"))

	return v
}
