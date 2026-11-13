package crew

import (
	"fmt"
	"strings"
	"time"
)

// The defaults and the limit of the later_sweep key (see LaterSweep).
const (
	DefaultLaterSweepEvery = 7 * 24 * time.Hour
	DefaultLaterSweepOlder = 14 * 24 * time.Hour
	DefaultLaterSweepMax   = 12
	// LaterSweepMaxItems is the most items one sweep may hand a role: its packet lists each of them.
	LaterSweepMaxItems = 25
)

// LaterSweep is the crew's automatic sweep of the parked list: the ticker makes a sweep task for Role (zmina later sweep)
// once Every has passed since the last one, when none is open and enough items have gone stale. Absent: no automatic
// sweep, as before; a sweep by hand needs none of it.
type LaterSweep struct {
	// Role is the seat that gets the sweep.
	Role string `yaml:"role"`
	// Every is how long after the last sweep task was made the next may be (default 7d), Older how old, and how long since
	// it was last kept, an item has to be to go in (default 14d), Max how many items one sweep takes (default 12).
	Every string `yaml:"every"`
	Older string `yaml:"older"`
	Max   int    `yaml:"max"`
}

// LaterSweepOn reports whether the ticker sweeps the parked list by itself.
func (c *Crew) LaterSweepOn() bool { return c.LaterSweep != nil }

// EverySpan is the time between sweeps.
func (l LaterSweep) EverySpan() time.Duration { return spanOr(l.Every, DefaultLaterSweepEvery) }

// OlderSpan is how old an item has to be, and how long it may have gone unreviewed, to be swept.
func (l LaterSweep) OlderSpan() time.Duration { return spanOr(l.Older, DefaultLaterSweepOlder) }

// MaxItems is the most items one sweep takes.
func (l LaterSweep) MaxItems() int {
	if l.Max > 0 {
		return l.Max
	}

	return DefaultLaterSweepMax
}

func spanOr(v string, def time.Duration) time.Duration {
	if strings.TrimSpace(v) == "" {
		return def
	}

	d, err := ParseSpan(v)
	if err != nil || d <= 0 {
		return def
	}

	return d
}

// checkLaterSweep says what is wrong with the later_sweep block: no role, a role the crew does not have, a span that is
// not one, or a number of items out of range.
func (c *Crew) checkLaterSweep() error {
	l := c.LaterSweep
	if l == nil {
		return nil
	}

	role := strings.TrimSpace(l.Role)

	switch _, ok := c.Roles[role]; {
	case role == "":
		return fmt.Errorf("later_sweep: role is a role of this crew (%s)", strings.Join(c.Names(), ", "))
	case !ok:
		return fmt.Errorf("later_sweep: role %q is not one of the roles (%s)", role, strings.Join(c.Names(), ", "))
	}

	for _, f := range []struct{ key, value string }{{"every", l.Every}, {"older", l.Older}} {
		if strings.TrimSpace(f.value) == "" {
			continue
		}

		if d, err := ParseSpan(f.value); err != nil || d <= 0 {
			return fmt.Errorf("later_sweep: %s %q: a span of time such as 12h or 7d", f.key, f.value)
		}
	}

	if l.Max < 0 || l.Max > LaterSweepMaxItems {
		return fmt.Errorf("later_sweep: max is a number of items from 1 to %d, not %d", LaterSweepMaxItems, l.Max)
	}

	return nil
}
