// Package move is what a role's move to another node (zmina move) is made of that is no node's own act: the journal the node the
// role leaves keeps while the move is open, the bundle of work that travels, what each node tells of itself before a move, what a
// node answers, and the checks every field of a bundle passes before the node that takes it acts on it. It has no crew state and
// runs no git: the bundle is data from another node, read as untrusted.
package move

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/mesh"
	"github.com/iamnikolie/zmina/internal/record"
	"github.com/iamnikolie/zmina/internal/worktree"
)

// BundleMax is the most bytes the work of a move may weigh as it travels: the task fields and the records, never a path to a file.
const BundleMax = 1 << 20

// BodyMax is the most bytes of the small body a move's request carries (a check, a begin, a commit).
const BodyMax = 64 << 10

// Version is the bundle's format. A node takes only a bundle of a version it knows.
const Version = 1

// Phases of the journal of a move, as the node the role leaves keeps it, each one after the one before.
const (
	// PhaseBegun: the journal is open; the role has not been asked to park its work yet.
	PhaseBegun = "begun"
	// PhaseAsked: the role was asked to write its checkpoints and park its tasks; the move waits for it, up to a grace.
	PhaseAsked = "asked"
	// PhaseReleased: every task is parked and the role is down.
	PhaseReleased = "released"
	// PhaseReady: each task's branch is on origin and the bundle is built and kept in the journal.
	PhaseReady = "ready"
)

// Answers of a poll (PollAnswer.State).
const (
	// StatePending: the move goes on at the node; Why says what it waits for.
	StatePending = "pending"
	// StateReady: the bundle is built.
	StateReady = "ready"
	// StateTaken: the node holds a receipt of a take of the move: it took the work.
	StateTaken = "taken"
	// StateTaking: the node the role goes to has begun a take of the move and has no receipt yet: it runs, or it was cut short.
	StateTaking = "taking"
	// StateNone: the node knows no open move of that id.
	StateNone = "none"
	// StateCommitted and StateAborted answer the end of a move at the node the role left: closed as taken, or called off.
	StateCommitted = "committed"
	StateAborted   = "aborted"
)

var (
	idRe   = regexp.MustCompile(`^mv-[0-9a-f]{12}$`)
	roleRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	nodeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	taskRe = regexp.MustCompile(`^T-[0-9]{1,9}$`)
	sumRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// NewID is a fresh id of a move: mv- and twelve hex digits.
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)

	return "mv-" + hex.EncodeToString(b)
}

// ValidID reports whether s is an id NewID would make.
func ValidID(s string) bool { return idRe.MatchString(s) }

// ValidRole reports whether s is the name of a role a move may carry (no copy: dev#2 is a copy).
func ValidRole(s string) bool { return roleRe.MatchString(s) }

// ValidNode reports whether s is a node's name.
func ValidNode(s string) bool { return nodeRe.MatchString(s) }

// ValidSum reports whether s is the hash of a crew file.
func ValidSum(s string) bool { return sumRe.MatchString(s) }

// ValidTask reports whether s is a task id (T-<n>).
func ValidTask(s string) bool { return taskRe.MatchString(s) }

// TaskNumber is the n of a task id T-<n>, 0 for anything else: the part of a private ref's name that tells which task it is of.
func TaskNumber(id string) int {
	if !ValidTask(id) {
		return 0
	}

	n := 0
	for _, c := range id[2:] {
		n = n*10 + int(c-'0')
	}

	return n
}

