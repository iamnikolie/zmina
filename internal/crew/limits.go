package crew

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/iamnikolie/zmina/internal/limits"
)

// Limits is the crew file's optional limits: key, how the crew spends its plans. A share not given keeps the
// default (limits.DefaultPolicy: the rules the dispatcher has always followed; limits.DefaultThresholds).
//
//	limits:
//	  claude: {pinned: 80, stop: 99, warn: 75, checkpoint: 90, hold: 97}
//	  codex:  {pinned: 80, warn: 75, checkpoint: 90}
//	  # hold: a number holds the short window (Claude's 5-hour one); off holds none; a map says it per window:
//	  #   hold: {five_hour: 97, seven_day: 98}
//
// pinned and stop are percent of the 7-day (Claude) and the weekly (Codex) window and route work. warn and checkpoint are
// percent of every window the plan reports and say what the crew does as it fills; hold is percent of the windows it names.
// Any of them may be off.
type Limits struct {
	Claude *ClaudeLimits `yaml:"claude"`
	Codex  *CodexLimits  `yaml:"codex"`
}

// ClaudeLimits: from Pinned percent of the 7-day window only work that needs Claude goes to it, from Stop percent
// nothing does.
type ClaudeLimits struct {
	Pinned     *int        `yaml:"pinned"`
	Stop       *int        `yaml:"stop"`
	Warn       *Share      `yaml:"warn"`
	Checkpoint *Share      `yaml:"checkpoint"`
	Hold       *HoldShares `yaml:"hold"`
}

// CodexLimits: from Pinned percent of the weekly window only work that needs Codex goes to it.
type CodexLimits struct {
	Pinned     *int        `yaml:"pinned"`
	Warn       *Share      `yaml:"warn"`
	Checkpoint *Share      `yaml:"checkpoint"`
	Hold       *HoldShares `yaml:"hold"`
}

// Share is a threshold of the limits: key as the crew file spells it, a percent of a window or off. Whether it is in range is
// checked with the rest of the key (checkLimits), so that the error names the key.
type Share struct {
	// Off is set for the word off; N is the percent otherwise.
	Off bool
	N   int
}

// parseShare reads a whole number or the word off.
func parseShare(n *yaml.Node) (Share, error) {
	bad := fmt.Errorf("line %d: a share of the window is a number from 1 to 100, or off, not %q", n.Line, n.Value)

	if n.Kind != yaml.ScalarNode {
		return Share{}, bad
	}

	word := strings.ToLower(strings.TrimSpace(n.Value))
	if word == "off" {
		return Share{Off: true}, nil
	}

	v, err := strconv.Atoi(word)
	if err != nil {
		return Share{}, bad
	}

	return Share{N: v}, nil
}

// UnmarshalYAML reads a whole number or the word off.
func (s *Share) UnmarshalYAML(n *yaml.Node) error {
	v, err := parseShare(n)
	if err != nil {
		return err
	}

	*s = v

	return nil
}

// value is the share as the limits package holds it: its percent, 0 for off.
func (s Share) value() limits.Share {
	if s.Off {
		return 0
	}

	return limits.Share(s.N)
}

// HoldShares is the hold: key: a number or off for the short window (Claude's 5-hour one; Codex has none), or a share for each window
// it names, by the names the limits data uses (five_hour, seven_day, week). A window the key does not name keeps its default: the
// short one 97, a long one not held.
type HoldShares struct {
	// Short is the scalar form; Windows the map form.
	Short   *Share
	Windows map[string]Share
}

// UnmarshalYAML reads a number or off, or a map of windows to numbers or off.
func (h *HoldShares) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		v, err := parseShare(n)
		if err != nil {
			return err
		}

		*h = HoldShares{Short: &v}

		return nil
	}

	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: hold is a number or off (the short window), or a share for each window: {five_hour: 97, seven_day: off}", n.Line)
	}

	out := HoldShares{Windows: map[string]Share{}}

	for i := 0; i+1 < len(n.Content); i += 2 {
		v, err := parseShare(n.Content[i+1])
		if err != nil {
			return err
		}

		out.Windows[n.Content[i].Value] = v
	}

	*h = out

	return nil
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

