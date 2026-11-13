package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/fsx"
	"github.com/iamnikolie/zmina/internal/later"
	"github.com/iamnikolie/zmina/internal/shift"
	"github.com/iamnikolie/zmina/internal/task"
	"github.com/iamnikolie/zmina/internal/tmux"
)

const (
	// sweepMin is how few stale items the ticker does not make a sweep for.
	sweepMin = 3
	// sweepRecheck is how long the ticker leaves the question alone when it has just found it cannot be answered yes: too few
	// items, a sweep still open, a task file that does not read, an attempt that failed.
	sweepRecheck = time.Hour
	// sweepFailEvent is the event a failed attempt leaves, once a day for each reason (sweepFailEvery).
	sweepFailEvent = "later-sweep-failed"
	sweepFailEvery = 24 * time.Hour
	// sweepMarkerMax bounds what is read of the marker.
	sweepMarkerMax = 1 << 12
)

// sweepMarker is the small file (Layout.LaterSweep) that lets a beat, a new process every minute, know it is too early without
// reading the ledger or the parked list. NotBefore is the time before which no sweep is due, Failed when the last attempt failed
// and Reason (a hash) and Told which failure was last put in the event log and when. A marker that is missing or does not read is
// one that says to look; nothing decides by it but when to look.
type sweepMarker struct {
	NotBefore time.Time `json:"not_before,omitzero"`
	Failed    time.Time `json:"failed,omitzero"`
	Reason    string    `json:"reason,omitempty"`
	Told      time.Time `json:"told,omitzero"`
}

// readSweepMarker reads the marker; the zero one when it is not there or does not read.
func readSweepMarker(path string) sweepMarker {
	var m sweepMarker

	f, err := os.Open(path)
	if err != nil {
		return m
	}

	raw, _ := io.ReadAll(io.LimitReader(f, sweepMarkerMax))
	_ = f.Close()

	if json.Unmarshal(raw, &m) != nil {
		return sweepMarker{}
	}

	return m
}

// write keeps the marker. A directory that cannot keep it makes every beat look, which is no worse than having none.
func (m sweepMarker) write(path string) {
	if raw, err := json.Marshal(m); err == nil {
		_ = fsx.WriteFile(path, append(raw, '\n'), 0o600)
	}
}

// failureKey names a failure's reason in the marker: a hash, so that the marker keeps none of its words.
func failureKey(reason string) string {
	sum := sha256.Sum256([]byte(reason))

	return hex.EncodeToString(sum[:8])
}

// sweepCandidate is what a beat found when it looked: the items to sweep and the ledger as it read it, or none, and until when
// not to look again (the zero time: next beat).
type sweepCandidate struct {
	cfg   crew.LaterSweep
	crew  *crew.Crew
	view  ledgerView
	items []later.Item
	stale int
	hold  time.Time
}

