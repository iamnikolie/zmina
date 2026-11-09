package crew

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The defaults of a lessons block: the strongest model, one run a day at six, eight dollars a run and fifty a week
// at most (the owner's figure: lessons are worth it).
const (
	DefaultLessonsModel      = "opus"
	DefaultLessonsAt         = "06:00"
	DefaultLessonsBudget     = 8.0
	DefaultLessonsWeekBudget = 50.0
)

// Lessons is the crew's lessons triage. Off unless the crew file turns it on: `lessons: on`, or a block (a block
// is on unless it says enabled: false). The repository is the crew's own agentic-os, found from its agents dir;
// Repo and Branch only override that. zmina is public: nothing here spends money or pushes unless asked for.
type Lessons struct {
	On bool `yaml:"-"`
	// Repo is the instruction repository (default: the git toplevel of the crew's agents dir), Branch the branch
	// a lessons MR targets (default: its origin's default branch).
	Repo   string `yaml:"repo"`
	Branch string `yaml:"branch"`
	// At is when the daily run may start (HH:MM, the crew's clock); Model the sorting seat's model; Budget the
	// most one run may cost, in API-equivalent dollars.
	At     string  `yaml:"at"`
	Model  string  `yaml:"model"`
	Budget float64 `yaml:"budget"`
	// WeekBudget is the most the crew's runs may cost in a week (from Monday, the crew's clock).
	WeekBudget float64 `yaml:"week_budget"`
	// Deny is a file of the crew's own words (client names) no lesson may carry, one a line.
	Deny string `yaml:"deny"`
	// Enabled false turns a block off without removing it.
	Enabled *bool `yaml:"enabled"`
}

// UnmarshalYAML reads on/off (true/false) or a block.
func (l *Lessons) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		switch strings.ToLower(strings.TrimSpace(n.Value)) {
		case "on", "true", "yes":
			*l = Lessons{On: true}
		case "off", "false", "no", "":
			*l = Lessons{}
		default:
			return fmt.Errorf("lessons: %q: want on, off or a block", n.Value)
		}

		return nil
	case yaml.MappingNode:
		type plain Lessons

		var p plain
		if err := n.Decode(&p); err != nil {
			return fmt.Errorf("lessons: %w", err)
		}

		*l = Lessons(p)
		l.On = l.Enabled == nil || *l.Enabled

		return nil
	}

	return fmt.Errorf("lessons: want on, off or a block")
}

// MarshalYAML writes on/off for a plain switch, else the block.
func (l Lessons) MarshalYAML() (any, error) {
	if l.Repo == "" && l.Branch == "" && l.At == "" && l.Model == "" && l.Budget == 0 && l.WeekBudget == 0 &&
		l.Deny == "" && l.Enabled == nil {
		if l.On {
			return "on", nil
		}

		return nil, nil
	}

	type plain Lessons

	return plain(l), nil
}

// RunAt is the time of day the run may start.
func (l Lessons) RunAt() (hour, minute int) {
	t, err := time.Parse("15:04", strings.TrimSpace(or(l.At, DefaultLessonsAt)))
	if err != nil {
		t, _ = time.Parse("15:04", DefaultLessonsAt)
	}

	return t.Hour(), t.Minute()
}

// RunModel is the sorting seat's model.
func (l Lessons) RunModel() string { return or(strings.TrimSpace(l.Model), DefaultLessonsModel) }

// RunBudget is the most one run may cost.
func (l Lessons) RunBudget() float64 {
	if l.Budget > 0 {
		return l.Budget
	}

	return DefaultLessonsBudget
}

// RunWeekBudget is the most a week of runs may cost.
func (l Lessons) RunWeekBudget() float64 {
	if l.WeekBudget > 0 {
		return l.WeekBudget
	}

	return DefaultLessonsWeekBudget
}

// check reports what is wrong with the block.
func (l Lessons) check() error {
	if l.At != "" {
		if _, err := time.Parse("15:04", strings.TrimSpace(l.At)); err != nil {
			return fmt.Errorf("lessons.at %q: want HH:MM", l.At)
		}
	}

	if l.Budget < 0 || l.WeekBudget < 0 {
		return fmt.Errorf("lessons.budget %v, week_budget %v: dollars above 0", l.Budget, l.WeekBudget)
	}

	return nil
}
