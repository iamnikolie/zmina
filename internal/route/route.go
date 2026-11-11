// Package route is the dispatcher's routing rules as code: given the crew file's ordered rules, what a piece of work
// is (a repo, a project, a kind, a task it goes on from or is a review of), how the seats stand and how full the
// plans are, it says which seat takes the work, and why.
//
// It is advisory and pure. It reads no crew file, no state, no clock and no file system: the caller hands it all of
// that, and the same inputs always give the same answer (every ordering is stable). Choosing what the work is (its
// kind, its project when no repo says it) stays with the caller.
//
// The rules are the eight the dispatcher has always followed, first match wins. Rule 1, the owner naming the seat, is
// not a row of the table: it is the caller naming the seat itself. The others have a fixed kind each, not an
// expression language:
//
//	0 other_crew     a repo under a directory belongs to another crew: tell its dispatcher
//	2 continuation  the seat that did T-n goes on, a fresh copy when its context is over a share
//	3 project        a project has an owner seat
//	4 kind           a kind of work has a seat
//	5 review         the first review of a task by one seat, a re-review by a free one that is not the author
//	6 pool           the rest: a pool of seats, by how full the plans are
//	7 busy           applied to every seat picked: its copy, or a free seat of the same plan
package route

import (
	"fmt"
	"strconv"
	"strings"
)

// Kind is the kind of a rule.
type Kind string

// The rule kinds, as the crew file names them.
const (
	OtherCrew    Kind = "other_crew"
	Continuation Kind = "continuation"
	Project      Kind = "project"
	WorkKind     Kind = "kind"
	Review       Kind = "review"
	Pool         Kind = "pool"
)

// Kinds lists the rule kinds in the order of the dispatcher's rules.
var Kinds = []Kind{OtherCrew, Continuation, Project, WorkKind, Review, Pool}

// Known reports whether k is a rule kind.
func (k Kind) Known() bool { return k.Number() >= 0 }

// Number is the rule's number among the dispatcher's eight rules (1 is the owner naming the seat, 7 the busy policy:
// neither is a row of the table); -1 for a kind that is none.
func (k Kind) Number() int {
	switch k {
	case OtherCrew:
		return 0
	case Continuation:
		return 2
	case Project:
		return 3
	case WorkKind:
		return 4
	case Review:
		return 5
	case Pool:
		return 6
	}

	return -1
}

// Label is the rule's name in a sentence.
func (k Kind) Label() string {
	switch k {
	case OtherCrew:
		return "other crew"
	case Continuation:
		return "continuation"
	case Project:
		return "project"
	case WorkKind:
		return "kind"
	case Review:
		return "review"
	case Pool:
		return "pool by limits"
	}

	return string(k)
}

// DefaultFreshOver is the share of its context at which the seat that did a task is no longer gone on with: a fresh
// copy takes the continuation.
const DefaultFreshOver = 80

// ReviewKind is the --kind of a review: the review rule answers it, and only it.
const ReviewKind = "review"

// Rule is one row of the table.
type Rule struct {
	Kind Kind

	// Under is the directory (absolute) a repo has to be under for other_crew, UnderText how the crew file wrote it,
	// Tell the <node>/<crew> to tell then.
	Under, UnderText, Tell string
	// FreshOver is continuation's share of context, in percent.
	FreshOver int
	// Owners is project's and kind's name -> seat.
	Owners map[string]string
	// First is the seat of a first review, Again the seats a re-review may use, in order.
	First string
	Again []string
	// Seats is the pool.
	Seats []string
}

// BusyPolicy is what a busy seat is replaced by.
type BusyPolicy string

// The busy policies.
const (
	// CopyOrPool: its copy (an idle one, else a new one), else a free seat of the same plan from the pool.
	CopyOrPool BusyPolicy = "copy-or-pool"
	// PoolOnly: a free seat of the same plan from the pool, never a copy.
	PoolOnly BusyPolicy = "pool"
)

// Table is the crew file's routes.
type Table struct {
	Rules []Rule
	// Busy is the policy for a busy seat; empty is CopyOrPool.
	Busy BusyPolicy
}

// Task is a task the work refers to: the one it goes on from or the one it reviews.
type Task struct {
	ID string
	// Seat is the role that held it, a copy (dev#2) as it was named.
	Seat string
}

// Input is what the work is. Every field comes from a flag, never from parsing words.
type Input struct {
	// Repo is the repository the work is in, an absolute cleaned path; empty when there is none.
	Repo string
	// Project is the project it is for; the repo's directory name when empty.
	Project string
	// Kind is the kind of work: design, film, code, review ...
	Kind string
	// After is the task the work goes on from (--after); Of the task it reviews (--of).
	After, Of *Task
	// Rereview says the review is not the first of its task.
	Rereview bool
}

