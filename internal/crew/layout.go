package crew

import "path/filepath"

// Layout is where a crew keeps its files: all of them next to the crew file, so a crew is one directory.
type Layout struct {
	Dir string
}

// Layout of this crew.
func (c *Crew) Layout() Layout { return Layout{Dir: filepath.Dir(c.Path)} }

// State is zmina's state file.
func (l Layout) State() string { return filepath.Join(l.Dir, "state.json") }

// Lock is the file every writing command and the ticker lock.
func (l Layout) Lock() string { return filepath.Join(l.Dir, "lock") }

// Tasks is the task ledger's directory (T-<n>.json).
func (l Layout) Tasks() string { return filepath.Join(l.Dir, "tasks") }

// Later is the parked list's directory (L-<n>.json).
func (l Layout) Later() string { return filepath.Join(l.Dir, "later") }

// Events is the event log.
func (l Layout) Events() string { return filepath.Join(l.Dir, "events.jsonl") }

// Screens holds the last screens of ended roles.
func (l Layout) Screens() string { return filepath.Join(l.Dir, "screens") }

// Worktrees holds the tasks' git worktrees.
func (l Layout) Worktrees() string { return filepath.Join(l.Dir, "worktrees") }

// Watch is one watch's own directory (its cache); its seen file sits beside it.
func (l Layout) Watch(name string) string { return filepath.Join(l.Dir, "watch", name) }

// TickLog is where the ticker's output goes.
func (l Layout) TickLog() string { return filepath.Join(l.Dir, "tick.log") }
