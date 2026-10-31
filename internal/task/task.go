// Package task is the shift's work ledger: one JSON file per task under tasks/ next to the crew file, so who holds
// what survives a compaction or a restart of the dispatcher. The files are plain on purpose — other tools read them.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// State is where a task is.
type State string

const (
	// Open tasks are handed to their role and in work.
	Open State = "open"
	// Waiting tasks wait for the owner's answer; the role stays up.
	Waiting State = "waiting"
	// Done tasks are finished.
	Done State = "done"
	// Dropped tasks were given up.
	Dropped State = "dropped"
	// Parked tasks were moved to the later list when their role was released; Task.Later names the item.
	Parked State = "parked"
)

// States lists every state, in order.
var States = []State{Open, Waiting, Done, Dropped, Parked}

// Known reports whether the state is one zmina knows.
func (s State) Known() bool { return slices.Contains(States, s) || s.legacyWaiting() }

// legacyWaiting reports the first name Waiting had, waiting-<owner>: it is still read and accepted as Waiting.
func (s State) legacyWaiting() bool {
	return strings.HasPrefix(string(s), string(Waiting)+"-") && len(s) > 8
}

// Active reports whether the task still holds its role: open, waiting for the owner, or a state this zmina does not
// know — an unknown state is taken as active, so a scheduled run never starts over work it cannot read.
func (s State) Active() bool { return s != Done && s != Dropped && s != Parked }

func (s State) normalize() State {
	if s.legacyWaiting() {
		return Waiting
	}

	return s
}

// ParseState checks a state name.
func ParseState(s string) (State, error) {
	if State(s).legacyWaiting() {
		return Waiting, nil
	}

	if State(s) == Parked {
		return "", errors.New("a task is parked with zmina task park, which also puts it on the later list")
	}

	for _, st := range States {
		if string(st) == s {
			return st, nil
		}
	}

	return "", fmt.Errorf("state %q: one of open, waiting, done, dropped", s)
}