// Task is what the journal keeps of one task the role held when the move began.
type Task struct {
	ID string `json:"id"`
	// Item is the later item the task was parked as (L-<n>), closed as taken when the move commits.
	Item string `json:"item,omitempty"`
	// Repo is the task's repository; Branch its branch, Tip the commit of it that travels (the branch's tip, never a commit made of
	// uncommitted work), WIP the commit the move made of the worktree's uncommitted work ("" when there was none; it is kept under a
	// private ref, never on the branch), Pushed that Tip is on origin and WIPPushed that WIP is on origin under its private ref.
	Repo      string `json:"repo,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Tip       string `json:"tip,omitempty"`
	WIP       string `json:"wip,omitempty"`
	Pushed    bool   `json:"pushed,omitempty"`
	WIPPushed bool   `json:"wip_pushed,omitempty"`
}

// Journal is the record of an open move of a role off this node, kept at moves/<role>.json for as long as it is open. While it is
// there the ticker and every start refuse the role.
type Journal struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	// From is this node, To the node the role goes to.
	From string `json:"from"`
	To   string `json:"to"`
	By   string `json:"by,omitempty"`
	// Phase is how far the move has come (Phase...).
	Phase string `json:"phase"`
	// Force is that the owner chose to drop the role's queue and the uncommitted work of its own directory. Up is that the role was up
	// when the move began.
	Force   bool      `json:"force,omitempty"`
	Up      bool      `json:"up,omitempty"`
	Started time.Time `json:"started"`
	// Asked is when the role was asked to park; the leftover is parked a grace later.
	Asked time.Time `json:"asked,omitzero"`
	// Base is the sum of the crew file when the move began, After the sum it will have once the move's edit is made (place:): both
	// ends hold After when the move is done.
	Base  string `json:"base"`
	After string `json:"after"`
	// Role's tasks, as the move found them.
	Tasks []Task `json:"tasks,omitempty"`
	// Dropped are the queued lines a forced move dropped, told to the dispatcher.
	Dropped []string `json:"dropped,omitempty"`
	// Bundle is the work that travels, built at PhaseReady.
	Bundle json.RawMessage `json:"bundle,omitempty"`
	// LastErr is what the last try of the beat said, so that it is told when it changes and not every beat.
	LastErr string `json:"last_err,omitempty"`
	// Cuts is how often the beat stopped the step the move is in for want of time (CutLimit of them in a row are told).
	Cuts
}

// Open reads the journal of the open move of the role, false when there is none. A journal that does not read is an error: the role
// stays refused, which is the safe side.
func Open(l crew.Layout, role string) (Journal, bool, error) {
	var j Journal

	raw, err := os.ReadFile(l.Move(role))
	if errors.Is(err, os.ErrNotExist) {
		return j, false, nil
	}

	if err != nil {
		return j, false, fmt.Errorf("move journal of %s: %w", role, err)
	}

	if err := json.Unmarshal(raw, &j); err != nil {
		return j, false, fmt.Errorf("move journal of %s does not read: %w", role, err)
	}

	return j, true, nil
}

// OpenID is the id of the open move of the role, "" when there is none. A journal that does not read has no id the role could be
// told by, so it is the word "unreadable": the role is refused all the same.
func OpenID(l crew.Layout, role string) string {
	j, ok, err := Open(l, role)

	switch {
	case err != nil:
		return "unreadable"
	case !ok:
		return ""
	}

	return j.ID
}

// Save writes the journal.
func Save(l crew.Layout, j Journal) error {
	if err := fsx.MkdirAll(l.Moves(), 0o700); err != nil {
		return err
	}

	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}

	return fsx.WriteFile(l.Move(j.Role), append(raw, '\n'), 0o600)
}

// Close takes the journal away: the move is over.
func Close(l crew.Layout, role string) error {
	if err := os.Remove(l.Move(role)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

// Drive is what the owner's node keeps of a move it drives, apart from the journal on the node the role leaves (which may be
// another node): enough for its ticker to take the move on again.
type Drive struct {
	ID      string    `json:"id"`
	Role    string    `json:"role"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	Base    string    `json:"base"`
	Started time.Time `json:"started"`
	// Holds, Repos, Dir and Namespace are what the source said of itself before the move began (its check, its begin): the tasks it
	// holds, the repositories compared on both nodes, the crew's directory and namespace. The bundle the source hands over is
	// checked against them, and refused when it names anything else. Empty on a record that was made again after it was lost.
	Holds     []string `json:"holds,omitempty"`
	Repos     []string `json:"repos,omitempty"`
	Dir       string   `json:"dir,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	// Recorded says Holds, Repos, Dir and Namespace are what the preflight found (an empty Holds is then a role that holds nothing);
	// a record made again after it was lost says nothing of the sort, and the bundle is checked only for its move, role and nodes.
	Recorded bool `json:"recorded,omitempty"`
	// LastErr is what the last try of the ticker said, so that it is told when it changes and not every beat.
	LastErr string `json:"last_err,omitempty"`
	// Cuts is how often the beat stopped the step the drive is in (the poll of the node the role leaves, or the take) for want of time.
	Cuts
}

// OpenDrive reads what the owner's node keeps of the move of a role it drives, false when there is none.
func OpenDrive(l crew.Layout, role string) (Drive, bool, error) {
	var d Drive

	raw, err := os.ReadFile(l.MoveDrive(role))
	if errors.Is(err, os.ErrNotExist) {
		return d, false, nil
	}

	if err != nil {
		return d, false, err
	}

	if err := json.Unmarshal(raw, &d); err != nil {
		return d, false, fmt.Errorf("move record of %s does not read: %w", role, err)
	}

	return d, true, nil
}

// SaveDrive writes the owner's record of a move.
func SaveDrive(l crew.Layout, d Drive) error {
	if err := fsx.MkdirAll(l.Moves(), 0o700); err != nil {
		return err
	}

	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}

	return fsx.WriteFile(l.MoveDrive(d.Role), append(raw, '\n'), 0o600)
}

// CloseDrive takes the owner's record away.
func CloseDrive(l crew.Layout, role string) error {
	if err := os.Remove(l.MoveDrive(role)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

// Drives lists the moves the owner's node drives for this crew: the roles whose record is there.
func Drives(l crew.Layout) []Drive {
	matches, _ := filepath.Glob(filepath.Join(l.Moves(), "*.drive.json"))

	var out []Drive

	for _, m := range matches {
		role := strings.TrimSuffix(filepath.Base(m), ".drive.json")
		if d, ok, err := OpenDrive(l, role); err == nil && ok {
			out = append(out, d)
		}
	}

	return out
}

// Facts is what a node tells of itself for a move of a role, as its answer to a check: the owner compares the two ends'.
type Facts struct {
	Node string `json:"node"`
	// Sum is the hash of the crew file there; "" when there is no crew of that name.
	Sum string `json:"sum"`
	// Dir and Namespace are the crew's directory and its Claude namespace there, home-relative (Portable).
	Dir       string `json:"dir"`
	Namespace string `json:"namespace"`
	// Listed is that the crew file lists this node in nodes:; Spread that it declares nodes: at all.
	Listed bool `json:"listed"`
	Spread bool `json:"spread"`
	// Known is that the crew file has the role; Placed where it places it.
	Known  bool   `json:"known"`
	Placed string `json:"placed,omitempty"`
	// Moving is the id of an open move of the role off this node, "" when none.
	Moving string `json:"moving,omitempty"`
	// Holds are the ids of the tasks the role holds here; Queued the lines waiting for it; Up that it is up.
	Holds  []string `json:"holds,omitempty"`
	Queued int      `json:"queued,omitempty"`
	Up     bool     `json:"up,omitempty"`
	// Dirty names the checkouts of the role that hold uncommitted work a move does not carry: its own directory, and the main
	// checkout of a task that has no worktree. Unfit names the worktrees whose work cannot travel at all (a merge, rebase or
	// cherry-pick in progress, conflicts, a nested repository): no --force carries them.
	Dirty []string `json:"dirty,omitempty"`
	Unfit []string `json:"unfit,omitempty"`
	// Taking is the id of a take of a move of the role that is running on this node or was cut short, "" when none.
	Taking string `json:"taking,omitempty"`
	// Repos are the repositories the move needs, each with its origin there (a check of the node the role goes to answers for the
	// ones the other end named).
	Repos []RepoFact `json:"repos,omitempty"`
}

// RepoFact is one repository: its top level, home-relative, and its origin URL as git resolves it (a url.<base>.insteadOf rewrite
// applied); Found is false for a top level that is no repository there (or is none at all), Origin "" for a repository with no origin.
// Push is the URL pushes to origin use, as git resolves it (pushurl and pushInsteadOf applied): a repository whose pushes go elsewhere
// than the two nodes fetch from is not the same repository on both.
type RepoFact struct {
	Top    string `json:"top"`
	Found  bool   `json:"found"`
	Origin string `json:"origin,omitempty"`
	Push   string `json:"push,omitempty"`
}

// CheckBody is what a check of the node the role goes to carries: the repositories to look up there.
type CheckBody struct {
	Repos []string `json:"repos,omitempty"`
}

// BeginBody is what the beginning of a move carries: the node the role goes to.
type BeginBody struct {
	To string `json:"to"`
}

// CommitBody is what the end of a move carries: the node the role went to, for each task of the role the task it became there
// ("T-14": "beta/T-9"), and the sum of the crew file there afterwards.
type CommitBody struct {
	To    string            `json:"to"`
	Taken map[string]string `json:"taken"`
	Sum   string            `json:"sum,omitempty"`
}

// PollAnswer is what a node says of a move.
type PollAnswer struct {
	State string `json:"state"`
	Phase string `json:"phase,omitempty"`
	Why   string `json:"why,omitempty"`
	// Bundle is the work, once it is ready; Taken what the node the role goes to holds as the receipt of its take. Holds are the
	// tasks the node the role leaves holds for the move, in its answer to the beginning.
	Bundle json.RawMessage `json:"bundle,omitempty"`
	Taken  *TakeAnswer     `json:"taken,omitempty"`
	Holds  []string        `json:"holds,omitempty"`
}

// Taken is a task the move carried and the task it became.
type Taken struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// TakeAnswer is what the node the role goes to says of the work it took; it is kept as the receipt, so that a take made again
// is the same answer.
type TakeAnswer struct {
	Move string `json:"move"`
	Node string `json:"node"`
	Role string `json:"role"`
	// Tasks are the tasks made here, with the ones they came from.
	Tasks []Taken `json:"tasks"`
	// Sum is the hash of the crew file here once the role is placed here.
	Sum string `json:"sum"`
	// Started says the role was started on its work here; Why what stood in the way when it was not.
	Started bool      `json:"started"`
	Why     string    `json:"why,omitempty"`
	At      time.Time `json:"at"`
}

// Receipt reads the receipt of a take of the move, false when none was made.
func Receipt(l crew.Layout, id string) (TakeAnswer, bool, error) {
	var a TakeAnswer

	if !ValidID(id) {
		return a, false, fmt.Errorf("move id %q", id)
	}

	raw, err := os.ReadFile(l.MoveTaken(id))
	if errors.Is(err, os.ErrNotExist) {
		return a, false, nil
	}

	if err != nil {
		return a, false, err
	}

	if err := json.Unmarshal(raw, &a); err != nil {
		return a, false, fmt.Errorf("the receipt of %s does not read: %w", id, err)
	}

	return a, true, nil
}

// SaveReceipt keeps the answer of a take.
func SaveReceipt(l crew.Layout, a TakeAnswer) error {
	if !ValidID(a.Move) {
		return fmt.Errorf("move id %q", a.Move)
	}

	if err := fsx.MkdirAll(filepath.Dir(l.MoveTaken(a.Move)), 0o700); err != nil {
		return err
	}

	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}

	return fsx.WriteFile(l.MoveTaken(a.Move), append(raw, '\n'), 0o600)
}

// BTask is one task of the bundle: the fields that make it, never a path of this node's.
type BTask struct {
	// ID is the task there (T-14): its Origin here is <node>/T-14.
	ID       string   `json:"id"`
	Asked    string   `json:"asked"`
	Title    string   `json:"title,omitempty"`
	Criteria []string `json:"criteria,omitempty"`
	Project  string   `json:"project,omitempty"`
	URL      string   `json:"url,omitempty"`
	Budget   float64  `json:"budget,omitempty"`
	Note     string   `json:"note,omitempty"`
	// Repo is the task's repository, home-relative (Portable); Origin its origin URL there. The node that takes the task looks
	// the repository up by the same top level and checks the origin; it takes no path.
	Repo   string `json:"repo,omitempty"`
	Origin string `json:"origin,omitempty"`
	// Branch, Base and Fork are the task's own; Tip is the commit of the branch that was pushed; WIP is the commit a move made of the
	// worktree's uncommitted work, one commit over Tip, which is on origin under the move's private ref and never on the branch: the
	// node that takes the task restores it onto the branch's tip, as uncommitted files.
	Branch string `json:"branch,omitempty"`
	Base   string `json:"base,omitempty"`
	Fork   string `json:"fork,omitempty"`
	Tip    string `json:"tip,omitempty"`
	WIP    string `json:"wip,omitempty"`
	// Incomplete is that the checkpoint was not current when the task was parked.
	Incomplete bool `json:"incomplete,omitempty"`
	// Checkpoint and Result are the task's latest ones, as documents; the node that takes the task saves them again under the new
	// task's id.
	Checkpoint *record.Document `json:"checkpoint,omitempty"`
	Result     *record.Document `json:"result,omitempty"`
}

// Bundle is the work of a move as it travels: what the other node needs to go on with the role's tasks.
type Bundle struct {
	Version int    `json:"version"`
	Move    string `json:"move"`
	Role    string `json:"role"`
	From    string `json:"from"`
	To      string `json:"to"`
	// Base and After are the sums of the crew file before and after the move's edit.
	Base  string `json:"base"`
	After string `json:"after"`
	// Dir and Namespace are the crew's, home-relative: the node that takes the work holds the same.
	Dir       string  `json:"dir"`
	Namespace string  `json:"namespace"`
	Tasks     []BTask `json:"tasks"`
}

// Marshal is the bundle as it travels. A bundle over BundleMax is an error: it is never sent.
func (b Bundle) Marshal() ([]byte, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}

	if len(raw) > BundleMax {
		return nil, fmt.Errorf("the work to carry is %d bytes (task texts and records): a move carries at most %d; shorten the checkpoint "+
			"or the task's note", len(raw), BundleMax)
	}

	return raw, nil
}

// ParseBundle reads a bundle that came from another node and checks every field of it; it is data, not instructions. The size is
// checked before anything is parsed. Each field must be what it claims to be: ids, node names, the branch (a plain, valid branch
// name), commit ids, repository and origin as plain text; a path is never taken from it.
func ParseBundle(raw []byte) (Bundle, error) {
	var b Bundle

	if len(raw) == 0 || len(raw) > BundleMax {
		return b, fmt.Errorf("the bundle is %d bytes: 1 to %d", len(raw), BundleMax)
	}

	if err := json.Unmarshal(raw, &b); err != nil {
		return Bundle{}, fmt.Errorf("the bundle does not read: %w", err)
	}

	b.Sanitize()

	return b, b.Validate()
}

// Sanitize strips the control characters from every text field of the bundle, before it is stored or printed: what another node says
// is data, never a sequence the owner's terminal, or a role's, should act on. The task's asked text, its note and the values of its
// records are text of several lines: a line break and a tab stay, every other control character, and every format character that hides
// or reorders text, goes. The other fields are one line each: a line break is a space there. The ids, names, paths, branches and URLs
// are not touched, only checked (Validate): they are never text to be shown as it came.
func (b *Bundle) Sanitize() {
	for i := range b.Tasks {
		t := &b.Tasks[i]

		t.Asked, t.Note = mesh.PlainText(t.Asked), mesh.PlainText(t.Note)
		t.Title, t.Project, t.URL = mesh.Plain(t.Title), mesh.Plain(t.Project), mesh.Plain(t.URL)

		for j, c := range t.Criteria {
			t.Criteria[j] = mesh.Plain(c)
		}

		for _, d := range []*record.Document{t.Checkpoint, t.Result} {
			sanitizeDocument(d)
		}
	}
}

func sanitizeDocument(d *record.Document) {
	if d == nil {
		return
	}

	d.By = mesh.Plain(d.By)
	mesh.PlainAll(&d.Shape)

	values := make(map[string]string, len(d.Values))
	for k, v := range d.Values {
		values[mesh.Plain(k)] = mesh.PlainText(v)
	}

	d.Values = values
}

const (
	taskMax     = 20
	criteriaMax = 100
	textMax     = 256 << 10
	lineMax     = 2000
)

// Validate checks a bundle's fields.
func (b Bundle) Validate() error {
	switch {
	case b.Version != Version:
		return fmt.Errorf("bundle version %d: this zmina takes version %d (update the node that is older)", b.Version, Version)
	case !ValidID(b.Move):
		return fmt.Errorf("bundle move id %q", b.Move)
	case !ValidRole(b.Role):
		return fmt.Errorf("bundle role %q", b.Role)
	case !ValidNode(b.From) || !ValidNode(b.To) || b.From == b.To:
		return fmt.Errorf("bundle nodes %q to %q", b.From, b.To)
	case !ValidSum(b.Base) || !ValidSum(b.After):
		return errors.New("bundle crew file sums are not hashes")
	case len(b.Tasks) > taskMax:
		return fmt.Errorf("a bundle carries at most %d tasks, not %d", taskMax, len(b.Tasks))
	}

	if err := plainPath("dir", b.Dir); err != nil {
		return err
	}

	if err := plainPath("namespace", b.Namespace); err != nil {
		return err
	}

	seen := map[string]bool{}

	for _, t := range b.Tasks {
		if seen[t.ID] {
			return fmt.Errorf("bundle task %s twice", t.ID)
		}

		seen[t.ID] = true

		if err := t.validate(); err != nil {
			return fmt.Errorf("bundle task %s: %w", t.ID, err)
		}
	}

	return nil
}

func (t BTask) validate() error {
	if !ValidTask(t.ID) {
		return fmt.Errorf("id %q", t.ID)
	}

	for name, v := range map[string]string{"asked": t.Asked, "note": t.Note} {
		if len(v) > textMax {
			return fmt.Errorf("%s is %d bytes", name, len(v))
		}
	}

	for name, v := range map[string]string{"title": t.Title, "project": t.Project, "url": t.URL} {
		if len(v) > lineMax {
			return fmt.Errorf("%s is %d bytes", name, len(v))
		}
	}

	if len(t.Criteria) > criteriaMax {
		return fmt.Errorf("%d criteria", len(t.Criteria))
	}

	for _, c := range t.Criteria {
		if len(c) > lineMax || strings.ContainsAny(c, "\r\n") {
			return errors.New("a criterion is one line")
		}
	}

	if t.Budget < 0 || math.IsNaN(t.Budget) || t.Budget > 1e9 {
		return fmt.Errorf("budget %v", t.Budget)
	}

	if t.Repo == "" {
		if t.Branch != "" || t.Origin != "" || t.Tip != "" || t.Base != "" || t.Fork != "" || t.WIP != "" {
			return errors.New("a branch without a repository")
		}
	} else {
		if err := plainPath("repo", t.Repo); err != nil {
			return err
		}

		if err := plainText("origin", t.Origin); err != nil || t.Origin == "" {
			return fmt.Errorf("origin %q: a repository is carried with its origin", t.Origin)
		}
	}

	if t.Branch != "" {
		if err := worktree.CheckBranch(t.Branch); err != nil {
			return err
		}

		if !worktree.IsSHA(t.Tip) {
			return fmt.Errorf("tip %q is no commit id", t.Tip)
		}

		if t.Base != "" && !worktree.IsSHA(t.Base) {
			if err := worktree.CheckBranch(t.Base); err != nil {
				return fmt.Errorf("base: %w", err)
			}
		}

		if t.Fork != "" && !worktree.IsSHA(t.Fork) {
			return fmt.Errorf("fork %q is no commit id", t.Fork)
		}

		if t.WIP != "" && !worktree.IsSHA(t.WIP) {
			return fmt.Errorf("wip %q is no commit id", t.WIP)
		}
	} else if t.Tip != "" || t.WIP != "" {
		return errors.New("a tip without a branch")
	}

	for kind, d := range map[string]*record.Document{"checkpoint": t.Checkpoint, "result": t.Result} {
		if d == nil {
			continue
		}

		if d.Kind != kind {
			return fmt.Errorf("the %s is a record of kind %q", kind, d.Kind)
		}

		if err := record.Validate(d.Shape, d.Values); err != nil {
			return fmt.Errorf("the %s: %w", kind, err)
		}
	}

	return nil
}

// plainPath checks a home-relative or absolute path told by another node: text with no control character, clean, and never a path that
// climbs out of where it starts. It is compared and looked up, never written to.
func plainPath(what, p string) error {
	if err := plainText(what, p); err != nil {
		return err
	}

	if p == "" {
		return fmt.Errorf("%s is empty", what)
	}

	if !strings.HasPrefix(p, "/") && p != "~" && !strings.HasPrefix(p, "~/") {
		return fmt.Errorf("%s %q is not an absolute or home-relative path", what, p)
	}

	if filepath.Clean(strings.TrimPrefix(p, "~")) != strings.TrimPrefix(p, "~") && p != "~" {
		return fmt.Errorf("%s %q is not a clean path", what, p)
	}

	return nil
}

func plainText(what, s string) error {
	if len(s) > lineMax {
		return fmt.Errorf("%s is %d bytes", what, len(s))
	}

	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s has a control character", what)
		}
	}

	return nil
}

// Portable is a path as the nodes compare it: under the home directory it is ~/<the rest>, so that two nodes whose users have
// different home directories (/Users/alex and /home/alex) hold the same place; a path outside it stays as it is.
func Portable(p string) string {
	if p == "" {
		return ""
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}

	homes := []string{filepath.Clean(home)}
	if real, err := filepath.EvalSymlinks(home); err == nil && filepath.Clean(real) != homes[0] {
		homes = append(homes, filepath.Clean(real))
	}

	realP := p
	if r, err := filepath.EvalSymlinks(p); err == nil {
		realP = r
	}

	for _, h := range homes {
		for _, candidate := range []string{p, realP} {
			switch {
			case candidate == h:
				return "~"
			case strings.HasPrefix(candidate, h+string(filepath.Separator)):
				return "~/" + filepath.ToSlash(strings.TrimPrefix(candidate, h+string(filepath.Separator)))
			}
		}
	}

	return p
}

// Expand is a path of Portable on this node: ~ is this node's home.
func Expand(p string) string { return crew.ExpandHome(p) }
