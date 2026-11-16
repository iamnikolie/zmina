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

// Moves is where a role's move to another node is kept (zmina move): the journal of an open one on the node the role leaves
// (moves/<role>.json), the receipts of the takes the node made (moves/taken/<id>.json) and, on the owner's node, the record
// of a move it drives (moves/<role>.drive.json).
func (l Layout) Moves() string { return filepath.Join(l.Dir, "moves") }

// Move is the journal of the open move of a role off this node. While it is there the ticker and every start refuse the role.
func (l Layout) Move(role string) string { return filepath.Join(l.Moves(), role+".json") }

// MoveDrive is what the owner's node keeps of a move it drives, so that its ticker takes the move on from where it stopped.
func (l Layout) MoveDrive(role string) string { return filepath.Join(l.Moves(), role+".drive.json") }

// MoveTaken is the receipt of a take of the move with this id, which makes a repeated take the same answer.
func (l Layout) MoveTaken(id string) string { return filepath.Join(l.Moves(), "taken", id+".json") }

// MoveTaking is the marker a node keeps of a take of a role's move from before the take's first side effect until it has the receipt:
// while it is there the take is running or was cut short, and the move is not called off.
func (l Layout) MoveTaking(role string) string { return filepath.Join(l.Moves(), role+".taking.json") }

// MoveAborted is the tombstone of the move with this id that was called off: a take of it is refused from then on.
func (l Layout) MoveAborted(id string) string { return filepath.Join(l.Moves(), "aborted", id+".json") }

// MoveLock is the lock file of a role's move on this node: one step of it runs at a time (a poll, a commit, an abort, a take).
func (l Layout) MoveLock(role string) string { return filepath.Join(l.Moves(), role+".lock") }

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

// ScreenRules is the crew's own screen rules (screen-rules.yaml), optional: rows added to, replaced in or disabled in
// zmina's embedded tables of what it reads on a role's screen (internal/screenrules).
func (l Layout) ScreenRules() string { return filepath.Join(l.Dir, "screen-rules.yaml") }

// ScreenRulesTold is where the crew's commands keep when each reason a screen-rules.yaml was refused was last put in the event
// log (a map of a hash of the reason to the time), so that a ticker that is a new process every beat tells a reason once a day
// and does not read the log to find out. Nothing decides by it but that.
func (l Layout) ScreenRulesTold() string { return filepath.Join(l.Dir, "screen-rules.told") }

// LaterSweep is where the ticker keeps when its next sweep of the parked list may be due and how its last failed attempt
// went (see cmd's laterSweepBeat): one small file, so that a beat that is a new process reads nothing else to know it is too
// early. Nothing decides by it but that; a file that is missing or does not read says to look.
func (l Layout) LaterSweep() string { return filepath.Join(l.Dir, "later-sweep.json") }

// Hooks is the directory of what a role's hooks recorded for zmina to read: one small file a role and kind, replaced by
// the next record (HookQuestion, HookFailure, HookTurn). A file here is only ever read as a hint, with the session it is about
// and the time it was written; a role started before a hook existed has none.
func (l Layout) Hooks() string { return filepath.Join(l.Dir, "hooks") }

// HookQuestion is the question a role's PermissionRequest hook saw an AskUserQuestion put on its screen.
func (l Layout) HookQuestion(role string) string {
	return filepath.Join(l.Hooks(), role+".question.json")
}

// HookPermission is the tool call a role's PermissionRequest hook saw a permission prompt put on its screen: which tool, and what
// it was asked to do.
func (l Layout) HookPermission(role string) string {
	return filepath.Join(l.Hooks(), role+".permission.json")
}

// HookFailure is the API error a role's StopFailure hook saw end its turn.
func (l Layout) HookFailure(role string) string {
	return filepath.Join(l.Hooks(), role+".failure.json")
}

