package crew

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/iamnikolie/zmina/internal/route"
)

// The default and the most rounds of the auto_review key.
const (
	DefaultAutoReviewRounds = 2
	MaxAutoReviewRounds     = 5
)

// AutoReview is the crew's review without the dispatcher: when a role closes a task that has a worktree as done, zmina
// makes the review task for the seat the routes' review rule picks, and hands a reject back to the author, up to Rounds
// reviews of one task. Absent or off: Rounds is 0 and nothing changes. Written `auto_review: on` (the default rounds),
// `off`, or `auto_review: {rounds: 3}`.
type AutoReview struct {
	Rounds int `yaml:"-"`
}

// UnmarshalYAML reads on or off, or a block with rounds, the only key it has: another is an error, since a mistyped key
// would otherwise be a review loop that silently runs another number of times.
func (a *AutoReview) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		switch strings.ToLower(strings.TrimSpace(n.Value)) {
		case "on", "true", "yes":
			*a = AutoReview{Rounds: DefaultAutoReviewRounds}
		case "off", "false", "no", "":
			*a = AutoReview{}
		default:
			return fmt.Errorf("auto_review: %q: want on, off or {rounds: N}", n.Value)
		}

		return nil
	case yaml.MappingNode:
		rounds := DefaultAutoReviewRounds

		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			if key.Value != "rounds" {
				return fmt.Errorf("line %d: auto_review has no key %q (rounds)", key.Line, key.Value)
			}

			v, err := strconv.Atoi(strings.TrimSpace(value.Value))
			if err != nil || v < 1 || v > MaxAutoReviewRounds {
				return fmt.Errorf("line %d: auto_review rounds is a number from 1 to %d, not %q", value.Line, MaxAutoReviewRounds, value.Value)
			}

			rounds = v
		}

		*a = AutoReview{Rounds: rounds}

		return nil
	}

	return fmt.Errorf("auto_review: want on, off or {rounds: N}")
}

// AutoReviewRounds is how many reviews of one task zmina makes by itself: 0 when auto_review is off, and when the routes
// have no review rule, which picks the reviewer (the key does nothing then, and zmina doctor says so).
func (c *Crew) AutoReviewRounds() int {
	if _, ok := c.ReviewRule(); !ok {
		return 0
	}

	return c.AutoReview.Rounds
}

// ReviewRule is the routes' review rule, which says who reviews; ok is false when they have none.
func (c *Crew) ReviewRule() (rule route.Rule, ok bool) {
	for _, r := range c.routes.Rules {
		if r.Kind == route.Review {
			return r, true
		}
	}

	return route.Rule{}, false
}
