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

// Shares are the three thresholds of one plan, each in percent of a window spent. Warn tells the dispatcher once and Checkpoint
// asks the roles holding a task for a checkpoint once, each for every window the plan reports. Hold is per window: from its
// share nothing is delivered to the plan's roles until that window resets, and a window with no hold share (or off) is never
// held. By default only the short window is held. Warn <= Checkpoint <= each hold share that is set.
type Shares struct {
	Warn       Share            `json:"warn"`
	Checkpoint Share            `json:"checkpoint"`
	Hold       map[string]Share `json:"hold"`
}

// HoldOf is the hold share of one window: 0, off, for a window that has none.
func (s Shares) HoldOf(window string) Share { return s.Hold[window] }

// Thresholds are the crew's shares per plan. They are apart from Policy on purpose: the policy decides where work is
// routed, the thresholds what the crew does as a window fills.
type Thresholds struct {
	Claude Shares `json:"claude"`
	Codex  Shares `json:"codex"`
}

// The default thresholds: warn and checkpoint on every window of both plans, hold on the short window only.
const (
	DefaultWarn       = 75
	DefaultCheckpoint = 90
	DefaultHold       = 97
)

// PlanWindows are the windows a plan reports, short first: Claude's 5-hour and 7-day, Codex's week (its rollouts carry no
// short window yet, so by default Codex holds nothing).
func PlanWindows(plan Plan) []string {
	switch plan {
	case PlanClaude:
		return []string{WindowFiveHour, WindowSevenDay}
	case PlanCodex:
		return []string{WindowWeek}
	}

	return nil
}

// DefaultShares are the thresholds of a crew file that says nothing of them: warn 75 and checkpoint 90, and a hold at 97 on the
// plan's short window (Claude's 5-hour window) and none on a long one.
func DefaultShares(plan Plan) Shares {
	s := Shares{Warn: DefaultWarn, Checkpoint: DefaultCheckpoint, Hold: map[string]Share{}}

	for _, w := range PlanWindows(plan) {
		s.Hold[w] = 0
	}

	if plan == PlanClaude {
		s.Hold[WindowFiveHour] = DefaultHold
	}

	return s
}

// DefaultThresholds are the thresholds of a crew file with no limits: key.
func DefaultThresholds() Thresholds {
	return Thresholds{Claude: DefaultShares(PlanClaude), Codex: DefaultShares(PlanCodex)}
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

func (s Shares) any() bool {
	if s.Warn.On() || s.Checkpoint.On() {
		return true
	}

	for _, h := range s.Hold {
		if h.On() {
			return true
		}
	}

	return false
}

// windowShort is a window as the thresholds line says it: 5h, 7d, week.
func windowShort(window string) string {
	switch window {
	case WindowFiveHour:
		return "5h"
	case WindowSevenDay:
		return "7d"
	}

	return window
}

// String is the shares in one line: "warn 75%, checkpoint 90%, hold 5h 97%, 7d off", an off one as "off".
func (s Shares) String() string {
	part := func(name string, v Share) string {
		if !v.On() {
			return name + " off"
		}

		return fmt.Sprintf("%s %d%%", name, int(v))
	}

	var holds []string

	for _, w := range []string{WindowFiveHour, WindowSevenDay, WindowWeek} {
		h, ok := s.Hold[w]
		if !ok {
			continue
		}

		if h.On() {
			holds = append(holds, fmt.Sprintf("%s %d%%", windowShort(w), int(h)))
		} else {
			holds = append(holds, windowShort(w)+" off")
		}
	}

	hold := "hold off"
	if len(holds) > 0 {
		hold = "hold " + strings.Join(holds, ", ")
	}

	return part("warn", s.Warn) + ", " + part("checkpoint", s.Checkpoint) + ", " + hold
}

// String is the thresholds in one line, as `zmina limits` prints them.
func (t Thresholds) String() string {
	return "claude: " + t.Claude.String() + " · codex: " + t.Codex.String()
}

// Order reports what is wrong with the order of the shares: warn above checkpoint, or either above the hold share of a window,
// among those set.
func (s Shares) Order() error {
	if s.Warn.On() && s.Checkpoint.On() && s.Warn > s.Checkpoint {
		return fmt.Errorf("warn %d is above checkpoint %d: they act in the order warn, checkpoint, hold", int(s.Warn), int(s.Checkpoint))
	}

	for _, w := range []string{WindowFiveHour, WindowSevenDay, WindowWeek} {
		h := s.Hold[w]
		if !h.On() {
			continue
		}

		for _, lower := range []struct {
			name string
			v    Share
		}{{"checkpoint", s.Checkpoint}, {"warn", s.Warn}} {
			if lower.v.On() && lower.v > h {
				return fmt.Errorf("%s %d is above the hold of the %s %d: they act in the order warn, checkpoint, hold", lower.name,
					int(lower.v), WindowLabel(w), int(h))
			}
		}
	}

	return nil
}
