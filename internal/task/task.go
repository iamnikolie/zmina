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

	"github.com/iamnikolie/zmina/internal/donecheck"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/record"
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
	// CheckpointTaskUpdated binds explicit checkpoint validation to this task snapshot.
	CheckpointTaskUpdated time.Time      `json:"checkpoint_task_updated,omitempty"`
	HandoffIncomplete     bool           `json:"handoff_incomplete,omitempty"`
	Delivery              *Delivery      `json:"delivery,omitempty"`
	Conversations         []Conversation `json:"conversations,omitempty"`
	Result                *record.Ref    `json:"result,omitempty"`
	Checkpoint            *record.Ref    `json:"checkpoint,omitempty"`
	ID                    string         `json:"id"`
	Role                  string         `json:"role"`
	// Asked is what was asked, in the owner's words where they gave them.
	Asked string `json:"asked"`
	State State  `json:"state"`
	// Repo is the repository the work is in, when there is one.
	Repo string `json:"repo,omitempty"`
	// Worktree, Branch and Base are set when the task works in its own git worktree.
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Base     string `json:"base,omitempty"`
	// Fork is the commit the worktree was made at, kept so that a branch merged into its base can be told from one
	// nothing was committed on; empty for a task made before it was kept.
	Fork string `json:"fork,omitempty"`
	Note string `json:"note,omitempty"`
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
	// AnsweredBy is who made the newer of those two marks: owner, plugin <name>, role <name>; empty on a task marked
	// before this was kept.
	AnsweredBy string `json:"answered_by,omitempty"`
	// Later is the later item a parked task became.
	Later string `json:"later,omitempty"`
	// Sweep is the parked items (L-<n>, oldest first) a sweep of the parked list handed this task for a verdict each
	// (zmina later sweep); a task that has some is a sweep, and closing it as done waits on zmina later swept.
	Sweep []string `json:"sweep,omitempty"`
	// Budget is what the task may cost, in API-equivalent dollars, before the ticker tells the dispatcher; 0: none.
	Budget float64 `json:"budget,omitempty"`
	// Closed is who closed the task, when, and with what note; nil while it is active, or closed by a zmina that
	// did not keep it.
	Closed *Closure `json:"closed,omitempty"`
	// Route is what the crew file's routes said when the task was made, when the crew has routes (zmina route).
	Route *Route `json:"route,omitempty"`
	// Review is set on a task zmina made to review another (the crew file's auto_review): which, which round, and the verdict
	// its reviewer closed it with. A task that has it is closed as done only with a verdict.
	Review *Review `json:"review,omitempty"`
	// Criteria is what must be true for the task to count as finished, one statement each; the handoff packet
	// carries them and the worker's result says for each whether it holds. Empty: the worker decides.
	Criteria []string `json:"criteria,omitempty"`
	// Check is the command closing the task as done runs, set by the owner or the dispatcher; nil: the crew file's
	// done_check, when it has one and the task has a worktree. See donecheck.
	Check *donecheck.Spec `json:"check,omitempty"`
	// CheckEnv is the last run of the check that could not run for the environment it met (exit 126 or 127, a shell that
	// did not start, the check's env_fault_rc): the work was not judged and the task stays open. It is kept so that the
	// dispatcher is told once for each cause; closing the task or changing its check takes it away.
	CheckEnv *donecheck.Result `json:"check_env,omitempty"`
	// CheckEnvTold are the causes (donecheck.Result.Fault) of the faults the dispatcher was told of for this task, the last
	// CheckEnvToldMax of them, newest last: a cause is told once, wherever it comes back, and the task keeps no more than
	// that. It is set only after the note was queued and the state saved.
	CheckEnvTold []string `json:"check_env_told,omitempty"`
	// Late is the reports that sessions made on the task after it was handed to another, newest last: what was kept
	// of each and what was refused. They change nothing else of the task, and its handoff packet does not carry them.
	Late []Late `json:"late,omitempty"`
}

// CheckEnvToldMax is how many causes of an environment fault a task remembers having told.
const CheckEnvToldMax = 8

// LateMax is how many late reports a task keeps; the older ones go, the event log has them all.
const LateMax = 20

// Late is a report on a task from a session that did not hold it any more: a result or a checkpoint kept as a revision
// of the record that is not the task's, or a close or change that was refused (What), by whom, from which conversation,
// and the hand-out that was current then.
type Late struct {
	At       time.Time `json:"at"`
	What     string    `json:"what"`
	Revision string    `json:"revision,omitempty"`
	By       string    `json:"by"`
	Session  string    `json:"session,omitempty"`
	Gen      int       `json:"gen"`
	// Other says the caller's role is not the task's: it never held it. Taken says the revision was saved as the task's own
	// since, by the session that holds it: it is not late any more.
	Other bool `json:"other,omitempty"`
	Taken bool `json:"taken,omitempty"`
}

