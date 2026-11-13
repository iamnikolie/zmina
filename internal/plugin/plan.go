package plugin

import (
	"sort"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
)

// MaxPerBeat is the most events one plugin gets in one beat; the rest wait for the next.
const MaxPerBeat = 10

// ThingClass is what a thing is: an item on the owner's todo, or a question on a role's screen.
type ThingClass int

// The classes of a thing.
const (
	ItemThing ThingClass = iota
	DialogThing
)

// Thing is something open now that a plugin may be told of. Key is its name in the ledger (ItemKey, DialogKey).
type Thing struct {
	Key, ID string
	Class   ThingClass
	Opened  time.Time
	Role    string
	// Blocking: it holds a role up. Reminder: it is a reminder ringing or a parked item back.
	Blocking, Reminder bool
}

// Due is one event to send. A new one has no Line (the caller builds it from the thing); a retry or the answered of an
// entry that closed carries what it needs. Entry is what the ledger keeps of the thing.
type Due struct {
	Key   string
	Kind  string
	Entry Entry
	// Retry is the event as it was built and failed; nil for a new one.
	Retry *Queued
}

// QueueKey is the key the event waits under when its run fails: the thing's, or the closing's for an answered.
func (d Due) QueueKey() string {
	if d.Kind == Answered {
		return ClosingKey(d.Key)
	}

	return d.Key
}

// Inputs is what a plan is made of.
type Inputs struct {
	Plugin crew.Plugin
	State  *State
	// Things are what is open now. Whole says it is all of it: a thing told of that is not among them has ended; with Whole
	// false (a file did not read) nothing is judged ended.
	Things []Thing
	Whole  bool
	Now    time.Time
	// OwnerActive is when the owner was last at the machine (zero: unknown), asked for only when it can matter.
	OwnerActive func() time.Time
}

// Plan is what a beat does for one plugin: events to send in order, queued events no longer wanted, and entries to close
// without an event (the plugin takes no answered).
type Plan struct {
	Send   []Due
	Cancel []string
	Settle []string
}

// Make plans a beat for one plugin: the events waiting for a retry that are due, then the answered of what ended (before the
// new ones, so that a task that waits on the owner a second time, which has the same id, is closed before it is opened
// again), then the new ones (a blocking item first, then oldest first) — at most MaxPerBeat in all. A new event is held while the owner
// was at the machine within the plugin's quiet_when_active, and one of a question on a role's screen until it has stood
// dialog_after; it goes when the span has passed if it is still open, so what the owner answered at the keyboard never
// pages. An answered is never held.
func Make(in Inputs) Plan {
	var p Plan

	st, pl := in.State, in.Plugin
	open := map[string]Thing{}

	for _, t := range in.Things {
		open[t.Key] = t
	}

	// Events that failed and wait: due ones go again as they were built, unless what they are about has ended meanwhile
	// (then the plugin was never told, and is not to be).
	for _, q := range st.Queue {
		if q.Next.After(in.Now) {
			continue
		}

		if q.Event != Answered && in.Whole {
			if _, ok := open[q.Key]; !ok {
				p.Cancel = append(p.Cancel, q.Key)

				continue
			}
		}

		q := q
		p.Send = append(p.Send, Due{Key: keyOf(q), Kind: q.Event, Entry: q.Entry, Retry: &q})
	}

	// What ended: the plugin is told, once, if it takes answered and was told of it.
	if in.Whole {
		var ended []string

		for key, e := range st.Told {
			if e.Dropped || !e.Closed.IsZero() {
				continue
			}

			if _, still := open[key]; still {
				continue
			}

			if _, waiting := st.queued(ClosingKey(key)); waiting {
				continue
			}

			ended = append(ended, key)
		}

		sort.Strings(ended)

		for _, key := range ended {
			if !pl.Takes(Answered) {
				p.Settle = append(p.Settle, key)

				continue
			}

			p.Send = append(p.Send, Due{Key: key, Kind: Answered, Entry: *st.Told[key]})
		}
	}

	var fresh []Due

	var active time.Time

	asked := false
	quiet := pl.QuietD()

	held := func() bool {
		if quiet <= 0 {
			return false
		}

		if !asked {
			asked = true
			if in.OwnerActive != nil {
				active = in.OwnerActive()
			}
		}

		return !active.IsZero() && in.Now.Sub(active) < quiet
	}

	for _, t := range in.Things {
		// Told, and open to the plugin still: nothing more. A question on a screen that was told and closed, and is on the screen
		// again, is a new question: the same words, another time.
		if e, told := st.Told[t.Key]; told && e.Closed.IsZero() {
			continue
		}

		if _, waiting := st.queued(t.Key); waiting {
			continue
		}

		kind := KindFor(pl, t)
		if kind == "" {
			continue
		}

		switch {
		case t.Class == DialogThing:
			if in.Now.Sub(st.Seen[t.Key]) < pl.DialogAfterD()-Jitter {
				continue
			}
		case kind != Blocking && t.Opened.Before(st.Since):
			// Open before the plugin was first looked at: not new to it, unless it holds a role up.
			continue
		}

		if held() {
			continue
		}

		fresh = append(fresh, Due{Key: t.Key, Kind: kind, Entry: Entry{ID: t.ID, Kind: kind, Role: t.Role, Opened: t.Opened}})
	}

	byKey := map[string]Thing{}
	for _, t := range in.Things {
		byKey[t.Key] = t
	}

	sort.SliceStable(fresh, func(i, j int) bool {
		a, b := fresh[i], fresh[j]
		if (a.Kind == Blocking) != (b.Kind == Blocking) {
			return a.Kind == Blocking
		}

		return byKey[a.Key].Opened.Before(byKey[b.Key].Opened)
	})

	p.Send = append(p.Send, fresh...)

	if len(p.Send) > MaxPerBeat {
		p.Send = p.Send[:MaxPerBeat]
	}

	return p
}

// keyOf is the thing's key of a queued event: the closing key is the queue's name for an answered, the thing's is under it.
func keyOf(q Queued) string {
	if q.Event == Answered {
		return q.Key[len("closing:"):]
	}

	return q.Key
}

// KindFor is the event a thing goes to the plugin as, "" for none: a reminder as reminder when the plugin takes it, an item
// that holds a role up as blocking when it takes that, else as ask; a dialog as dialog.
func KindFor(p crew.Plugin, t Thing) string {
	if t.Class == DialogThing {
		if p.Takes(Dialog) {
			return Dialog
		}

		return ""
	}

	switch {
	case t.Reminder && p.Takes(Reminder):
		return Reminder
	case t.Blocking && p.Takes(Blocking):
		return Blocking
	case p.Takes(Ask):
		return Ask
	}

	return ""
}
