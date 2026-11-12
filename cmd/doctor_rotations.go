package cmd

import (
	"strings"
	"time"

	"github.com/iamnikolie/zmina/internal/crew"
	"github.com/iamnikolie/zmina/internal/shift"
)

// rotationStale is how long a rotation may stay open before doctor says so: the beat tells the dispatcher of one at 15 min and
// closes it once it confirmed or its time ran out, never while the role is at work; one open for an hour is a role at work
// all that time, a ticker that is not running, or a record and a state that do not agree.
const rotationStale = time.Hour

// checkRotations warns of each rotation of the crew that is open past rotationStale. It only reads the records, and says nothing
// when none is.
func checkRotations(c *crew.Crew, now time.Time, add func(name, level, format string, args ...any)) {
	rots, err := shift.ReadRotations(c.Layout(), "")
	if err != nil {
		add("rotations", "warn", "%v", err)

		return
	}

	for _, r := range rots {
		base, _, _ := strings.Cut(r.Role, "#")
		if _, ok := c.Roles[base]; !ok || !r.Open() || r.Trim() || now.Sub(r.Prepared) < rotationStale {
			continue
		}

		since := r.Prepared
		if !r.Started.IsZero() {
			since = r.Started
		}

		add("rotations", "warn", "%s: rotation %s (%s) has been open for %s, %s: zmina rotation show %s; zmina rotation abort %s puts it "+
			"back on its old conversation", r.Role, r.ID, r.Kind, age(now.Sub(since)), r.Phase, r.Role, r.Role)
	}
}
