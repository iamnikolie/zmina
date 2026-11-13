package crew

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/iamnikolie/zmina/internal/limits"
)

// Limits is the crew file's optional limits: key, how the crew spends its plans. A share not given keeps the
// default (limits.DefaultPolicy: the rules the dispatcher has always followed).
//
//	limits:
//	  claude: {pinned: 80, stop: 99, warn: 75, checkpoint: 90, hold: 97}
//	  codex:  {pinned: 80, warn: 75, checkpoint: 90, hold: 97}
//
// pinned and stop are percent of the 7-day (Claude) and the weekly (Codex) window and route work. warn, checkpoint and
// hold are percent of every window the plan reports, each on its own, and say what the crew does as it fills; any of
// them may be off.
type Limits struct {
	Claude *ClaudeLimits `yaml:"claude"`
	Codex  *CodexLimits  `yaml:"codex"`
}

// ClaudeLimits: from Pinned percent of the 7-day window only work that needs Claude goes to it, from Stop percent
// nothing does.
type ClaudeLimits struct {
	Pinned     *int   `yaml:"pinned"`
	Stop       *int   `yaml:"stop"`
	Warn       *Share `yaml:"warn"`
	Checkpoint *Share `yaml:"checkpoint"`
	Hold       *Share `yaml:"hold"`
}

// CodexLimits: from Pinned percent of the weekly window only work that needs Codex goes to it.
type CodexLimits struct {
	Pinned     *int   `yaml:"pinned"`
	Warn       *Share `yaml:"warn"`
	Checkpoint *Share `yaml:"checkpoint"`
	Hold       *Share `yaml:"hold"`
}

// Share is a threshold of the limits: key as the crew file spells it, a percent of a window or off. Whether it is in range is
// checked with the rest of the key (checkLimits), so that the error names the key.
type Share struct {
	// Off is set for the word off; N is the percent otherwise.
	Off bool
	N   int
}

// UnmarshalYAML reads a whole number or the word off.
func (s *Share) UnmarshalYAML(n *yaml.Node) error {
	bad := fmt.Errorf("line %d: a share of the window is a number from 1 to 100, or off, not %q", n.Line, n.Value)

	if n.Kind != yaml.ScalarNode {
		return bad
	}

	word := strings.ToLower(strings.TrimSpace(n.Value))
	if word == "off" {
		*s = Share{Off: true}

		return nil
	}

	v, err := strconv.Atoi(word)
	if err != nil {
		return bad
	}

	*s = Share{N: v}

	return nil
}

// value is the share as the limits package holds it: its percent, 0 for off.
func (s *Share) value() limits.Share {
	if s.Off {
		return 0
	}

	return limits.Share(s.N)
}

// LimitsPolicy is the policy the crew file states over the defaults.
func (c *Crew) LimitsPolicy() limits.Policy {
	p := limits.DefaultPolicy()
	if c.Limits == nil {
		return p
	}

	if cl := c.Limits.Claude; cl != nil {
		if cl.Pinned != nil {
			p.Claude.Pinned = *cl.Pinned
		}

		if cl.Stop != nil {
			p.Claude.Stop = *cl.Stop
		}
	}

	if cx := c.Limits.Codex; cx != nil && cx.Pinned != nil {
		p.Codex.Pinned = *cx.Pinned
	}

	return p
}

// LimitsThresholds are the thresholds the crew file states over the defaults (limits.DefaultThresholds: warn 75, checkpoint 90,
// hold 97, for both plans), a share at a time. A share that is off is off.
func (c *Crew) LimitsThresholds() limits.Thresholds {
	t := limits.DefaultThresholds()
	if c.Limits == nil {
		return t
	}

	apply := func(to *limits.Shares, warn, checkpoint, hold *Share) {
		for _, f := range []struct {
			from *Share
			to   *limits.Share
		}{{warn, &to.Warn}, {checkpoint, &to.Checkpoint}, {hold, &to.Hold}} {
			if f.from != nil {
				*f.to = f.from.value()
			}
		}
	}

	if cl := c.Limits.Claude; cl != nil {
		apply(&t.Claude, cl.Warn, cl.Checkpoint, cl.Hold)
	}

	if cx := c.Limits.Codex; cx != nil {
		apply(&t.Codex, cx.Warn, cx.Checkpoint, cx.Hold)
	}

	return t
}

