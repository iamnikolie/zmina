package cost

import (
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/task"
)

// How a reply was given to a task.
const (
	// ByCwd: it ran inside the task's worktree.
	ByCwd = "cwd"
	// ByWindow: its role held that one task, and no other, at the time.
	ByWindow = "window"
	// Shared: its role held several tasks at the time; the reply is theirs together, split to none.
	Shared = "shared"
	// Untasked: its role held none.
	Untasked = "untasked"
)

// Hold is a stretch of time a role held a task: handed or opened to done, dropped, parked or handed on. To is zero
// while it still holds it.
type Hold struct {
	Task, Role string
	From, To   time.Time
}

func (h Hold) covers(at time.Time) bool {
	return !at.Before(h.From) && (h.To.IsZero() || at.Before(h.To))
}

// Tree is a task's worktree.
type Tree struct {
	Task, Path string
}

// Ledger is what the crew's task ledger says about who worked on what, when.
type Ledger struct {
	Trees []Tree
	Holds []Hold
}

// NewLedger reads the tasks and their history in the event log.
func NewLedger(tasks []task.Task, evs []events.Event) Ledger {
	var l Ledger

	byTask := map[string][]events.Event{}

	for _, e := range evs {
		if e.Event == "task" && e.Task != "" {
			byTask[e.Task] = append(byTask[e.Task], e)
		}
	}

	for _, t := range tasks {
		if t.Worktree != "" {
			l.Trees = append(l.Trees, Tree{Task: t.ID, Path: filepath.Clean(t.Worktree)})
		}

		l.Holds = append(l.Holds, holds(t, byTask[t.ID])...)
	}

	// The deepest worktree first: one inside another belongs to the inner task.
	sort.SliceStable(l.Trees, func(i, j int) bool { return len(l.Trees[i].Path) > len(l.Trees[j].Path) })

	return l
}

// holds walks one task's history. Each event names the role holding the task after it, and a state change comes
// first in its detail. A task whose history is gone (the log rotated past it) is taken from its file: its role
// from its creation to its last change when closed.
func holds(t task.Task, evs []events.Event) []Hold {
	if len(evs) == 0 {
		h := Hold{Task: t.ID, Role: t.Role, From: t.Created}
		if !t.State.Active() {
			h.To = t.Updated
		}

		return []Hold{h}
	}

	sort.SliceStable(evs, func(i, j int) bool { return evs[i].TS.Before(evs[j].TS) })

	var (
		out    []Hold
		cur    *Hold
		active = true
	)

	// The first event may be later than the task's start when the log rotated past its beginning.
	start := t.Created
	if start.IsZero() || evs[0].TS.Before(start) {
		start = evs[0].TS
	}

	for i, e := range evs {
		if st, ok := stateOf(e.Detail); ok {
			active = st.Active()
		}

		at := e.TS
		if i == 0 {
			at = start
		}

		if cur != nil && (!active || cur.Role != e.Role) {
			cur.To = e.TS
			out = append(out, *cur)
			cur = nil
		}

		if cur == nil && active && e.Role != "" {
			cur = &Hold{Task: t.ID, Role: e.Role, From: at}
		}
	}

	if cur != nil {
		out = append(out, *cur)
	}

	return out
}

// stateOf reads the state a task event's detail starts with: "done", "open, role dev", "parked L-3",
// "new: <title>" (a new task is open).
func stateOf(detail string) (task.State, bool) {
	first, _, _ := strings.Cut(detail, ", ")
	if strings.HasPrefix(first, "new: ") || first == "new" {
		return task.Open, true
	}

	word, _, _ := strings.Cut(first, " ")
	if word == string(task.Parked) {
		return task.Parked, true
	}

	st, err := task.ParseState(word)

	return st, err == nil
}

// Attribution is the task (or tasks) a reply belongs to and how that was decided.
type Attribution struct {
	// Tasks is one task for cwd and window, several for shared, none for untasked.
	Tasks []string
	How   string
}

// Key names the attribution as a row: T-5, shared T-3+T-5, untasked.
func (a Attribution) Key() string {
	switch a.How {
	case Untasked:
		return Untasked
	case Shared:
		return Shared + " " + strings.Join(a.Tasks, "+")
	}

	return a.Tasks[0]
}

// Of gives a reply of a role (or instance, dev#2), run in cwd at a time, to a task.
func (l Ledger) Of(role, cwd string, at time.Time) Attribution {
	if cwd != "" {
		cwd = filepath.Clean(cwd)
		for _, t := range l.Trees {
			if cwd == t.Path || strings.HasPrefix(cwd, t.Path+string(filepath.Separator)) {
				return Attribution{Tasks: []string{t.Task}, How: ByCwd}
			}
		}
	}

	var held []string

	for _, h := range l.Holds {
		if h.Role == role && h.covers(at) && !slices.Contains(held, h.Task) {
			held = append(held, h.Task)
		}
	}

	switch len(held) {
	case 0:
		return Attribution{How: Untasked}
	case 1:
		return Attribution{Tasks: held, How: ByWindow}
	}

	sort.Slice(held, func(i, j int) bool { return taskNum(held[i]) < taskNum(held[j]) })

	return Attribution{Tasks: held, How: Shared}
}

func taskNum(id string) int {
	n := 0
	for _, c := range strings.TrimPrefix(id, "T-") {
		if c < '0' || c > '9' {
			return n
		}

		n = n*10 + int(c-'0')
	}

	return n
}
