package crew

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The events the crew's notify command is run for (notify_on).
const (
	// NotifyReminder is a reminder for the owner ringing, a parked item coming back.
	NotifyReminder = "reminder"
	// NotifyBlocking is an item on the owner's todo that holds a role up: an ask with --blocks, a task waiting on
	// him while its role sits idle.
	NotifyBlocking = "blocking"
	// NotifyAsk is every new ask on the todo, a decision or a check, once.
	NotifyAsk = "ask"
)

var notifyEvents = []string{NotifyReminder, NotifyBlocking, NotifyAsk}

// defaultNotifyAgain is how long a blocking item waits before it is pushed again while it still blocks.
const defaultNotifyAgain = 24 * time.Hour

type notifyRules struct {
	on         []string
	quiet      bool
	quietFrom  time.Duration // past midnight
	quietUntil time.Duration
	again      time.Duration // 0: once per item
}

func (c *Crew) parseNotify() error {
	r := notifyRules{on: []string{NotifyReminder, NotifyBlocking}, again: defaultNotifyAgain}

	if len(c.NotifyOn) > 0 {
		r.on = nil

		for _, e := range c.NotifyOn {
			e = strings.TrimSpace(e)
			if !slices.Contains(notifyEvents, e) {
				return fmt.Errorf("notify_on %q: one of %s", e, strings.Join(notifyEvents, ", "))
			}

			if !slices.Contains(r.on, e) {
				r.on = append(r.on, e)
			}
		}
	}

	if v := strings.TrimSpace(c.QuietHours); v != "" && v != "off" {
		from, until, ok := strings.Cut(v, "-")
		f, err1 := time.Parse("15:04", strings.TrimSpace(from))
		u, err2 := time.Parse("15:04", strings.TrimSpace(until))

		if !ok || err1 != nil || err2 != nil || f.Equal(u) {
			return fmt.Errorf("quiet_hours %q: two times of day such as 23:00-08:00, or off", c.QuietHours)
		}

		r.quiet = true
		r.quietFrom = time.Duration(f.Hour())*time.Hour + time.Duration(f.Minute())*time.Minute
		r.quietUntil = time.Duration(u.Hour())*time.Hour + time.Duration(u.Minute())*time.Minute
	}

	switch v := strings.TrimSpace(c.NotifyAgain); v {
	case "":
	case "off", "0":
		r.again = 0
	default:
		d, err := ParseSpan(v)
		if err != nil || d < time.Hour {
			return fmt.Errorf("notify_again %q: a span of an hour at least, such as 6h or 1d, or off", c.NotifyAgain)
		}

		r.again = d
	}

	c.notify = r

	return nil
}

// NotifiesOn reports whether the notify command runs for an event (NotifyReminder, NotifyBlocking, NotifyAsk): a
// command is set and notify_on names the event.
func (c *Crew) NotifiesOn(event string) bool {
	return !c.Notify.Empty() && slices.Contains(c.notify.on, event)
}

// NotifyAgainD is how long an item that still blocks waits before it is pushed again; zero is never.
func (c *Crew) NotifyAgainD() time.Duration { return c.notify.again }

// QuietAt reports whether t falls in the crew's quiet hours (on its clock, Location), and when they end.
func (c *Crew) QuietAt(t time.Time) (bool, time.Time) {
	r := c.notify
	if !r.quiet {
		return false, time.Time{}
	}

	local := t.In(c.Location())
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
	at := func(day time.Time, d time.Duration) time.Time {
		return time.Date(day.Year(), day.Month(), day.Day(), int(d/time.Hour), int(d%time.Hour/time.Minute), 0, 0,
			day.Location())
	}

	// The stretch that started today, and the one that started yesterday (it may run past midnight).
	for _, day := range []time.Time{midnight, midnight.AddDate(0, 0, -1)} {
		from := at(day, r.quietFrom)
		until := at(day, r.quietUntil)

		if !until.After(from) {
			until = at(day.AddDate(0, 0, 1), r.quietUntil)
		}

		if !local.Before(from) && local.Before(until) {
			return true, until
		}
	}

	return false, time.Time{}
}
