// Package limits reads how full the subscription plans are: Claude's 5-hour and 7-day windows, as Claude Code's
// status line reports them (a tap in the role's status line writes them to a file, see Tap), and Codex's weekly
// window, as the newest rollout of a Codex home records it. Nothing here guesses: a value whose window has reset,
// or that was seen too long ago, is marked stale and printed as unknown with the figure it last had.
//
// The package is pure in the sense that it knows no crew and no state: callers hand it directories and a clock.
package limits

import (
	"fmt"
	"time"
)

const (
	// StaleAfter is how old a reading may be before it stands for nothing: no session has drawn a status line or
	// run a turn since, so the plan may have been used elsewhere.
	StaleAfter = 6 * time.Hour
	// Heartbeat is how often the tap refreshes a reading whose values did not change: the file then says when a
	// status line was last drawn, not when the figures last moved, and an idle crew is not mistaken for a silent one.
	Heartbeat = 5 * time.Minute
)

// Window is one rate-limit window as a source last saw it.
type Window struct {
	// Used is the percent of the window spent.
	Used float64 `json:"used"`
	// Resets is when the window ends and its percent starts over; nil when the source did not say.
	Resets *time.Time `json:"resets,omitempty"`
	// Stale is set when reading: the window has reset since, or the reading is too old. Used is then the figure it
	// last had, not the plan's present state.
	Stale bool `json:"stale,omitempty"`
}

// judge marks the window stale when it ended since the reading at, or the reading is older than StaleAfter.
func (w *Window) judge(at, now time.Time) {
	w.Stale = (w.Resets != nil && !now.Before(*w.Resets)) || now.Sub(at) > StaleAfter
}

// same reports whether two windows hold the same figures (what the tap compares before it writes).
func (w *Window) same(o *Window) bool {
	if w == nil || o == nil {
		return w == o
	}

	if w.Used != o.Used || (w.Resets == nil) != (o.Resets == nil) {
		return false
	}

	return w.Resets == nil || w.Resets.Equal(*o.Resets)
}

// latest is the window that stands when w, a reading just drawn, meets o, the one stored: o when w is older by its own
// evidence (it resets before o does, or at the same moment with less spent), else w. A window that says nothing of its
// reset has no evidence of age, and w stands.
func (w *Window) latest(o *Window) *Window {
	if w == nil || o == nil || w.Resets == nil || o.Resets == nil {
		return w
	}

	if w.Resets.Before(*o.Resets) || (w.Resets.Equal(*o.Resets) && w.Used < o.Used) {
		return o
	}

	return w
}

// Claude is what Claude Code's status line last said about the plan. It is also the file the tap writes.
type Claude struct {
	FiveHour *Window `json:"five_hour,omitempty"`
	SevenDay *Window `json:"seven_day,omitempty"`
	// At is when a status line last drew these figures.
	At     time.Time `json:"at"`
	Source string    `json:"source"`
}

// SourceStatusline is Claude's source: the status line's JSON.
const SourceStatusline = "statusline"

// Judge marks each window stale or not as of now.
func (c *Claude) Judge(now time.Time) {
	for _, w := range []*Window{c.FiveHour, c.SevenDay} {
		if w != nil {
			w.judge(c.At, now)
		}
	}
}

// sameFigures reports whether o holds the figures c does.
func (c *Claude) sameFigures(o *Claude) bool {
	return c.FiveHour.same(o.FiveHour) && c.SevenDay.same(o.SevenDay)
}

// Codex is what the newest rollout of a Codex home last recorded about the plan.
type Codex struct {
	// Week is the 10080-minute window: the weekly limit.
	Week *Window `json:"week,omitempty"`
	// Plan is the plan name Codex records (plan_type).
	Plan string `json:"plan,omitempty"`
	// At is when the rollout recorded it.
	At time.Time `json:"at"`
	// Source names the rollout's home: "rollout <seat>".
	Source string `json:"source"`
}

