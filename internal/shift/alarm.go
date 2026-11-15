package shift

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/alarm"
	"github.com/iamnikolie/zmina/internal/asks"
	"github.com/iamnikolie/zmina/internal/crew"
)

// lateAfter is how late an alarm may ring before it says when it was due: the Mac slept, the ticker was down.
const lateAfter = 5 * time.Minute

// effectGap is how long the part of an alarm outside the lock (the notify command, the tell) waits after a try
// before the next one: a beat is a minute, and a push service that is down for a moment is not asked every beat.
const effectGap = 5 * time.Minute

// effectTries is how many times that part is tried, the first included, before the alarm is given up as failed and
// the dispatcher is told: with effectGap, some 25 minutes.
const effectTries = 6

// AlarmPrefix starts every line an alarm types into a role or tells another crew.
const AlarmPrefix = "[zmina alarm] "

// Rung is the part of an alarm left for the caller, outside the lock: Notify is the line for the crew's notify
// command (a reminder for the owner, a parked item back), Tell the crew (node/crew) whose dispatcher is told Line.
// The alarm is not rung until the caller has run it and said how it went (RungDone).
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
// An alarm with a part outside the lock (Rung) is rung only once that part went out (RungDone): until then it stays
// set, owing the line, and a later beat hands the part over again, effectGap after the last try, up to effectTries
// times. It returns lines for the log and notes for the dispatcher.
func (s *Shift) ring(now time.Time, stopped bool) (did, notes []string) {
	if s.Alarms == nil {
		return nil, nil
	}

	for _, a := range s.Alarms.Due(now) {
		if a.Owed != "" {
			// What is left is the part outside the lock; what the alarm did inside it is done.
			line, note := s.retry(a, now)
			if line != "" {
				did = append(did, line)
			}

			if note != "" {
				notes = append(notes, note)
			}

			continue
		}

		if a.Role != "" && a.To == "" && stopped {
			continue
		}

		// A wake of a role whose plan is held waits as it waits for a stopped shift: it rings once the hold is over.
		if a.Role != "" && a.To == "" && a.Kind != alarm.Later && s.planHeld(a.Role, now) {
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
			s.owe(&a, now, AlarmPrefix+body(a)+late)
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

		// Only once the alarm is saved as owing its part: a part run for an alarm not saved would run again.
		if a.Owed != "" {
			s.Rang = append(s.Rang, rungOf(a))
		}

		s.log(a.Role, "alarm", a.Later, line)
		did = append(did, line)
	}

	return did, notes
}

// owe makes the alarm one that is not rung until its part outside the lock went out: it keeps the line for that
// part and counts the first try, which the beat is about to make. An alarm with nothing to run outside — no notify
// command for a reminder for the owner — is rung at once, as before.
func (s *Shift) owe(a *alarm.Alarm, now time.Time, line string) {
	if a.To == "" && !s.Crew.NotifiesOn(crew.NotifyReminder) {
		return
	}

	a.State, a.Rang = alarm.Set, time.Time{}
	a.Owed, a.Tries, a.Tried = line, 1, now
}

// rungOf is the part outside the lock that an alarm owes.
func rungOf(a alarm.Alarm) Rung {
	if a.To != "" {
		return Rung{ID: a.ID, Tell: a.To, Line: a.Owed}
	}

	return Rung{ID: a.ID, Notify: a.Owed}
}

// retry hands the part outside the lock of an alarm that owes it over again, when the last try is effectGap behind.
// Past effectTries it gives the alarm up: the try that was the last left no result, the beat that made it ended
// during it. It takes the lock's time for files only; the part itself runs in the caller, outside.
func (s *Shift) retry(a alarm.Alarm, now time.Time) (line, note string) {
	if now.Sub(a.Tried) < effectGap {
		return "", ""
	}

	if a.Tries >= effectTries {
		return s.giveUp(a, now)
	}

	// Counted before it starts: a beat that dies during the try leaves the count, and the gap, behind.
	a.Tries, a.Tried, a.Updated = a.Tries+1, now, now
	if err := s.Alarms.Put(a); err != nil {
		return a.ID + ": not tried again, not saved: " + err.Error(), ""
	}

	s.Rang = append(s.Rang, rungOf(a))

	return fmt.Sprintf("%s: trying again (%d of %d)", a.ID, a.Tries, effectTries), ""
}

// giveUp records an alarm whose part outside the lock did not go out in effectTries tries as failed, and returns
// the note for the dispatcher, once: a failed alarm is not rung again.
func (s *Shift) giveUp(a alarm.Alarm, now time.Time) (line, note string) {
	if a.TryErr == "" {
		a.TryErr = "how the last try ended is not known: the beat that made it ended during it, or could not record it"
	}

	a.State, a.Updated = alarm.Failed, now
	if err := s.Alarms.Put(a); err != nil {
		return a.ID + ": given up, but not saved: " + err.Error(), ""
	}

	line = fmt.Sprintf("%s: not delivered after %d tries: %s", a.ID, a.Tries, a.TryErr)
	note = fmt.Sprintf("%s: alarm for %s (%s) not delivered after %d tries: %s; zmina remind ls shows it, "+
		"zmina remind off %s clears it", a.ID, a.For(), clipRunes(oneLine(a.Text), 60), a.Tries, a.TryErr, a.ID)

	s.log(a.Role, "alarm-failed", a.Later, line)

	return line, note
}

// RungDone records how the part of an alarm outside the lock went: the caller ran it (a Rung of this shift's tick)
// and err is how it ended. Done, the alarm is rung. Failed, it stays set with the error, to be tried again, and past
// effectTries it is failed and the dispatcher gets a note. It returns lines for the log.
//
// The part runs first and is recorded after, in a lock of its own for each: a crash between a part that went out
// and this record runs it again on a later beat, the one duplicate this allows. The other order — rung first, as it
// was — loses an alarm whose part failed or never ran, and the owner never hears it; a reminder twice is the lesser
// harm. Taking the lock again after each part, not once after all, keeps that window to one part.
func (s *Shift) RungDone(id string, err error) []string {
	if s.Alarms == nil {
		return nil
	}

	a, gerr := s.Alarms.Get(id)
	// Taken off while the part ran, or recorded already: nothing here is this call's to write.
	if gerr != nil || !a.Pending() || a.Owed == "" {
		return nil
	}

	now := s.Now()
	a.Updated = now

	if err == nil {
		a.State, a.Rang, a.Owed, a.TryErr = alarm.Rang, now, "", ""
		if perr := s.Alarms.Put(a); perr != nil {
			return []string{id + ": went out, but not saved as rung: " + perr.Error()}
		}

		return nil
	}

	a.TryErr = err.Error()

	if a.Tries >= effectTries {
		line, note := s.giveUp(a, now)
		if note != "" {
			s.addNotes([]string{note})
		}

		return []string{line}
	}

	if perr := s.Alarms.Put(a); perr != nil {
		return []string{id + ": failed, not saved: " + perr.Error()}
	}

	return nil
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

	s.owe(a, now, a.Text+late)

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

	s.owe(a, now, "back from later: "+it.Text+late)

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
	queued, _ := s.trackLine(name, AlarmPrefix+body(*a)+late)
	s.enqueue(name, queued, Facts{From: senderOf(a.By)})
	line = a.ID + ": " + name + " told"

	if s.has(name) {
		return line, ""
	}

	o := UpOptions{Auto: true}

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

	// What the start did, not what it was asked: a conversation gone from the namespace started the role fresh.
	if r := s.Resumed(name); r != "" {
		return line + ", resumed " + r, ""
	}

	if o.Resume != "" {
		return line + ", started fresh (" + o.Resume + " is gone)", ""
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

	// A sleep is a down and a start at its end: refused for a role placed on another node, as a restart is.
	if err := s.errPlacedAway(name); err != nil {
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

			return a, wait, Partial(s.Alarms.Put(a)) // the down is begun: it is saved even when the alarm is not
		}
	}

	a, err := s.Alarms.Add(alarm.Alarm{Kind: alarm.Sleep, Role: name, At: at, When: when, Text: text, Note: note,
		By: s.By}, s.Now())
	if err != nil {
		return a, wait, Partial(err)
	}

	s.log(name, "sleep", "", a.ID+" until "+at.In(s.Crew.Location()).Format("Mon 2 Jan 15:04"))

	return a, wait, nil
}
