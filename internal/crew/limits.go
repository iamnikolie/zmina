package crew

import (
	"fmt"
	"slices"
	"strings"

	"github.com/iamnikolie/zmina/internal/limits"
)

// Limits is the crew file's optional limits: key, how the crew spends its plans. A share not given keeps the
// default (limits.DefaultPolicy: the rules the dispatcher has always followed).
//
//	limits:
//	  claude: {pinned: 80, stop: 99}   # % of the 7-day window
//	  codex:  {pinned: 80}             # % of the weekly window
type Limits struct {
	Claude *ClaudeLimits `yaml:"claude"`
	Codex  *CodexLimits  `yaml:"codex"`
}

// ClaudeLimits: from Pinned percent of the 7-day window only work that needs Claude goes to it, from Stop percent
// nothing does.
type ClaudeLimits struct {
	Pinned *int `yaml:"pinned"`
	Stop   *int `yaml:"stop"`
}

// CodexLimits: from Pinned percent of the weekly window only work that needs Codex goes to it.
type CodexLimits struct {
	Pinned *int `yaml:"pinned"`
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

// checkLimits reports what is wrong with the limits: key: a share outside 1 to 100, Claude's pinned above its stop.
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
