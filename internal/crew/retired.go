package crew

import (
	"fmt"
	"regexp"
)

// movedTo is what moved_to may say: <node>/<crew> in plain words, since it goes into a role's boot line.
var movedTo = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// IsRetired reports a crew the owner marked as no longer the live one (zmina retire). Nothing but that mark says so:
// two crews with one name are not known to be one crew.
func (c *Crew) IsRetired() bool { return c.Retired != "" || c.MovedTo != "" }

// RetiredLine says so, and where the live crew is; empty for a crew not retired.
func (c *Crew) RetiredLine() string {
	if !c.IsRetired() {
		return ""
	}

	line := "This crew is retired"
	if c.Retired != "" {
		line += " since " + c.Retired
	}

	if c.MovedTo != "" {
		return fmt.Sprintf("%s: its live crew is %s (zmina ls %s; zmina task ls %s)", line, c.MovedTo, c.MovedTo, c.MovedTo)
	}

	return line + ", no live crew named"
}

// checkRetired keeps a moved_to that does not read out of every line that quotes it; the crew stays retired.
func (c *Crew) checkRetired() {
	if c.MovedTo == "" || movedTo.MatchString(c.MovedTo) {
		return
	}

	c.Warnings = append(c.Warnings, fmt.Sprintf("moved_to %q: want <node>/<crew>, ignored", c.MovedTo))
	c.MovedTo = ""

	if c.Retired == "" {
		c.Retired = "unknown date"
	}
}

// NewMovedTo checks what moved_to is about to say.
func NewMovedTo(v string) (string, error) {
	if !movedTo.MatchString(v) {
		return "", fmt.Errorf("moved_to %q: want <node>/<crew> in plain words", v)
	}

	return v, nil
}