// Judge marks the window stale or not as of now.
func (c *Codex) Judge(now time.Time) {
	if c.Week != nil {
		c.Week.judge(c.At, now)
	}
}

// Report is everything `zmina limits` shows.
type Report struct {
	// Claude and Codex are nil when no reading was ever found.
	Claude *Claude `json:"claude"`
	Codex  *Codex  `json:"codex"`
	Policy Policy  `json:"policy"`
}

// Collect reads both plans as of now: Claude's from the tap's file in dir, Codex's from the rollouts under homes.
// A reading that cannot be read leaves its plan unknown; the error says why, and the rest is still returned.
func Collect(dir string, homes []Home, policy Policy, now time.Time) (Report, error) {
	r := Report{Policy: policy}

	var err error

	if r.Claude, err = ReadClaude(dir); r.Claude != nil {
		r.Claude.Judge(now)
	}

	if r.Codex = ReadCodex(homes); r.Codex != nil {
		r.Codex.Judge(now)
	}

	return r, err
}

// In is the report with every time on loc's clock.
func (r Report) In(loc *time.Location) Report {
	window := func(w *Window) *Window {
		if w == nil {
			return nil
		}

		cp := *w
		if cp.Resets != nil {
			t := cp.Resets.In(loc)
			cp.Resets = &t
		}

		return &cp
	}

	if r.Claude != nil {
		c := *r.Claude
		c.FiveHour, c.SevenDay, c.At = window(c.FiveHour), window(c.SevenDay), c.At.In(loc)
		r.Claude = &c
	}

	if r.Codex != nil {
		c := *r.Codex
		c.Week, c.At = window(c.Week), c.At.In(loc)
		r.Codex = &c
	}

	return r
}

// Policy is how the crew spends its plans: the shares of the 7-day (Claude) and weekly (Codex) window at which
// work narrows. The defaults are the rules the dispatcher has always followed.
type Policy struct {
	Claude ClaudePolicy `json:"claude"`
	Codex  CodexPolicy  `json:"codex"`
}

// ClaudePolicy: from Pinned percent only work that needs Claude goes there, from Stop percent nothing does.
type ClaudePolicy struct {
	Pinned int `json:"pinned"`
	Stop   int `json:"stop"`
}

// CodexPolicy: from Pinned percent only work that needs Codex goes there.
type CodexPolicy struct {
	Pinned int `json:"pinned"`
}

// The default thresholds.
const (
	DefaultClaudePinned = 80
	DefaultClaudeStop   = 99
	DefaultCodexPinned  = 80
)

// DefaultPolicy is the policy of a crew file with no limits: key.
func DefaultPolicy() Policy {
	return Policy{
		Claude: ClaudePolicy{Pinned: DefaultClaudePinned, Stop: DefaultClaudeStop},
		Codex:  CodexPolicy{Pinned: DefaultCodexPinned},
	}
}

// Band is what a plan takes at a given share used.
type Band string

const (
	// Open: any work may go to the plan.
	Open Band = "open"
	// PinnedOnly: only work that needs this plan.
	PinnedOnly Band = "pinned"
	// Stopped: nothing.
	Stopped Band = "stopped"
)

// Band is what Claude takes at used percent of its 7-day window.
func (p ClaudePolicy) Band(used float64) Band {
	switch {
	case used >= float64(p.Stop):
		return Stopped
	case used >= float64(p.Pinned):
		return PinnedOnly
	}

	return Open
}

// Band is what Codex takes at used percent of its weekly window.
func (p CodexPolicy) Band(used float64) Band {
	if used >= float64(p.Pinned) {
		return PinnedOnly
	}

	return Open
}

// String is the policy in one line, as `zmina limits` prints it.
func (p Policy) String() string {
	return fmt.Sprintf("claude: pinned work only from %d%%, nothing from %d%% · codex: pinned work only from %d%%",
		p.Claude.Pinned, p.Claude.Stop, p.Codex.Pinned)
}
