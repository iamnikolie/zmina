package later

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/task"
)

// A sweep (zmina later sweep) hands a role the parked items that went stale and asks each for a verdict: done, dropped,
// taken, or kept with a reason. The task it makes names the items (task.Task.Sweep); what is below is the part that
// needs no crew, no state and no clock: which items qualify, and which of a task's items still lack a verdict.

// Stale is the open items a sweep is about, oldest first: made at least older ago, not reviewed (Keep) within older, not
// waiting on a day to come back (back maps an item to when its alarm rings: one still ahead of now keeps it out) and not
// already back on the owner's todo (Back). The items given are left as they are.
func Stale(items []Item, back map[string]time.Time, now time.Time, older time.Duration) []Item {
	var out []Item

	for _, it := range items {
		if !it.Open() || !it.Back.IsZero() || now.Sub(it.Created) < older {
			continue
		}

		if !it.Reviewed.IsZero() && now.Sub(it.Reviewed) < older {
			continue
		}

		if at, ok := back[it.ID]; ok && at.After(now) {
			continue
		}

		out = append(out, it)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}

		return num(out[i].ID) < num(out[j].ID)
	})

	return out
}

// Unswept is the ids, of the ones a sweep task covers, that have no verdict yet: an item that is still open and was not
// reviewed since the task was made (since). Closed, dropped and taken items have theirs, so has one kept after since. An id
// no item answers to (a file that does not read) is returned too: nothing shows a verdict for it. items is every item
// (Store.Scan), in any order; the answer follows ids.
func Unswept(items []Item, ids []string, since time.Time) []string {
	byID := make(map[string]Item, len(items))
	for _, it := range items {
		byID[it.ID] = it
	}

	var out []string

	for _, id := range ids {
		it, ok := byID[id]

		switch {
		case !ok:
		case !it.Open():
			continue
		case !it.Reviewed.IsZero() && !it.Reviewed.Before(since):
			continue
		}

		out = append(out, id)
	}

	return out
}

// OpenSweep is the oldest sweep task still active (open, waiting, or a state this zmina does not know).
func OpenSweep(tasks []task.Task) (task.Task, bool) {
	for _, t := range sortedByNumber(tasks) {
		if len(t.Sweep) > 0 && t.State.Active() {
			return t, true
		}
	}

	return task.Task{}, false
}

// InSweep is the open sweep task that lists the item, for the closing of an item that asks for its evidence.
func InSweep(tasks []task.Task, id string) (task.Task, bool) {
	for _, t := range sortedByNumber(tasks) {
		if !t.State.Active() {
			continue
		}

		for _, covered := range t.Sweep {
			if covered == id {
				return t, true
			}
		}
	}

	return task.Task{}, false
}

// LastSweep is when the newest sweep task was made, whatever became of it; zero when there has been none.
func LastSweep(tasks []task.Task) time.Time {
	var last time.Time

	for _, t := range tasks {
		if len(t.Sweep) > 0 && t.Created.After(last) {
			last = t.Created
		}
	}

	return last
}

// sortedByNumber is the tasks by their number, T-2 before T-10, in a slice of its own.
func sortedByNumber(tasks []task.Task) []task.Task {
	out := append([]task.Task(nil), tasks...)
	n := func(t task.Task) int { v, _ := strconv.Atoi(strings.TrimPrefix(t.ID, "T-")); return v }

	sort.SliceStable(out, func(i, j int) bool { return n(out[i]) < n(out[j]) })

	return out
}