// Same reports whether l is the report o already says: one caller on one hand-out doing one thing, a revision of a
// record being the same record. A repeat of it is not told again.
func (l Late) Same(o Late) bool {
	return l.What == o.What && l.Revision == o.Revision && l.By == o.By && l.Session == o.Session && l.Gen == o.Gen
}

// LateRevision reports whether the revision of a record of kind (result or checkpoint) was kept as a late report and has not
// become the task's since: it is a revision on disk that is no session's of the hand-out the task is in.
func (t Task) LateRevision(kind, revision string) bool {
	ref := t.Result
	if kind == "checkpoint" {
		ref = t.Checkpoint
	}

	if ref != nil && ref.Revision == revision {
		return false
	}

	return slices.ContainsFunc(t.Late, func(l Late) bool { return l.What == kind && l.Revision == revision && !l.Taken })
}

// TakeLate marks the late reports of the revision of a record of kind as taken: a session that holds the task saved the same
// content, which is the task's now. It reports whether there was one.
func (t *Task) TakeLate(kind, revision string) bool {
	taken := false

	for i := range t.Late {
		if l := &t.Late[i]; l.What == kind && l.Revision == revision && !l.Taken {
			l.Taken, taken = true, true
		}
	}

	return taken
}

// NextGen is the number of the hand-out a new delivery of the task is: one over the last delivery, and over the
// generation of the result, the checkpoint, the closing and the late reports it kept, so that a number a rolled-back
// delivery gave up, or a late report was told for, is not given again to another session's.
func (t Task) NextGen() int {
	n := 0
	if t.Delivery != nil {
		n = t.Delivery.Gen
	}

	for _, l := range t.Late {
		n = max(n, l.Gen)
	}

	for _, r := range []*record.Ref{t.Result, t.Checkpoint} {
		if r != nil {
			n = max(n, r.Gen)
		}
	}

	if t.Closed != nil {
		n = max(n, t.Closed.Gen)
	}

	return n + 1
}

// CleanCriteria checks a list of criteria as given on the command line: each is one non-empty line, its edges
// trimmed. An empty list stays empty.
func CleanCriteria(values []string) ([]string, error) {
	var out []string

	for _, v := range values {
		v = strings.TrimSpace(v)

		switch {
		case v == "":
			return nil, errors.New("a criterion is not empty")
		case strings.ContainsAny(v, "\r\n"):
			return nil, fmt.Errorf("criterion %q: one line each", clipRunes(v, 40))
		}

		out = append(out, v)
	}

	return out, nil
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}

	return s
}

// Route is the crew file's routes' answer for a task: the rule that answered (its number, 0 or 2 to 6, and name) and
// why. When the task went to another seat than the one the routes named, Said is that seat and Override why it was
// picked by hand (task new --because; empty when none was given).
type Route struct {
	Rule     int    `json:"rule"`
	Name     string `json:"name,omitempty"`
	Why      string `json:"why,omitempty"`
	Said     string `json:"said,omitempty"`
	Override string `json:"override,omitempty"`
}

// Closure is how a task was closed: by whom (a role, "hand" for the owner), when, as what, and the note it was
// closed with.
type Closure struct {
	By    string    `json:"by"`
	At    time.Time `json:"at"`
	State State     `json:"state"`
	Note  string    `json:"note,omitempty"`
	// Gen is the hand-out the task was closed under (Delivery.Gen); Unbound says the closing session showed no
	// identity to tie it to that hand-out (ledger.Current): it was taken for the role's on its name alone.
	Gen     int  `json:"gen,omitempty"`
	Unbound bool `json:"unbound,omitempty"`
	// Check is the check that ran, and passed, before the task was closed as done: what it ran, at which commit, and
	// the tail of its output. It says that command exited 0 there, not that the work is right.
	Check *donecheck.Result `json:"check,omitempty"`
	// Unchecked is set when the task had a check and was closed without it, by force.
	Unchecked *Unchecked `json:"unchecked,omitempty"`
	// Review is set when a role closed the task as done under the crew file's auto_review: its review is due, and zmina makes
	// the review task from it. It is written with the closing, so a crash after the close leaves it to be found.
	Review *ReviewDue `json:"review,omitempty"`
}

// The verdicts a reviewer closes a review task with.
const (
	VerdictAccept = "accept"
	VerdictReject = "reject"
)

// Review says a task is a review of task Of, in round Round of that task's reviews, and what its reviewer decided: Verdict
// is accept or reject, empty while the review is open and when it was closed without one (by force).
type Review struct {
	Of      string `json:"of"`
	Round   int    `json:"round"`
	Verdict string `json:"verdict,omitempty"`
}

