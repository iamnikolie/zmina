package alarm

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

// Kind is what an alarm does when it rings.
type Kind string

const (
	// Remind is a reminder: for the owner (it lands on the todo and goes through the crew's notify command), for a
	// role (the role is started if down and told), or for another crew's dispatcher (To: a tell).
	Remind Kind = "remind"
	// Sleep wakes a role put to sleep: it starts on its last conversation and is told the note.
	Sleep Kind = "sleep"
	// Later brings a parked item back to the owner's todo.
	Later Kind = "later"
)

// State is where an alarm is.
type State string

const (
	// Set alarms wait for their time.
	Set State = "set"
	// Rang alarms went off (Rang says when).
	Rang State = "rang"
	// Off alarms were taken off before their time, or found nothing left to ring for.
	Off State = "off"
)

// Alarm is one thing to ring at a time.
type Alarm struct {
	ID   string    `json:"id"`
	Kind Kind      `json:"kind"`
	At   time.Time `json:"at"`
	// When is the words it was set with ("tomorrow 09:00").
	When string `json:"when,omitempty"`
	// Text is what it is about, in one line; Note what is told when it rings (a file's text), when there is more.
	Text string `json:"text"`
	Note string `json:"note,omitempty"`
	// Role is the role it wakes or tells; empty: the owner (or To).
	Role string `json:"role,omitempty"`
	// To is another crew, node/crew, whose dispatcher hears it as a tell.
	To string `json:"to,omitempty"`
	// Later is the parked item a Later alarm brings back.
	Later string `json:"later,omitempty"`
	// State is set until it rings or is taken off; an unknown state is taken as set, so nothing is lost.
	State State `json:"state"`
	// Rang is when it went off; Ask the todo item it became; Err why it could not do its part.
	Rang time.Time `json:"rang,omitzero"`
	Ask  string    `json:"ask,omitempty"`
	Err  string    `json:"err,omitempty"`
	By   string    `json:"by,omitempty"`

	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// Pending reports whether the alarm still waits to ring.
func (a Alarm) Pending() bool { return a.State != Rang && a.State != Off }

// For is whom the alarm is for, in words: the owner, a role, or another crew.
func (a Alarm) For() string {
	switch {
	case a.To != "":
		return a.To
	case a.Role != "":
		return a.Role
	}

	return "you"
}

var (
	idRe   = regexp.MustCompile(`^R-([0-9]+)$`)
	fileRe = regexp.MustCompile(`^R-([0-9]+)\.json$`)
)

// Store keeps one crew's alarms. Callers hold zmina's state lock while they write.
type Store struct {
	Dir string
}

// Add gives the alarm the next id — above every R-<n>.json in the directory, readable or not — and writes it as
// set. It never replaces an existing file.
func (s *Store) Add(a Alarm, now time.Time) (Alarm, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return a, fmt.Errorf("alarm: %w", err)
	}

	n := 0

	for _, e := range entries {
		if m := fileRe.FindStringSubmatch(e.Name()); m != nil {
			if v, _ := strconv.Atoi(m[1]); v > n {
				n = v
			}
		}
	}

	a.ID = "R-" + strconv.Itoa(n+1)
	a.State = Set
	a.Created, a.Updated = now, now

	return a, s.write(a, true)
}

// Put writes an alarm over its file.
func (s *Store) Put(a Alarm) error { return s.write(a, false) }

func (s *Store) write(a Alarm, fresh bool) error {
	if !idRe.MatchString(a.ID) {
		return fmt.Errorf("alarm: bad id %q", a.ID)
	}

	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("alarm: %w", err)
	}

	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}

	path := filepath.Join(s.Dir, a.ID+".json")

	if fresh {
		// Claim the name first: O_EXCL fails if anything is there.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("alarm: %w", err)
		}

		_ = f.Close()
	}

	if err := os.WriteFile(path+".tmp", raw, 0o600); err != nil {
		return fmt.Errorf("alarm: %w", err)
	}

	return os.Rename(path+".tmp", path)
}

// Get reads one alarm; "3", "r-3" and "R-3" are R-3.
func (s *Store) Get(id string) (Alarm, error) {
	id = Normalize(id)
	if !idRe.MatchString(id) {
		return Alarm{}, fmt.Errorf("no alarm %s", id)
	}

	a, err := read(filepath.Join(s.Dir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return a, fmt.Errorf("no alarm %s", id)
	}

	return a, err
}

func read(path string) (Alarm, error) {
	var a Alarm

	raw, err := os.ReadFile(path)
	if err != nil {
		return a, err
	}

	name := strings.TrimSuffix(filepath.Base(path), ".json")

	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("alarm %s: %w", name, err)
	}

	if a.ID != name {
		return a, fmt.Errorf("alarm %s: the file says it is %q", name, a.ID)
	}

	return a, nil
}

// Scan reads every alarm, in id order, and names the files it could not read.
func (s *Store) Scan() (all []Alarm, problems []string, err error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}

	if err != nil {
		return nil, nil, fmt.Errorf("alarm: %w", err)
	}

	for _, e := range entries {
		if !fileRe.MatchString(e.Name()) {
			continue
		}

		a, err := read(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			problems = append(problems, err.Error())

			continue
		}

		all = append(all, a)
	}

	sort.Slice(all, func(i, j int) bool { return num(all[i].ID) < num(all[j].ID) })

	return all, problems, nil
}

// Pending is every alarm still to ring, soonest first.
func (s *Store) Pending() []Alarm {
	all, _, _ := s.Scan()

	var out []Alarm

	for _, a := range all {
		if a.Pending() {
			out = append(out, a)
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })

	return out
}

// Due is every pending alarm whose time has come, soonest first.
func (s *Store) Due(now time.Time) []Alarm {
	var out []Alarm

	for _, a := range s.Pending() {
		if !a.At.After(now) {
			out = append(out, a)
		}
	}

	return out
}

// Move sets a pending alarm to another time.
func (s *Store) Move(id string, at time.Time, when string, now time.Time) (Alarm, error) {
	a, err := s.Get(id)
	if err != nil {
		return a, err
	}

	if !a.Pending() {
		return a, fmt.Errorf("%s already %s", a.ID, a.State)
	}

	a.At, a.When, a.Updated = at, when, now

	return a, s.Put(a)
}

// TakeOff takes a pending alarm off.
func (s *Store) TakeOff(id string, now time.Time) (Alarm, error) {
	a, err := s.Get(id)
	if err != nil {
		return a, err
	}

	if !a.Pending() {
		return a, fmt.Errorf("%s already %s", a.ID, a.State)
	}

	a.State, a.Updated = Off, now

	return a, s.Put(a)
}

// Normalize turns 3, r-3 and R-3 into R-3.
func Normalize(id string) string {
	id = strings.ToUpper(strings.TrimSpace(id))
	if _, err := strconv.Atoi(id); err == nil {
		return "R-" + id
	}

	return id
}

func num(id string) int {
	v, _ := strconv.Atoi(strings.TrimPrefix(id, "R-"))

	return v
}