// Task is one piece of work handed to a role.
type Task struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	// Asked is what was asked, in the owner's words where they gave them.
	Asked string `json:"asked"`
	State State  `json:"state"`
	// Repo is the repository the work is in, when there is one.
	Repo string `json:"repo,omitempty"`
	// Worktree, Branch and Base are set when the task works in its own git worktree.
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Base     string `json:"base,omitempty"`
	Note     string `json:"note,omitempty"`
	// On is who a waiting task waits on when it is not the owner: a role of the crew or a person; it is off the
	// owner's todo then.
	On string `json:"on,omitempty"`
	// Title is the work in one short plain sentence, in the owner's language; Project the project or product it is
	// for (default: the repo's directory name); URL the live thing to look at.
	Title   string    `json:"title,omitempty"`
	Project string    `json:"project,omitempty"`
	URL     string    `json:"url,omitempty"`
	By      string    `json:"by,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	// Answered is when the owner last answered the task from the todo (zmina todo, the ui page); a waiting task
	// answered since its last change waits on the dispatcher, not on the owner.
	Answered time.Time `json:"answered,omitzero"`
	// Ignored is when the owner set the waiting task aside on the todo; it stays off the todo until the task
	// changes, and the task itself is left as it is.
	Ignored time.Time `json:"ignored,omitzero"`
	// Later is the later item a parked task became.
	Later string `json:"later,omitempty"`
}

// titleMax is how much of the asked text stands in for a missing title.
const titleMax = 80

// Headline is the task's title, else the start of what was asked.
func (t Task) Headline() string {
	if t.Title != "" {
		return t.Title
	}

	r := []rune(strings.Join(strings.Fields(t.Asked), " "))
	if len(r) <= titleMax {
		return string(r)
	}

	return string(r[:titleMax-1]) + "…"
}

// Status is the task's state, with whom a waiting task waits on when not the owner ("waiting on dev2") and the
// item a parked one became ("parked L-15").
func (t Task) Status() string {
	if t.State == Waiting && t.On != "" {
		return string(t.State) + " on " + t.On
	}

	if t.State == Parked && t.Later != "" {
		return string(t.State) + " " + t.Later
	}

	return string(t.State)
}

// Where is the task's project, else its repository's directory name.
func (t Task) Where() string {
	if t.Project != "" || t.Repo == "" {
		return t.Project
	}

	return filepath.Base(t.Repo)
}

var (
	idRe   = regexp.MustCompile(`^T-([0-9]+)$`)
	fileRe = regexp.MustCompile(`^T-([0-9]+)\.json$`)
)

// ErrExists is returned when a new task's file is already there.
var ErrExists = errors.New("task file already exists")

// Store keeps the tasks of one crew. Callers hold zmina's state lock while they write.
type Store struct {
	Dir string
}

// Add gives t the next id — above every T-<n>.json in the directory, readable or not, so a damaged file is never
// written over — fills its times and state, and writes it. It never replaces an existing file.
func (s *Store) Add(t Task, now time.Time) (Task, error) {
	n, err := s.highest()
	if err != nil {
		return t, err
	}

	t.ID = "T-" + strconv.Itoa(n+1)
	if t.State == "" {
		t.State = Open
	}

	t.Created, t.Updated = now, now

	return t, s.write(t, true)
}

// highest is the largest task number among the files in the directory.
func (s *Store) highest() (int, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("task: %w", err)
	}

	n := 0

	for _, e := range entries {
		if m := fileRe.FindStringSubmatch(e.Name()); m != nil {
			if v, _ := strconv.Atoi(m[1]); v > n {
				n = v
			}
		}
	}

	return n, nil
}

// Put writes a task over its file.
func (s *Store) Put(t Task) error { return s.write(t, false) }

func (s *Store) write(t Task, fresh bool) error {
	if !idRe.MatchString(t.ID) {
		return fmt.Errorf("task: bad id %q", t.ID)
	}

	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("task: %w", err)
	}

	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}

	path := filepath.Join(s.Dir, t.ID+".json")

	if fresh {
		// Claim the name first: O_EXCL fails if anything is there.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("task %s: %w", t.ID, ErrExists)
			}

			return fmt.Errorf("task: %w", err)
		}

		_ = f.Close()
	}

	if err := os.WriteFile(path+".tmp", raw, 0o600); err != nil {
		return fmt.Errorf("task: %w", err)
	}

	return os.Rename(path+".tmp", path)
}

// Remove deletes a task's file.
func (s *Store) Remove(id string) error {
	return os.Remove(filepath.Join(s.Dir, Normalize(id)+".json"))
}

// Get reads one task; the id is matched case-insensitively and "14" means T-14.
func (s *Store) Get(id string) (Task, error) {
	id = Normalize(id)
	if !idRe.MatchString(id) {
		// Only T-<n>: an id never names a path outside the ledger.
		return Task{}, fmt.Errorf("no task %s", id)
	}

	t, err := readTask(filepath.Join(s.Dir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return t, fmt.Errorf("no task %s", id)
	}

	return t, err
}

func readTask(path string) (Task, error) {
	var t Task

	raw, err := os.ReadFile(path)
	if err != nil {
		return t, err
	}

	name := strings.TrimSuffix(filepath.Base(path), ".json")

	if err := json.Unmarshal(raw, &t); err != nil {
		return t, fmt.Errorf("task %s: %w", name, err)
	}

	if t.ID != name {
		return t, fmt.Errorf("task %s: the file says it is %q", name, t.ID)
	}

	t.State = t.State.normalize()

	return t, nil
}

// Scan reads every task, oldest first, and says what it could not read: damaged files, a file whose id does not
// match its name, a state this zmina does not know.
func (s *Store) Scan() (tasks []Task, problems []string, err error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}

	if err != nil {
		return nil, nil, fmt.Errorf("task: %w", err)
	}

	for _, e := range entries {
		if !fileRe.MatchString(e.Name()) {
			continue
		}

		t, err := readTask(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			problems = append(problems, err.Error())

			continue
		}

		if !t.State.Known() {
			problems = append(problems, fmt.Sprintf("task %s: unknown state %q, counted as active", t.ID, t.State))
		}

		tasks = append(tasks, t)
	}

	sort.Slice(tasks, func(i, j int) bool { return num(tasks[i].ID) < num(tasks[j].ID) })

	return tasks, problems, nil
}

// All reads every readable task, oldest first.
func (s *Store) All() ([]Task, error) {
	tasks, _, err := s.Scan()

	return tasks, err
}

// Active is every task still holding its role.
func (s *Store) Active() []Task {
	all, _ := s.All()

	var out []Task

	for _, t := range all {
		if t.State.Active() {
			out = append(out, t)
		}
	}

	return out
}

// Normalize turns 14, t-14 and T-14 into T-14.
func Normalize(id string) string {
	id = strings.ToUpper(strings.TrimSpace(id))
	if _, err := strconv.Atoi(id); err == nil {
		return "T-" + id
	}

	return id
}

func num(id string) int {
	v, _ := strconv.Atoi(strings.TrimPrefix(id, "T-"))

	return v
}
