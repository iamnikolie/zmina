package shift

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/asks"
)

// lateAfter is how late an alarm may ring before it says when it was due: the Mac slept, the ticker was down.
const lateAfter = 5 * time.Minute

// AlarmPrefix starts every line an alarm types into a role or tells another crew.
const AlarmPrefix = "[zmina alarm] "

// Rung is an alarm that went off this beat with a part left for the caller, outside the lock: Notify is the line
// for the crew's notify command (a reminder for the owner, a parked item back), Tell the crew (node/crew) whose
// dispatcher is told Line.
type Rung struct {
	ID     string
	Notify string
	Tell   string
	Line   string
}

// Clock is how this crew reads the times of alarms: its timezone and day_start.
func (s *Shift) Clock() alarm.Clock {
	return alarm.Clock{Loc: s.Crew.Location(), DayStart: s.Crew.DayStart}
}

// ring rings every alarm whose time has come — once, however late: an alarm missed while the machine slept or the
// ticker was down rings on the first beat after, and says when it was due. A reminder for the owner becomes a check
// on the todo (and goes through the notify command), a parked item comes back to the todo, a role is started if it
// is down (on its last conversation after a sleep, or on the conversation its parked work came from) and gets the
// alarm's line in its queue, another crew is told. While the shift is stopped a role's alarm waits: nothing starts.
// It returns lines for the log and notes for the dispatcher.
func (s *Shift) ring(now time.Time, stopped bool) (did, notes []string) {
	if s.Alarms == nil {
		return nil, nil
	}

	for _, a := range s.Alarms.Due(now) {
		if a.Role != "" && a.To == "" && stopped {
			continue
		}

		late := ""
		if now.Sub(a.At) > lateAfter {
			late = " (due " + a.At.In(s.Crew.Location()).Format("Mon 2 Jan 15:04") + ")"
		}

		a.State, a.Rang, a.Updated = alarm.Rang, now, now
		line := ""

		switch {
		case a.To != "":
			line = a.ID + ": told " + a.To
			s.Rang = append(s.Rang, Rung{ID: a.ID, Tell: a.To, Line: AlarmPrefix + body(a) + late})
		case a.Kind == alarm.Later:
			line = s.bringBack(&a, now, late)
		case a.Role != "":
			var note string
			if line, note = s.wake(&a, late); note != "" {
				notes = append(notes, note)
			}
		default:
			line = s.remindOwner(&a, now, late)
		}

		if err := s.Alarms.Put(a); err != nil {
			did = append(did, a.ID+": rang, but not saved: "+err.Error())

			continue
		}

		s.log(a.Role, "alarm", a.Later, line)
		did = append(did, line)
	}

	return did, notes
}

// remindOwner puts a reminder for the owner on the todo as a check and hands its line to the notify command.
func (s *Shift) remindOwner(a *alarm.Alarm, now time.Time, late string) string {
	text := a.Text
	if a.Note != "" {
		text += "\n\n" + a.Note
	}

	from := a.By
	if from == "" || from == "hand" || from == "ui" {
		from = "remind"
	}

	line := a.ID + ": reminded you: " + a.Text + late

	if s.Asks != nil {
		ask, err := s.Asks.Add(asks.Ask{Kind: asks.Check, Title: a.Text, Text: text + late, Role: from, By: "alarm"}, now)
		if err != nil {
			a.Err = "todo: " + err.Error()
			line += "; not on the todo: " + err.Error()
		} else {
			a.Ask = ask.ID
			line += " → " + ask.ID
		}
	}

	s.Rang = append(s.Rang, Rung{ID: a.ID, Notify: a.Text + late})

	return line
}

// bringBack puts a parked item back on the owner's todo. An item no longer parked has nothing to come back: the
// alarm goes off quietly.
func (s *Shift) bringBack(a *alarm.Alarm, now time.Time, late string) string {
	it, err := s.Later.Get(a.Later)
	if err != nil || !it.Open() {
		a.State = alarm.Off

		return a.ID + ": " + a.Later + " is no longer parked, nothing to bring back"
	}

	it.Back, it.Updated = now, now
	if err := s.Later.Put(it); err != nil {
		a.Err = err.Error()

		return a.ID + ": " + a.Later + " not back: " + err.Error()
	}

	s.Rang = append(s.Rang, Rung{ID: a.ID, Notify: "back from later: " + it.Text + late})

	return a.ID + ": " + it.ID + " is back on your todo" + late
}