// HookTurn is the question a role's Stop hook found in its last reply: prose to the owner that nothing on his todo carries
// (hooks/<role>.turn.json). It is removed when the next reply holds none.
func (l Layout) HookTurn(role string) string {
	return filepath.Join(l.Hooks(), role+".turn.json")
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

// DeliveryOwnerTries is the failed tries of the one line of a held role's queue that is not its head, the owner's own line taken
// through a hold (delivery/<role>.owner-tries): the tries counted in the state are the head's. It is written and read by the
// delivery under the state lock, apart from state.json, and never decides anything but when that line is tried again.
func (l Layout) DeliveryOwnerTries(role string) string {
	return filepath.Join(l.Delivery(), role+".owner-tries")
}

// RotationsDir is the directory of the rotation records: one append-only file a role (Rotations), the report of each
// restart of the role into a fresh conversation. A record is read by looks and never decides a restart.
func (l Layout) RotationsDir() string { return filepath.Join(l.Dir, "rotations") }

// Rotations is one role's rotation records (rotations/<role>.jsonl), one line a step of a rotation. It is appended to by
// the shift under the state lock and, for what the role itself did, by the role's own commands without it. The role's
// name is the file's, as the delivery cards' are (a copy is dev#2): two roles never share one.
func (l Layout) Rotations(role string) string { return filepath.Join(l.RotationsDir(), role+".jsonl") }

// RotationsFailed is where the shift keeps, for each role, when it last told that its rotation record could not be written
// (a map of role to time), so that a ticker that is a new process every beat tells it once a day and not every beat.
func (l Layout) RotationsFailed() string { return filepath.Join(l.Dir, "rotations.failed") }

// Worktrees holds the tasks' git worktrees.
func (l Layout) Worktrees() string { return filepath.Join(l.Dir, "worktrees") }

// Watch is one watch's own directory (its cache); its seen file sits beside it.
func (l Layout) Watch(name string) string { return filepath.Join(l.Dir, "watch", name) }

// CourierLog is where the courier's output goes when a service runs it.
func (l Layout) CourierLog() string { return filepath.Join(l.Dir, "courier.log") }

// TickLog is where the ticker's output goes.
func (l Layout) TickLog() string { return filepath.Join(l.Dir, "tick.log") }

// Plugins is the plugins' directory: the programs a crew keeps of its own (plugins/<name>, an executable), and what zmina
// keeps of them — each plugin's lock and its told file, and the owner's mark (PluginOwner).
func (l Layout) Plugins() string { return filepath.Join(l.Dir, "plugins") }

// PluginTold is what one plugin was told of and what is owed it (plugins/<name>.told.json): so that each event goes once and
// an `answered` goes only to a plugin that was told. A plugin's file is written only by the run that holds its lock
// (PluginLock), apart from state.json; nothing decides by it but what to send next.
func (l Layout) PluginTold(name string) string { return filepath.Join(l.Plugins(), name+".told.json") }

// PluginLock is held by the run that delivers to one plugin, so that one event is on its way at a time and a run that
// outlasts a beat is not met by the next.
func (l Layout) PluginLock(name string) string { return filepath.Join(l.Plugins(), name+".lock") }

// PluginArmed is the one-shot marks of a plugin's dialogs (plugins/<name>.armed.json): each dialog event it was told carries one,
// and the first press made with it disarms it. It is written by the delivery that told the event and by the choose that pressed,
// apart from the told file, under a lock of its own (PluginArmedLock).
func (l Layout) PluginArmed(name string) string {
	return filepath.Join(l.Plugins(), name+".armed.json")
}

// PluginArmedLock is the short lock the two writers of PluginArmed take.
func (l Layout) PluginArmedLock(name string) string {
	return filepath.Join(l.Plugins(), name+".armed.lock")
}

// PluginOwner is the owner's mark (plugins/owner.json): when he last answered in a terminal, and what he chose of a dialog,
// written by the answer commands under the state lock and read by the plugins' run for quiet_when_active and for how a
// dialog was closed.
func (l Layout) PluginOwner() string { return filepath.Join(l.Plugins(), "owner.json") }