// ReviewDue says the review of a closed task is due: which round, and the review task once zmina made it.
type ReviewDue struct {
	Round int    `json:"round"`
	Task  string `json:"task,omitempty"`
}

// Unchecked says who closed a task past its check, and which check it was.
type Unchecked struct {
	By  string `json:"by"`
	Run string `json:"run"`
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
func (s *Store) Add(t Task, now time.Time) (Task, error) { return s.AddFunc(t, now, nil) }

// AddFunc is Add with a last word: fill, when given, changes the task once it has its id and times and before it is
// written, for a text that names its own task. The file is written once, whole.
func (s *Store) AddFunc(t Task, now time.Time, fill func(*Task)) (Task, error) {
	n, err := s.highest()
	if err != nil {
		return t, err
	}

	t.ID = "T-" + strconv.Itoa(n+1)
	if t.State == "" {
		t.State = Open
	}

	t.Created, t.Updated = now, now

	if fill != nil {
		fill(&t)
	}

	return t, s.write(t, true)
}

// Highest is the largest task number among the files in the directory, readable or not; 0 for none. A caller that scans
// the tasks takes it first, and later reads only what came after (After).
func (s *Store) Highest() (int, error) { return s.highest() }

// After reads the tasks numbered above n, which a scan that saw Highest n did not: they are how a decision made on that scan
// is checked again without reading every task. It names the files that do not read, as ScanFiles does.
func (s *Store) After(n int) (tasks []Task, unreadable map[string]string, err error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}

	if err != nil {
		return nil, nil, fmt.Errorf("task: %w", err)
	}

	for _, e := range entries {
		m := fileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}

		if v, _ := strconv.Atoi(m[1]); v <= n {
			continue
		}

		t, err := readTask(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			if unreadable == nil {
				unreadable = map[string]string{}
			}

			unreadable[e.Name()] = err.Error()

			continue
		}

		tasks = append(tasks, t)
	}

	sort.Slice(tasks, func(i, j int) bool { return num(tasks[i].ID) < num(tasks[j].ID) })

	return tasks, unreadable, nil
}

// ScanFiles reads every readable task, oldest first, and names (by file name, with why) each file that does not read: one pass
// for a caller that needs both.
func (s *Store) ScanFiles() (tasks []Task, unreadable map[string]string, err error) {
	tasks, _, unreadable, err = s.scan()

	return tasks, unreadable, err
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

	if err := fsx.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("task: %w", err)
	}

	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}

	path := filepath.Join(s.Dir, t.ID+".json")

	if fresh {
		// A new task takes its name with its content, and only when the name is free.
		if err := fsx.CreateFile(path, raw, 0o600); err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("task %s: %w", t.ID, ErrExists)
			}

			return fmt.Errorf("task: %w", err)
		}

		return nil
	}

	if err := fsx.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("task: %w", err)
	}

	return nil
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
	tasks, problems, _, err = s.scan()

	return tasks, problems, err
}

// Unreadable names each task file that does not read (by its file name) with why. Such a task is in no list: not
// active, not holding its role; the ticker tells the dispatcher and doctor names it.
func (s *Store) Unreadable() (map[string]string, error) {
	_, _, bad, err := s.scan()

	return bad, err
}

func (s *Store) scan() (tasks []Task, problems []string, unreadable map[string]string, err error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, nil
	}

	if err != nil {
		return nil, nil, nil, fmt.Errorf("task: %w", err)
	}

	for _, e := range entries {
		if !fileRe.MatchString(e.Name()) {
			continue
		}

		t, err := readTask(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			problems = append(problems, err.Error())

			if unreadable == nil {
				unreadable = map[string]string{}
			}

			unreadable[e.Name()] = err.Error()

			continue
		}

		if !t.State.Known() {
			problems = append(problems, fmt.Sprintf("task %s: unknown state %q, counted as active", t.ID, t.State))
		}

		tasks = append(tasks, t)
	}

	sort.Slice(tasks, func(i, j int) bool { return num(tasks[i].ID) < num(tasks[j].ID) })

	return tasks, problems, unreadable, nil
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

// RecordSources is where t's records of kind (result or checkpoint) are kept: under t itself, and under the task its
// current one was carried from (later take), when there is one.
func (t Task) RecordSources(kind string) []string {
	ref := t.Result
	if kind == "checkpoint" {
		ref = t.Checkpoint
	}
	if ref != nil && ref.Task != "" && ref.Task != t.ID {
		return []string{t.ID, ref.Task}
	}
	return []string{t.ID}
}