// wake gives a role its alarm: it is started if it is down — a slept role on its last conversation, a role whose
// work was parked on that work's conversation — and its line waits in its queue for its prompt.
func (s *Shift) wake(a *alarm.Alarm, late string) (line, note string) {
	name := a.Role
	if _, err := s.Role(name); err != nil {
		a.Err = err.Error()

		return a.ID + ": " + err.Error(), a.ID + ": alarm for " + name + " not delivered: " + err.Error()
	}

	rs := s.State.Role(name)
	rs.Inbox = append(rs.Inbox, s.trackLine(name, AlarmPrefix+body(*a)+late))
	line = a.ID + ": " + name + " told"

	if s.has(name) {
		return line, ""
	}

	o := UpOptions{}

	switch {
	case a.Kind == alarm.Sleep && rs.LastSession != "":
		o.Resume = rs.LastSession
	case a.Kind == alarm.Remind:
		o.Resume = s.parkedSession(name)
	}

	if err := s.UpWith(name, o); err != nil {
		a.Err = err.Error()

		return line + ", not started: " + err.Error(), a.ID + ": the alarm for " + name + " could not start it: " +
			err.Error() + "; the line waits in its queue"
	}

	if o.Resume != "" {
		return line + ", resumed " + o.Resume, ""
	}

	return line + ", started", ""
}

// parkedSession is the conversation the role's newest parked work came from, empty when none is parked.
func (s *Shift) parkedSession(name string) string {
	if s.Later == nil {
		return ""
	}

	items := s.Later.Parked()
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Role == name && items[i].Session != "" {
			return items[i].Session
		}
	}

	return ""
}

// body is what an alarm says: its text and note in one line, or a sleep's note alone.
func body(a alarm.Alarm) string {
	switch {
	case a.Note == "":
		return oneLine(a.Text)
	case a.Kind == alarm.Sleep:
		return oneLine(a.Note)
	}

	return oneLine(a.Text + " — " + a.Note)
}

// Asleep is when a role put to sleep wakes, zero when it is not asleep.
func (s *Shift) Asleep(name string) time.Time {
	if s.Alarms == nil {
		return time.Time{}
	}

	for _, a := range s.Alarms.Pending() {
		if a.Kind == alarm.Sleep && a.Role == name {
			return a.At
		}
	}

	return time.Time{}
}

// Sleep puts a role down holding what it holds, and sets it to wake at at with note (a line of its own when empty).
// It begins the down (the caller waits outside the lock and finishes it) and returns the alarm. A role already
// asleep has its alarm moved instead. The dispatcher and a copy (dev#2, gone once down) do not sleep.
func (s *Shift) Sleep(name string, at time.Time, when, note string, o DownOptions) (alarm.Alarm, bool, error) {
	if s.Alarms == nil {
		return alarm.Alarm{}, false, errors.New("no alarms here")
	}

	if name == s.Crew.Dispatcher {
		return alarm.Alarm{}, false, fmt.Errorf("%s is the dispatcher and never sleeps", name)
	}

	if isInstance(name) {
		return alarm.Alarm{}, false, fmt.Errorf("%s is a copy, gone once down: it cannot sleep", name)
	}

	if _, err := s.Role(name); err != nil {
		return alarm.Alarm{}, false, err
	}

	wait := false

	if s.has(name) {
		var err error
		if wait, err = s.BeginDown(name, o); err != nil {
			return alarm.Alarm{}, false, err
		}
	}

	// Down already: held all the same, so keep: always does not start it before its time.
	s.State.Role(name).Held = true

	text := "you were put to sleep " + s.Now().In(s.Crew.Location()).Format("Mon 15:04") + "; go on with what you hold"
	if tasks := s.held()[name]; len(tasks) > 0 {
		text = "you were put to sleep " + s.Now().In(s.Crew.Location()).Format("Mon 15:04") + "; go on with " +
			strings.Join(tasks, ", ")
	}

	for _, a := range s.Alarms.Pending() {
		if a.Kind == alarm.Sleep && a.Role == name {
			a.At, a.When, a.Updated = at, when, s.Now()
			if note != "" {
				a.Note = note
			}

			return a, wait, s.Alarms.Put(a)
		}
	}

	a, err := s.Alarms.Add(alarm.Alarm{Kind: alarm.Sleep, Role: name, At: at, When: when, Text: text, Note: note,
		By: s.By}, s.Now())
	if err != nil {
		return a, wait, err
	}

	s.log(name, "sleep", "", a.ID+" until "+at.In(s.Crew.Location()).Format("Mon 2 Jan 15:04"))

	return a, wait, nil
}