// State is how a seat stands.
type State string

// The states of a seat.
const (
	// StateDown: no session, holds nothing.
	StateDown State = "down"
	// StateIdle: up, between turns, holds no task.
	StateIdle State = "idle"
	// StateBusy: up and working, or holding a task.
	StateBusy State = "busy"
	// StateUnavailable: it cannot be given work now (asleep, outside zmina, its directory gone); Note says why.
	StateUnavailable State = "unavailable"
)

// Seat is a role of the crew, or a live copy of one, as it stands.
type Seat struct {
	// Name is the role, or <role>#N for a copy.
	Name string
	// Runtime is claude or codex: the plan the seat bills.
	Runtime string
	State   State
	// Note says why a busy seat is busy (busy on T-12) or an unavailable one is (asleep until Mon 09:00).
	Note string
	// Tasks are the active tasks it holds.
	Tasks []string
	// Context is how many tokens its conversation fills, ContextBudget the tokens it may fill (0: not known).
	Context, ContextBudget int
}

// Base is the role a copy is of: dev for dev#2, the name itself for a role.
func (s Seat) Base() string { return Base(s.Name) }

// Base is the role name without its copy number.
func Base(name string) string {
	b, _, _ := strings.Cut(name, "#")

	return b
}

// copyNumber is N of dev#N; 0 for a role.
func copyNumber(name string) int {
	_, n, ok := strings.Cut(name, "#")
	if !ok {
		return 0
	}

	v, _ := strconv.Atoi(n)

	return v
}

// ContextShare is how full the seat's conversation is, in percent of what it may fill; ok is false when that is not
// known.
func (s Seat) ContextShare() (int, bool) {
	if s.Context <= 0 || s.ContextBudget <= 0 {
		return 0, false
	}

	return s.Context * 100 / s.ContextBudget, true
}

// why is the seat's state in a few words: down, idle, or its note.
func (s Seat) why() string {
	switch {
	case s.State == StateBusy && s.Note != "":
		return s.Note
	case s.State == StateUnavailable && s.Note != "":
		return s.Note
	}

	return string(s.State)
}

// Action is what the caller does with the answer.
type Action string

// The actions.
const (
	// ActionTask: record the task for the seat; it is up.
	ActionTask Action = "task"
	// ActionTell: the work is another crew's: zmina tell its dispatcher.
	ActionTell Action = "tell"
	// ActionUpTask: record the task for the seat, which is down, and start it on the task.
	ActionUpTask Action = "up+task"
	// ActionCopy: a copy of the seat (dev#2) takes it: record the task for it and start the copy on the task.
	ActionCopy Action = "copy"
	// ActionNone: no rule matched; pick by hand.
	ActionNone Action = "none"
)

// Alternative is a seat the rule looked at and did not pick.
type Alternative struct {
	To  string `json:"to"`
	Why string `json:"why"`
}

// Step is how one rule of the table answered, in the order they were asked.
type Step struct {
	Rule    int    `json:"rule"`
	Name    string `json:"name"`
	Matched bool   `json:"matched"`
	Why     string `json:"why"`
}

// Decision is the answer.
type Decision struct {
	// To is the seat (dev2, or a copy dev#2), or the <node>/<crew> to tell; empty for ActionNone.
	To     string
	Action Action
	// Rule is the rule's number (0, 2 to 6); -1 for ActionNone. Name is its name.
	Rule int
	Name string
	// Why says why this seat, in a line.
	Why string
	// Alternatives are the other seats the rule looked at, with why each was not picked.
	Alternatives []Alternative
	// Limits are the plans' readings in percent, nil for one that is unknown; Stale is true when either is.
	Limits LimitsView
	Stale  bool
	// Trace is every rule asked, in order, up to the one that answered.
	Trace []Step
}

// LimitsView is the Claude 7-day and the Codex weekly share used, in percent; nil when the reading is unknown or
// stale.
type LimitsView struct {
	Claude *int `json:"claude"`
	Codex  *int `json:"codex"`
}

// Matched reports whether a rule answered.
func (d Decision) Matched() bool { return d.Action != ActionNone }

// Headline is the answer in one line: the seat, the rule and why; or what went wrong.
func (d Decision) Headline() string {
	if !d.Matched() {
		if len(d.Trace) == 0 {
			return d.Why
		}

		return "no rule matched: " + d.Why
	}

	to := d.To
	if d.Action == ActionTell {
		to = "tell " + d.To
	}

	return fmt.Sprintf("%s · rule %d %s: %s", to, d.Rule, d.Name, d.Why)
}

// Agrees reports whether a hand pick of seat is what the route said.
func (d Decision) Agrees(seat string) bool { return d.Matched() && d.To == seat }
