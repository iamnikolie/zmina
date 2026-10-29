// Package asks is what the dispatcher asks the owner: a decision it keeps asking, with its recommendation and a
// reason, or a check only the owner can do. One JSON file per ask under asks/ next to the crew file, written alone
// like tasks and the parked list. An answered ask stays on disk with its state and the owner's words.
package asks

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

// Kind is what the owner is asked for.
type Kind string

const (
	// Decision is a question with a recommendation: yes takes it.
	Decision Kind = "decision"
	// Check is something only the owner can do: done says it is done.
	Check Kind = "check"
)

// ParseKind reads a kind; empty is a decision when there is a recommendation, else a check.
func ParseKind(s, rec string) (Kind, error) {
	switch Kind(s) {
	case Decision, Check:
		return Kind(s), nil
	case "":
		if rec != "" {
			return Decision, nil
		}

		return Check, nil
	}

	return "", fmt.Errorf("kind %q: decision or check", s)
}

// State is where an ask is.
type State string

const (
	// Open asks wait on the owner.
	Open State = "open"
	// Done: the owner did the check.
	Done State = "done"
	// Yes: the owner took the recommendation.
	Yes State = "yes"
	// Answered: the owner answered in their own words (Ask.Answer).
	Answered State = "answered"
	// Parked: put on the parked list for later.
	Parked State = "parked"
)

// Ask is one thing waiting on the owner.
type Ask struct {
	ID   string `json:"id"`
	Kind Kind   `json:"kind"`
	Text string `json:"text"`
	// Rec is the recommended answer, Reason why in one line.
	Rec    string `json:"rec,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Role is who asks, Task the task it is about, Blocks the role it holds up until answered.
	Role   string `json:"role,omitempty"`
	Task   string `json:"task,omitempty"`
	Blocks string `json:"blocks,omitempty"`
	// State is open until answered; an unknown state is taken as open, so nothing is lost.
	State   State     `json:"state"`
	Answer  string    `json:"answer,omitempty"`
	By      string    `json:"by,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// Open reports whether the ask still waits on the owner.
func (a Ask) Open() bool {
	return a.State != Done && a.State != Yes && a.State != Answered && a.State != Parked
}

var (
	idRe   = regexp.MustCompile(`^A-([0-9]+)$`)
	fileRe = regexp.MustCompile(`^A-([0-9]+)\.json$`)
)

// Store keeps one crew's asks. Callers hold zmina's state lock while they write.
type Store struct {
	Dir string
}

// Add gives the ask the next id — above every A-<n>.json in the directory, readable or not — and writes it. It
// never replaces an existing file.
func (s *Store) Add(it Ask, now time.Time) (Ask, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return it, fmt.Errorf("asks: %w", err)
	}

	n := 0

	for _, e := range entries {
		if m := fileRe.FindStringSubmatch(e.Name()); m != nil {
			if v, _ := strconv.Atoi(m[1]); v > n {
				n = v
			}
		}
	}

	it.ID = "A-" + strconv.Itoa(n+1)
	it.State = Open
	it.Created, it.Updated = now, now

	return it, s.write(it, true)
}

// Put writes an ask over its file.
func (s *Store) Put(it Ask) error { return s.write(it, false) }

func (s *Store) write(it Ask, fresh bool) error {
	if !idRe.MatchString(it.ID) {
		return fmt.Errorf("asks: bad id %q", it.ID)
	}

	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("asks: %w", err)
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
			return fmt.Errorf("asks: %w", err)
		}

		_ = f.Close()
	}

	if err := os.WriteFile(path+".tmp", raw, 0o600); err != nil {
		return fmt.Errorf("asks: %w", err)
	}

	return os.Rename(path+".tmp", path)
}

// Get reads one ask; "3", "a-3" and "A-3" are A-3.
func (s *Store) Get(id string) (Ask, error) {
	id = Normalize(id)
	if !idRe.MatchString(id) {
		return Ask{}, fmt.Errorf("no ask %s", id)
	}

	it, err := read(filepath.Join(s.Dir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return it, fmt.Errorf("no ask %s", id)
	}

	return it, err
}

func read(path string) (Ask, error) {
	var it Ask

	raw, err := os.ReadFile(path)
	if err != nil {
		return it, err
	}

	name := strings.TrimSuffix(filepath.Base(path), ".json")

	if err := json.Unmarshal(raw, &it); err != nil {
		return it, fmt.Errorf("ask %s: %w", name, err)
	}

	if it.ID != name {
		return it, fmt.Errorf("ask %s: the file says it is %q", name, it.ID)
	}

	return it, nil
}

// Scan reads every ask, oldest first, and names the files it could not read.
func (s *Store) Scan() (items []Ask, problems []string, err error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}

	if err != nil {
		return nil, nil, fmt.Errorf("asks: %w", err)
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

// Pending is every ask still waiting on the owner, oldest first.
func (s *Store) Pending() []Ask {
	all, _, _ := s.Scan()

	var out []Ask

	for _, it := range all {
		if it.Open() {
			out = append(out, it)
		}
	}

	return out
}

// Close takes an open ask off the list as done, yes, answered or parked; answer, when given, is the owner's words.
func (s *Store) Close(id string, st State, answer string, now time.Time) (Ask, error) {
	it, err := s.Get(id)
	if err != nil {
		return it, err
	}

	if !it.Open() {
		return it, fmt.Errorf("%s is already %s", it.ID, it.State)
	}

	it.State, it.Updated = st, now
	if answer != "" {
		it.Answer = answer
	}

	return it, s.Put(it)
}

// Normalize turns 3, a-3 and A-3 into A-3.
func Normalize(id string) string {
	id = strings.ToUpper(strings.TrimSpace(id))
	if _, err := strconv.Atoi(id); err == nil {
		return "A-" + id
	}

	return id
}

func num(id string) int {
	v, _ := strconv.Atoi(strings.TrimPrefix(id, "A-"))

	return v
}