// laterSweepBeat is the ticker's automatic sweep (later_sweep in the crew file): a sweep task for the crew's role when no
// sweep is open, the last was made more than every ago, at least sweepMin items are stale, the shift is not stopped, no task
// file is unreadable and the role holds no active task (a scheduled run is skipped on the same terms). Like the daily worktree
// sweep it runs after the beat's locked phases. The marker is read first and says when it is too early; then the ledger is read
// once for every question and the parked list only when the ledger says it may be time, both with no lock held; the lock is
// taken once, for the task, after asking again what came to the ledger meanwhile. The last sweep is the newest sweep task on
// the ledger, so nothing is kept in state.json. It fails nothing: what goes wrong is a line of the ticker's output, an event
// once a day for each reason, and an hour before the next attempt.
func (a *app) laterSweepBeat(now time.Time) {
	var c sweepCandidate

	if a.readState(func(s *shift.Shift) error {
		c = a.lookForSweep(s, now)

		return nil
	}) != nil {
		return
	}

	path := ""
	if c.crew != nil {
		path = c.crew.Layout().LaterSweep()
	}

	if !c.hold.IsZero() {
		m := readSweepMarker(path)
		m.NotBefore = c.hold
		m.write(path)
	}

	if len(c.items) == 0 {
		return
	}

	var (
		made    task.Task
		down    bool
		failure error
	)

	err := a.withState(func(s *shift.Shift) error {
		s.By = "ticker"

		// Nobody else may have made a sweep, stopped the shift or handed the role work since it was read.
		if !s.Crew.LaterSweepOn() || !s.State.Stopped.IsZero() || s.Crew.IsRetired() ||
			sweepRaced(s.Crew, c.view, c.cfg.Role) != "" {
			return nil
		}

		t, pane, err := sweepStarter(a, s, sweepSpec{Role: c.cfg.Role, Items: c.items, Stale: c.stale, Older: c.cfg.OlderSpan()})
		made, down, failure = t, pane.State != tmux.Up, err

		// The task is on the ledger when its file reads back: what failed after it is said, and the open sweep stops the next
		// attempt. A task that was never written is no task.
		if t.ID != "" {
			if _, gerr := s.Tasks.Get(t.ID); gerr != nil {
				made = task.Task{}
			}
		}

		return nil
	})
	if err != nil {
		failure = err
	}

	switch {
	case made.ID != "":
		line := fmt.Sprintf("later sweep: %s for %s, %d of %d stale parked items", made.ID, made.Role, len(made.Sweep), c.stale)

		switch {
		case failure != nil:
			line += ": " + refusalReason(failure)
		case down:
			line += " (" + made.Role + " is down: it is told to the dispatcher if the task stays unaccepted)"
		}

		a.printTick([]string{line})

		m := readSweepMarker(path)
		m.NotBefore = made.Created.Add(c.cfg.EverySpan())
		m.write(path)
	case failure != nil:
		a.sweepFailed(c, path, now, failure)
	}
}

// lookForSweep is the beat's reading, with no lock: whether a sweep may be made now and of what. It says in hold until when to
// look no more, when it learned something that will not change in a beat.
func (a *app) lookForSweep(s *shift.Shift, now time.Time) sweepCandidate {
	if !s.Crew.LaterSweepOn() || !s.State.Stopped.IsZero() || s.Crew.IsRetired() {
		return sweepCandidate{}
	}

	c := sweepCandidate{cfg: *s.Crew.LaterSweep, crew: s.Crew}

	if readSweepMarker(c.crew.Layout().LaterSweep()).NotBefore.After(now) {
		return c
	}

	c.view = readLedger(c.crew)

	// A sweep may be in a file that does not read, or is open: an hour on is soon enough to look again.
	if _, open := later.OpenSweep(c.view.tasks); c.view.err != nil || len(c.view.unreadable) > 0 || open {
		c.hold = now.Add(sweepRecheck)

		return c
	}

	if last := later.LastSweep(c.view.tasks); !last.IsZero() && now.Sub(last) < c.cfg.EverySpan() {
		c.hold = last.Add(c.cfg.EverySpan())

		return c
	}

	for _, t := range c.view.tasks {
		if t.Role == c.cfg.Role && t.State.Active() {
			// Free again any minute: no marker, the next beat reads the ledger again.
			return c
		}
	}

	items := laterStale(c.crew, c.view.tasks, now, c.cfg.OlderSpan())
	if len(items) < sweepMin {
		c.hold = now.Add(sweepRecheck)

		return c
	}

	c.stale = len(items)
	c.items = items[:min(len(items), c.cfg.MaxItems())]

	return c
}

// sweepFailed is what a failed attempt leaves: the line in the tick output, the marker that keeps the next attempt an hour
// away, and one event for each reason a day.
func (a *app) sweepFailed(c sweepCandidate, path string, now time.Time, err error) {
	reason := refusalReason(err)
	m := readSweepMarker(path)
	key := failureKey(reason)

	m.NotBefore, m.Failed = now.Add(sweepRecheck), now

	if m.Reason != key || m.Told.After(now) || now.Sub(m.Told) >= sweepFailEvery {
		m.Reason, m.Told = key, now

		_ = eventLog(c.crew).Add(events.Event{TS: now, Event: sweepFailEvent, Role: c.cfg.Role, By: "ticker", Detail: reason})
	}

	m.write(path)

	a.printTick([]string{fmt.Sprintf("later sweep: %s (tried again in %s)", reason, spanText(sweepRecheck))})
}
