// Package lesson is what the roles of a crew learn and write down: a rule that should hold from now on, why, who it
// applies to and whether an instruction file already says it. One JSON file per lesson under lessons/ next to the
// crew file, written alone like tasks and asks. A later triage turns lessons into edits of the instruction files;
// this package is only the storage, the markdown reader (parse.go) and the scan that keeps secrets and customer
// data out of the text (scan.go).
package lesson

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

// Applies is how far a lesson reaches.
type Applies string

const (
	// Every: every seat of the crew.
	Every Applies = "every"
	// Role: the role that wrote it.
	Role Applies = "role"
	// Task: the one task it was learned on.
	Task Applies = "task"
	// Unsure: the writer does not know; the triage decides.
	Unsure Applies = "unsure"
)

// ParseApplies reads the words roles use for it, in any case: every seat, this role, one task, not sure, and the
// short forms (every, all, role, one role, task, unsure).
func ParseApplies(s string) (Applies, error) {
	switch strings.Join(strings.Fields(strings.ToLower(s)), " ") {
	case "every seat", "every", "all":
		return Every, nil
	case "this role", "role", "one role":
		return Role, nil
	case "one task", "task":
		return Task, nil
	case "not sure", "unsure":
		return Unsure, nil
	}

	return "", fmt.Errorf("applies to %q: every seat, this role, one task or not sure", s)
}

// State is where a lesson is on its way into the instruction files.
type State string

const (
	// New lessons wait for the triage.
	New State = "new"
	// InMR: an edit for it is in a merge request (Lesson.MR).
	InMR State = "in-mr"
	// Merged: the edit landed.
	Merged State = "merged"
	// Dropped: the triage turned it down (Lesson.Reason says why).
	Dropped State = "dropped"
)

// Lesson is one lasting rule a role learned.
type Lesson struct {
	ID string `json:"id"`
	// Rule is the rule in one line, Why the reason for it.
	Rule string `json:"rule"`
	Why  string `json:"why"`
	// Applies says how far it reaches; InFile is the instruction file that already says it, if the writer knows one.
	Applies Applies `json:"applies"`
	InFile  string  `json:"in_file,omitempty"`
	// Role is who wrote it, Task the task it came out of.
	Role string    `json:"role,omitempty"`
	Task string    `json:"task,omitempty"`
	At   time.Time `json:"at"`
	// State is New until the triage moves it; an unknown value is kept as it is.
	State State `json:"state"`
	// Verdict and Reason are the triage's call and its words; MR is the merge request that carries the edit.
	Verdict string    `json:"verdict,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	MR      string    `json:"mr,omitempty"`
	Updated time.Time `json:"updated"`
}

var (
	idRe     = regexp.MustCompile(`^LS-([0-9]+)$`)
	fileRe   = regexp.MustCompile(`^LS-([0-9]+)\.json$`)
	digitsRe = regexp.MustCompile(`^[0-9]+$`)
)

// Store keeps one crew's lessons. Callers hold zmina's state lock while they write.
type Store struct {
	Dir string
}

// Add gives the lesson the next id — above every LS-<n>.json in the directory, readable or not — marks it new and
// writes it. It never replaces an existing file, and it makes the directory when there is none.
func (s *Store) Add(l Lesson, now time.Time) (Lesson, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return l, fmt.Errorf("lesson: %w", err)
	}

	n := 0

	for _, e := range entries {
		if m := fileRe.FindStringSubmatch(e.Name()); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil && v > n {
				n = v
			}
		}
	}

	l.ID = "LS-" + strconv.Itoa(n+1)
	l.State = New
	l.At, l.Updated = now, now

	return l, s.write(l, true)
}

// Put writes a lesson over its file, whole or not at all.
func (s *Store) Put(l Lesson) error { return s.write(l, false) }

func (s *Store) write(l Lesson, fresh bool) error {
	if !idRe.MatchString(l.ID) {
		return fmt.Errorf("lesson: bad id %q", l.ID)
	}

	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("lesson: %w", err)
	}

	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("lesson %s: %w", l.ID, err)
	}

	raw = append(raw, '\n')
	path := filepath.Join(s.Dir, l.ID+".json")

	if fresh {
		// Claim the name first: O_EXCL fails if anything is there.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("lesson: %w", err)
		}

		_ = f.Close()
	}

	if err := replace(path, raw); err != nil {
		if fresh {
			// The claim is still empty; leaving it would be a lesson nobody can read.
			_ = os.Remove(path)
		}

		return fmt.Errorf("lesson %s: %w", l.ID, err)
	}

	return nil
}

// replace puts raw at path through a temporary file, so a reader sees the old content or the new, never half.
func replace(path string, raw []byte) error {
	tmp := path + ".tmp"

	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)

		return err
	}

	return nil
}

// Get reads one lesson; "7", "ls-7" and "LS-7" are LS-7.
func (s *Store) Get(id string) (Lesson, error) {
	id = Normalize(id)
	if !idRe.MatchString(id) {
		return Lesson{}, fmt.Errorf("no lesson %s", id)
	}

	l, err := read(filepath.Join(s.Dir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return l, fmt.Errorf("no lesson %s", id)
	}

	return l, err
}

func read(path string) (Lesson, error) {
	var l Lesson

	raw, err := os.ReadFile(path)
	if err != nil {
		return l, err
	}

	name := strings.TrimSuffix(filepath.Base(path), ".json")

	if err := json.Unmarshal(raw, &l); err != nil {
		return Lesson{}, fmt.Errorf("lesson %s: %w", name, err)
	}

	if l.ID != name {
		return Lesson{}, fmt.Errorf("lesson %s: the file says it is %q", name, l.ID)
	}

	return l, nil
}

// All reads every lesson that can be read, in the order of their numbers. A file that cannot be read is left out,
// so one damaged lesson does not hide the rest; Get names the damage when asked for that one.
func (s *Store) All() ([]Lesson, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("lesson: %w", err)
	}

	var out []Lesson

	for _, e := range entries {
		if !fileRe.MatchString(e.Name()) {
			continue
		}

		l, err := read(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			continue
		}

		out = append(out, l)
	}

	sort.Slice(out, func(i, j int) bool { return num(out[i].ID) < num(out[j].ID) })

	return out, nil
}

// Normalize turns 7, ls-7 and LS-7 into LS-7.
func Normalize(id string) string {
	id = strings.ToUpper(strings.TrimSpace(id))
	if digitsRe.MatchString(id) {
		return "LS-" + id
	}

	return id
}

func num(id string) int {
	v, _ := strconv.Atoi(strings.TrimPrefix(id, "LS-"))

	return v
}
