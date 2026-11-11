package crew

import (
	"fmt"
	"strings"
	"time"
)

// The retention key: how long finished work stays where it is before the ticker's daily pass moves it to the
// archive directory. A span of days, at least RetentionFloor; off turns the pass off. Absent: DefaultRetention.
const (
	RetentionOff     = "off"
	RetentionFloor   = 30 * 24 * time.Hour
	DefaultRetention = 30 * 24 * time.Hour
)

// RetentionOn reports whether the ticker runs the daily retention pass (retention is not off).
func (c *Crew) RetentionOn() bool { return strings.TrimSpace(c.Retention) != RetentionOff }

// RetentionSpan is how old finished work has to be before the pass moves it: the retention key's span, else
// DefaultRetention. A hand run of `zmina retention` uses it whether the key is off or not.
func (c *Crew) RetentionSpan() time.Duration {
	v := strings.TrimSpace(c.Retention)
	if v == "" || v == RetentionOff {
		return DefaultRetention
	}

	d, err := ParseSpan(v)
	if err != nil || d < RetentionFloor {
		return DefaultRetention
	}

	return d
}

func checkRetention(v string) error {
	v = strings.TrimSpace(v)
	if v == "" || v == RetentionOff {
		return nil
	}

	if d, err := ParseSpan(v); err != nil || d < RetentionFloor {
		return fmt.Errorf("retention %q: a span of 30 days at least, such as 45d or 90d, or %s", v, RetentionOff)
	}

	return nil
}
