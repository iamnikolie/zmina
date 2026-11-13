package limits

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Plan is a subscription the crew's roles bill: the one a Claude role runs on, the one a Codex role runs on.
type Plan string

const (
	PlanClaude Plan = "claude"
	PlanCodex  Plan = "codex"
)

// The windows a plan reports, by the names that key a period and that `zmina limits --json` uses.
const (
	WindowFiveHour = "five_hour"
	WindowSevenDay = "seven_day"
	WindowWeek     = "week"
)

// WindowLabel is a window in words, as a note names it: "5h window", "7d window", "weekly window".
func WindowLabel(window string) string {
	switch window {
	case WindowFiveHour:
		return "5h window"
	case WindowSevenDay:
		return "7d window"
	case WindowWeek:
		return "weekly window"
	}

	return window + " window"
}

// Share is a threshold in percent of a window spent: 1 to 100, or 0 for off.
type Share int

// On reports whether the threshold is set.
func (s Share) On() bool { return s > 0 }

// String is the share as the crew file spells it: 75, or off.
func (s Share) String() string {
	if s <= 0 {
		return "off"
	}

	return strconv.Itoa(int(s))
}

// MarshalJSON writes a share as a number, and off as "off".
func (s Share) MarshalJSON() ([]byte, error) {
	if s <= 0 {
		return []byte(`"off"`), nil
	}

	return json.Marshal(int(s))
}

// UnmarshalJSON reads what MarshalJSON writes.
func (s *Share) UnmarshalJSON(raw []byte) error {
	var n int
	if json.Unmarshal(raw, &n) == nil {
		*s = Share(n)

		return nil
	}

	var word string
	if json.Unmarshal(raw, &word) != nil || strings.TrimSpace(word) != "off" {
		return fmt.Errorf("a share is a number or off, not %s", raw)
	}

	*s = 0

	return nil
}

// Shares are the three thresholds of one plan, each in percent of a window spent, each applied to every window the plan
// reports: Warn tells the dispatcher once, Checkpoint asks the roles holding a task for a checkpoint once, and from Hold
// nothing is delivered to the plan's roles until the window resets. Warn <= Checkpoint <= Hold where they are set.
type Shares struct {
	Warn       Share `json:"warn"`
	Checkpoint Share `json:"checkpoint"`
	Hold       Share `json:"hold"`
}

// Thresholds are the crew's shares per plan. They are apart from Policy on purpose: the policy decides where work is
// routed, the thresholds what the crew does as a window fills.
type Thresholds struct {
	Claude Shares `json:"claude"`
	Codex  Shares `json:"codex"`
}

// The default thresholds: on for both plans.
const (
	DefaultWarn       = 75
	DefaultCheckpoint = 90
	DefaultHold       = 97
)

// DefaultShares are the thresholds of a crew file that says nothing of them.
func DefaultShares() Shares {
	return Shares{Warn: DefaultWarn, Checkpoint: DefaultCheckpoint, Hold: DefaultHold}
}

// DefaultThresholds are the thresholds of a crew file with no limits: key.
func DefaultThresholds() Thresholds {
	return Thresholds{Claude: DefaultShares(), Codex: DefaultShares()}
}

// For is the shares of one plan; none (all off) for a plan that is not known.
func (t Thresholds) For(plan Plan) Shares {
	switch plan {
	case PlanClaude:
		return t.Claude
	case PlanCodex:
		return t.Codex
	}

	return Shares{}
}

// Any reports whether any threshold of any plan is set.
func (t Thresholds) Any() bool { return t.Claude.any() || t.Codex.any() }

func (s Shares) any() bool { return s.Warn.On() || s.Checkpoint.On() || s.Hold.On() }

// String is the shares in one line: "warn 75%, checkpoint 90%, hold 97%", an off one as "hold off".
func (s Shares) String() string {
	part := func(name string, v Share) string {
		if !v.On() {
			return name + " off"
		}

		return fmt.Sprintf("%s %d%%", name, int(v))
	}

	return part("warn", s.Warn) + ", " + part("checkpoint", s.Checkpoint) + ", " + part("hold", s.Hold)
}

// String is the thresholds in one line, as `zmina limits` prints them.
func (t Thresholds) String() string {
	return "claude: " + t.Claude.String() + " · codex: " + t.Codex.String()
}

// Order reports what is wrong with the order of the shares: warn above checkpoint, or either above hold, among those set.
func (s Shares) Order() error {
	set := []struct {
		name string
		v    Share
	}{{"warn", s.Warn}, {"checkpoint", s.Checkpoint}, {"hold", s.Hold}}

	prev := set[0]

	for _, cur := range set[1:] {
		if !cur.v.On() {
			continue
		}

		if prev.v.On() && prev.v > cur.v {
			return fmt.Errorf("%s %d is above %s %d: they act in the order warn, checkpoint, hold", prev.name, int(prev.v), cur.name, int(cur.v))
		}

		prev = cur
	}

	return nil
}