// LimitsThresholds are the thresholds the crew file states over the defaults (limits.DefaultThresholds: warn 75 and checkpoint 90
// for every window of both plans, a hold at 97 on Claude's 5-hour window and none on a long one), a share at a time. A share that is
// off is off.
func (c *Crew) LimitsThresholds() limits.Thresholds {
	t := limits.DefaultThresholds()
	if c.Limits == nil {
		return t
	}

	apply := func(to *limits.Shares, short string, warn, checkpoint *Share, hold *HoldShares) {
		for _, f := range []struct {
			from *Share
			to   *limits.Share
		}{{warn, &to.Warn}, {checkpoint, &to.Checkpoint}} {
			if f.from != nil {
				*f.to = f.from.value()
			}
		}

		if hold == nil {
			return
		}

		if hold.Short != nil && short != "" {
			to.Hold[short] = hold.Short.value()
		}

		for w, v := range hold.Windows {
			to.Hold[w] = v.value()
		}
	}

	if cl := c.Limits.Claude; cl != nil {
		apply(&t.Claude, limits.WindowFiveHour, cl.Warn, cl.Checkpoint, cl.Hold)
	}

	if cx := c.Limits.Codex; cx != nil {
		apply(&t.Codex, "", cx.Warn, cx.Checkpoint, cx.Hold)
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

// checkThresholds checks the warn, checkpoint and hold of each plan: each a whole share, 1 to 100, or off; a hold that names only
// windows the plan reports, and Codex's none as a number (it has no short window); and the order they act in, among those that are set
// (the defaults count as set, so one share raised past the others' defaults is refused).
func (c *Crew) checkThresholds() error {
	type plan struct {
		plan       limits.Plan
		warn, ckpt *Share
		hold       *HoldShares
	}

	var plans []plan

	if cl := c.Limits.Claude; cl != nil {
		plans = append(plans, plan{limits.PlanClaude, cl.Warn, cl.Checkpoint, cl.Hold})
	}

	if cx := c.Limits.Codex; cx != nil {
		plans = append(plans, plan{limits.PlanCodex, cx.Warn, cx.Checkpoint, cx.Hold})
	}

	inRange := func(key string, s *Share) error {
		if s != nil && !s.Off && (s.N < 1 || s.N > 100) {
			return fmt.Errorf("limits.%s is a share of the window, 1 to 100, or off, not %d", key, s.N)
		}

		return nil
	}

	for _, p := range plans {
		if err := inRange(string(p.plan)+".warn", p.warn); err != nil {
			return err
		}

		if err := inRange(string(p.plan)+".checkpoint", p.ckpt); err != nil {
			return err
		}

		if p.hold == nil {
			continue
		}

		if s := p.hold.Short; s != nil {
			if err := inRange(string(p.plan)+".hold", s); err != nil {
				return err
			}

			if p.plan == limits.PlanCodex && !s.Off {
				return fmt.Errorf("limits.codex.hold: a number holds the short window, and Codex reports only a weekly one: write hold: {week: %d}", s.N)
			}
		}

		windows := limits.PlanWindows(p.plan)

		for _, w := range slices.Sorted(maps.Keys(p.hold.Windows)) {
			if !slices.Contains(windows, w) {
				return fmt.Errorf("limits.%s.hold: no window %q: the windows are %s", p.plan, w, strings.Join(windows, ", "))
			}

			v := p.hold.Windows[w]
			if err := inRange(string(p.plan)+".hold."+w, &v); err != nil {
				return err
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

// CodexHomeOf is the Codex home a Codex seat runs with: its own codex_home, else ~/.codex. Seats of different homes bill
// different accounts. Empty for a seat that is not Codex's.
func (c *Crew) CodexHomeOf(r *Role) string {
	if r == nil || !r.IsCodex() {
		return ""
	}

	return or(r.CodexHome, ExpandHome("~/.codex"))
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
