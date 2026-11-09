package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/events"
	"github.com/iamnikolie/zmina/internal/shift"
)

// Pushes to the owner. Each beat the ticker reads the owner's todo and sends what is due through the crew's notify
// command, as one message: an item that holds a role up (notify_on: blocking) the first time it does and again every
// notify_again while it still does, a new ask (notify_on: ask) once — not one a reminder made, whose text went with
// the reminder. Nothing goes in quiet_hours; what is due then goes on the first beat after them, if it is still
// there. The state keeps when each item went (pushed).

// pushTick sends this beat's push, outside the beat's lock.
func (a *app) pushTick(now time.Time) {
	var (
		c   *crew.Crew
		due []todoItem
	)

	// A crew that pushes nothing, and has no record of pushing, takes no lock for it. A crew file that does not load
	// sends nothing: the beat goes on.
	idle := true
	if a.readState(func(s *shift.Shift) error {
		idle = !s.Crew.NotifiesOn(crew.NotifyBlocking) && !s.Crew.NotifiesOn(crew.NotifyAsk) &&
			s.State.Pushed == nil && s.State.PushFrom.IsZero()

		return nil
	}) != nil || idle {
		return
	}

	// Claimed under the lock, so a beat that overlaps this one sends nothing twice.
	err := a.withState(func(s *shift.Shift) error {
		c = s.Crew
		if !c.NotifiesOn(crew.NotifyBlocking) && !c.NotifiesOn(crew.NotifyAsk) {
			s.State.Pushed, s.State.PushFrom = nil, time.Time{}

			return nil
		}

		items, _ := todo(c, now, s.Views())
		due = pushDue(c, s.State, items, now)

		return nil
	})
	if err != nil || len(due) == 0 {
		return
	}

	ids := make([]string, len(due))
	event := crew.NotifyAsk

	for i, it := range due {
		ids[i] = it.ID
		if it.Blocks != "" {
			event = crew.NotifyBlocking
		}
	}

	text := pushText(due)
	sendErr := notify(c, event, strings.Join(ids, " "), text)

	_ = a.withState(func(s *shift.Shift) error {
		if sendErr != nil {
			line := strings.Join(ids, " ") + ": notify: " + sendErr.Error()
			_ = s.Events.Add(events.Event{TS: now, Event: "notify-failed", By: "ticker", Detail: line})
			a.printTick([]string{"push: " + line})

			return nil
		}

		s.Log("", "pushed", "", text)
		a.printTick([]string{"push: " + strings.Join(ids, " ")})

		return nil
	})
}

// pushDue is what goes in this beat's push, marked as pushed now in the state. Items that left the todo leave the
// state's record; asks already open when pushing began are not new.
func pushDue(c *crew.Crew, st *shift.State, items []todoItem, now time.Time) []todoItem {
	if st.PushFrom.IsZero() {
		st.PushFrom = now
	}

	open := map[string]bool{}
	for _, it := range items {
		open[it.ID] = true
	}

	for id := range st.Pushed {
		if !open[id] {
			delete(st.Pushed, id)
		}
	}

	if quiet, _ := c.QuietAt(now); quiet {
		return nil
	}

	blocking, asks, again := c.NotifiesOn(crew.NotifyBlocking), c.NotifiesOn(crew.NotifyAsk), c.NotifyAgainD()

	var due []todoItem

	for _, it := range items {
		last, was := st.Pushed[it.ID]

		switch {
		case blocking && it.Blocks != "" && (!was || again > 0 && now.Sub(last) >= again):
		case asks && !was && (it.Kind == "decision" || it.Kind == "check") && !it.Created.Before(st.PushFrom) &&
			(!it.byAlarm || !c.NotifiesOn(crew.NotifyReminder)):
		default:
			continue
		}

		due = append(due, it)
	}

	if len(due) > 0 && st.Pushed == nil {
		st.Pushed = map[string]time.Time{}
	}

	for _, it := range due {
		st.Pushed[it.ID] = now
	}

	return due
}

// pushText is a push in plain words: each item's id with its title, what it holds up; several in one message.
func pushText(items []todoItem) string {
	one := func(it todoItem) string {
		title := it.Title
		if title == "" {
			title = it.Text
		}

		line := it.ID + " " + clip(oneLine(title), 100)

		switch {
		case it.Blocks != "":
			line += " (blocks " + it.Blocks + ")"
		case it.Kind == "decision" && it.Rec != "":
			line += " (recommended: " + clip(oneLine(it.Rec), 60) + ")"
		}

		return line
	}

	if len(items) == 1 {
		return "waits on you: " + one(items[0])
	}

	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = one(it)
	}

	return fmt.Sprintf("%d wait on you: %s", len(items), strings.Join(parts, "; "))
}

// withPushed sets each todo item's last push from the state.
func withPushed(items []todoItem, st *shift.State) {
	for i := range items {
		items[i].Pushed = st.Pushed[items[i].ID]
	}
}