// checkLimits reports what is wrong with the limits: key: a share outside 1 to 100, Claude's pinned above its stop, a plan's
// warn, checkpoint and hold out of order.
func (c *Crew) checkLimits() error {
	if c.Limits == nil {
		return nil
	}

	share := func(key string, v *int) error {
		if v != nil && (*v < 1 || *v > 100) {
			return fmt.Errorf("limits.%s is a share of the window, 1 to 100, not %d", key, *v)
		}

		return nil
	}

	if cl := c.Limits.Claude; cl != nil {
		if err := share("claude.pinned", cl.Pinned); err != nil {
			return err
		}

		if err := share("claude.stop", cl.Stop); err != nil {
			return err
		}
	}

	if cx := c.Limits.Codex; cx != nil {
		if err := share("codex.pinned", cx.Pinned); err != nil {
			return err
		}
	}

	if p := c.LimitsPolicy(); p.Claude.Pinned > p.Claude.Stop {
		return fmt.Errorf("limits.claude: pinned %d is above stop %d: pinned work narrows before nothing goes at all",
			p.Claude.Pinned, p.Claude.Stop)
	}

	return c.checkThresholds()
}

// checkThresholds checks the warn, checkpoint and hold of each plan: each a whole share, 1 to 100, or off, and in the order they
// act, among those that are set (the defaults count as set, so one share raised past the others' defaults is refused).
func (c *Crew) checkThresholds() error {
	type plan struct {
		name string
		set  []*Share
		keys []string
	}

	var plans []plan

	if cl := c.Limits.Claude; cl != nil {
		plans = append(plans, plan{"claude", []*Share{cl.Warn, cl.Checkpoint, cl.Hold}, []string{"warn", "checkpoint", "hold"}})
	}

	if cx := c.Limits.Codex; cx != nil {
		plans = append(plans, plan{"codex", []*Share{cx.Warn, cx.Checkpoint, cx.Hold}, []string{"warn", "checkpoint", "hold"}})
	}

	for _, p := range plans {
		for i, s := range p.set {
			if s != nil && !s.Off && (s.N < 1 || s.N > 100) {
				return fmt.Errorf("limits.%s.%s is a share of the window, 1 to 100, or off, not %d", p.name, p.keys[i], s.N)
			}
		}
	}

	t := c.LimitsThresholds()
	for _, p := range []struct {
		name   string
		shares limits.Shares
	}{{"claude", t.Claude}, {"codex", t.Codex}} {
		if err := p.shares.Order(); err != nil {
			return fmt.Errorf("limits.%s: %w", p.name, err)
		}
	}

	return nil
}

// CodexHomes are the Codex homes whose rollouts say how full the Codex plan is: ~/.codex, which every Codex seat
// without a codex_home runs with, and each seat's own codex_home. A home is labelled with the seats that run with
// it, the dispatcher first; ~/.codex with none keeps that name.
func (c *Crew) CodexHomes() []limits.Home {
	byHome := map[string][]string{}
	order := []string{}
	def := ExpandHome("~/.codex")

	add := func(home string, seats ...string) {
		if _, ok := byHome[home]; !ok {
			order = append(order, home)
		}

		byHome[home] = append(byHome[home], seats...)
	}

	add(def)

	for _, name := range c.Names() {
		if r := c.Roles[name]; r.IsCodex() {
			add(or(r.CodexHome, def), name)
		}
	}

	var out []limits.Home

	for _, home := range order {
		label := strings.Join(slices.Compact(byHome[home]), ", ")
		if label == "" {
			label = "~/.codex"
		}

		out = append(out, limits.Home{Path: home, Label: label})
	}

	return out
}
