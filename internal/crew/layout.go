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

// History is where the crew file's earlier versions are kept (crew.d/history; see History).
func (l Layout) History() string { return filepath.Join(l.Dir, "crew.d", "history") }

// Lock is the file every writing command and the ticker lock.
func (l Layout) Lock() string { return filepath.Join(l.Dir, "lock") }

// CourierLock is the lock file of the courier (zmina _courier): one courier runs per crew. It is its own file, never the
// state lock, so a courier that lives holds nothing the beat or a command waits for.
func (l Layout) CourierLock() string { return filepath.Join(l.Dir, "courier.lock") }

// LockHolder is the advisory record, beside the lock, of who holds it now (pid, command, phase); empty of a record
// when the lock is free.
func (l Layout) LockHolder() string { return filepath.Join(l.Dir, "lock.holder") }

// Tasks is the task ledger's directory (T-<n>.json).
func (l Layout) Tasks() string { return filepath.Join(l.Dir, "tasks") }

// Later is the parked list's directory (L-<n>.json).
func (l Layout) Later() string { return filepath.Join(l.Dir, "later") }

// Alarms is the directory of reminders and wake-ups (R-<n>.json).
func (l Layout) Alarms() string { return filepath.Join(l.Dir, "alarms") }

// Asks is the directory of what the dispatcher asks the owner (A-<n>.json).
func (l Layout) Asks() string { return filepath.Join(l.Dir, "asks") }

// Inbox holds the whole text of lines too long to type into the dispatcher (in-<hex>.json).
func (l Layout) Inbox() string { return filepath.Join(l.Dir, "inbox") }

// Events is the event log.
func (l Layout) Events() string { return filepath.Join(l.Dir, "events.jsonl") }

// Reads is the read meter: one line per zmina read or grep of a role, and per step of its read guard.
func (l Layout) Reads() string { return filepath.Join(l.Dir, "reads.jsonl") }

// ReadsRate holds each session's count of reader calls in its current minute (the meter's bulk mark).
func (l Layout) ReadsRate() string { return filepath.Join(l.Dir, "reads-rate.json") }

// Limits holds what the plans' limits were last seen at: the Claude status-line tap writes claude.json here
// (zmina limits reads it; the Codex figures come from Codex's own rollouts and are not kept).
func (l Layout) Limits() string { return filepath.Join(l.Dir, "limits") }

// Screens holds the last screens of ended roles.
func (l Layout) Screens() string { return filepath.Join(l.Dir, "screens") }

// Hooks is the directory of what a role's hooks recorded for zmina to read: one small file a role and kind, replaced by
// the next record (HookQuestion, HookFailure). A file here is only ever read as a hint, with the session it is about
// and the time it was written; a role started before a hook existed has none.
func (l Layout) Hooks() string { return filepath.Join(l.Dir, "hooks") }

// HookQuestion is the question a role's PermissionRequest hook saw an AskUserQuestion put on its screen.
func (l Layout) HookQuestion(role string) string {
	return filepath.Join(l.Hooks(), role+".question.json")
}

// HookFailure is the API error a role's StopFailure hook saw end its turn.
func (l Layout) HookFailure(role string) string {
	return filepath.Join(l.Hooks(), role+".failure.json")
}

// Delivery is the directory of the delivery cards: one file a role (DeliveryCards), the facts zmina saw of each line it
// queued, typed or posted. Cards are read by looks and never decide a delivery.
func (l Layout) Delivery() string { return filepath.Join(l.Dir, "delivery") }

// DeliveryCards is one role's cards (delivery/<role>.json).
func (l Layout) DeliveryCards(role string) string { return filepath.Join(l.Delivery(), role+".json") }

// DeliveryFetched is one role's fetches (delivery/<role>.fetched.jsonl): one line each time the text of a doorbell it was
// rung was read with zmina inbox show. It is appended to without the state lock, and read by the delivery.
func (l Layout) DeliveryFetched(role string) string {
	return filepath.Join(l.Delivery(), role+".fetched.jsonl")
}

// DeliveryBell is the mark of a role's start that told it what a doorbell is (delivery/<role>.bell): written by the start,
// read before a doorbell is rung. A role with none, or a mark of another start, is sent every line whole.
func (l Layout) DeliveryBell(role string) string { return filepath.Join(l.Delivery(), role+".bell") }

// Worktrees holds the tasks' git worktrees.
func (l Layout) Worktrees() string { return filepath.Join(l.Dir, "worktrees") }

// Watch is one watch's own directory (its cache); its seen file sits beside it.
func (l Layout) Watch(name string) string { return filepath.Join(l.Dir, "watch", name) }

// CourierLog is where the courier's output goes when a service runs it.
func (l Layout) CourierLog() string { return filepath.Join(l.Dir, "courier.log") }

// TickLog is where the ticker's output goes.
func (l Layout) TickLog() string { return filepath.Join(l.Dir, "tick.log") }
